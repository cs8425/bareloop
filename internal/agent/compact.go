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
	// Split marks a cut inside a user-message span (phase 2).
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
	return s.doCompact(ctx, trig, manual, force, "")
}

// CompactNow is trigger T3: the user pressed the button. Manual compaction bypasses the
// trigger condition and the protective budget floor, never the safety floors (§1.1), and it
// still runs the full acceptance gate.
func (s *Session) CompactNow(ctx context.Context, instruction string) (CompactResult, error) {
	res := CompactResult{Trigger: compact.TriggerManual, Manual: true}
	if s.isGenerating() {
		return res, errors.New(string(compact.ReasonNotStreaming) + " — abort the run first, then compact")
	}
	if !s.compMu.TryLock() {
		return res, errors.New(string(compact.ReasonAlreadyActive))
	}
	defer s.compMu.Unlock()
	r := s.runCompact(ctx, compact.TriggerManual, true, true, instruction)
	return r, nil
}

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

func (s *Session) leaveCompaction() {
	s.mu.Lock()
	s.compacting = false
	if s.phase == phaseCompacting {
		s.phase = phaseIdle
	}
	s.mu.Unlock()
	s.compMu.Unlock()
	s.emitState()
	// Any exit path releases the queued message (§1.4/§6): if the path now ends with a user
	// message nobody answered, continue.
	if s.endsWithUserMsg() {
		s.mu.Lock()
		if s.phase == phaseIdle {
			s.startRunLocked(context.Background(), 0)
		}
		s.mu.Unlock()
	}
}

// doCompact wraps runCompact with the lock.
func (s *Session) doCompact(ctx context.Context, trig compact.Trigger, manual, force bool, instruction string) CompactResult {
	if !s.enterCompaction() {
		r := CompactResult{Trigger: trig, Manual: manual}
		r.Skipped = string(compact.ReasonAlreadyActive)
		r.Noop = !manual
		return r
	}
	defer s.leaveCompaction()
	return s.runCompact(ctx, trig, manual, force, instruction)
}

