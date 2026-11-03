package agent

// A request that has gone out owes the path an answer, and that debt is the third shape answeringLocked
// knows about (park.go). It is the one shape nobody could see in the log: there is no entry to walk back
// to, only a payload that is gone and a reply that has not arrived. What that cost was measured by hand —
// a message sent while the model was talking was committed in front of the reply that had never seen it,
// and when the turn asked for no tools the reply became the tip, so `needsRun` never asked about the
// message again and nothing ever would.
//
// These tests drive a real run through a scripted provider, because the assertions are about a moment in
// one request's life: "still on the wire" cannot be observed through a stub of the predicate alone, and
// cannot be held still by a real HTTP server either. The provider records every payload it is shown, so a
// test can say not only what got written but what the model was made to read, in which order.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// ---------------------------------------------------------------- the scripted provider

// gate is the test's hand on a request that is on the wire. A scripted answer waits here, so the moment
// "the request is in flight and nothing has come back" is a state a test can enter, assert inside of, and
// leave whenever it chooses.
type gate struct{ ch chan struct{} }

func (g *gate) release() { close(g.ch) }

// wait lets an answer sit on the wire until the test releases it, or until the run's context dies —
// whichever of the two the test is about. Honouring ctx is what keeps a test that asserts nothing more
// from hanging in Close.
func (g *gate) wait(ctx context.Context) {
	select {
	case <-g.ch:
	case <-ctx.Done():
	}
}

// turn is one scripted answer: what the fake does with one request, given that request and the gate the
// test holds. Returning a channel is what closes the stream; returning an error is what refuses it.
type turn func(ctx context.Context, req llm.Request, g *gate) (<-chan llm.Event, error)

