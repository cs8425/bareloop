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
	"bareloop/internal/builtin"
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
	s.beginRunLocked(ctx, runStart{})
}

// runStart is what a run is built from when it is not the ordinary "the user sent a message" run —
// which today means one thing: a wake (park.go). Every field is about the call the wake exists to
// answer, and each is empty for a run that is not a wake.
type runStart struct {
	// tail is the part of the path that was never finished: the assistant entry holding the unanswered
	// call and anything after it. It is the whole tail rather than the one call because a model can ask
	// for `wait` next to another tool in one turn, and the wake is the last chance those other calls get.
	tail []*ir.Entry
	// w is why this run exists — what was waited for, and whether anybody got tired of waiting.
	w wake
	// resume is the call this run exists to answer, together with the two facts about it that the call
	// cannot witness: the moment it was asked, and this daemon's explanation for answering late — "the
	// daemon restarted". nil for every run a message starts. One value addressed to one call rather than
	// ambient fields, because the run keeps working after that answer is written, and a wait the model
	// asks for later in the same run has nothing of either to explain (see resumeCall).
	resume *resumeCall
}

// beginRunLocked is the one place a run comes into existence. Everything about a run's start is here —
// the phase, the context, the tool snapshot — so a wake cannot forget one of them and produce a run
// that the rest of the session does not know about.
func (s *Session) beginRunLocked(ctx context.Context, st runStart) {
	if s.phase != phaseIdle {
		return
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	if st.w.callID != "" {
		// Only the call that was waiting is told it ended early, and not by cancelling this run: that
		// run could no longer answer the message the interrupt brought (see endedCallKey).
		runCtx = withEndedCall(runCtx, st.w.callID, st.w.cause)
	}
	if st.resume != nil {
		// Put on the run rather than handed to the calls, because it is not the run that has to know where it
		// belongs: it carries the call it is about, and runTools is what reads it back off for that one call
		// (see resumeCall).
		runCtx = withResumeCall(runCtx, *st.resume)
	}
	s.phase = phaseRunning
	s.cancel = cancel
	s.done = make(chan struct{})
	s.turn = 0
	// The run publishes one tool array from start to finish, snapshotted here. The tool block is
	// rendered in front of the history, so a policy change that slipped in at turn three would
	// invalidate the KV prefix of everything already generated — and could not be undone anyway,
	// because turn one's request is long gone. Changes made now take effect when this run is over
	// (an abort counts as over); see Session.SetToolPolicy.
	s.snapshotToolsLocked()
	go s.run(runCtx, cancel, st)
}

// snapshotToolsLocked takes the run's tool array, which has to be the one it starts publishing at its
// first request — so it happens at the same moment the phase changes, under the same lock, rather than
// somewhere the registry cannot tell a run's first turn from its third.
func (s *Session) snapshotToolsLocked() {
	s.tmu.Lock()
	s.toolSnapshot = s.reg.Tools()
	s.tmu.Unlock()
}

// Stop aborts whatever this session is doing right now. The partial assistant message is
// kept and marked interrupted — the user asked to stop output, not to lose it.
//
// For a session parked on a wait there is nothing generating, and Stop does the only thing "stop" can
// mean: it ends the wait. The run that answers says so and carries on, because ending a wait is a
// decision the model has to be told about rather than left to discover.
//
// A manual summary is the third thing this session can be doing, and the only one of the three with
// nothing to keep: it writes its one entry at the end or nothing at all, so stopping it loses no half
// answer - the path is simply still the one it was. Stop is the only hand on one (CompactNow runs on a
// context of its own, so no disconnect ends it), which is why it answers for it here instead of replying
// "nothing running". A run wins when both exist: stopping the run is what the button means, and a second
// press then reaches the summary.
func (s *Session) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		// No cause, deliberately: nobody ended this run in order to say something, so a wait that was
		// sleeping in it reports a bare "interrupted" (see builtin.interruptReason).
		s.cancel(nil)
		return nil
	}
	if s.compactCancel != nil {
		s.logf("stop: ending the summary this session was writing; nothing was committed")
		s.compactCancel(context.Canceled)
		return nil
	}
	if s.park != nil {
		// An early wake with no cause: the wait says "interrupted" and names nobody, because nothing of
		// anybody's is coming after it.
		s.askWakeLocked(nil)
		return nil
	}
	return fmt.Errorf("agent: nothing running in session %s (phase %s)", s.ID, phaseToString(s.phase))
}