// runCompact is the whole compaction run: plan, summarise, gate, commit.
func (s *Session) runCompact(ctx context.Context, trig compact.Trigger, manual, force bool, instruction string) CompactResult {
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
	if nctx <= 0 {
		// Without n_ctx there is no basis for a budget, so refuse and keep everything (§3.10).
		res.Skipped = string(compact.ReasonNCTXUnknown)
		return res
	}
	if !manual && !available {
		res.Skipped = string(compact.ReasonNoUsage)
		return res
	}
	plan, ok := compact.Select(region, sizes, nctx, p, manual || force)
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

	// Decide the summary shape once, here, where the parameters are owned: the entry then records
	// the mode that actually ran. Recording "prefix-stable" while the serial chain folded a
	// 194k-token region is the kind of contradiction that costs an hour later, because the entry meta
	// is the durable record and the log line is not. summarize() keeps its own check as a net.
	sumRegion := region
	if plan.Cut < len(region) {
		sumRegion = region[:plan.Cut]
	}
	if p.SummaryMode != compact.ModeIterative && p.SummaryMode != compact.ModeMapReduce &&
		compact.NeedsIterative(sumRegion, sizes, nctx, p) {
		s.logf("compaction (%s): region of %d tokens does not fit a %d-token request, summarising serially",
			trig, compact.RegionTokens(sumRegion, sizes), p.ChunkFit(nctx))
		p.SummaryMode = compact.ModeIterative
	}

	// The prefix of the summary request is the last payload actually sent (§5.2). After a
	// process restart nothing was sent by this process, so fall back to the payload the
	// assembler produces now and say so in the log.
	prefix, prefixTools := s.lastSentPayload()
	if len(prefix) == 0 {
		prefix = built.All()
		s.logf("compaction (%s): no payload sent yet in this process, summary prefix rebuilt from storage", trig)
	}
	if len(prefixTools) == 0 {
		prefixTools = tools
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
			text, size, err = s.summarize(ctx, cfg, p, plan, region, sizes, prefix, prefixTools, extra, attempt)
			if err != nil {
				s.logf("compaction (%s): attempt %d failed: %v", trig, attempt, err)
				extra = ""
				continue
			}
			res.SummaryTokens = size
			res.SummaryText = text
			if p.Accepts(nctx, size) {
				return s.commitCompaction(cfg, p, plan, text, size, summaryModel, res)
			}
			s.logf("compaction (%s): attempt %d produced %d tokens, cap is %d (%d%% of n_ctx)",
				trig, attempt, size, res.SummaryCap, p.SummaryMaxPct)
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
		decision := s.askGate(&res)
		switch decision {
		case GateAccept:
			res.Gate = string(GateAccept)
			return s.commitCompaction(cfg, p, plan, res.SummaryText, res.SummaryTokens, summaryModel, res)
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
func (s *Session) summarizeIterative(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan,
	cut []*ir.Entry, sizes *asm.Sizes, extra string, attempt int) (string, int, error) {

	nctx := s.nctxLocked()
	chunks := compact.PlanChunks(cut, sizes, nctx, p)
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
			compact.RegionTokens(cut, sizes), p.ChunkFit(nctx))
	}

	prev, reported := "", 0
	for i, c := range chunks {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		call := p.ChunkRequest(nctx, c.Entries, prev, i+1, len(chunks), plan.Split, extra)
		req := llm.Request{
			Model:       cfg.LLM.Model,
			Messages:    call.Messages,
			Tools:       call.Tools,
			Stream:      false,
			Temperature: &call.Temperature,
			MaxTokens:   call.MaxTokens,
			Extra:       cfg.Extra,
		}
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
		prev = text
	}
	if strings.TrimSpace(prev) == "" {
		return "", 0, errors.New("empty summary")
	}
	return prev, compact.SummarySize(reported, prev), nil
}

// summarize performs one summary attempt with the complete (non-streaming) call of §3.5.
func (s *Session) summarize(ctx context.Context, cfg Config, p compact.Params, plan *compact.Plan,
	region []*ir.Entry, sizes *asm.Sizes, prefix []asm.Message, tools []llm.Tool, extra string, attempt int) (string, int, error) {

	// The summary request is itself bound by n_ctx. When the region does not fit one call there is
	// no way to summarise it in one piece, and the session is stuck too big to send and too big to
	// summarise - so fall back to the serial chain, which never asks for more than one fit.
	sumRegion := region
	if plan.Cut < len(region) {
		sumRegion = region[:plan.Cut]
	}
	switch p.SummaryMode {
	case compact.ModeIterative, compact.ModeMapReduce:
		return s.summarizeIterative(ctx, cfg, p, plan, sumRegion, sizes, extra, attempt)
	default:
		if compact.NeedsIterative(sumRegion, sizes, s.nctxLocked(), p) {
			s.logf("compaction: region of %d tokens does not fit a %d-token request, summarising serially",
				compact.RegionTokens(sumRegion, sizes), p.ChunkFit(s.nctxLocked()))
			return s.summarizeIterative(ctx, cfg, p, plan, sumRegion, sizes, extra, attempt)
		}
	}

	call := p.BuildRequest(s.nctxLocked(), prefix, tools, plan.Split, extra)
	if p.SummaryMode != compact.ModePrefixStable {
		// Shape B: no cache affinity; the flat transcript of the region being replaced is the
		// payload (spec §3.1).
		var cut []*ir.Entry
		if plan.Cut <= len(region) {
			cut = region[:plan.Cut]
		}
		call.Messages = append(call.Messages, asm.Message{
			Role:    "user",
			Content: "Conversation to compress:\n\n" + compact.Transcript(cut),
		})
	}
	req := llm.Request{
		Model:       cfg.LLM.Model,
		Messages:    call.Messages,
		Tools:       call.Tools,
		Stream:      false,
		Temperature: &call.Temperature,
		MaxTokens:   call.MaxTokens,
		Extra:       cfg.Extra,
	}
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
func (s *Session) commitCompaction(cfg Config, p compact.Params, plan *compact.Plan, text string, size int, model string, res CompactResult) CompactResult {
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

// askGate shows the three-option card and waits (§3.4). Aborting the wait counts as skip.
func (s *Session) askGate(res *CompactResult) GateAnswer {
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