func stream(events ...llm.Event) <-chan llm.Event {
	ch := make(chan llm.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// says is an ordinary reply: one assistant message, no tools, whole.
func says(text string) turn {
	return func(context.Context, llm.Request, *gate) (<-chan llm.Event, error) {
		return stream(llm.Event{Content: text}, llm.Event{FinishReason: "stop"}), nil
	}
}

// gatedSays is the same reply held on the wire until the test lets it land. This is the shape every
// "sent while it was generating" test needs.
func gatedSays(text string) turn {
	return func(ctx context.Context, _ llm.Request, g *gate) (<-chan llm.Event, error) {
		g.wait(ctx)
		return stream(llm.Event{Content: text}, llm.Event{FinishReason: "stop"}), nil
	}
}

// gatedToolCalls is a reply asking for one call, held on the wire. The call names a builtin the fixture
// turns on, so runTools answers it for real and the test can see where the result ends up.
func gatedToolCalls(id, tool, args string) turn {
	return func(ctx context.Context, _ llm.Request, g *gate) (<-chan llm.Event, error) {
		g.wait(ctx)
		return stream(
			llm.Event{ToolCalls: []llm.ToolCallDelta{{Index: 0, ID: id, Name: tool, Args: args}}},
			llm.Event{FinishReason: "tool_calls"},
		), nil
	}
}

// onTheWireUntilCancelled is a request that never answers: it sits on the wire, and when the context dies
// it fails the way an HTTP request fails when its context is cancelled — no entry at all. Every
// interrupted-before-the-first-token test needs exactly this, because it is the only moment the loop has
// ever seen a request with no reply (generate returns nil, and turns() has to live with that).
func onTheWireUntilCancelled() turn {
	return func(ctx context.Context, _ llm.Request, _ *gate) (<-chan llm.Event, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// halfOnCancel is a provider that had already written half a sentence into the socket when the abort
// arrived. What it managed to say is the entry that gets committed, marked interrupted — the shape an
// interrupted run leaves behind, as opposed to the shape above, which leaves nothing.
func halfOnCancel(text string) turn {
	return func(ctx context.Context, _ llm.Request, _ *gate) (<-chan llm.Event, error) {
		<-ctx.Done()
		return stream(llm.Event{Content: text}), nil
	}
}

// refused is a provider that answers nothing: a 4xx the loop knows better than to retry.
func refused(msg string) turn {
	return func(context.Context, llm.Request, *gate) (<-chan llm.Event, error) {
		return nil, errors.New(msg)
	}
}

// askedRequest is one request as the provider saw it, together with the gate still attached to it.
type askedRequest struct {
	req  llm.Request
	gate *gate
}

// fakeProvider is a script of answers with a hand on the clock. Requests are recorded the moment they
// arrive — which is the moment markSent has already run, and therefore the moment a message sent by a
// test is genuinely sent into an open debt rather than into a race with one.
type fakeProvider struct {
	mu     sync.Mutex
	asked  []askedRequest
	script []turn
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	f.mu.Lock()
	g := &gate{ch: make(chan struct{})}
	n := len(f.asked)
	f.asked = append(f.asked, askedRequest{req: req, gate: g})
	var next turn
	if n < len(f.script) {
		next = f.script[n]
	}
	f.mu.Unlock()
	if next == nil {
		// Loud rather than silent: a second request nobody scripted is a run that kept going when
		// these tests say it stopped, and that is worth an error entry in the path it is asserted on.
		return nil, fmt.Errorf("fake provider: nothing scripted for request %d", n+1)
	}
	return next(ctx, req, g)
}

// Complete is what a compaction summary calls. Nothing scripted here compacts, and an answer would only
// hide a compaction that was never supposed to run.
func (f *fakeProvider) Complete(context.Context, llm.Request) (*llm.Result, error) {
	return nil, errors.New("fake provider: no scripted summary")
}

func (f *fakeProvider) NCTX(context.Context) int { return 0 }

// waitForRequest blocks until the provider has been asked n times (1-based) and returns that payload.
func (f *fakeProvider) waitForRequest(t *testing.T, n int) llm.Request {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		enough := len(f.asked) >= n
		var req llm.Request
		if enough {
			req = f.asked[n-1].req
		}
		f.mu.Unlock()
		if enough {
			return req
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the provider was never asked a %dth time: %d request(s): %s", n, f.count(), f.describe())
	return llm.Request{}
}

// gate returns the gate of the nth request (1-based), and fails the test if that request has not been
// made. It is only ever called after waitForRequest, so the request is there.
func (f *fakeProvider) gate(t *testing.T, n int) *gate {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > len(f.asked) {
		t.Fatalf("there is no %dth request to release: %d made", n, len(f.asked))
	}
	return f.asked[n-1].gate
}

func (f *fakeProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func (f *fakeProvider) describe() string { return fmt.Sprintf("%d request(s) made", f.count()) }

// ---------------------------------------------------------------- the fixtures and assertions

// awaitingFixture is a session whose model is a script. Everything else is the real thing: real storage,
// real run, real tools — the two builtins are on so a reply can ask for one and a result can arrive.
func awaitingFixture(t *testing.T, cfg Config, script ...turn) (*Session, *fakeProvider) {
	t.Helper()
	if cfg.ID == "" {
		t.Fatal("a fixture needs a session id")
	}
	cfg.BuiltinTools = []string{"wait", "get_datetime"}
	// The base URL is never dialled: the provider below is what answers. It is a plain field rather than
	// a factory argument, which is the whole reason a request can be held open from a test.
	cfg.LLM = llm.Config{BaseURL: "http://127.0.0.1:1/v1"}

	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create session %s: %v", cfg.ID, err)
	}
	f := &fakeProvider{script: script}
	s.provider = f
	t.Cleanup(func() { _ = s.Close() })
	return s, f
}

// send is the test's version of a person pressing send, and it insists the message was accepted: held or
// appended, the sender is told it arrived either way (D85).
func send(t *testing.T, s *Session, text string) {
	t.Helper()
	if err := s.Send(context.Background(), Message{Text: text}); err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
}

func waitForIdle(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s.Status().Phase == PhaseIdle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the session is still %q after 20s: %s", s.Status().Phase, describePath(s.path()))
}

// wantQueued and wantOnPath are the two halves of the only claim these tests keep making: a message is
// either on the path or in the queue, never in neither, and never in both.
func wantQueued(t *testing.T, s *Session, n int) {
	t.Helper()
	if st := s.Status(); st.Queued != n {
		t.Errorf("queued = %d, want %d (path: %s)", st.Queued, n, describePath(s.path()))
	}
}

func wantPending(t *testing.T, s *Session, n int) {
	t.Helper()
	if st := s.Status(); st.Pending != n {
		t.Errorf("pending = %d, want %d: pending counts the path, and what is queued is not on it", st.Pending, n)
	}
}

// payloadDump is one line per message, for a failure a reader can actually read.
func payloadDump(req llm.Request) string {
	var b strings.Builder
	for i, m := range req.Messages {
		fmt.Fprintf(&b, "\n  %d %s %s", i, m.Role, clip(fmt.Sprintf("%v", m.Content), 120))
	}
	return b.String()
}

// indexOfMessage is the position in the payload of the first message whose text contains text, or -1.
func indexOfMessage(req llm.Request, text string) int {
	for i, m := range req.Messages {
		if strings.Contains(fmt.Sprintf("%v", m.Content), text) {
			return i
		}
	}
	return -1
}

// indexOfRole is the position of the nth message (1-based) with that role, or -1. The content of a tool
// result is folded on the way to the payload (asm), so a role is the reliable thing to order by.
func indexOfRole(req llm.Request, role string, nth int) int {
	for i, m := range req.Messages {
		if m.Role != role {
			continue
		}
		if nth--; nth == 0 {
			return i
		}
	}
	return -1
}

// assertReadsBefore fails unless the model was shown both texts, the first one ahead of the second. Every
// one of these tests is about order, and the order that matters is the one the model reads.
func assertReadsBefore(t *testing.T, req llm.Request, first, second string) {
	t.Helper()
	i, j := indexOfMessage(req, first), indexOfMessage(req, second)
	if i < 0 {
		t.Errorf("the model was never shown %q:%s", first, payloadDump(req))
		return
	}
	if j < 0 {
		t.Errorf("the model was never shown %q:%s", second, payloadDump(req))
		return
	}
	if i > j {
		t.Errorf("the model reads %q (message %d) before %q (message %d):%s", second, j, first, i, payloadDump(req))
	}
}

// wantUserAt pins the nth entry of the path as exactly that user message.
func wantUserAt(t *testing.T, path []*ir.Entry, n int, text string) {
	t.Helper()
	if n >= len(path) {
		t.Errorf("the path has %d entries, nothing at %d: %s", len(path), n, describePath(path))
		return
	}
	e := path[n]
	if e.Kind != ir.KindUser || e.Content != text {
		t.Errorf("entry %d = %s %q, want the user message %q: %s", n, e.Kind, e.Content, text, describePath(path))
	}
}

// ---------------------------------------------------------------- the tests

func TestAMessageSentWhileARequestIsInFlightStaysOffThePath(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "inflight"},
		gatedSays("the first answer"),
		says("the second answer"),
	)
	send(t, s, "the first question")
	f.waitForRequest(t, 1)

	// From here the session has sent a payload and not received its reply: the debt answeringLocked
	// calls the request in flight. Appending now would put this message in front of an answer that
	// cannot include it any more — the model would read the message after having written the reply
	// that answers a question the message never touched.
	send(t, s, "added while you were busy")

	if n := len(s.path()); n != 1 {
		t.Errorf("the path grew to %d entries while a request was in flight: %s", n, describePath(s.path()))
	}
	wantQueued(t, s, 1)
	wantPending(t, s, 0)
	if st := s.Status(); st.Phase != PhaseRunning {
		t.Errorf("phase = %q, want %q", st.Phase, PhaseRunning)
	}

	// Leave nothing blocked: the run answers, and the message takes its place behind that answer.
	f.gate(t, 1).release()
	f.waitForRequest(t, 2)
	waitForIdle(t, s)
}

func TestAMessageSentWhileARequestIsInFlightIsAnsweredByTheSameRun(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "same-run"},
		gatedSays("the first answer"),
		says("the second answer"),
	)
	send(t, s, "the first question")
	f.waitForRequest(t, 1)
	send(t, s, "added while you were busy")
	f.gate(t, 1).release()

	// The reply, then the message, then the reply to the message: the run is not finished with the
	// conversation just because it finished with the request. needsRun reads the tip, finds the message
	// the flush put there, and asks again — which is the whole fix, and the thing that used to be
	// impossible: the tip was the reply, so the loop stopped and nobody ever asked about the message
	// again, in this run or in any later one (Resume refuses an assistant tip too).
	path := waitForPath(t, s, 4)
	f.waitForRequest(t, 2)
	waitForIdle(t, s)

	if n := len(path); n != 4 {
		t.Fatalf("the path is %d entries, want 4: %s", n, describePath(path))
	}
	if path[1].Kind != ir.KindAssistant || path[1].Content != "the first answer" {
		t.Errorf("the second entry is %s %q, want the first answer: %s", path[1].Kind, path[1].Content, describePath(path))
	}
	wantUserAt(t, path, 2, "added while you were busy")
	if path[3].Kind != ir.KindAssistant || path[3].Content != "the second answer" {
		t.Errorf("the last entry is %s %q, want the answer to the held message: %s",
			path[3].Kind, path[3].Content, describePath(path))
	}
	wantQueued(t, s, 0)

	// What the model was actually shown, in the order it was shown: the message joins after the reply
	// that never saw it, and it is there for the turn that answers it.
	second := f.waitForRequest(t, 2)
	assertReadsBefore(t, second, "the first question", "the first answer")
	assertReadsBefore(t, second, "the first answer", "added while you were busy")
}

func TestAMessageSentBehindAToolCallReplyWaitsForTheResult(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "tool-call"},
		gatedToolCalls("call-1", "get_datetime", `{}`),
		says("after the result"),
	)
	send(t, s, "the first question")
	f.waitForRequest(t, 1)
	send(t, s, "added while you were busy")
	f.gate(t, 1).release()

	// A reply that asks for tools is not the end of the debt, so it is not the flush point either: the
	// message belongs after the call's answer, which is where the assembler says any message held for a
	// call belongs. The log is append-only, so the final order is the proof that it landed there.
	path := waitForPath(t, s, 5)
	waitForIdle(t, s)

	if n := len(path); n != 5 {
		t.Fatalf("the path is %d entries, want 5: %s", n, describePath(path))
	}
	if path[1].Kind != ir.KindAssistant || len(path[1].ToolCalls) != 1 {
		t.Errorf("entry 1 is a %s with %d call(s), want the turn asking for one: %s",
			path[1].Kind, len(path[1].ToolCalls), describePath(path))
	}
	if path[2].Kind != ir.KindTool || path[2].ToolCallID != "call-1" {
		t.Errorf("entry 2 = %s answering %q, want the result of call-1: %s",
			path[2].Kind, path[2].ToolCallID, describePath(path))
	}
	wantUserAt(t, path, 3, "added while you were busy")

	asked := f.waitForRequest(t, 2)
	toolAt, userAt := indexOfRole(asked, "tool", 1), indexOfMessage(asked, "added while you were busy")
	if toolAt < 0 || userAt < 0 {
		t.Fatalf("the second payload is missing the tool result or the message:%s", payloadDump(asked))
	}
	if toolAt > userAt {
		t.Errorf("the model reads the new message before the tool answer it came after (%d vs %d):%s",
			toolAt, userAt, payloadDump(asked))
	}
}

