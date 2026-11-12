package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
	Split      bool `json:"split,omitempty"`
	Replaced   int  `json:"replaced,omitempty"`
	Summarized int  `json:"summarizeTokens,omitempty"`
	Kept       int  `json:"keptTokens,omitempty"`
	Attempts   int  `json:"attempts,omitempty"`
	// SummaryUsage is every summariser call this compaction made, failures included —
	// the line items behind the committed entry's usage total, and the whole record for a
	// round that committed nothing (a stop has no entry to attach a bill to).
	SummaryUsage  []ir.SummaryAttempt `json:"summaryUsage,omitempty"`
	Model         string              `json:"model,omitempty"`
	SummaryText   string              `json:"-"`
	SummaryTokens int                 `json:"summaryTokens,omitempty"`
	SummaryCap    int                 `json:"summaryCap,omitempty"`
	// Before is the real contextUsed before the commit; After is the derived estimate after
	// it (the real number only comes back with the next completion, spec §3.8.6).
	Before int `json:"before"`
	After  int `json:"after"`
	// Gate records what the user decided when the summary stayed oversized.
	Gate string `json:"gate,omitempty"`
	// Rounds is the replay round this result committed through: 1 for an ordinary compaction, N
	// when the catch-up replays (each round commits its own entry; this reports the last one).
	// Absent when nothing committed.
	Rounds int `json:"rounds,omitempty"`
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

// runCompact is the whole compaction run: plan, summarise, gate, commit — replayed until the
// context is back inside the line. manual decides only whether real usage is required (§1.1);
// compact.Select chooses the same cut either way.
//
// Replay is what "the conversation grew past the window" was always specced to mean: pretend the
// messages are still being appended one at a time and run the normal compaction at the message that
// tipped the context over — once per tipping point, in one breath. Round 1 summarises
// [cmp0?, ...msgX] into cmp1; round 2 plans against [cmp1, kept..., ...msgY] exactly like the next
// turn would have, and so on until the estimate of the whole real path (sizes.Total, the number
// every trigger reads) is back under threshold% of n_ctx. An ordinary compaction — a path that
// tipped once — runs exactly one round and behaves exactly as it always did: the loop is invisible,
// there is no new switch, and every rule (floors, acceptance, attempts, the gate) binds each round
// as if it were the only one. A gate skip aborts the whole replay: the rounds already committed are
// real compactions and stay, which is what a stop mid-catch-up meant in the one-message-at-a-time
// world this replays.
func (s *Session) runCompact(ctx context.Context, trig compact.Trigger, manual bool, instruction string) CompactResult {
	res := CompactResult{Trigger: trig, Manual: manual}
	for round := 1; ; round++ {
		var more bool
		res, more = s.compactOnce(ctx, trig, manual, instruction, round, res)
		if !more {
			return res
		}
	}
}

// maxReplayRounds guards the replay against a world gone wrong — not against a long conversation:
// every round commits a region of at least one entry (Select's floors), so the path shrinks
// monotonically and honest rounds run out long before the guard can be reached.
const maxReplayRounds = 100

