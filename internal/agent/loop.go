package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// startRunLocked starts the agentic run. The caller must hold s.mu.
//
// The run gets its own context, independent of the HTTP/WS request that triggered it: the
// only way to stop it is Stop()/Interrupt(). That is what makes "instant abort" work even
// when the browser tab that started the run is gone.
func (s *Session) startRunLocked(ctx context.Context, _ int) {
	if s.phase != phaseIdle {
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.phase = phaseRunning
	s.cancel = cancel
	s.done = make(chan struct{})
	s.turn = 0
	go s.run(runCtx, cancel)
}

// Stop aborts whatever this session is doing right now. The partial assistant message is
// kept and marked interrupted — the user asked to stop output, not to lose it.
func (s *Session) Stop() error {
	s.mu.Lock()
	cancel, doing := s.cancel, phaseToString(s.phase)
	s.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("agent: nothing running in session %s (phase %s)", s.ID, doing)
	}
	cancel()
	return nil
}

// Interrupt aborts the current generation and, when a message is supplied, appends it and
// continues immediately ("force interrupt and early send").
func (s *Session) Interrupt(msg Message) error {
	has := strings.TrimSpace(msg.Text) != "" || len(msg.Images) > 0
	s.mu.Lock()
	if s.phase == phaseRunning && has {
		m := msg
		s.forceMsg = &m
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		if !has {
			return errors.New("agent: nothing running and no message to send")
		}
		return s.Send(context.Background(), msg)
	}
	if !has {
		s.logf("interrupt without a message: aborting the run")
	}
	cancel()
	return nil
}

func (s *Session) takeForceMsg() *Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.forceMsg
	s.forceMsg = nil
	return m
}

