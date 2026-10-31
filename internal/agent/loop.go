package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	// The run publishes one tool array from start to finish, snapshotted here. The tool block is
	// rendered in front of the history, so a policy change that slipped in at turn three would
	// invalidate the KV prefix of everything already generated — and could not be undone anyway,
	// because turn one's request is long gone. Changes made now take effect when this run is over
	// (an abort counts as over); see Session.SetToolPolicy.
	s.tmu.Lock()
	s.toolSnapshot = s.reg.Tools()
	s.tmu.Unlock()
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

// Resume starts a run from the current tip without appending anything. The path already ends on a
// boundary that expects an answer - a user message, a tool result, or a compaction summary - so this is
// how an interrupted run is picked up again: the tool result is already here, ask the model again.
//
// It deliberately does not call needsRun(): that function also drives the loop's own auto-continuation,
// where a compaction boundary must say no. Here a compaction boundary is exactly what you resume from,
// because the run that compacted the context ends by asking the user to send again - which should not
// have to be a new message.
func (s *Session) Resume(ctx context.Context) error {
	s.mu.Lock()
	if s.phase != phaseIdle {
		s.mu.Unlock()
		return fmt.Errorf("agent: cannot resume while %s", phaseToString(s.phase))
	}
	s.mu.Unlock()

	path := s.path()
	if len(path) == 0 {
		return errors.New("agent: nothing to resume: the path is empty")
	}
	last := path[len(path)-1]
	switch last.Kind {
	case ir.KindUser, ir.KindTool, ir.KindCompaction:
	default:
		return fmt.Errorf("agent: cannot resume from %s (kind %s): the path must end with a user message, a tool result or a compaction summary",
			last.ID, last.Kind)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseIdle {
		return fmt.Errorf("agent: cannot resume while %s", phaseToString(s.phase))
	}
	s.startRunLocked(ctx, 0)
	if s.phase != phaseRunning {
		return errors.New("agent: resume did not start a run")
	}
	s.logf("resume: continuing from %s (kind %s, %d entries on the path)", last.ID, last.Kind, len(path))
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
		// A tool policy that waited for this run is applied here, before the session reports itself
		// idle again: a run started by the very next message must not pick up the previous tool
		// array. The registry only re-filters the tools/list results it already cached, so running
		// this at every run end is free — and it beats a stored "pending" flag, which could only be
		// a second answer to "did the change land?", and a stale one at that.
		toolsChanged := s.reg.SetPolicy(s.Config().MCP)
		// The snapshot is dropped while the phase still says running, so no new run can have taken
		// one yet and this cannot wipe somebody else's.
		s.tmu.Lock()
		s.toolSnapshot = nil
		s.tmu.Unlock()
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
		if toolsChanged {
			s.logf("tool policy applied now that the run is over")
			s.emitTools()
		}
		s.emitState()
		s.emitPending()
	}()

	for {
		grown := len(s.path())
		aborted, err := s.turns(ctx)
		if err != nil {
			s.emit(Event{Type: EvtError, Info: err.Error()})
			s.logf("run error: %v", err)
		}
		if aborted {
			return
		}
		if len(s.path()) == grown {
			// Nothing was committed this round, so needsRun() still says an answer is owed and the
			// loop below would ask the provider the same question again — forever. One round per
			// failure is enough: the error has been emitted, and it is the user's turn now.
			if err == nil {
				s.logf("no entry committed this round: stopping the loop")
				s.emit(Event{Type: EvtInfo, Info: "nothing was committed this round: stopping"})
			}
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

// logPayload writes one line per request to the process log: what the model is about to see.
// With BARELOOP_PAYLOAD_DUMP=1 the exact body (messages + tool definitions) is appended as JSON,
// which is how you check that nothing but the user's own words steers the model — language and
// script included. The dump is the whole payload, so it is large: set it only when debugging.
func (s *Session) logPayload(turn int, built *asm.Built, tools []llm.Tool) {
	if built.System == "" {
		s.logf("turn %d: %d messages, NO system prompt (session systemPrompt is empty), %d tools, %d bytes",
			turn, len(built.Messages), len(tools), built.Bytes())
	} else {
		s.logf("turn %d: %d messages + system prompt (%d bytes), %d tools, %d bytes",
			turn, len(built.Messages), len(built.System), len(tools), built.Bytes())
	}
	if os.Getenv("BARELOOP_PAYLOAD_DUMP") == "" {
		return
	}
	dump, err := json.Marshal(map[string]any{"system": built.System, "messages": built.All(), "tools": tools})
	if err != nil {
		s.logf("payload dump: %v", err)
		return
	}
	s.logf("turn %d payload: %s", turn, dump)
}

// turns runs generate → tool calls → generate … until the model answers without asking for
// tools. It reports whether the user aborted.
func (s *Session) turns(ctx context.Context) (bool, error) {
	cfg := s.Config()
	for {
		// Ask the server once, before this session has sent anything: the configured window is a
		// ceiling, not a fact, and discovering that it shrank by means of a refused request costs a
		// failed turn (see nctx.go for the two moments we probe).
		if s.needsProbe() {
			s.probeNCTX("first send of this session")
		}

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
		s.logPayload(turn, built, tools)

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

		// "That request does not fit" is both an error and a measurement. Re-probe (the second and
		// last moment we ask) and fold the context now: with the serial summary there is always a
		// way to make the payload fit again, while leaving it alone would brick the session, since
		// every later turn would send the same oversized payload and be refused the same way.
		if entry.Label == "llm error" && isContextOverflow(entry.Content) {
			s.probeNCTX("the provider refused the request as too large")
			window := s.nctxLocked()
			res := s.compactHere(ctx, compact.TriggerInLoop, true)
			if res.Committed {
				s.emit(Event{Type: EvtCompact, Data: res})
				return false, fmt.Errorf("request did not fit the model window (%d tokens): the context was compacted, please send again", window)
			}
			skip := res.Skipped
			if skip == "" {
				skip = "no legal boundary"
			}
			return false, fmt.Errorf("request does not fit the model window (%d tokens) and compaction could not shrink it (%s): trim the input or lower compact.keepTarget",
				window, skip)
		}

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

// maxLLMRetries is how often a request that failed before returning anything is repeated. It is
// deliberately small: it exists for a server that is momentarily busy, not as a way of waiting for
// a broken request to fix itself.
const maxLLMRetries = 2

// transientLLMError sorts a provider failure into "ask again" and "do not ask again". A transport
// error never reached the model, and the 5xx family plus 429 say the server could not serve this
// request just now. A 4xx that describes the request itself — bad JSON, too long, bad key — is
// answered identically every time, and re-sending it is how one bad message becomes a flood.
//
// The status is read out of the provider's own message ("llm: 503 Service Unavailable: …"), which
// is what internal/llm builds for every non-2xx response.
func transientLLMError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, code := range []string{"400", "401", "402", "403", "404", "405", "406", "410",
		"413", "414", "415", "422", "431", "451"} {
		if strings.Contains(s, "llm: "+code+" ") {
			return false
		}
	}
	return true
}

// generate streams one assistant message and returns the entry to commit.
func (s *Session) generate(ctx context.Context, built *asm.Built, tools []llm.Tool, turn int) (*ir.Entry, string, bool) {
	cfg := s.Config()
	// Only while tokens flow is compaction forbidden (spec §1.1).
	s.setGenerating(true)
	defer s.setGenerating(false)
	streamID := fmt.Sprintf("%s-t%d-%d", s.ID, turn, time.Now().UnixMilli())
	start := time.Now()
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
	// Ask the provider, retrying only what a retry can fix. Two extra attempts cover a busy
	// llama.cpp or a dropped connection; a request the server rejected (400, 401, 413 …) is not
	// retried, because the same bytes will keep failing the same way.
	var (
		ch  <-chan llm.Event
		err error
	)
	for attempt := 0; ; attempt++ {
		ch, err = s.provider.Stream(ctx, req)
		if err == nil || ctx.Err() != nil || attempt >= maxLLMRetries || !transientLLMError(err) {
			break
		}
		wait := time.Duration(attempt+1) * 800 * time.Millisecond
		s.logf("turn %d: llm request failed (%v): retry %d/%d in %s", turn, err, attempt+1, maxLLMRetries, wait)
		s.emit(Event{Type: EvtInfo,
			Info: fmt.Sprintf("llm retry %d/%d in %s: %s", attempt+1, maxLLMRetries, wait, clip(err.Error(), 200))})
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	if err != nil {
		s.emit(Event{Type: EvtError, Info: "llm: " + err.Error()})
		s.logf("turn %d: llm request failed: %v", turn, err)
		if ctx.Err() != nil {
			return nil, streamID, true // stopped or interrupted before anything came back
		}
		// Commit the failure as the answer. This is what ends the turn: while the path ends with
		// the user message the loop still owes it an answer, so it asks again — and a provider that
		// answers 500 will keep doing so, forever, one identical request per round. The entry says
		// what went wrong, so the user rather than the loop decides what happens next.
		return &ir.Entry{
			Kind:    ir.KindAssistant,
			Role:    "assistant",
			Content: "[generation failed: " + clip(err.Error(), 600) + "]",
			Model:   cfg.LLM.Model,
			Finish:  "error",
			Label:   "llm error",
			Ms:      time.Since(start).Milliseconds(),
		}, streamID, false
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
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
	if streamErr != "" && !aborted {
		// A stream that died halfway leaves the output up to that point. Say so: an answer that is
		// quietly truncated reads like an answer that is complete.
		entry.Label = "llm error"
		if entry.Content == "" {
			entry.Content = "[generation failed: " + clip(streamErr, 600) + "]"
		} else {
			entry.Content += "\n\n[stream error: " + clip(streamErr, 300) + "]"
		}
	}
	if usage != nil && usage.HasPrompt {
		s.mu.Lock()
		s.ctxUsed, s.ctxKnown = usage.ContextUsed(), true
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
		text, parts, structured := toolOutput(res)
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
			// A failed call never reports which server it was going to call, so fall back to what the
			// registry had assigned: a dead stdio server still owns its own error.
			Server:     refOr(ref.Server, run.Server),
			Content:    text,
			Parts:      parts,
			Structured: structured,
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
		run.Structured = structured
		run.Images = countImages(parts)
		run.Ms = ms
		s.emit(Event{Type: EvtToolResult, Run: run})
		s.emit(Event{Type: EvtEntry, Entry: entry})
		s.emitState()
	}
	return aborted
}

// countImages counts the images in a tool result — the number the UI reports, and text parts
// have never been images.
func countImages(parts []ir.Part) int {
	n := 0
	for _, p := range parts {
		if p.Type == ir.PartImage {
			n++
		}
	}
	return n
}

// refOr names an attachment by whatever the server actually gave us.
func refOr(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return "unnamed"
}

// toolOutput flattens an MCP call result into the three things we store: the text, the image
// parts, and the structuredContent exactly as it arrived.
func toolOutput(res *mcp.CallResult) (string, []ir.Part, json.RawMessage) {
	if res == nil {
		return "", nil, nil
	}
	var parts []ir.Part
	for _, c := range res.Content {
		switch {
		case c.Type == "image":
			if data, mime, ok := c.Image(); ok {
				parts = append(parts, ir.Part{Type: ir.PartImage, MimeType: mime, Data: data, Name: c.Name})
			} else {
				// An image block with no bytes is a reference. Keep it visible: a tool result that
				// silently dropped the screenshot says less than one that says file:///tmp/shot.png.
				parts = append(parts, ir.Part{Type: ir.PartText,
					Text: "[image " + refOr(c.URI, c.Name) + " — the result carried no image data]"})
			}
		case c.Type == "audio":
			if _, mime, ok := c.Audio(); ok {
				parts = append(parts, ir.Part{Type: ir.PartText, Text: fmt.Sprintf("[audio result, %s, not shown]", mime)})
			}
		case c.Type == "resource":
			if c.Resource == nil {
				continue
			}
			switch {
			case strings.HasPrefix(c.Resource.MimeType, "image/") && c.Resource.Blob != "":
				// An embedded image is an image: it belongs in the parts, where it can be shown
				// (and sent as one), not dumped into the text as base64.
				parts = append(parts, ir.Part{Type: ir.PartImage, MimeType: c.Resource.MimeType,
					Data: c.Resource.Blob, Name: refOr(c.Name, c.Resource.URI)})
			case c.Resource.Text != "":
				parts = append(parts, ir.Part{Type: ir.PartText, Text: c.Resource.Text})
			case c.Resource.Blob != "":
				parts = append(parts, ir.Part{Type: ir.PartText,
					Text: fmt.Sprintf("[resource %s, %d bytes of binary data]", c.Resource.URI, len(c.Resource.Blob))})
			default:
				parts = append(parts, ir.Part{Type: ir.PartText,
					Text: "[resource " + refOr(c.Resource.URI, c.Name) + " — the result carried no data]"})
			}
		}
	}
	text := res.Text()
	// The JSON is handed back untouched instead of appended to the text: what the model sees is
	// composed per session and per server by the assembler (asm.StructuredMode). Folding it in here
	// would either store the same bytes twice or freeze a decision the user is meant to be able to
	// change — and a tool result whose JSON was pasted into its own text is indistinguishable from
	// one the server really did send that way.
	return text, parts, res.Structured
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
	// Nothing cached (fresh process, or the cache was invalidated by a commit): re-derive from the
	// log, but only from the region that is still on the wire. A usage older than the active
	// compaction boundary is a stale number, and spec 1.3 says unavailable means "do not check, do
	// not guess" - the pre-send guard already falls back to the built payload size in that case.
	_, _, cmp := s.effective()
	stop := cmp != nil && cmp.Compaction != nil && !cmp.Compaction.Broken
	if u := ir.LastUsage(s.path(), stop); u != nil && u.HasPrompt {
		return u.ContextUsed(), true
	}
	return 0, false
}

func (s *Session) sizesNow() (*asm.Built, *asm.Sizes) {
	built, sizes, _ := s.build()
	return built, sizes
}