func TestInterruptingBeforeTheFirstAnswerSendsTheMessageAndAsksAgain(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "interrupt-early"},
		onTheWireUntilCancelled(),
		says("the interrupt, answered"),
	)
	send(t, s, "the first question")
	f.waitForRequest(t, 1)

	// The abort lands before a single token has come back, so there is no half reply and no entry at
	// all: generate has nothing to hand turns(), which is the one case that used to leave the message
	// appended by nobody and answered by nobody. The message is the last word of this test's name for a
	// reason — a force interrupt that is not answered is a message that was thrown away.
	if err := s.Interrupt(Message{Text: "no, read this instead"}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	f.waitForRequest(t, 2)
	path := waitForPath(t, s, 3)
	waitForIdle(t, s)

	if n := len(path); n != 3 {
		t.Fatalf("the path is %d entries, want 3: %s", n, describePath(path))
	}
	wantUserAt(t, path, 1, "no, read this instead")
	if path[2].Kind != ir.KindAssistant {
		t.Fatalf("entry 2 is a %s, want the answer to the interrupt: %s", path[2].Kind, describePath(path))
	}
	// The second request is the point of the test, and this is the assertion that says whether it was
	// real: a request built on the cancelled context would come back as "[generation failed: context
	// canceled]", which is what a force interrupt used to produce.
	if path[2].Content != "the interrupt, answered" || path[2].Label == "llm error" {
		t.Errorf("the run after the interrupt answered %q (label %q), want a live answer: %s",
			path[2].Content, path[2].Label, describePath(path))
	}
	asked := f.waitForRequest(t, 2)
	if indexOfMessage(asked, "no, read this instead") < 0 {
		t.Errorf("the second request did not carry the message that interrupted it:%s", payloadDump(asked))
	}
	if got := f.count(); got != 2 {
		t.Errorf("the provider was asked %d times, want 2: one dead request and the run that answered", got)
	}
	wantQueued(t, s, 0)
}

