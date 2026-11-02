package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// The wake path is tested through a real session because the thing being tested is which entries land
// on which path in which order — and that answer lives in storage, not in any one function. No run ever
// reaches a model: the LLM points at a port nothing listens on, so a wake that overran the tool result
// and asked the model anyway fails in about a second rather than hanging.

// parkFixture is a session whose path ends the way a run leaves it when a wait is handed over: the
// assistant entry with the call, and no result. That is all a parked session is made of, which is why
// the fixture goes through the same appendEntry the loop goes through rather than a shape that merely
// looks similar — and why `asked` is the timestamp of the entry: a wait is due when the call said it
// would be, and the call is in that entry. It is named after the moment rather than after the context
// value the resumed call is handed (park.go askedAt) so the two cannot be confused from inside a test.
func parkFixture(t *testing.T, id, toolName string, asked time.Time, args string) *Session {
	t.Helper()
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(context.Background(), Config{
		ID: id,
		// Both of the daemon's own tools are on, so that a test can say which of the two a trailing
		// call turned out to be.
		BuiltinTools: []string{"wait", "get_datetime"},
		LLM:          llm.Config{BaseURL: "http://127.0.0.1:1/v1"},
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.appendEntry(&ir.Entry{
		Kind:      ir.KindAssistant,
		Role:      "assistant",
		Content:   "I will be quiet for a while.",
		Model:     "test-model",
		Finish:    "tool_calls",
		Ts:        asked.UTC(),
		ToolCalls: []ir.ToolCall{{ID: "call-1", Name: toolName, Args: json.RawMessage(args)}},
	}); err != nil {
		t.Fatalf("append the %s call: %v", toolName, err)
	}
	return s
}

// waitForPath polls until the path is at least n entries long. Everything a wake does happens on the
// run's own goroutine, so "the entry is on the path" is the only honest form of "it happened" — and the
// deadline is this test's, not the sleep the wait is no longer taking.
func waitForPath(t *testing.T, s *Session, n int) []*ir.Entry {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p := s.path(); len(p) >= n {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	p := s.path()
	t.Fatalf("the path is still %d entries after 20s, want %d: %s", len(p), n, describePath(p))
	return nil
}

func describePath(path []*ir.Entry) string {
	var b strings.Builder
	for _, e := range path {
		b.WriteString("\n  " + string(e.Kind) + " " + clip(e.Content, 120))
	}
	return b.String()
}

// toolResult is the entry a woken wait leaves behind, and its content is two things: the tool's own
// JSON, and the daemon's note after it. Splitting them is what the assertions are about — the JSON has
// to stay parseable on its own, or the model cannot read the answer.
func toolResult(t *testing.T, e *ir.Entry) (map[string]any, string) {
	t.Helper()
	if e.Kind != ir.KindTool {
		t.Fatalf("entry %s is a %s, want a tool result: %s", e.ID, e.Kind, describePath([]*ir.Entry{e}))
	}
	head, note, _ := strings.Cut(e.Content, "\n\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(head), &got); err != nil {
		t.Fatalf("the wait's answer is not JSON on its own (%v):\n%s", err, e.Content)
	}
	return got, note
}

func TestADeadlineFromThePreviousProcessIsReArmed(t *testing.T) {
	// Asked for two hours ago, six hours long: due in four hours, and the moment is the ask's rather
	// than this open's — which is the whole reason recovery is possible at all.
	s := parkFixture(t, "rearm", "wait", time.Now().Add(-2*time.Hour), `{"duration":"6h"}`)
	s.recoverPark()

	if !s.isParked() {
		t.Fatal("the session did not pick the wait up")
	}
	st := s.Status()
	if st.Phase != PhaseRunning {
		t.Errorf("phase = %q, want %q: a session holding a wait is not idle", st.Phase, PhaseRunning)
	}
	if st.Queued != 0 {
		t.Errorf("queued = %d, want 0", st.Queued)
	}
	if st.Pending != 0 {
		t.Errorf("pending = %d, want 0: a parked session has nothing waiting to be sent", st.Pending)
	}
	due, err := time.Parse(time.RFC3339, st.WakeAt)
	if err != nil {
		t.Fatalf("wakeAt %q is not an RFC3339 moment: %v", st.WakeAt, err)
	}
	if want := time.Now().Add(4 * time.Hour); due.Sub(want) > time.Minute || want.Sub(due) > time.Minute {
		t.Errorf("wakeAt = %s, want about %s", st.WakeAt, want.Format(time.RFC3339))
	}
	if len(s.path()) != 1 {
		t.Errorf("the path grew to %d entries while nothing was due: %s", len(s.path()), describePath(s.path()))
	}
}

func TestAWakeDueWhileTheDaemonWasDownIsAnsweredLate(t *testing.T) {
	asked := time.Now().Add(-7 * time.Hour)
	s := parkFixture(t, "late", "wait", asked, `{"duration":"6h"}`)
	s.recoverPark()

	path := waitForPath(t, s, 2)
	got, note := toolResult(t, path[1])
	if got["status"] != "completed" {
		t.Errorf("status = %v, want completed: the wait was slept by the world, not by this process", got["status"])
	}
	if got["resumed_at"] == nil || got["scheduled_at"] == nil {
		t.Errorf("the answer does not carry both moments it owes: %v", got)
	}
	// The moment it was due is six hours from the ask, and the ask is the entry in the log. Counted from the
	// wake instead, this call would have found itself due seven hours from then and handed itself over again
	// — a wait that is woken forever and never answered, which is what this assertion is really guarding.
	// Compared as moments rather than as text, because which zone the answer spells the moment in is the
	// session's timezone setting and not this test's business.
	if raw, ok := got["scheduled_at"].(string); !ok {
		t.Errorf("scheduled_at is not a moment: %v", got["scheduled_at"])
	} else if due, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Errorf("scheduled_at %q is not RFC3339: %v", raw, err)
	} else if want := asked.Add(6 * time.Hour).Truncate(time.Second); !due.Equal(want) {
		t.Errorf("scheduled_at = %s, want %s: the promise was made when the call was, not when this daemon woke up",
			raw, want.Format(time.RFC3339))
	}
	if !strings.Contains(note, "the daemon restarted") {
		t.Errorf("note = %q, want the reason only this daemon can witness", note)
	}
	// An hour is how long the answer was late: asked seven hours ago, six hours long.
	if !strings.Contains(note, "1h0m0s") {
		t.Errorf("note = %q, want it to say how late the answer was", note)
	}
	if strings.Contains(note, "resumed_at") {
		t.Errorf("note repeats a timestamp the JSON already gives: %q", note)
	}
}

func TestAScheduledWakeDoesNotExplainItself(t *testing.T) {
	// Due half a second from now, so the timer's own decision can be asked about without waiting for it,
	// and the wake is the ordinary kind: the tool sleeps what is left of the wait and answers for itself.
	s := parkFixture(t, "ontime", "wait", time.Now().Add(500*time.Millisecond-6*time.Hour), `{"duration":"6h"}`)
	s.recoverPark()
	if !s.isParked() {
		t.Fatal("the session did not pick the wait up")
	}

	s.parkTickAt(time.Now().Add(time.Minute))
	got, note := toolResult(t, waitForPath(t, s, 2)[1])
	if got["status"] != "completed" {
		t.Errorf("status = %v, want completed: a wake that came on time is not an interruption", got["status"])
	}
	if note != "" {
		t.Errorf("note = %q, want nothing: a wait that came due on schedule owes nobody an explanation", note)
	}
}

func TestAnEarlyTickRearmsInsteadOfWaking(t *testing.T) {
	s := parkFixture(t, "early", "wait", time.Now().Add(-2*time.Hour), `{"duration":"6h"}`)
	s.recoverPark()

	// A tick from a day ago is what a suspended machine delivers: the process was not alive to count,
	// so the only honest answer is "not yet" and a shorter timer.
	s.parkTickAt(time.Now().Add(-24 * time.Hour))

	if !s.isParked() {
		t.Fatal("a tick that arrived a day early woke the wait")
	}
	s.mu.Lock()
	running, armed := s.cancel != nil, s.park.timer != nil
	s.mu.Unlock()
	if running {
		t.Error("a tick that arrived early started a run")
	}
	if !armed {
		t.Error("a tick that arrived early did not leave a timer behind")
	}
	if n := len(s.path()); n != 1 {
		t.Errorf("the path grew to %d entries: %s", n, describePath(s.path()))
	}
}

func TestAMessageSentWhileWaitingStaysOffThePath(t *testing.T) {
	s := parkFixture(t, "queue", "wait", time.Now().Add(-time.Hour), `{"duration":"6h"}`)
	s.recoverPark()

	for _, text := range []string{"are you there?", "and now?"} {
		if err := s.Send(context.Background(), Message{Text: text}); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
	}

	// Appending them now would put a user message between a tool call and its result, which is the one
	// shape the assembler answers by inventing a failure for a tool that is only still waiting.
	if n := len(s.path()); n != 1 {
		t.Errorf("the path grew to %d entries while waiting: %s", n, describePath(s.path()))
	}
	st := s.Status()
	if st.Queued != 2 {
		t.Errorf("queued = %d, want 2", st.Queued)
	}
	if st.Pending != 0 {
		t.Errorf("pending = %d, want 0: what is queued is not on the path, so it cannot be pending", st.Pending)
	}
}

func TestInterruptingAWaitAnswersBeforeItSpeaks(t *testing.T) {
	s := parkFixture(t, "interrupt", "wait", time.Now().Add(-time.Hour), `{"duration":"6h"}`)
	s.recoverPark()
	if err := s.Send(context.Background(), Message{Text: "first, while you were asleep"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	if err := s.Interrupt(Message{Text: "wake up and read this"}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	path := waitForPath(t, s, 3)
	got, note := toolResult(t, path[1])
	if got["status"] != "interrupted" {
		t.Errorf("status = %v, want interrupted", got["status"])
	}
	if got["reason"] != "user interrupt" {
		t.Errorf("reason = %v, want %q: a model that reads a bare interrupted schedules the same wait again",
			got["reason"], "user interrupt")
	}
	if note != "" {
		t.Errorf("note = %q, want nothing: the interrupt is the explanation", note)
	}
	if path[2].Kind != ir.KindUser || path[2].Content != "first, while you were asleep" {
		t.Errorf("the queued message is not next after the result: %s", describePath(path[2:]))
	}
}

func TestStoppingAWaitNamesNoBlame(t *testing.T) {
	s := parkFixture(t, "stop", "wait", time.Now().Add(-time.Hour), `{"duration":"6h"}`)
	s.recoverPark()

	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	path := waitForPath(t, s, 2)
	got, _ := toolResult(t, path[1])
	if got["status"] != "interrupted" {
		t.Errorf("status = %v, want interrupted", got["status"])
	}
	// The key must be absent rather than empty: Stop is not a person, and nothing of anybody's is
	// coming after this result to be read.
	if _, ok := got["reason"]; ok {
		t.Errorf("a stop named a reason: %v", got["reason"])
	}
}

func TestAPathThatMovedUnderTheWaitGivesItUp(t *testing.T) {
	s := parkFixture(t, "void", "wait", time.Now().Add(-time.Hour), `{"duration":"6h"}`)
	s.recoverPark()
	if err := s.Send(context.Background(), Message{Text: "the entry it was waiting for is gone"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Deleting the entry takes the call out of the path — which is what an edit in the UI can do to a
	// parked session, because editing has never asked whether a run was in flight.
	if err := s.DeleteEntry(s.path()[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if s.isParked() {
		t.Fatal("the session is still waiting on a call that is no longer on the path")
	}
	path := waitForPath(t, s, 1)
	if path[0].Kind != ir.KindUser {
		t.Errorf("the queued message did not reach the path: %s", describePath(path))
	}
	// A park that is given up has to give the phase back too, or nothing this session is asked next
	// will be believed to be startable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.Status().Phase == PhaseIdle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("phase = %q, want idle: nothing is running and nothing is waiting", s.Status().Phase)
}

func TestAnUnansweredCallThatIsNotAWaitIsLeftAlone(t *testing.T) {
	// Two different ways a trailing call is not this code's to answer: a builtin that schedules nothing,
	// and a name that resolves to no tool at all (a tool the session switched off, or an MCP server that
	// is not in this config). Resuming either would mean either an invented deadline or running a tool
	// whose side effect nobody can confirm has not already happened.
	for _, tc := range []struct{ toolName, args string }{
		{"get_datetime", `{"timezone":"UTC"}`},
		{"shell", `{"command":"rm -rf /"}`},
	} {
		t.Run(tc.toolName, func(t *testing.T) {
			s := parkFixture(t, "notwait-"+strings.ReplaceAll(tc.toolName, "_", "-"), tc.toolName,
				time.Now().Add(-7*time.Hour), tc.args)
			s.recoverPark()
			if s.isParked() {
				t.Error("the scan picked up a call it does not own")
			}
			if n := len(s.path()); n != 1 {
				t.Errorf("the path grew to %d entries: %s", n, describePath(s.path()))
			}
		})
	}
}

func TestTheFactsAboutAResumedCallReachOnlyThatCall(t *testing.T) {
	// What this pins is the shape of one bug: a wake run does not end with the call it resumed, so the two
	// facts about a resumed wait — when it was asked, and why the answer is late — merely left where the run
	// could read them would be handed to every later call of that run too. Including a wait the model asks for
	// afterwards, whose deadline a re-counted ask would turn from a continuation into a restart, and whose
	// perfectly on-time answer `ResumeNote` would then claim was an hour late. It needs no session and no
	// model, because the fact is about a context value.
	asked, err := time.Parse(time.RFC3339, "2026-11-01T14:25:00Z")
	if err != nil {
		t.Fatalf("the fixture is not a moment: %v", err)
	}
	const note = "[the daemon restarted: this wait was due at ...]"
	ctx := withResumeCall(context.Background(), resumeCall{callID: "call-1", askedAt: asked, note: note})

	// This is what runTools does with the address, reproduced here because it is the pair that has to arrive
	// together: one value the tool computes its promise from, one value it puts after its own answer.
	c, ok := resumeCallFor(ctx, "call-1")
	if !ok {
		t.Fatal("the call this wake exists to answer was not told it is the one being resumed")
	}
	callCtx := withDaemonNote(withAskedAt(ctx, c.askedAt), c.note)
	if got := askedAt(callCtx); !got.Equal(asked) {
		t.Errorf("the resumed call was told it was asked at %s, want %s: recomputed from now it restarts forever",
			got.Format(time.RFC3339), asked.Format(time.RFC3339))
	}
	if got := daemonNote(callCtx); got != note {
		t.Errorf("the note does not survive into the call's context: %q", got)
	}

	// Every other call in the run is told there are no such facts, which is what keeps a second wait a new
	// promise rather than the continuation of one it never made.
	for _, other := range []string{"call-2", "", "call-1x"} {
		if got, ok := resumeCallFor(ctx, other); ok {
			t.Errorf("call %q was given the facts meant for call-1: %+v", other, got)
		}
	}
	// An address is not a fact: neither value is readable until runTools turns it into a per-call value, and
	// the run's own context must carry neither — that context is what every later call of this run is built
	// from, so a fact left on it is a fact about everything that follows.
	if got := askedAt(ctx); !got.IsZero() {
		t.Errorf("the run's own context carries an ask time (%s), which is how a later wait of this run "+
			"would be told it is a resumed one", got.Format(time.RFC3339))
	}
	if got := daemonNote(ctx); got != "" {
		t.Errorf("the run's own context carries the note, which is how every later tool of this run would read it: %q", got)
	}

	// Two shapes that mean "nothing here" and one that does not:
	//   - facts naming no call reach nobody, and
	//   - a wake with nothing to explain still has to say which call it is answering, because the ask's
	//     timestamp is not optional on a resumed wait — an empty note is the ordinary scheduled wake, and
	//     forgetting the address there is how a wait on time becomes a wait restarted.
	if _, ok := resumeCallFor(withResumeCall(context.Background(), resumeCall{askedAt: asked, note: note}), "call-1"); ok {
		t.Error("the facts about a call nobody named reached a call")
	}
	silent, ok := resumeCallFor(withResumeCall(context.Background(), resumeCall{callID: "call-1", askedAt: asked}), "call-1")
	if !ok || silent.note != "" || !silent.askedAt.Equal(asked) {
		t.Errorf("a wake with nothing to explain lost the address or the ask: %+v", silent)
	}
}