// run is the outer loop: agentic turns until the model answers without tool calls, then the
// end-of-turn compaction check, then — only if a user message arrived in the meantime —
// another pass. That last pass is the auto-injection of a message sent while the loop was
// busy (spec §6: "a new message during compaction is queued and sent as soon as the lock is
// released"; the same mechanism covers a busy loop).
func (s *Session) run(ctx context.Context, cancel context.CancelFunc) {
	defer func() {
		if r := recover(); r != nil {
			s.emit(Event{Type: EvtError, Info: fmt.Sprintf("panic in run: %v", r)})
			s.logf("panic in run: %v", r)
		}
		cancel()
		s.mu.Lock()
		s.phase = phaseIdle
		s.cancel = nil
		force := s.forceMsg
		s.forceMsg = nil
		done := s.done
		s.done = nil
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
		if force != nil {
			// The run ended before the forced message could be appended: append it now.
			if e, err := s.appendEntry(s.userEntry(*force)); err == nil {
				s.emit(Event{Type: EvtEntry, Entry: e})
			}
		}
		s.emitState()
		s.emitPending()
	}()

	for {
		aborted, err := s.turns(ctx)
		if err != nil {
			s.emit(Event{Type: EvtError, Info: err.Error()})
			s.logf("run error: %v", err)
		}
		if aborted {
			return
		}
		// End-of-turn trigger (spec §1.1 T1) — the only automatic trigger by default.
		if res := s.compactHere(ctx, compact.TriggerEndOfTurn, false); !res.Noop {
			s.emit(Event{Type: EvtCompact, Data: res})
		}
		if !s.needsRun() {
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.logf("a message arrived while the loop was busy: continuing with it")
		s.emit(Event{Type: EvtInfo, Info: "injecting queued message"})
	}
}

// turns runs generate → tool calls → generate … until the model answers without asking for
// tools. It reports whether the user aborted.
func (s *Session) turns(ctx context.Context) (bool, error) {
	cfg := s.Config()
	for {
		// Rebuild the payload from storage on every turn. This is the mechanism that injects
		// a message the user sent mid-run: it is already on the path, so it simply becomes
		// part of this turn's payload, right after the tool results.
		built, sizes, tools := s.build()
		s.markSent()
		s.emitPending()

		if err := s.guardFit(sizes); err != nil {
			return false, err
		}

		// In-loop trigger (spec §1.1 T2), on unless compact.inLoop is false.
		if cfg.Compact.UseInLoop() {
			if res := s.compactHere(ctx, compact.TriggerInLoop, false); res.Committed {
				s.emit(Event{Type: EvtCompact, Data: res})
				built, sizes, tools = s.build()
				s.markSent()
				s.emitPending()
				if err := s.guardFit(sizes); err != nil {
					return false, err
				}
			}
		}

		s.mu.Lock()
		s.turn++
		turn := s.turn
		s.mu.Unlock()
		s.emitState()

		// Remember the exact payload that goes out: the summary request reuses it as its
		// prefix so the provider's KV cache stays warm (spec §5.2).
		s.rememberSent(built.All(), tools)

		entry, streamID, aborted := s.generate(ctx, built, tools, turn)
		if entry == nil {
			return aborted, nil
		}
		if aborted {
			entry.Interrupted = true
			entry.Label = "interrupted by user"
		}
		committed, err := s.appendEntry(entry)
		if err != nil {
			return false, fmt.Errorf("commit assistant entry: %w", err)
		}
		s.emit(Event{Type: EvtEntry, Entry: committed})
		s.emit(Event{Type: EvtDelta, Delta: &Delta{StreamID: streamID, Done: true}})
		s.emitState()

		if aborted {
			// Force-interrupt: keep the half output, append the user's message and keep
			// going. Any tool call that never ran gets a synthetic result from the assembler
			// (spec §5.1: an assistant message with tool_calls must never be sent bare).
			if fm := s.takeForceMsg(); fm != nil {
				if e, err := s.appendEntry(s.userEntry(*fm)); err == nil {
					s.emit(Event{Type: EvtEntry, Entry: e})
					s.emitPending()
					continue
				}
			}
			return true, nil
		}
		if len(entry.ToolCalls) == 0 {
			return false, nil // normal stop
		}
		if cfg.MaxTurns > 0 && s.turnCount() >= cfg.MaxTurns {
			s.emit(Event{Type: EvtInfo, Info: fmt.Sprintf("stopped after %d turns (session maxTurns)", s.turnCount())})
			return false, nil
		}

		abortedTools := s.runTools(ctx, entry.ToolCalls)
		if abortedTools {
			if fm := s.takeForceMsg(); fm != nil {
				if e, err := s.appendEntry(s.userEntry(*fm)); err == nil {
					s.emit(Event{Type: EvtEntry, Entry: e})
					continue
				}
			}
			return true, nil
		}
	}
}

func (s *Session) turnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// generate streams one assistant message and returns the entry to commit.
func (s *Session) generate(ctx context.Context, built *asm.Built, tools []llm.Tool, turn int) (*ir.Entry, string, bool) {
	cfg := s.Config()
	// Only while tokens flow is compaction forbidden (spec §1.1).
	s.setGenerating(true)
	defer s.setGenerating(false)
	streamID := fmt.Sprintf("%s-t%d-%d", s.ID, turn, time.Now().UnixMilli())
	req := llm.Request{
		Model:           cfg.LLM.Model,
		Messages:        built.All(),
		Tools:           tools,
		Stream:          true,
		Temperature:     cfg.Temperature,
		MaxTokens:       cfg.MaxTokens,
		ReasoningEffort: cfg.ReasoningEffort,
		Extra:           cfg.Extra,
	}
	ch, err := s.provider.Stream(ctx, req)
	if err != nil {
		s.emit(Event{Type: EvtError, Info: "llm: " + err.Error()})
		return nil, streamID, ctx.Err() != nil
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		start     = time.Now()
		finish    string
		usage     *ir.Usage
		streamErr string
		aborted   bool
		byIndex   = map[int]*ir.ToolCall{}
		order     []int
	)
	emitDelta := func(d Delta) {
		d.StreamID = streamID
		s.emit(Event{Type: EvtDelta, Delta: &d})
	}

	for ev := range ch {
		if ev.Content != "" {
			content.WriteString(ev.Content)
			emitDelta(Delta{Content: ev.Content})
		}
		if ev.Reasoning != "" {
			reasoning.WriteString(ev.Reasoning)
			emitDelta(Delta{Reasoning: ev.Reasoning})
		}
		for _, tc := range ev.ToolCalls {
			cur, ok := byIndex[tc.Index]
			if !ok {
				cur = &ir.ToolCall{}
				byIndex[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Name != "" {
				cur.Name = tc.Name
			}
			// Args arrive as string fragments of one JSON object; ir.ToolCall.Args is raw JSON.
			cur.Args = append(cur.Args, tc.Args...)
			emitDelta(Delta{ToolCall: &ToolCallProgress{ID: cur.ID, Name: cur.Name, Args: tc.Args, Index: tc.Index}})
		}
		if ev.FinishReason != "" {
			finish = ev.FinishReason
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Err != nil {
			streamErr = ev.Err.Error()
		}
		if ctx.Err() != nil {
			// Keep draining the (already closing) channel but stop emitting: the provider saw
			// the cancelled context and closed the HTTP request, which aborts generation at
			// the LLM server too.
			aborted = true
		}
	}
	if ctx.Err() != nil {
		aborted = true
	}
	var calls []ir.ToolCall
	for _, idx := range order {
		calls = append(calls, *byIndex[idx])
	}
	switch {
	case finish != "":
	case aborted:
		finish = "interrupted"
	case streamErr != "":
		finish = "error"
	case len(calls) > 0:
		finish = "tool_calls"
	default:
		finish = "stop"
	}
	if streamErr != "" {
		s.emit(Event{Type: EvtError, Info: "llm stream: " + streamErr})
	}

	entry := &ir.Entry{
		Kind:        ir.KindAssistant,
		Role:        "assistant",
		Content:     content.String(),
		Reasoning:   reasoning.String(),
		ToolCalls:   calls,
		Model:       cfg.LLM.Model,
		Usage:       usage,
		Finish:      finish,
		Ms:          time.Since(start).Milliseconds(),
		Interrupted: aborted,
	}
	if content.Len() == 0 && reasoning.Len() == 0 && len(calls) == 0 && streamErr == "" && !aborted {
		entry.Content = "[empty completion]"
	}
	if usage != nil && usage.HasPrompt {
		s.mu.Lock()
		s.ctxUsed, s.ctxKnown = usage.ContextUsed(), true
		if s.nctx == 0 {
			s.nctx = s.provider.NCTX(context.Background())
		}
		s.mu.Unlock()
		s.logf("turn %d: contextUsed=%d (prompt=%d cache=%d completion=%d)",
			turn, usage.ContextUsed(), usage.PromptTokens, usage.CacheTokens, usage.CompletionTokens)
	}
	return entry, streamID, aborted
}

// runTools executes every tool call the model asked for, in order, recording one entry each.
// A result is always recorded, even on failure or abort: the next payload must never contain
// a tool call without a result (spec §5.1).
func (s *Session) runTools(ctx context.Context, calls []ir.ToolCall) (aborted bool) {
	for _, tc := range calls {
		run := &RunInfo{ID: tc.ID, CallID: tc.ID, Tool: tc.Name, Args: string(tc.Args), Status: "running", Rounds: s.turnCount()}
		if ref, ok := s.reg.resolve(tc.Name); ok {
			run.Server = ref.Server
		}
		s.emit(Event{Type: EvtToolCall, Run: run})
		start := time.Now()

		onEvent := func(e mcp.Event) {
			s.emit(Event{Type: EvtInfo, Info: fmt.Sprintf("%s %s %s", e.Server, e.Method, clip(string(e.Params), 300))})
		}
		res, ref, err := s.reg.call(ctx, tc.Name, orEmpty(tc.Args), onEvent)

		ms := time.Since(start).Milliseconds()
		text, parts := toolOutput(res)
		isErr := res != nil && res.IsError
		if err != nil {
			isErr = true
			if text == "" {
				text = "tool error: " + err.Error()
			} else {
				text += "\n" + err.Error()
			}
		}
		if ctx.Err() != nil {
			// The call was killed because the user aborted (or because an input request was
			// never answered): say so instead of leaving an empty result.
			isErr = true
			if text == "" {
				text = "[tool call aborted before it produced a result]"
			}
			aborted = true
		}
		entry, aerr := s.appendEntry(&ir.Entry{
			Kind:       ir.KindTool,
			Role:       "tool",
			ToolCallID: tc.ID,
			Tool:       tc.Name,
			Server:     ref.Server,
			Content:    text,
			Parts:      parts,
			IsError:    isErr,
			Ms:         ms,
		})
		if aerr != nil {
			s.emit(Event{Type: EvtError, Info: "commit tool result: " + aerr.Error()})
			return aborted
		}
		run.Status = "ok"
		if isErr {
			run.Status = "error"
			run.Error = text
		}
		run.Result = text
		run.Images = len(parts)
		run.Ms = ms
		s.emit(Event{Type: EvtToolResult, Run: run})
		s.emit(Event{Type: EvtEntry, Entry: entry})
		s.emitState()
	}
	return aborted
}

// toolOutput flattens an MCP call result into the text plus image parts we store.
func toolOutput(res *mcp.CallResult) (string, []ir.Part) {
	if res == nil {
		return "", nil
	}
	var parts []ir.Part
	for _, c := range res.Content {
		switch {
		case c.Type == "image":
			if mime, data, ok := c.Image(); ok {
				parts = append(parts, ir.Part{Type: ir.PartImage, MimeType: mime, Data: data, Name: c.Name})
			}
		case c.Type == "audio":
			if mime, _, ok := c.Audio(); ok {
				parts = append(parts, ir.Part{Type: ir.PartText, Text: fmt.Sprintf("[audio result, %s, not shown]", mime)})
			}
		case c.Type == "resource":
			if c.Resource != nil {
				if c.Resource.Text != "" {
					parts = append(parts, ir.Part{Type: ir.PartText, Text: c.Resource.Text})
				} else if c.Resource.Blob != "" {
					parts = append(parts, ir.Part{Type: ir.PartText, Text: fmt.Sprintf("[resource %s, %d bytes of binary data]", c.Resource.URI, len(c.Resource.Blob))})
				}
			}
		}
	}
	text := res.Text()
	if len(res.Structured) > 0 && !strings.Contains(text, string(res.Structured)) {
		if text != "" {
			text += "\n"
		}
		text += string(res.Structured)
	}
	return text, parts
}

// orEmpty normalises missing tool arguments to an empty JSON object: a provider that streamed
// no arguments at all still needs `{}` as the MCP call payload.
func orEmpty(raw json.RawMessage) json.RawMessage {
	if strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// rememberSent stores the exact payload that went out, for the summary request prefix
// (spec §5.2: the prefix must be the last payload actually sent, same tools and model).
func (s *Session) rememberSent(msgs []asm.Message, tools []llm.Tool) {
	s.mu.Lock()
	s.lastSent = msgs
	s.lastTools = tools
	s.mu.Unlock()
}

func (s *Session) lastSentPayload() ([]asm.Message, []llm.Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSent, s.lastTools
}

// guardFit is the last line of defence before a send (spec §3.10): refuse to send a payload
// that cannot fit instead of corrupting the history with a truncated request. It is
// deliberately conservative (98%) and tells the user what to do next.
func (s *Session) guardFit(sizes *asm.Sizes) error {
	nctx := s.nctxLocked()
	if nctx <= 0 {
		return nil // unknown window: fail open, the provider will complain if it must
	}
	used, known := s.usedNow()
	if !known {
		// Nothing has been sent yet (or nothing reported usage): fall back to the estimate.
		used = sizes.Total()
	}
	if used < nctx*98/100 {
		return nil
	}
	// It does not fit: try one forced compaction before refusing.
	res := s.compactHere(context.Background(), compact.TriggerManual, true)
	if res.Committed {
		s.emit(Event{Type: EvtCompact, Data: res})
		used, known = s.usedNow()
		if !known {
			_, s2 := s.sizesNow()
			used = s2.Total()
		}
		if used < nctx*98/100 {
			return nil
		}
	}
	msg := fmt.Sprintf("context will not fit: %d tokens used, model window %d, threshold %d%%", used, nctx, s.Config().Compact.ThresholdPct)
	if res.Skipped != "" {
		msg += ": " + res.Skipped
	}
	return errors.New(msg)
}

// usedNow returns the real context usage of the last completed turn (spec §0: the trigger
// never uses an estimate; only the pre-send guard falls back to one).
func (s *Session) usedNow() (int, bool) {
	s.mu.Lock()
	used, known := s.ctxUsed, s.ctxKnown
	s.mu.Unlock()
	if known {
		return used, true
	}
	if u := ir.LastUsage(s.path()); u != nil && u.HasPrompt {
		return u.ContextUsed(), true
	}
	return 0, false
}

func (s *Session) sizesNow() (*asm.Built, *asm.Sizes) {
	built, sizes, _ := s.build()
	return built, sizes
}