func TestTwoInterruptsInARowBothReachThePath(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "two-interrupts"},
		halfOnCancel("half a sentence"),
		onTheWireUntilCancelled(),
		says("both messages, both read"),
	)
	send(t, s, "the first question")
	f.waitForRequest(t, 1)

	// Two presses, two messages. The first interrupt is answered by a run that is already on its way
	// (the half output is committed, marked interrupted), and the second finds that fresh run and
	// interrupts it too. There used to be one slot for a forced message, so the second press quietly
	// replaced the first and nobody said a word about the message that vanished.
	if err := s.Interrupt(Message{Text: "first correction"}); err != nil {
		t.Fatalf("interrupt 1: %v", err)
	}
	f.waitForRequest(t, 2)
	if err := s.Interrupt(Message{Text: "no, both of them"}); err != nil {
		t.Fatalf("interrupt 2: %v", err)
	}

	f.waitForRequest(t, 3)
	path := waitForPath(t, s, 5)
	waitForIdle(t, s)

	if n := len(path); n != 5 {
		t.Fatalf("the path is %d entries, want 5: %s", n, describePath(path))
	}
	if path[1].Kind != ir.KindAssistant || !path[1].Interrupted {
		t.Errorf("entry 1 = %s interrupted=%v, want the half reply kept and marked: %s",
			path[1].Kind, path[1].Interrupted, describePath(path))
	}
	wantUserAt(t, path, 2, "first correction")
	wantUserAt(t, path, 3, "no, both of them")

	asked := f.waitForRequest(t, 3)
	assertReadsBefore(t, asked, "first correction", "no, both of them")
	wantQueued(t, s, 0)
}