// compactOnce runs one round of the replay: one normal compaction of this round's virtual path,
// committing at most one entry. It reports whether another round follows (a commit whose estimate
// is still over the line, commitRound). res carries across rounds and the report aggregates: the
// per-round fields describe the last round, SummaryUsage is every round's bill — while each
// committed entry carries only what its own round spent, never the run's running total.
func (s *Session) compactOnce(ctx context.Context, trig compact.Trigger, manual bool, instruction string, round int, res CompactResult) (CompactResult, bool) {
	cfg := s.Config()
	p := cfg.Compact

	// Planning input = assembler input: both come out of the single choke point (§3.2).
	built, sizes, tools, err := s.build()
	if err != nil {
		// The window could not be read: there is no planning input, and a compaction without one
		// is exactly the thing §1.1 refuses to guess its way through. Mid-replay the earlier rounds
		// keep — what committed is a real compaction, and nothing here guesses a second one.
		res.Skipped = "window read failed: " + err.Error()
		s.logf("compaction (%s): skipped: %v", trig, err)
		return res, false
	}
	region := built.Entries
	nctx := s.nctxLocked()
	res.Model = cfg.LLM.Model
	if nctx <= 0 {
		// Without n_ctx there is no basis for a budget, so refuse and keep everything (§3.10).
		res.Skipped = string(compact.ReasonNCTXUnknown)
		return res, false
	}
	before, available := s.usedNow()
	// A replay round has no "now" to measure: usedNow() was zeroed by the first commit (spec 1.5)
	// and is restored by the next completion, while usedNow's real subject — the payload before the
	// first commit — no longer exists. Its before is the estimate of what the path is.
	replay := round > 1
	if !manual && !available && !replay {
		res.Skipped = string(compact.ReasonNoUsage)
		return res, false
	}
	if replay {
		res.Before = sizes.Total()
	} else {
		res.Before = before
	}

	// This round plans against a virtual path: the real one cut at the entry whose estimate first
	// tips it past the trigger line (compact.Tipping). A path that never tips past the line by more
	// than one message — every ordinary compaction — is planned whole, exactly as it always was;
	// past that point each round is asked only about the bytes up to the tipping message, and the
	// entries behind it do not exist for this round's plan, request, or bill.
	level := p.TriggerLevel(nctx)
	virt, vsizes := compact.Tipping(region, sizes, level)
	if replay {
		s.logf("compaction (%s): replay round %d: %d of %d entries, the virtual path is ~%d tokens against the %d-token line",
			trig, round, len(virt), len(region), vsizes.Total(), level)
	}
	plan, ok := compact.Select(virt, vsizes, nctx, p)
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
		return res, false
	}
	res.SummaryCap = p.SummaryCap(nctx)
	if plan.KeptTokens > plan.Budget {
		// The kept tail is bigger than the budget the user configured, and minKeptMessages is the only
		// thing that can do that (§2.1). Say it out loud: a reader who compares this tail with keepBudget
		// finds no configuration that explains it, and starts doubting the arithmetic
		// instead of the floor they set.
		s.logf("compaction (%s): kept tail %d tokens over its %d-token budget (minKeptMessages %d wins over the cap)",
			trig, plan.KeptTokens, plan.Budget, p.MinKeptMessages)
	}

	// The summary request is cut from the last payload actually sent (§5.2), because that is the byte
	// sequence the provider's KV cache holds: a first call that starts anywhere else is a new
	// conversation, and "new conversation" at 180k tokens is eight minutes of GPU. After a restart
	// nothing was sent by this process, so the payload assembled now is the only honest prefix, and
	// the log says so.
	sentMsgs, sentIDs, _ := s.lastSentPayload()
	refs := summaryPayload(sentMsgs, sentIDs, built.All(), asm.EntryIDs(built), tools, len(built.Entries))
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
	// This round's bill starts here: every call record appends from this line to the commit belongs
	// to the entry this round commits — a gate retry's extra attempts included, because those calls
	// were this entry's money spent — while res keeps the same calls as the run's running total for
	// the report. Round 2 must not be billed for round 1's requests, and the entry written in round
	// 2 must not claim them either.
	start := len(res.SummaryUsage)

	for {
		var (
			text string
			size int
			err  error
		)
		// roundText/roundSize are the attempts of THIS round and only this round. res is the
		// run's carry-over across the replay — and its SummaryText/SummaryTokens still hold the
		// PREVIOUS round's committed summary when this round's every attempt fails. The oversized
		// gate reads res, so without this the gate would offer — and an Accept would commit —
		// a stale summary at a boundary that never produced it (live-fire 2026-11-12: round 3's
		// three failed attempts opened the gate quoting round 2's 3405 tokens).
		var (
			roundText string
			roundSize int
		)
		// One line per call the summariser actually made, into the result that carries every
		// outcome — committed, gated or skipped — because a summariser call costs what it costs
		// whether or not its text was kept.
		record := func(a ir.SummaryAttempt) { res.SummaryUsage = append(res.SummaryUsage, a) }
		for attempt := 1; attempt <= tries; attempt++ {
			res.Attempts = attempt
			text, size, err = s.summarize(ctx, cfg, p, plan, virt, vsizes, refs, extra, attempt, round, record)
			if err != nil {
				s.logf("compaction (%s): attempt %d failed: %v", trig, attempt, err)
				extra = ""
				continue
			}
			res.SummaryTokens = size
			res.SummaryText = text
			roundText, roundSize = text, size
			if p.Accepts(nctx, size) {
				return s.commitRound(ctx, cfg, p, plan, text, size, summaryModel, res, res.SummaryUsage[start:], level, round)
			}
			// The percentage printed beside the cap is the config's own summaryCap — the number
			// that sized this very request and now grades its answer; there is no second number
			// left to misprint.
			s.logf("compaction (%s): attempt %d produced %d tokens, cap is %d (summaryCap %d%% of n_ctx)",
				trig, attempt, size, res.SummaryCap, p.SummaryCapPct)
			extra = fmt.Sprintf("your previous attempt was %d tokens and the cap is %d: compress further, drop detail instead of structure",
				size, res.SummaryCap)
		}
		// the attempt of a later replay round must not be measured against an earlier
		// round's kept summary, which res still carries when this round produced nothing
		if roundText == "" {
			// Every attempt failed on the provider: fail open, keep everything (§1.1). Mid-replay the
			// earlier rounds keep — res still reports the entry they committed.
			res.Skipped = "summary generation failed"
			if err != nil {
				res.Skipped += ": " + err.Error()
			}
			return res, false
		}
		// Oversized after maxAttempts: the three-option gate (§3.4). The compaction lock is
		// still held while waiting, so nothing else can write during the wait.
		s.logf("compaction (%s): oversized after %d attempt(s): the summary is %d tokens and the cap is %d",
			trig, p.MaxAttempts, roundSize, res.SummaryCap)
		decision := s.askGate(ctx, &res)
		switch decision {
		case GateAccept:
			res.Gate = string(GateAccept)
			return s.commitRound(ctx, cfg, p, plan, roundText, roundSize, summaryModel, res, res.SummaryUsage[start:], level, round)
		case GateRetry:
			tries = p.MaxAttempts
			if tries < 1 {
				tries = 1
			}
			extra = fmt.Sprintf("the earlier attempts were %d tokens and the cap is %d: this must fit", roundSize, res.SummaryCap)
			continue
		default: // skip / aborted: history is left exactly as it was (§3.4) — the whole replay ends,
			// committed rounds included; a person said stop, not "stop after the next entry fits"
			res.Gate = string(GateSkip)
			res.Skipped = "user chose to keep the full history"
			return res, false
		}
	}
}