// Interrupt aborts the current generation and makes sure the message it carries both reaches the path and
// gets an answer - "force interrupt and early send".
//
// A message is required, by the very rule a send is held to (emptyMessage): an interrupt that carries nothing
// is not an interrupt, it is a stop, and Stop is the verb that means that. It used to be otherwise - a body of
// spaces arrived here, aborted the run and threw the words away with one line in the daemon log, while the
// identical body sent to send was refused. One question, two answers.
//
// The message is not appended here. The path it would join is exactly the shape the assembler cannot use:
// half an assistant reply, or a call whose result is on its way. So it joins the queue - the same place every
// other message sent into a hole goes, reached directly rather than through holdMessageLocked because that
// function's job is to answer "is there a hole?", and here the answer is already known - and forceRun promises
// it a run.
//
// forceRun is not a courtesy to the impatient. Cancelling is not polite: from the cancel below this line
// onward, every request the aborted run builds dies in the HTTP client on its way out, so the loop staying in
// this run could only ever produce another error entry. A force-interrupt that kept its message in the run it
// had just killed was answered by nothing at all.
//
// With nothing running and nothing parked there is nothing to interrupt, and the honest answer is the message
// itself: it goes out as a send, which is what pressing the bolt on an idle session means.
//
// A summary someone asked for is none of these, so it is refused rather than quietly downgraded into a send.
// The bolt promises "stop what it is doing and answer this instead"; a summary is not generating, has nothing
// on disk yet to keep, and the only hand on it is Stop - so the honest answer names the button that works.
func (s *Session) Interrupt(msg Message) error {
	if emptyMessage(msg) {
		return errEmptyMessage
	}
	s.mu.Lock()
	if s.manualSummaryLocked() {
		s.mu.Unlock()
		return fmt.Errorf("agent: session %s is writing a summary, not generating: POST /api/session/%s/stop ends it (nothing is written until it finishes), then interrupt works",
			s.ID, s.ID)
	}
	if s.park != nil {
		// Waiting rather than generating: the message cannot go on the path (it would land in front of
		// the call's result), so it is queued and the wait ends as what it is. The order the model reads
		// is the wait saying "user interrupt", then this message, then the answer to it. If a run still
		// owns the session for a moment, askWakeLocked leaves the wake to a timer rather than starting a
		// second run, and the message stays held with everything else until that wake lands.
		//
		// No forceRun here, unlike the running branch below: a wake is already coming, and it is the run
		// that owes this message its answer.
		s.holdMessageLocked(msg)
		s.askWakeLocked(builtin.ErrUserInterrupt)
		s.mu.Unlock()
		return nil
	}
	queued := false
	if s.phase == phaseRunning {
		// Queued and promised a run, in that order, under the lock the epilogue needs to see both.
		// Appending rather than overwriting: two presses are two messages the person wrote, and the
		// single slot this used to be had the second one swallow the first without a word.
		s.queue = append(s.queue, msg)
		s.forceRun = true
		queued = true
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		if queued {
			// A running session with no way to cancel it is not a state today's fields allow (a run owns
			// phase and cancel together, and a park answers to the branch above). Should it ever exist, the
			// message has still been accepted: falling through to Send would put a second copy of it on the
			// path, and a duplicate is a worse answer than a surprising one.
			return nil
		}
		return s.Send(context.Background(), msg)
	}
	// The cause is what lets a `wait` that dies here say who killed it, which matters because the very
	// next thing in the payload is that person's message.
	cancel(builtin.ErrUserInterrupt)
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

// run is the outer loop: agentic turns until the model answers without tool calls, then the
// end-of-turn compaction check, then — only if the path now ends with something unanswered —
// another pass. That last pass is how a message sent while the loop was busy gets its answer: it is
// either already on the path (a message that found no debt open appends like any other) or it was
// queued and has just been flushed behind the reply it was held for, and either way it is the entry
// needsRun reads here (spec §6: "a new message during compaction is queued and sent as soon as the lock
// is released"; the same mechanism covers a busy loop).
func (s *Session) run(ctx context.Context, cancel context.CancelCauseFunc, st runStart) {
	defer func() {
		if r := recover(); r != nil {
			s.emit(Event{Type: EvtError, Info: fmt.Sprintf("panic in run: %v", r)})
			s.logf("panic in run: %v", r)
		}
		cancel(nil)
		// A tool policy that waited for this run is applied here, before the session reports itself
		// idle again: a run started by the very next message must not pick up the previous tool
		// array. The registry only re-filters the tools/list results it already cached, so running
		// this at every run end is free — and it beats a stored "pending" flag, which could only be
		// a second answer to "did the change land?", and a stale one at that.
		toolsChanged := s.reg.SetPolicy(s.Config())
		// The snapshot is dropped while the phase still says running, so no new run can have taken
		// one yet and this cannot wipe somebody else's.
		s.tmu.Lock()
		s.toolSnapshot = nil
		s.tmu.Unlock()
		s.mu.Lock()
		// why the guard: this run may be ending by handing a wait to the daemon (park.go), and a session
		// that is asleep is not idle — its wake-up is already armed, and idle would let a second run start
		// next to it. Everything else here still happens, which is the point of ending the run instead of
		// holding it: a parked session is reported, snapshotted and closed exactly like one that is
		// working, and the wait is all that differs.
		if s.park == nil {
			s.phase = phaseIdle
		}
		s.cancel = nil
		// The debt one request opened dies with the run, however the run ended: the reply is on the path
		// (turns() closed it at the commit) or it is never coming. Left open, it would keep holding the next
		// message for a request no goroutine in this process is waiting for any more.
		s.awaitingReply = false
		// The promise made to an interrupting message is collected here rather than kept: whether it turns
		// into a run is decided below, once the path holds everything it is going to hold.
		answer := s.forceRun
		s.forceRun = false
		done := s.done
		s.done = nil
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
		// A wake that never reached the end of its tail — aborted on the way, or handed a wait straight
		// back — still has the messages it was built holding. Unless the wait is being held again (in which
		// case the hold belongs to it, and the next wake is the one that owes those messages their place),
		// they go on the path now rather than staying in a memory nobody owns any more: the call they were
		// waiting for has been given up on by whoever aborted this run, the assembler says so on its own
		// terms, and losing a message that was already accepted is not this run's decision to make.
		if !s.isParked() {
			s.flushHeld()
		}
		// The run that was aborted cannot be the one that answers what aborted it: its context is cancelled,
		// and every request built from it is refused on the way out of the process. So the message that came
		// in by interrupt is answered by a run started right here — fresh context, payload assembled from a
		// path that now ends with that message.
		//
		// Asked rather than assumed, twice over: a session that parked on the way out is asleep with its wake
		// armed, and a path ending in anything but a user message or a tool result is not asking a question.
		// beginRunLocked holds the real veto, which is why this is allowed to be a guess.
		if answer && !s.isParked() && s.needsRun() {
			s.mu.Lock()
			if s.phase == phaseIdle {
				s.logf("the run that was interrupted cannot answer the message that interrupted it: starting the run that can")
				s.startRunLocked(context.Background(), 0)
			}
			s.mu.Unlock()
		}
		if toolsChanged {
			s.logf("tool policy applied now that the run is over")
			s.emitTools()
		}
		s.emitState()
		s.emitPending()
	}()

	// A wake starts here rather than at the model, because a wake exists to answer something: the tail
	// this session was left holding (park.go). Running it is what produces the wait's result — from the
	// code that actually waited, so the answer says when it resumed — and then the loop below takes over
	// from the result, which is an ordinary end-of-turn state.
	if len(st.tail) > 0 {
		// The run's own context goes in here. What the daemon knows about the call being resumed travels on
		// it addressed to that call rather than scoped to these few, because a scope would have to end with
		// the tail and a run does not (see resumeCall).
		if aborted := s.runTools(ctx, tailCalls(st.tail)); aborted || s.isParked() {
			// The wake ended the way any run ends when it is aborted, or when the wait it produced is
			// longer than a run will hold: the next moment belongs to the daemon, not to this goroutine.
			return
		}
		// The turn the wake exists to finish is over, which is the only moment the messages held through
		// it may be appended: after every result of that turn, so a call and its answer stay next to each
		// other and the message is the thing the model reads next.
		s.flushHeld()
	}

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

		// Rebuild the payload from storage on every turn. This is the mechanism that injects a message the
		// user sent mid-run: it is already on the path, so it becomes part of this turn's payload. What is kept
		// off the path is the other half of the mechanism: a message that arrives while the path is owed an
		// answer — a tool call with no result yet, or this very request's own reply — waits in the queue until
		// the answer it belongs behind has been written, and joins the path there (answeringLocked). So no
		// turn is ever assembled from a message that jumped ahead of the thing it was sent after.
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
			// No reply, and none coming: the request was refused on its way out, which in practice means the
			// context was cancelled while it was on the wire. The debt closes here rather than waiting for the
			// run, because a hold outlasting the request it holds for is how a message gets stuck behind an
			// answer that was never going to exist.
			s.stopAwaiting()
			return aborted, nil
		}
		if aborted {
			entry.Interrupted = true
			entry.Label = "interrupted by user"
		}
		committed, err := s.appendEntry(entry)
		// Whether or not that worked, this request has its final answer: on the path, or refused by storage.
		// Closing the debt before the returns below is what keeps the hold a debt with a fixed end.
		s.stopAwaiting()
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
			// why: this branch means the compaction never ran (a committed one returns above), so the only
			// advice worth printing is what stopped it. Naming compact.threshold would be naming the size of a
			// kept tail that was never produced: the floors in §2.3 are what make a boundary illegal, and a
			// minSummarizeTokens above the whole conversation is the one I have seen do it in person.
			return false, fmt.Errorf("request does not fit the model window (%d tokens) and compaction could not shrink it (%s): trim the input, or lower what stopped it (minSummarizeTokens, minKeptMessages, minKeptUserTurns)",
				window, skip)
		}

		if aborted {
			// The person stopped this output, and the half reply is kept and marked. Anything they sent while
			// doing so is in the queue, and nothing is appended from here: this context is cancelled, so the run
			// that reads a payload containing that message is the next one, and forceRun is what promises it.
			// Any tool call that never ran gets its synthetic result from the assembler (spec §5.1: an
			// assistant message with tool_calls must never be sent bare).
			return true, nil
		}
		if len(entry.ToolCalls) == 0 {
			// Normal stop. Nothing is outstanding any more — not this reply, not a call — so what was held
			// behind it stops being held right here, before the loop is asked whether it owes another turn.
			// That question reads the path, and a message still sitting in memory when it is asked is a
			// message this run will never answer: the path ends on this reply, so needsRun says no, and the
			// reply at the tip is the reason nobody ever asks again.
			s.flushHeld()
			return false, nil // normal stop
		}
		if cfg.MaxTurns > 0 && s.turnCount() >= cfg.MaxTurns {
			s.emit(Event{Type: EvtInfo, Info: fmt.Sprintf("stopped after %d turns (session maxTurns)", s.turnCount())})
			// This run is finished, and the call it stops on is a legal place to leave a path: the assembler
			// writes that call's result as the interruption it was. A message held behind it has nothing left
			// to be held for, so it joins the path before this function stops asking questions about it.
			s.flushHeld()
			return false, nil
		}

		abortedTools := s.runTools(ctx, entry.ToolCalls)
		if s.isParked() {
			// A wait this run handed over is neither the end of the conversation nor a place to compact:
			// the path ends on a call with no result, and a boundary here would put a summary in front of
			// an answer that has not been written yet. The run ends — the wake is what continues.
			return true, nil
		}
		// Every call of this turn has its answer on the path now, and that is the breakpoint the messages
		// which arrived mid-call were held for: after the results they interrupted, before the model is
		// asked again (D84). Waiting for the end of the run instead would steer none of this run's remaining
		// turns with them, and the run would finish answering a question the person had already amended.
		s.flushHeld()
		if abortedTools {
			// Every call of this turn has its answer, and the messages that interrupted it are behind them
			// (flushed just above). Staying in this run would ask the model on a cancelled context, so the
			// answer is the next run's — promised, if the interrupt came with a message.
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
		// Mostly the run's own context. The exceptions are facts about one call that a whole run must not be
		// told, because a run outlives all of them: a wait this daemon is resuming on somebody else's behalf
		// is told it ended, by this cause, while the rest of the run still has an answer to produce for the
		// message that interrupted it; and what the daemon knows about the call it found unfinished — the
		// moment it was asked, and why the answer is late — is about that call, not about the next one this
		// run is asked to sleep.
		callCtx := ctx
		if cause, ok := endedCallCause(ctx, tc.ID); ok {
			var stop context.CancelCauseFunc
			callCtx, stop = context.WithCancelCause(ctx)
			// The child is cancelled the moment it exists: that is the whole message to this one call.
			stop(cause)
		}
		if c, ok := resumeCallFor(ctx, tc.ID); ok {
			// Addressed to this call, and this is where the address is honoured: both facts become part of this
			// call's context and of no other, which is the only way a builtin can be told which wait it is
			// continuing rather than starting (registry.builtinInput reads them off per call). A wake with
			// nothing to explain is the ordinary scheduled one, and withAskedAt and withDaemonNote each hand
			// back the context unchanged when given nothing.
			callCtx = withDaemonNote(withAskedAt(callCtx, c.askedAt), c.note)
		}
		res, ref, err := s.reg.call(callCtx, tc.Name, orEmpty(tc.Args), onEvent)

		ms := time.Since(start).Milliseconds()

		// A builtin that decided this call outlives the run comes back as a handover rather than as an
		// answer, and there is nothing to record: the result belongs to the run that wakes, and writing a
		// second answer to one call is how a wait ends up with two memories.
		var p *builtin.Parked
		if errors.As(err, &p) {
			s.mu.Lock()
			s.parkWaitLocked(tc.Name, tc.ID, p.Until)
			s.mu.Unlock()
			run.Status = "parked"
			run.Result = p.Error()
			run.Ms = ms
			s.emit(Event{Type: EvtToolResult, Run: run})
			s.emitState()
			// The calls after this one are not run. They are not lost: they are still in the entry the
			// wake replays, and running an MCP tool twice is a worse bug than running it four hours late.
			return aborted
		}

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
			if text == "" {
				text = "[tool call aborted before it produced a result]"
				// why the flag belongs to that branch alone: a tool that had an answer ready when the abort
				// arrived has one worth reading, and calling it a failure turns a fact about what happened
				// into a guess about what it meant.
				isErr = true
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