func TestStoppingARunWithoutAMessageAsksNothingNew(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "stop-only"}, onTheWireUntilCancelled())
	send(t, s, "the first question")
	f.waitForRequest(t, 1)

	// Stop is not a person and brings no message, so it promises no run: pressing stop means "say no
	// more", not "ask again". Interrupt is the one that carries a message, and only it makes the promise.
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitForIdle(t, s)

	if got := f.count(); got != 1 {
		t.Errorf("the provider was asked %d times, want 1: nothing was sent, so nothing is owed an answer", got)
	}
	if n := len(s.path()); n != 1 {
		t.Errorf("the path is %d entries, want 1: %s", n, describePath(s.path()))
	}
	wantQueued(t, s, 0)
}

func TestStoppingARunStillLandsTheMessageSentFirst(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "stop-and-land"}, onTheWireUntilCancelled())
	send(t, s, "the first question")
	f.waitForRequest(t, 1)
	send(t, s, "one more thing")

	// The message was accepted before the stop, so the stop may end the run but may not end the
	// message: it is the run's epilogue that puts it on the path, and a queue nobody owns any more is a
	// message that was accepted and then lost. What stop withholds is the new run, not the write.
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	path := waitForPath(t, s, 2)
	waitForIdle(t, s)

	wantUserAt(t, path, 1, "one more thing")
	wantQueued(t, s, 0)
	if got := f.count(); got != 1 {
		t.Errorf("the provider was asked %d times, want 1: a stop asks nothing", got)
	}
}

func TestAFailedRequestClosesTheDebtItOpened(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "provider-error"},
		refused("llm: 400 Bad Request: nothing in the model likes this"),
		says("after the error"),
	)
	send(t, s, "the first question")
	waitForPath(t, s, 2)
	waitForIdle(t, s)

	// A request that came back as an error entry still came back: the debt it opened is closed by that
	// entry, and the next message is an ordinary send. A hold with no end is the worse bug of the two —
	// it parks a message in memory until the next restart, for the sake of an answer nobody is waiting
	// for any more.
	send(t, s, "the second question")
	// Queued is the race-free half of the claim: nothing can put the message back into the queue, while
	// the path may already have grown past this message by the time it is read (the answer is scripted
	// and instant). What must not be true is that it is waiting for the failed request's reply.
	wantQueued(t, s, 0)
	if len(s.path()) < 3 {
		t.Errorf("the message after a failed request was not appended like any other: %s", describePath(s.path()))
	}

	f.waitForRequest(t, 2)
	path := waitForPath(t, s, 4)
	waitForIdle(t, s)
	wantUserAt(t, path, 2, "the second question")
	if path[3].Kind != ir.KindAssistant || path[3].Content != "after the error" {
		t.Errorf("entry 3 = %s %q, want the answer to the second question: %s",
			path[3].Kind, path[3].Content, describePath(path))
	}
}