// ─── what a summary is cut from ─────────────────────────────────
//
// A summary is not asked about the conversation in the abstract; it is asked about a particular
// sequence of bytes, and the cheaper candidate is always the one already sitting in the provider's KV
// cache. Which one that is a fact about what this process has sent, so it is worked out once, here,
// and the answer is carried to the round's request.
type payloadRefs struct {
	// msgs is the payload a summary is cut from: the messages that went on the wire, or the ones
	// assembled now when nothing matches them.
	msgs []asm.Message
	// ids[i] is the entry behind msgs[i]; empty when the payload was recorded before ids were kept.
	ids []string
	// off is the number of messages before the first entry: 1 with a system prompt, 0 without. The
	// assembler builds the two lists in step (§2.1), so a region index is a message index plus this.
	off int
	// tools is the tool array of that same request. A chat template renders the definitions before the
	// messages, so a summary asked without them can reuse nothing — and is asked about a conversation
	// in which the assistant had no tools either.
	tools []llm.Tool
	// warm says the payload was sent by this process, so a prefix of it is a prefix of what the
	// provider has cached. When it is false the summary still runs — a summary is worth eight minutes
	// of prefill — but the log says what it costs, and nothing here may claim a cache it does not have.
	warm bool
}

// summaryPayload decides what a summary is cut from, and what may then be claimed about the cache.
// Those are two decisions and they used to be one, which is how the second one came to contradict the
// first: the bytes come from the payload assembled now, because that is the payload that describes
// every entry of the path, while the sent payload is only ever a prefix of it and a part reaching past
// that prefix still has to be summarised. The claim follows the prefix rule, because that is what a KV
// cache is: the bytes the provider already holds are worth their bytes only while they are still the
// front of the path. One entry differing — a time travel, a deleted message, an assembler setting
// flipped mid-run, a restart — and there is nothing to reuse, so the log stops saying otherwise.
func summaryPayload(sent []asm.Message, sentIDs []string,
	cur []asm.Message, curIDs []string, curTools []llm.Tool, entries int) payloadRefs {
	// Where the entries begin in the payload is a fact about that payload's own shape, told by the
	// marker EntryIDs leaves on the one message that is not an entry — not a subtraction against an
	// entry count that may have come from a different payload.
	off := asm.HeadLen(curIDs)
	if len(cur) != off+entries {
		// ids does not describe msgs, so no boundary through it can be trusted. Everything still gets
		// summarised — the parts fall back to a flat transcript and say so in the log — because a
		// summary that is merely slow beats a compaction that refuses to run.
		return payloadRefs{tools: curTools}
	}
	// Warmth is a claim about bytes, so it is checked as bytes and not as ids. The ids say the two
	// payloads describe the same conversation in the same order; the comparison says the front they
	// share really is the same request. The second half is not decoration: an edited entry keeps its
	// id, so ids alone will call an edited message a prefix, and a stale prefix is not a slow summary —
	// it is a summary of a conversation that no longer exists, billed to a cache that was never filled.
	//
	// An id list of nothing but empty strings says the payload came from a caller that never named its
	// entries, which is a payload no claim can be checked against: no ids means no claim.
	warm := len(sent) > 0 && named(sentIDs) && samePrefix(sentIDs, curIDs) && sameBytes(sent, cur)
	return payloadRefs{msgs: cur, ids: curIDs, off: off, tools: curTools, warm: warm}
}

