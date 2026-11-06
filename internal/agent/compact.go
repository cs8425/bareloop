package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// CompactResult is what one compaction attempt reports. Every path out of the compactor
// returns a reason (spec §3.9: never fail silently — the inspector and the log line carry
// the exact reason a compaction did not happen).
type CompactResult struct {
	// Noop means the caller should not even report it (below trigger, trigger disabled…).
	Noop bool `json:"noop,omitempty"`
	// Committed means a compaction entry was written.
	Committed bool `json:"committed"`
	// Skipped is the human-readable reason nothing was committed.
	Skipped string `json:"skipped,omitempty"`

	Trigger  compact.Trigger `json:"trigger,omitempty"`
	Manual   bool            `json:"manual,omitempty"`
	EntryID  string          `json:"entryId,omitempty"`
	Boundary string          `json:"boundary,omitempty"`
	// Empty marks compact-everything: the boundary field is absent (spec §2.5).
	Empty bool `json:"empty,omitempty"`
	// Split marks a cut that is not a user-message span boundary (§2.3): the summary covers part of a
	// turn, or the whole path when Empty.
	Split         bool   `json:"split,omitempty"`
	Replaced      int    `json:"replaced,omitempty"`
	Summarized    int    `json:"summarizeTokens,omitempty"`
	Kept          int    `json:"keptTokens,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	Model         string `json:"model,omitempty"`
	SummaryText   string `json:"-"`
	SummaryTokens int    `json:"summaryTokens,omitempty"`
	SummaryCap    int    `json:"summaryCap,omitempty"`
	// Before is the real contextUsed before the commit; After is the derived estimate after
	// it (the real number only comes back with the next completion, spec §3.8.6).
	Before int `json:"before"`
	After  int `json:"after"`
	// Gate records what the user decided when the summary stayed oversized.
	Gate string `json:"gate,omitempty"`
}

// compactHere is the entry point used by the loop (T1/T2) and by the pre-send guard.
// It performs the cheap checks that keep the caller from taking the lock at all.
func (s *Session) compactHere(ctx context.Context, trig compact.Trigger, force bool) CompactResult {
	cfg := s.Config()
	manual := trig == compact.TriggerManual
	res := CompactResult{Trigger: trig, Manual: manual}
	if !manual && !cfg.Compact.Auto {
		res.Noop = true
		res.Skipped = string(compact.ReasonAutoOff)
		return res
	}
	if trig == compact.TriggerInLoop && !cfg.Compact.UseInLoop() {
		res.Noop = true
		res.Skipped = string(compact.ReasonInLoopOff)
		return res
	}
	if s.isGenerating() && !force {
		// Spec §1.1: compaction never runs while a generation is in flight.
		res.Skipped = string(compact.ReasonNotStreaming)
		return res
	}
	if !manual && !force {
		nctx := s.nctxLocked()
		used, available := s.usedNow()
		if nctx <= 0 {
			res.Noop = true
			res.Skipped = string(compact.ReasonNCTXUnknown)
			return res
		}
		if !available {
			res.Noop = true
			res.Skipped = string(compact.ReasonNoUsage)
			return res
		}
		// The trigger is a comparison against the last real measurement, and a forced compaction is
		// exactly the case where that number is known to be wrong: the provider refused the payload,
		// or the pre-send guard measured more than the window. Skipping then left the session bricked,
		// because every later turn sends the same oversized payload (observed: "could not shrink it
		// (context usage below threshold)" next to a 185k-token request against a 40960 window).
		// force overrides this measurement only - never the policy above it (auto off, in-loop off),
		// so "never compact automatically" is still respected and the caller says what to do instead.
		if !cfg.Compact.ShouldTrigger(nctx, used, available) {
			res.Noop = true
			res.Skipped = string(compact.ReasonBelowTrigger)
			return res
		}
	}
	return s.doCompact(ctx, trig, manual, "")
}

// CompactNow is trigger T3: the user pressed the button. Manual compaction bypasses the
// trigger condition and the need for real usage, never the safety floors and never the cut
// itself (§1.1), and it still runs the full acceptance gate.
//
// The summary belongs to the session rather than to the request that asked for it: nothing the work
// produces depends on the caller still being there, and a closed tab or a proxy that gave up is not a
// change of mind. So it runs on a context of its own, and the only hand on its cancellation is Stop() -
// which is also the only way to end one early, and ends it by committing nothing (the path is simply
// still the one it was, and the next compaction starts from scratch).
func (s *Session) CompactNow(ctx context.Context, instruction string) (CompactResult, error) {
	res := CompactResult{Trigger: compact.TriggerManual, Manual: true}
	if s.isGenerating() {
		return res, errors.New(string(compact.ReasonNotStreaming) + " — abort the run first, then compact")
	}
	// why a session holding an unanswered tool call is refused rather than allowed: it reports "running"
	// and is not generating, so nothing else stops a compaction from cutting the path at that call — and
	// the wake would then find the call it exists to answer sitting behind a summary boundary, with the
	// result it produces a second answer to a call that already has one.
	if s.isAnswering() {
		return res, errors.New(string(compact.ReasonUnanswered) + " — let it answer or interrupt it, then compact")
	}
	if !s.compMu.TryLock() {
		return res, errors.New(string(compact.ReasonAlreadyActive))
	}
	defer s.compMu.Unlock()

	ctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	// Announced the way doCompact announces its own, for the same three reasons: status says so, the
	// composer has something to press, and Send starts no run in the middle of a summary. Deliberately
	// not leaveCompaction, which continues a run when the path ends on a question - asking for a summary
	// is not asking for another turn.
	s.mu.Lock()
	s.compactCancel = cancel
	s.compacting = true
	if s.phase == phaseIdle {
		s.phase = phaseCompacting
	}
	s.mu.Unlock()
	s.emitState()
	defer s.endManualCompaction()

	r := s.runCompact(ctx, compact.TriggerManual, true, instruction)
	return r, nil
}

// endManualCompaction puts back what CompactNow announced, in the order leaveCompaction uses: the flags
// first and the state event after, so no reader is told about a summary by the event that ends it.
//
// Then it releases whatever waited behind the summary. That is safe here and only here, and the reason is
// worth keeping next to the line: a manual summary cannot share its session with a debt, because CompactNow
// refuses to start while a run is generating or while the path owes an answer. So anything in this session's
// queue is a message a person typed while the summary was running, and draining it cannot be writing
// between a tool call and its result - the hole flushHeld exists to avoid.
//
// If the flush wrote anything, this is the one case where a manual summary asks for a run: not because a
// summary wants a turn (it never does, which is why this deliberately is not leaveCompaction), but because
// a message that just landed is a question with no other owner. A summary that was skipped or stopped
// appended nothing, so a path that was already holding an older unanswered question keeps holding it.
func (s *Session) endManualCompaction() {
	s.mu.Lock()
	s.compactCancel = nil
	s.compacting = false
	if s.phase == phaseCompacting {
		s.phase = phaseIdle
	}
	// A summary holds a queue, and a held queue is a promise (park.go). This is where the promise is paid:
	// the flush below writes what waited, and the run started after it is the one that answers. Cleared here
	// rather than left for some run's epilogue to notice, because this function owns the queue from this
	// moment and a promise kept twice would be a run nobody asked for.
	s.acceptedWhileBusy = false
	s.mu.Unlock()
	s.emitState()
	if s.flushHeld() == 0 {
		return
	}
	s.mu.Lock()
	if s.phase == phaseIdle {
		s.startRunLocked(context.Background(), 0)
	}
	s.mu.Unlock()
}

// manualSummaryLocked reports whether this session is inside a manual summary - the one compaction a person
// asked for, and the only one whose cancellation the session owns, since compactCancel is written by
// CompactNow and cleared on every way out of it. A run's own in-loop summary is not this: that session has a
// run holding its messages, and answeringLocked already covers it. Callers hold s.mu.
func (s *Session) manualSummaryLocked() bool { return s.compactCancel != nil }

// enterCompaction takes the compaction lock. Spec §1.4 requires the lock to be taken before
// the first await, so the window "summary generated, entry not yet committed" cannot be
// entered twice and cannot overlap the workflow.
func (s *Session) enterCompaction() bool {
	if !s.compMu.TryLock() {
		return false
	}
	s.mu.Lock()
	s.compacting = true
	if s.phase == phaseIdle {
		s.phase = phaseCompacting
	}
	s.mu.Unlock()
	s.emitState()
	return true
}

// leaveCompaction puts back what enterCompaction announced. It deliberately does not drain anything held: the
// run that asked for this summary owns the queue and writes it at its own turn boundary (turns), and draining
// here would put a message between a tool call and its result. All it does is continue the loop when the path
// now ends on a question nobody has answered (§1.4/§6), which is the state an end-of-turn compaction leaves.
func (s *Session) leaveCompaction() {
	s.mu.Lock()
	s.compacting = false
	if s.phase == phaseCompacting {
		s.phase = phaseIdle
	}
	s.mu.Unlock()
	s.compMu.Unlock()
	s.emitState()
	// A path ending on a question nobody answered is the state an end-of-turn compaction leaves behind, and
	// it needs a run to read it (§1.4/§6).
	if s.endsWithUserMsg() {
		s.mu.Lock()
		if s.phase == phaseIdle {
			s.startRunLocked(context.Background(), 0)
		}
		s.mu.Unlock()
	}
}

// doCompact wraps runCompact with the lock.
func (s *Session) doCompact(ctx context.Context, trig compact.Trigger, manual bool, instruction string) CompactResult {
	if !s.enterCompaction() {
		r := CompactResult{Trigger: trig, Manual: manual}
		r.Skipped = string(compact.ReasonAlreadyActive)
		r.Noop = !manual
		return r
	}
	defer s.leaveCompaction()
	return s.runCompact(ctx, trig, manual, instruction)
}

// runCompact is the whole compaction run: plan, summarise, gate, commit. manual decides only
// whether real usage is required (§1.1); compact.Select chooses the same cut either way.
func (s *Session) runCompact(ctx context.Context, trig compact.Trigger, manual bool, instruction string) CompactResult {
	cfg := s.Config()
	p := cfg.Compact
	res := CompactResult{Trigger: trig, Manual: manual}

	// Planning input = assembler input: both come out of the single choke point (§3.2).
	built, sizes, tools := s.build()
	region := built.Entries
	nctx := s.nctxLocked()
	res.Model = cfg.LLM.Model

	before, available := s.usedNow()
	res.Before = before
	// The size table walks the boundary — that is what its telescoping sum is for (§2.4) — but it must
	// never price one entry, so the summary's own arithmetic gets a ruler of its own. It reads the same
	// usage the table reads, per reply: between two of the server's own prompt counts sits the measured
	// size of everything in between, spread over the entries that caused it rather than dropped on the
	// last one. What the server never counted falls back to an estimate, and only that estimate is
	// calibrated against the request total (§0).
	var realTotal int
	if available {
		realTotal = before
	}
	ruler := compact.NewRuler(built.Entries, built.StructuredCap, realTotal)
	if nctx <= 0 {
		// Without n_ctx there is no basis for a budget, so refuse and keep everything (§3.10).
		res.Skipped = string(compact.ReasonNCTXUnknown)
		return res
	}
	if !manual && !available {
		res.Skipped = string(compact.ReasonNoUsage)
		return res
	}
	plan, ok := compact.Select(region, sizes, nctx, p)
	if plan != nil {
		res.Replaced = plan.Replaced
		res.Summarized = plan.SummarizeTokens
		res.Kept = plan.KeptTokens
		res.Split = plan.Split
		res.Empty = plan.Empty
		if plan.BoundaryID != nil {
			res.Boundary = *plan.BoundaryID
		}
	}
	if !ok {
		res.Skipped = string(plan.Reason)
		s.logf("compaction (%s): skipped: %s", trig, plan.Reason)
		return res
	}
	res.SummaryCap = p.SummaryCap(nctx)
	if plan.KeptTokens > plan.Budget {
		// The kept tail is bigger than the budget the user configured, and minKeptMessages is the only
		// thing that can do that (§2.1). Say it out loud: a reader who compares this tail with threshold
		// and keepTarget finds no configuration that explains it, and starts doubting the arithmetic
		// instead of the floor they set.
		s.logf("compaction (%s): kept tail %d tokens over its %d-token budget (minKeptMessages %d wins over the cap)",
			trig, plan.KeptTokens, plan.Budget, p.MinKeptMessages)
	}

	// Decide the summary shape once, here, where the parameters are owned: the entry then records
	// the mode that actually ran. Recording "prefix-stable" while the serial chain folded a
	// 194k-token region is the kind of contradiction that costs an hour later, because the entry meta
	// is the durable record and the log line is not. summarize() keeps its own check as a net.
	sumRegion := region
	if plan.Cut < len(region) {
		sumRegion = region[:plan.Cut]
	}
	if p.SummaryMode != compact.ModeIterative && p.SummaryMode != compact.ModeMapReduce &&
		compact.NeedsIterative(sumRegion, ruler, nctx, p) {
		s.logf("compaction (%s): region of %d tokens does not fit a %d-token request, summarising serially",
			trig, compact.RegionTokens(sumRegion, ruler), p.ChunkFit(nctx))
		p.SummaryMode = compact.ModeIterative
	}

	// The summary request is cut from the last payload actually sent (§5.2), because that is the byte
	// sequence the provider's KV cache holds: a first call that starts anywhere else is a new
	// conversation, and "new conversation" at 180k tokens is eight minutes of GPU. After a restart
	// nothing was sent by this process, so the payload assembled now is the only honest prefix, and
	// the log says so.
	sentMsgs, sentIDs, sentTools := s.lastSentPayload()
	refs := summaryPayload(sentMsgs, sentIDs, sentTools, built.All(), asm.EntryIDs(built), tools, len(built.Entries))
	if !refs.warm {
		// Two different reasons, one honest sentence: nothing was sent by this process (a restart), or
		// something was and the path has moved since (a time travel, a deleted entry). Either way the
		// provider has nothing to reuse and the summary is worth what a full prefill costs.
		s.logf("compaction (%s): the payload this process sent is not a prefix of the path, so the summary is cut from the one assembled now: expect a full prefill", trig)
	}

	summaryModel := cfg.LLM.Model
	tries := p.MaxAttempts
	if tries < 1 {
		tries = 1
	}
	extra := strings.TrimSpace(instruction)

	for {
		var (
			text string
			size int
			err  error
		)
		for attempt := 1; attempt <= tries; attempt++ {
			res.Attempts = attempt
			text, size, err = s.summarize(ctx, cfg, p, plan, region, ruler, refs, extra, attempt)
			if err != nil {
				s.logf("compaction (%s): attempt %d failed: %v", trig, attempt, err)
				extra = ""
				continue
			}
			res.SummaryTokens = size
			res.SummaryText = text
			if p.Accepts(nctx, size) {
				return s.commitCompaction(ctx, cfg, p, plan, text, size, summaryModel, res)
			}
			// The cap is SummaryCap, i.e. keepTarget% of n_ctx, so the percentage printed beside it has
			// to be KeepTargetPct. Printing SummaryMaxPct here (max_tokens of the summary call, a
			// different number and a different setting) told a reader that an 8 192-token cap was "70% of
			// n_ctx" on a session whose config says keepTarget 20.
			s.logf("compaction (%s): attempt %d produced %d tokens, cap is %d (keepTarget %d%% of n_ctx)",
				trig, attempt, size, res.SummaryCap, p.KeepTargetPct)
			extra = fmt.Sprintf("your previous attempt was %d tokens and the cap is %d: compress further, drop detail instead of structure",
				size, res.SummaryCap)
		}
		if res.SummaryText == "" {
			// Every attempt failed on the provider: fail open, keep everything (§1.1).
			res.Skipped = "summary generation failed"
			if err != nil {
				res.Skipped += ": " + err.Error()
			}
			return res
		}
		// Oversized after maxAttempts: the three-option gate (§3.4). The compaction lock is
		// still held while waiting, so nothing else can write during the wait.
		decision := s.askGate(ctx, &res)
		switch decision {
		case GateAccept:
			res.Gate = string(GateAccept)
			return s.commitCompaction(ctx, cfg, p, plan, res.SummaryText, res.SummaryTokens, summaryModel, res)
		case GateRetry:
			tries = p.MaxAttempts
			if tries < 1 {
				tries = 1
			}
			extra = fmt.Sprintf("the earlier attempts were %d tokens and the cap is %d: this must fit", res.SummaryTokens, res.SummaryCap)
			continue
		default: // skip / aborted: history is left exactly as it was (§3.4)
			res.Gate = string(GateSkip)
			res.Skipped = "user chose to keep the full history"
			return res
		}
	}
}

// summarizeIterative runs the serial rolling summary (compact.ModeIterative, and the automatic
// fallback whenever the region is bigger than one summariser call can carry). Serial on purpose:
// this is a local model, so parallel parts would queue on the same GPU instead of helping.
//
// Every part is asked in the shape of §3.1 (see compact.ChunkRequest): the first part IS the prefix
// the loop already put in the provider's cache, and the parts after it are the payload's own
// messages behind the running summary. Cutting the region into parts is a decision about bytes, so
// it is made against the ruler and reported next to what the server counted.
func (s *Session) summarizeIterative(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan,
	cut []*ir.Entry, ruler *compact.Ruler, refs payloadRefs, extra string, attempt int) (string, int, error) {

	nctx := s.nctxLocked()
	chunks := compact.PlanChunks(cut, ruler, nctx, p)
	if len(chunks) == 0 {
		return "", 0, errors.New("nothing to summarise")
	}
	if p.SummaryMode == compact.ModeMapReduce {
		s.logf("compaction: summaryMode %q is not implemented yet, running the serial chain instead", compact.ModeMapReduce)
	}
	clipped := false
	for _, c := range chunks {
		if c.Clipped {
			clipped = true
		}
	}
	if note := compact.ChunkInstructionNote(len(chunks), clipped); note != "" {
		s.logf("compaction: %s (%d tokens to read, %d tokens fit one request)", note,
			compact.RegionTokens(cut, ruler), p.ChunkFit(nctx))
	}

	prev, reported := "", 0
	for i, c := range chunks {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		in := compact.ChunkInput{
			Head:  refs.head,
			Tools: refs.tools,
			Prev:  prev,
			Index: i + 1, Total: len(chunks),
			Split: plan.Split,
			Extra: extra,
		}
		// Entry number k of the region is message number k after the system head: the assembler builds
		// the two lists in step (§2.1), so a part boundary is an offset rather than a lookup.
		//
		// Part 1 takes the payload from its first message through the end of the part — that slice is
		// the cache hit, and the request that starts anywhere else pays for the conversation again.
		// It is also the whole part, so it carries no Chunk of its own.
		if i == 0 {
			in.Prefix, _ = refs.through(c.Start + len(c.Entries))
		}
		if len(in.Prefix) == 0 {
			if msgs, ok := refs.messages(c.Start, c.Start+len(c.Entries)); ok {
				in.Chunk = msgs
			} else {
				// The part is outside anything this process sent: a time travel, a deleted entry, or a
				// path rebuilt after a restart. Flattening it is worse for the cache and it loses a
				// structuredContent document the model would otherwise have been given, so it says so —
				// silently summarising less than the log claims is how this whole investigation started.
				s.logf("compaction: part %d/%d is outside the payload this process sent; its %d entries go as a flat transcript",
					i+1, len(chunks), len(c.Entries))
				in.Chunk = []asm.Message{{Role: "user",
					Content: "Conversation to compress:\n\n" + compact.Transcript(c.Entries)}}
			}
		}
		call := p.ChunkRequest(nctx, in)
		req := summaryRequest(cfg, call)
		s.emit(Event{Type: EvtCompact, Data: map[string]any{
			"stage": "summarizing", "attempt": attempt, "part": i + 1, "parts": len(chunks)}})
		res, err := s.provider.Complete(ctx, req)
		if err != nil {
			return "", 0, fmt.Errorf("part %d/%d: %w", i+1, len(chunks), err)
		}
		text := strings.TrimSpace(res.Content)
		if text == "" {
			return "", 0, fmt.Errorf("part %d/%d: empty summary", i+1, len(chunks))
		}
		if res.Usage != nil {
			reported = res.Usage.CompletionTokens
		}
		// The three numbers that used to live in three different places and could therefore disagree
		// forever: what the ruler said this part would cost, what the server counted, and what the
		// server could reuse. On one line they either agree or they name the bug.
		prompt, reused, answer := "n/a", "n/a", "n/a"
		if res.Usage != nil {
			answer = fmt.Sprint(res.Usage.CompletionTokens)
			if res.Usage.HasPrompt {
				prompt, reused = fmt.Sprint(res.Usage.PromptTokens), fmt.Sprint(res.Usage.CacheTokens)
			}
		}
		s.logf("compaction: part %d/%d read %d entries ≈%d tokens → prompt %s, reused %s (%s), answer %s",
			i+1, len(chunks), len(c.Entries), c.Tokens, prompt, reused,
			map[bool]string{true: "prefix of the last request", false: "payload rebuilt"}[refs.warm], answer)
		prev = text
	}
	if strings.TrimSpace(prev) == "" {
		return "", 0, errors.New("empty summary")
	}
	return prev, compact.SummarySize(reported, prev), nil
}

// ─── what a summary is cut from ─────────────────────────────────────────
//
// A summary is not asked about the conversation in the abstract; it is asked about a particular
// sequence of bytes, and the cheaper candidate is always the one already sitting in the provider's KV
// cache. Which one that is a fact about what this process has sent, so it is worked out once, here,
// and the answer is carried to every part of the chain.
type payloadRefs struct {
	// msgs is the payload a summary is cut from: the messages that went on the wire, or the ones
	// assembled now when nothing matches them.
	msgs []asm.Message
	// ids[i] is the entry behind msgs[i]; empty when the payload was recorded before ids were kept.
	ids []string
	// off is the number of messages before the first entry: 1 with a system prompt, 0 without. The
	// assembler builds the two lists in step (§2.1), so a region index is a message index plus this.
	off int
	// head is msgs[:off] alone: what every part of a chain repeats byte for byte, which for parts 2..N
	// is the only prefix there is.
	head []asm.Message
	// tools is the tool array of that same request. A chat template renders the definitions before the
	// messages, so a summary asked without them can reuse nothing — and is asked about a conversation
	// in which the assistant had no tools either.
	tools []llm.Tool
	// warm says the payload was sent by this process, so a prefix of it is a prefix of what the
	// provider has cached. When it is false the chain still runs — a summary is worth eight minutes of
	// prefill — but the log says what it cost, and nothing here may claim a cache it does not have.
	warm bool
}

// summaryPayload chooses between the request still in the provider's cache and the payload assembled
// now. The rule is the prefix rule, because that is what a KV cache is: the sent payload is worth its
// bytes only while it is still a prefix of the path. One entry differing — a time travel, a deleted
// message, an assembler setting flipped mid-run, a restart — and the provider has nothing to reuse,
// so the honest choice is the payload assembled now and the summary stops pretending to be cheap.
func summaryPayload(sent []asm.Message, sentIDs []string, sentTools []llm.Tool,
	cur []asm.Message, curIDs []string, curTools []llm.Tool, entries int) payloadRefs {
	msgs, ids, tools, warm := cur, curIDs, curTools, false
	// An id list of nothing but empty strings says the payload was assembled by a caller that never
	// named its entries, which is a payload that cannot be checked. Trusting it is what this function
	// replaced: a stale prefix is not a slow summary, it is a summary of a conversation that no longer
	// exists. So no ids means no claim, and the payload assembled now is used instead.
	if len(sent) > 0 && named(sentIDs) && samePrefix(sentIDs, curIDs) {
		msgs, ids, tools, warm = sent, sentIDs, sentTools, true
	}
	off := len(ids) - entries
	if off < 0 || off > len(msgs) || len(msgs) < off+entries {
		// ids does not describe msgs, so no boundary through it can be trusted. Everything still gets
		// summarised — the parts fall back to a flat transcript and say so in the log — because a
		// summary that is merely slow beats a compaction that refuses to run.
		return payloadRefs{tools: tools, warm: warm}
	}
	return payloadRefs{msgs: msgs, ids: ids, off: off, head: msgs[:off], tools: tools, warm: warm}
}

// samePrefix reports whether a is the front of b. A payload longer than the path is never a prefix of
// it: the path got shorter (a time travel, a rollback), which is exactly when the tail of the cache
// has been thrown away.
func samePrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// named reports whether every message of a payload is identified, which is the precondition of trusting
// samePrefix at all — a list of empty strings is a prefix of anything, and "anything" is precisely the
// answer that must not be yes.
func named(ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if id == "" {
			return false
		}
	}
	return true
}

// messages is the payload messages of region entries [lo:hi), and whether the payload describes them.
func (r payloadRefs) messages(lo, hi int) ([]asm.Message, bool) {
	if r.msgs == nil || lo < 0 || hi <= lo || r.off+hi > len(r.msgs) {
		return nil, false
	}
	return r.msgs[r.off+lo : r.off+hi], true
}

// through is part 1's prefix: the payload from its first message through the end of entry hi-1 — the
// whole of what the provider already holds that this part covers, which is what turns the first call
// of a chain into a cache hit instead of a new conversation of the same size.
func (r payloadRefs) through(hi int) ([]asm.Message, bool) {
	if r.msgs == nil || hi <= 0 || r.off+hi > len(r.msgs) {
		return nil, false
	}
	return r.msgs[:r.off+hi], true
}

// ─── the request every LLM call of a session is cut from ───────────────────
//
// A KV cache is keyed on bytes, and thinking is a byte-level change to the prompt: a server that is
// told to reason renders a different template around the same messages. So the summariser of a session
// that reasons must reason at the same level as the turn it is summarising, and no two calls of one
// conversation may differ in these fields by accident — which cannot be guaranteed from call sites
// that each build their own request, so there is one source and everything is cut from it.

// requestIdentity is the part of an llm.Request that must not vary between calls of one session: the
// model, the sampling, and above all the thinking switch and its level — here that is
// `reasoning_effort` plus whatever thinking knob the endpoint takes through `extra`.
func requestIdentity(cfg Config) llm.Request {
	return llm.Request{
		Model:           cfg.LLM.Model,
		Temperature:     cfg.Temperature,
		MaxTokens:       cfg.MaxTokens,
		ReasoningEffort: cfg.ReasoningEffort,
		Extra:           cfg.Extra,
	}
}

// summaryRequest is one summary call on top of that identity: the same thinking level as the turn the
// summary is about, and the same tools.
//
// Two fields are overridden on purpose — the summariser's own budget, and §3.5's request that a
// summariser be more deterministic than the conversation it reads — and both are sampling parameters
// applied after the prompt, so neither forks the prefix the way thinking does. Before this function
// existed the two call sites drifted by exactly one field that is not sampling: the loop sent
// `reasoning_effort` and the summary did not, so a chain of summaries over a 184k-token conversation
// reused 0 tokens of it. That is the price of a setting nobody chose.
func summaryRequest(cfg Config, call compact.SummaryCall) llm.Request {
	req := requestIdentity(cfg)
	temp := call.Temperature
	req.Messages = call.Messages
	req.Tools = call.Tools
	req.Temperature = &temp
	req.MaxTokens = call.MaxTokens
	return req
}

// summarize performs one summary attempt with the complete (non-streaming) call of §3.5.
func (s *Session) summarize(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan,
	region []*ir.Entry, ruler *compact.Ruler, refs payloadRefs, extra string, attempt int) (string, int, error) {

	// The summary request is itself bound by n_ctx. When the region does not fit one call there is
	// no way to summarise it in one piece, and the session is stuck too big to send and too big to
	// summarise - so fall back to the serial chain, which never asks for more than one fit.
	sumRegion := region
	if plan.Cut < len(region) {
		sumRegion = region[:plan.Cut]
	}
	switch p.SummaryMode {
	case compact.ModeIterative, compact.ModeMapReduce:
		return s.summarizeIterative(ctx, cfg, p, plan, sumRegion, ruler, refs, extra, attempt)
	default:
		if compact.NeedsIterative(sumRegion, ruler, s.nctxLocked(), p) {
			s.logf("compaction: region of %d tokens does not fit a %d-token request, summarising serially",
				compact.RegionTokens(sumRegion, ruler), p.ChunkFit(s.nctxLocked()))
			return s.summarizeIterative(ctx, cfg, p, plan, sumRegion, ruler, refs, extra, attempt)
		}
	}

	call := p.BuildRequest(s.nctxLocked(), refs.msgs, refs.tools, plan.Split, extra)
	if p.SummaryMode != compact.ModePrefixStable || len(refs.msgs) == 0 {
		// Shape B asks with no cache affinity, so the flat transcript of the region is the payload
		// (spec §3.1). The second half of the condition is the case shape B cannot be reached without:
		// a payload whose ids did not describe its messages, so there is nothing to send as a prefix.
		// Left alone that is a request of two messages — a system prompt and "compress the conversation
		// above" — over a conversation the model was never given, and the answer would pass acceptance
		// and replace the path with a fiction. The transcript is lossy (structuredContent is not in it);
		// a summary of nothing is not lossy, it is wrong.
		var cut []*ir.Entry
		if plan.Cut <= len(region) {
			cut = region[:plan.Cut]
		}
		if len(refs.msgs) == 0 {
			s.logf("compaction: the payload could not be sliced, so %d entries go to the summariser as a flat transcript", len(cut))
		}
		call.Messages = append(call.Messages, asm.Message{
			Role:    "user",
			Content: "Conversation to compress:\n\n" + compact.Transcript(cut),
		})
	}
	req := summaryRequest(cfg, call)
	s.emit(Event{Type: EvtCompact, Data: map[string]any{"stage": "summarizing", "attempt": attempt}})
	res, err := s.provider.Complete(ctx, req)
	if err != nil {
		return "", 0, err
	}
	text := strings.TrimSpace(res.Content)
	if text == "" {
		return "", 0, errors.New("empty summary")
	}
	reported := 0
	if res.Usage != nil {
		reported = res.Usage.CompletionTokens
	}
	return text, compact.SummarySize(reported, text), nil
}

// commitCompaction writes the entry and advances the path tip in one storage write (§3.8).
//
// It checks the context first because a summary is one entry written at the end or nothing at all, and the
// gap between the last token and this line is a moment somebody could have spent stopping it: a boundary
// arriving after a stop is a change to a path whose owner just said stop, and the text in hand is worth
// nothing to a session that stopped asking. This is what makes "nothing is written until it finishes" true
// even for a stop that lands on the final turn of the summary - which is the promise the composer's square
// and the summary row of the API both make.
func (s *Session) commitCompaction(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan, text string, size int, model string, res CompactResult) CompactResult {
	if err := ctx.Err(); err != nil {
		res.Skipped = "nothing was written: the summary was ended before it could be committed"
		return res
	}
	meta := compact.MetaOf(p, plan, model, res.Before, size)
	content := p.Prefix(text)
	entry, err := s.appendEntry(&ir.Entry{
		Kind:       ir.KindCompaction,
		Role:       "user",
		Content:    content,
		Compaction: &meta,
		Model:      model,
		Label:      fmt.Sprintf("compaction (%s)", res.Trigger),
	})
	if err != nil {
		res.Skipped = "commit failed: " + err.Error()
		s.logf("compaction commit failed: %v", err)
		return res
	}
	// Rebuild the in-memory session array with the assembler (§3.8 step 4). The loop re-reads
	// storage before every send, and new entries hang off the tip, which is now the
	// compaction entry — so the workflow attach point moved with it (§3.8 step 3).
	// The usage cached from the last completed turn priced the payload we just replaced, so it is
	// no longer a measurement of anything (spec 1.5). Drop it rather than substitute a number:
	// usedNow() then reports "unavailable", guardFit uses the built payload size (the only honest
	// answer for an unsent request), and the next completed turn restores the real value.
	s.mu.Lock()
	s.ctxUsed, s.ctxKnown = 0, false
	s.mu.Unlock()
	_, sizes, _ := s.build()
	res.Committed = true
	res.EntryID = entry.ID
	res.After = sizes.Total()
	res.SummaryCap = p.SummaryCap(s.nctxLocked())
	res.Replaced = plan.Replaced
	if res.SummaryText == "" {
		res.SummaryText = text
	}
	s.logf("compaction committed: entry=%s before=%d after~%d summary=%d/%d replaced=%d split=%v empty=%v",
		entry.ID, res.Before, res.After, res.SummaryTokens, res.SummaryCap, plan.Replaced, plan.Split, plan.Empty)
	s.emitState()
	return res
}

// askGate shows the three-option card and waits (§3.4). Aborting the wait counts as skip: so does the
// timeout, and so does the context this summary is running on dying (Stop ends a manual one, an interrupt
// ends an automatic one) - a card nobody can answer any more is a decision nobody is waiting for.
func (s *Session) askGate(ctx context.Context, res *CompactResult) GateAnswer {
	id := fmt.Sprintf("gate-%d", time.Now().UnixNano())
	ch := make(chan gateDecision, 1)
	s.mu.Lock()
	s.gate = ch
	s.gateID = id
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.gate = nil
		s.gateID = ""
		s.mu.Unlock()
	}()

	preview := res.SummaryText
	if len(preview) > 600 {
		preview = preview[:600] + "…"
	}
	s.emit(Event{Type: EvtRequest, Gate: &GateAsk{
		ID: id, SummarySize: res.SummaryTokens, SummaryCap: res.SummaryCap,
		Attempts: res.Attempts, Preview: preview,
	}})
	s.logf("compaction needs a decision: summary %d tokens, cap %d, %d attempts", res.SummaryTokens, res.SummaryCap, res.Attempts)

	decision := GateSkip
	select {
	case d := <-ch:
		decision = d.answer
	case <-ctx.Done():
		decision = GateSkip
	case <-time.After(30 * time.Minute):
		decision = GateSkip
	}
	s.emit(Event{Type: EvtRequestDone, Info: id})
	s.logf("gate decision: %s", decision)
	return decision
}

// AnswerGate delivers the user's answer to the oversized-summary gate.
func (s *Session) AnswerGate(id string, answer GateAnswer) error {
	s.mu.Lock()
	ch, gateID := s.gate, s.gateID
	s.mu.Unlock()
	if ch == nil || (id != "" && gateID != "" && id != gateID) {
		return fmt.Errorf("agent: no pending gate request (asked %q, waiting for %q)", id, gateID)
	}
	switch answer {
	case GateAccept, GateRetry, GateSkip:
	default:
		return fmt.Errorf("agent: unknown gate answer %q (accept|retry|skip)", answer)
	}
	ch <- gateDecision{answer: answer}
	return nil
}

// isGenerating reports whether an LLM stream is open right now (spec §1.1 forbids compaction
// while a generation is in flight; between turns is fine).
func (s *Session) isGenerating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generating
}

func (s *Session) setGenerating(v bool) {
	s.mu.Lock()
	s.generating = v
	s.mu.Unlock()
}