func TestMaxTurnsStopsOnAnUnansweredCallAndStillLandsTheMessage(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "max-turns", MaxTurns: 1},
		gatedToolCalls("call-1", "get_datetime", `{}`),
		says("the answer after"),
	)
	send(t, s, "the first question")
	f.waitForRequest(t, 1)
	send(t, s, "added while you were busy")
	f.gate(t, 1).release()

	// maxTurns stops the run on a call that will never be answered, and that is a legal place to stop:
	// the assembler writes that call's result as the interruption it was. It is no reason to keep a
	// message waiting for an answer this run has given up on — and the path shape it leaves,
	// [assistant(tool_calls), user], is exactly what the next payload has to be able to read.
	path := waitForPath(t, s, 4)
	waitForIdle(t, s)

	if n := len(path); n != 4 {
		t.Fatalf("the path is %d entries, want 4: %s", n, describePath(path))
	}
	if len(path[1].ToolCalls) != 1 {
		t.Errorf("entry 1 asks for %d call(s), want 1: %s", len(path[1].ToolCalls), describePath(path))
	}
	if path[2].Kind == ir.KindTool {
		t.Fatalf("the call ran after maxTurns stopped the run: %s", describePath(path))
	}
	wantUserAt(t, path, 2, "added while you were busy")

	asked := f.waitForRequest(t, 2)
	if indexOfMessage(asked, "no result recorded") < 0 {
		t.Errorf("the unanswered call was sent bare:%s", payloadDump(asked))
	}
	assertReadsBefore(t, asked, "no result recorded", "added while you were busy")
}

// TestEmptyMessageIsOneRule covers the rule itself, which is now written once (emptyMessage) rather than
// once per verb: whitespace is nothing, and a picture is a message even with no words next to it.
func TestEmptyMessageIsOneRule(t *testing.T) {
	picture := []ir.Part{{Type: ir.PartImage}}
	for _, c := range []struct {
		what  string
		in    Message
		empty bool
	}{
		{"words", Message{Text: "hello"}, false},
		{"nothing at all", Message{}, true},
		{"spaces", Message{Text: "   "}, true},
		{"a newline and a tab", Message{Text: "\n\t"}, true},
		{"a picture alone", Message{Images: picture}, false},
		{"a picture and spaces", Message{Text: "   ", Images: picture}, false},
	} {
		if got := emptyMessage(c.in); got != c.empty {
			t.Errorf("%s: emptyMessage = %v, want %v", c.what, got, c.empty)
		}
	}
}

// TestASendAndAnInterruptRefuseAnEmptyMessageInTheSameWay is the whole of F8. The bolt and Enter are two
// gestures and "you sent nothing" is one answer: an interrupt of nothing used to abort the run, answer ok and
// throw away whatever whitespace the body carried, while the same body sent to Send was refused.
func TestASendAndAnInterruptRefuseAnEmptyMessageInTheSameWay(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "empty-draft"})

	if err := s.Send(context.Background(), Message{}); err != errEmptyMessage {
		t.Errorf("send of nothing: %v, want %v", err, errEmptyMessage)
	}
	if err := s.Send(context.Background(), Message{Text: "   "}); err != errEmptyMessage {
		t.Errorf("send of spaces: %v, want %v", err, errEmptyMessage)
	}
	if err := s.Interrupt(Message{}); err != errEmptyMessage {
		t.Errorf("interrupt of nothing: %v, want %v", err, errEmptyMessage)
	}
	if err := s.Interrupt(Message{Text: " \t\n "}); err != errEmptyMessage {
		t.Errorf("interrupt of spaces: %v, want %v", err, errEmptyMessage)
	}

	// Refusing is all it does: nothing reached the path, nothing was queued, and no model was asked -
	// an interrupt in particular used to abort whatever was running before anyone looked at the body.
	if n := len(s.path()); n != 0 {
		t.Errorf("the path holds %d entries after four refusals: %s", n, describePath(s.path()))
	}
	wantQueued(t, s, 0)
	if got := f.count(); got != 0 {
		t.Errorf("the provider was asked %d times, want 0", got)
	}
	if st := s.Status(); st.Phase != PhaseIdle {
		t.Errorf("phase = %q after four refusals, want %q", st.Phase, PhaseIdle)
	}
}