// sameBytes reports whether sent is still the front of cur, message for message. Deep equality is the
// point: one assembler builds both, so anything but an equal prefix means the bytes the provider
// cached are not the bytes about to be sent.
func sameBytes(sent, cur []asm.Message) bool {
	if len(sent) > len(cur) {
		return false
	}
	for i := range sent {
		if !reflect.DeepEqual(sent[i], cur[i]) {
			return false
		}
	}
	return true
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

// through is a round's prefix: the payload from its first message through the end of entry hi-1 —
// the planning path up to the cut, in the bytes the provider already holds wherever they are warm.
// It is the conversation the summary is asked about and the cache boundary in the same slice: what
// follows it on the path is, for this round, not there yet.
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

// summarize performs one summary attempt of the replay round with the complete (non-streaming)
// call of §3.5. The request is cut from the round's virtual path: the payload bytes up to the
// plan's cut — [head?][cmp0?][...msgX][instruction] — so the model is asked about exactly what the
// round chose to replace, in the bytes the provider has cached where they are warm, and the entries
// behind the cut are neither sent nor paid for twice. Where those bytes cannot carry the question
// the region goes as a flat transcript instead, which is lossy but honest. record sees every
// summariser call this attempt made — kept, refused or broken — because the money was spent per
// call, not per kept text.
func (s *Session) summarize(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan,
	virt []*ir.Entry, vsizes *asm.Sizes, refs payloadRefs, extra string, attempt int, round int, record func(ir.SummaryAttempt)) (string, int, error) {

	nctx := s.nctxLocked()
	sumRegion := virt[:plan.Cut]
	instrTok := compact.TextTokens(p.SummaryInstruction(plan.Split)) + compact.TextTokens(extra)

	// The round's conversation is the planning path's own byte prefix, ending at the cut.
	prefix, sliced := refs.through(plan.Cut)
	call := p.BuildRequest(nctx, prefix, refs.tools, plan.Split, extra)
	if p.SummaryMode != compact.ModePrefixStable || !sliced || vsizes.Total()+instrTok > nctx {
		// Three roads to the flat transcript, all of them "these bytes cannot carry this question".
		// Shape B (mode "transcript") asks with no cache affinity, so the region itself is the payload
		// (spec §3.1). A payload whose ids did not describe its messages has nothing that may be
		// sliced at any boundary. And a planning path that still overfits the window when cut from
		// bytes — one pasted document larger than the window is enough — can only be asked about
		// flattened. Left alone, the last two are a request of two messages — a system prompt and
		// "compress the conversation above" — over a conversation the model was never given, and the
		// answer would pass acceptance and replace the path with a fiction. The transcript is lossy
		// (structuredContent is not in it); a summary of nothing is not lossy, it is wrong.
		call = p.BuildRequest(nctx, nil, refs.tools, plan.Split, extra)
		src := sumRegion
		if sliced && vsizes.Total()+instrTok > nctx {
			// Every entry bigger than what one request could read is clipped to its beginning and end;
			// a path that overfits without any single oversized entry can only hope the flattening
			// itself saved enough — the structuredContent a transcript drops is all it has to give.
			free := (nctx - instrTok) / 2
			var clipped int
			src, clipped = compact.ClipOversized(sumRegion, free)
			s.logf("compaction: the request cut from the payload bytes would be ~%d tokens against a %d-token window; %d entries go as a flat transcript (%d oversized messages read only at their beginning and end)",
				vsizes.Total()+instrTok, nctx, len(src), clipped)
		} else if !sliced {
			s.logf("compaction: the payload could not be sliced, so %d entries go to the summariser as a flat transcript", len(src))
		}
		call.Messages = append(call.Messages, asm.Message{
			Role:    "user",
			Content: "Conversation to compress:\n\n" + compact.Transcript(src),
		})
	}
	req := summaryRequest(cfg, call)
	// The summary's prefix arrives as references — what this process remembers
	// sending — and owes the same expansion the main send does: byte-identical
	// pixels, or a refusal that says which blob is gone.
	wireMsgs, err := asm.ExpandMessages(req.Messages, logBlobs{log: s.log})
	if err != nil {
		record(ir.SummaryAttempt{Attempt: attempt, Failed: err.Error()})
		return "", 0, err
	}
	req.Messages = wireMsgs
	s.emit(Event{Type: EvtCompact, Data: map[string]any{"stage": "summarizing", "attempt": attempt, "round": round}})
	started := time.Now()
	res, err := s.provider.Complete(ctx, req)
	att := ir.SummaryAttempt{Attempt: attempt, Ms: time.Since(started).Milliseconds()}
	if err != nil {
		// Nil-safe for the same reason as the chain: a failed call has a Result only if the
		// provider read one before it died.
		if res != nil && res.Usage != nil {
			att.Usage = res.Usage
		}
		att.Failed = err.Error()
		record(att)
		return "", 0, err
	}
	att.Usage = res.Usage
	text := strings.TrimSpace(res.Content)
	if text == "" {
		reason, tokens := "unknown", "n/a"
		if res.FinishReason != "" {
			reason = res.FinishReason
		}
		if res.Usage != nil {
			tokens = fmt.Sprint(res.Usage.CompletionTokens)
		}
		att.Failed = fmt.Sprintf("empty summary (finish_reason %s, %s completion tokens, %d characters of reasoning)",
			reason, tokens, len(res.Reasoning))
		record(att)
		return "", 0, errors.New(att.Failed)
	}
	record(att)
	reported, reasoned := 0, len(res.Reasoning)
	if res.Usage != nil {
		reported = res.Usage.CompletionTokens
	}
	return text, compact.SummarySize(reported, reasoned, text), nil
}

// commitRound commits one replay round and decides whether another one follows.
//
// The entry carries only the summariser calls this round made — res has been accumulating every
// round's since the run began, and writing that running list onto the second entry would bill
// round 1's requests a second time (the total of a catch-up must stay the sum of its entries, not
// grow into a triangle). The next round exists when the post-commit estimate is still over the line
// that started the replay; an estimate of 0 means the post-commit rebuild failed, and a round with
// no number to plan against stops rather than spins in the dark.
func (s *Session) commitRound(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan,
	text string, size int, model string, res CompactResult, roundAtts []ir.SummaryAttempt, level int, round int) (CompactResult, bool) {
	res = s.commitCompaction(ctx, cfg, p, plan, text, size, model, res, roundAtts)
	res.Rounds = round
	if !res.Committed || res.After == 0 || res.After <= level {
		return res, false
	}
	if round >= maxReplayRounds {
		s.logf("compaction (%s): replay stopped at the round cap with the context still ~%d tokens against the %d-token line — something is wrong with the sizing, not with the conversation",
			res.Trigger, res.After, level)
		return res, false
	}
	return res, true
}

// commitCompaction writes the entry and advances the path tip in one storage write (§3.8).
//
// roundAtts is the bill of the round that is committing: every summariser call it made, and only
// its own — the run's running total lives in res for the report, while the entry tells exactly what
// this compaction cost (§2.4's honest accounting: a compaction entry cost what its own calls cost).
//
// It checks the context first because a summary is one entry written at the end or nothing at all, and the
// gap between the last token and this line is a moment somebody could have spent stopping it: a boundary
// arriving after a stop is a change to a path whose owner just said stop, and the text in hand is worth
// nothing to a session that stopped asking. This is what makes "nothing is written until it finishes" true
// even for a stop that lands on the final turn of the summary - which is the promise the composer's square
// and the summary row of the API both make.
func (s *Session) commitCompaction(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan, text string, size int, model string, res CompactResult, roundAtts []ir.SummaryAttempt) CompactResult {
	if err := ctx.Err(); err != nil {
		res.Skipped = "nothing was written: the summary was ended before it could be committed"
		return res
	}
	meta := compact.MetaOf(p, plan, model, res.Before, size)
	meta.Summary = roundAtts
	content := p.Prefix(text)
	entry, err := s.appendEntry(&ir.Entry{
		Kind:    ir.KindCompaction,
		Role:    "user",
		Content: content,
		// The entry's own usage is the total of the summariser calls that wrote it, refused
		// retries included: those requests are exactly what this entry cost, and the cost bill
		// can then count a compaction like any other request without learning about meta. Under
		// replay this holds per entry — summing a catch-up's entries is its true total, never a
		// re-count of an earlier round.
		Usage:      summaryTotal(roundAtts),
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
	_, sizes, _, err := s.build()
	if err != nil {
		// The summary is committed either way; "after" is a number the report would like, not a
		// decision, and a dashboard that sees 0 beside a committed compaction reads the log line.
		s.logf("compaction: post-commit size: %v", err)
	} else {
		res.After = sizes.Total()
	}
	res.Committed = true
	res.EntryID = entry.ID
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

// summaryTotal adds the usage every recorded summariser call reported into one number for
// the committed entry. nil when no call reported anything: an unknown total is said as
// unknown, never as zero — zero would bill a compaction as free.
func summaryTotal(atts []ir.SummaryAttempt) *ir.Usage {
	var t ir.Usage
	seen := false
	for _, a := range atts {
		if a.Usage == nil {
			continue
		}
		seen = true
		t.PromptTokens += a.Usage.PromptTokens
		t.CacheTokens += a.Usage.CacheTokens
		t.CompletionTokens += a.Usage.CompletionTokens
		t.HasPrompt = t.HasPrompt || a.Usage.HasPrompt
		t.HasCompletion = t.HasCompletion || a.Usage.HasCompletion
	}
	if !seen {
		return nil
	}
	return &t
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
	why := ""
	select {
	case d := <-ch:
		decision = d.answer
	case <-ctx.Done():
		// Not a person's skip: the summary's own context died (Stop, an interrupt, or the
		// gate opening over an already-cancelled run). Say so — "gate decision: skip" after
		// a stop reads like someone clicked the button (live-fire 2026-11-12).
		decision, why = GateSkip, " (nothing waited: the summary was ended)"
	case <-time.After(30 * time.Minute):
		decision, why = GateSkip, " (nobody answered within 30 minutes)"
	}
	s.emit(Event{Type: EvtRequestDone, Info: id})
	s.logf("gate decision: %s%s", decision, why)
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