// ---------------------------------------------------------------- a manual summary holds the path too

// TestAMessageSentWhileASummaryIsRunningWaitsForTheBoundaryItIsWriting is the send half of the rule a manual
// summary adds: the entry it is about to append *is* the compaction boundary, and the plan for that boundary was
// made from a path without this message in it. So the message waits in memory and lands behind the boundary,
// instead of under a summary that cannot know it exists.
//
// A real summary cannot be driven from a test — the fake provider has no scripted summary, which is what makes an
// unplanned one fail loudly — so this stands the summary up exactly the way CompactNow leaves it: compacting on,
// a hand on the cancellation, the phase announced. What is asserted is the two rules the session keeps around
// those flags, not the summary itself.
func TestAMessageSentWhileASummaryIsRunningWaitsForTheBoundaryItIsWriting(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "send-during-summary"}, says("the answer that was waiting"))

	// A summary of this kind cannot be running while a run is — CompactNow refuses that, which is the only
	// reason what happens when it ends can hand the session straight to a run.
	s.mu.Lock()
	s.compacting = true
	s.compactCancel = func(error) {}
	s.phase = phaseCompacting
	s.mu.Unlock()

	send(t, s, "written while you were summarising")

	if n := len(s.path()); n != 0 {
		t.Errorf("the path grew to %d entries while a summary was writing: %s", n, describePath(s.path()))
	}
	wantQueued(t, s, 1)
	if got := f.count(); got != 0 {
		t.Errorf("the provider was asked %d time(s) during a summary, want 0: a held message must not start a run in the middle of a boundary", got)
	}

	// The summary lands. endManualCompaction is what CompactNow defers, so this is the whole of the way out.
	s.endManualCompaction()

	path := waitForPath(t, s, 1)
	wantUserAt(t, path, 0, "written while you were summarising")
	wantQueued(t, s, 0)

	// And it is not left as a question nobody owns: the run it earns is started for it, and reads the message
	// the summary could not have known about.
	asked := f.waitForRequest(t, 1)
	if indexOfMessage(asked, "written while you were summarising") < 0 {
		t.Errorf("the run started after the summary did not read the message:%s", payloadDump(asked))
	}
	waitForIdle(t, s)
}

// TestAnInterruptIsRefusedWhileASummaryIsRunning is the other half: the bolt during a summary used to be
// downgraded into a send. It answered ok while doing something else, and left the person who pressed it holding
// an abort that never happened — the one gesture in this daemon that promises to stop something, meeting the one
// thing that is not a generation. A summary is stopped, and the refusal names the button.
func TestAnInterruptIsRefusedWhileASummaryIsRunning(t *testing.T) {
	s, f := awaitingFixture(t, Config{ID: "interrupt-during-summary"}, says("the answer after the summary"))
	s.mu.Lock()
	s.compacting = true
	s.compactCancel = func(error) {}
	s.phase = phaseCompacting
	s.mu.Unlock()

	err := s.Interrupt(Message{Text: "stop and answer this instead"})
	if err == nil {
		t.Fatal("an interrupt of a running summary was accepted: it is a send wearing another name")
	}
	for _, want := range []string{"summary", "/stop", "interrupt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so the reader is left to guess: %v", want, err)
		}
	}

	// Refusing costs nothing: nothing was appended, nothing was queued, nothing was aborted, and the summary the
	// refusal was about is still running.
	if n := len(s.path()); n != 0 {
		t.Errorf("a refused interrupt still put %d entries on the path: %s", n, describePath(s.path()))
	}
	wantQueued(t, s, 0)
	if got := f.count(); got != 0 {
		t.Errorf("a refused interrupt asked the provider %d time(s), want 0", got)
	}
	s.mu.Lock()
	running := s.manualSummaryLocked()
	s.mu.Unlock()
	if !running {
		t.Error("refusing the interrupt ended the summary it was refusing")
	}

	// Once the summary is over, the same gesture means what it always meant on a session with nothing running:
	// the message goes out.
	s.endManualCompaction()
	if err := s.Interrupt(Message{Text: "stop and answer this instead"}); err != nil {
		t.Fatalf("interrupt after the summary: %v", err)
	}
	waitForPath(t, s, 1)
	f.waitForRequest(t, 1)
	waitForIdle(t, s)
}
