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

// Unload is the route that gives the memory of a conversation back while keeping the conversation, so
// every test here asks the same thing from both sides: is it gone from this process, and is it still on
// disk? Only the first is what the caller asked for, and losing the second is the failure this feature
// would most like to hide — it would look exactly like success.

// TestUnloadPutsTheConversationDownAndKeepsIt is the promise in one test.
func TestUnloadPutsTheConversationDownAndKeepsIt(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(context.Background(), Config{ID: "put-down", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.appendEntry(&ir.Entry{Kind: ir.KindUser, Role: "user", Content: "one", Ts: time.Now().UTC()}); err != nil {
		t.Fatalf("append: %v", err)
	}
	before := len(s.path())

	wasOpen, err := m.Unload("put-down")
	if err != nil {
		t.Fatalf("unload: %v", err)
	}
	if !wasOpen {
		t.Error("unloaded=false, and this process was holding the session a moment ago")
	}
	if _, still := m.Peek("put-down"); still {
		t.Fatal("the session is still open, so Unload closed nothing")
	}

	// The row is the other half of the promise: a session that vanishes from the list is a deleted session
	// as far as a sidebar can tell. The list is built from config.json and the index rather than from a
	// live session precisely so it can say "closed" instead of nothing.
	var row *Summary
	for _, sum := range m.List() {
		if sum.ID == "put-down" {
			got := sum
			row = &got
		}
	}
	if row == nil {
		t.Fatal("the unloaded session left the list of sessions")
	}
	if !row.Closed {
		t.Error("the row does not say closed, so a reader cannot tell it from a session that is running")
	}

	again, err := m.Get(context.Background(), "put-down")
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer func() { _ = again.Close() }()
	if n := len(again.path()); n != before {
		t.Errorf("the path came back %d entries long, want the %d that were there before it was put down", n, before)
	}
	if again.Status().Phase != PhaseIdle {
		t.Errorf("the reopened session is %s, want idle", again.Status().Phase)
	}
}

// TestUnloadRefusesWhileASessionIsBusy is the difference between this route and a stop button. The part
// that is easy to get wrong is the second assertion: a refusal has to leave the session open, because the
// caller's next request is the one that stops it.
func TestUnloadRefusesWhileASessionIsBusy(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	t.Cleanup(m.Close)
	s, err := m.Create(context.Background(), Config{
		ID:           "busy",
		BuiltinTools: []string{"wait", "get_datetime"},
		LLM:          llm.Config{BaseURL: "http://127.0.0.1:1/v1"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A parked wait is the cheapest way to be a non-idle session with nothing on a network: the wake is a
	// run with an unanswered call (park.go), and this one is not due for another four hours.
	if _, err := s.appendEntry(&ir.Entry{
		Kind:      ir.KindAssistant,
		Role:      "assistant",
		Model:     "test-model",
		Finish:    "tool_calls",
		Ts:        time.Now().Add(-2 * time.Hour).UTC(),
		ToolCalls: []ir.ToolCall{{ID: "call-1", Name: "wait", Args: json.RawMessage(`{"duration":"6h"}`)}},
	}); err != nil {
		t.Fatalf("append the wait call: %v", err)
	}
	s.recoverPark()
	if s.phaseNow() == phaseIdle {
		t.Fatal("the fixture is idle, so this test would be asserting nothing")
	}

	wasOpen, err := m.Unload("busy")
	if err == nil {
		t.Fatal("a busy session was unloaded")
	}
	if wasOpen {
		t.Error("unloaded=true for a request that was refused")
	}
	if !strings.Contains(err.Error(), PhaseRunning) {
		t.Errorf("error %q does not name the phase, which is the caller's next question", err)
	}
	if _, still := m.Peek("busy"); !still {
		t.Fatal("a refused unload cost the caller the session, and stopping it is the next request")
	}
	if n := len(s.path()); n != 1 {
		t.Errorf("the path is %d entries, want the one entry the fixture wrote", n)
	}
}

// TestUnloadOfASessionNobodyHadOpened answers not-an-error, because a session this process is not holding
// is already in the state the caller wanted; and answers an error for an id with no directory behind it,
// because that is a typo, and an ok to a typo is how a script spends an hour unloading nothing.
func TestUnloadOfASessionNobodyHadOpened(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	if err := m.SaveConfig(Config{ID: "never-opened"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	wasOpen, err := m.Unload("never-opened")
	if err != nil {
		t.Fatalf("unloading a session nobody had opened: %v", err)
	}
	if wasOpen {
		t.Error("unloaded=true for a session this process never opened")
	}

	if _, err := m.Unload("no-such-session"); err == nil {
		t.Error("an id with no directory behind it answered ok")
	} else if !strings.Contains(err.Error(), "no session") {
		t.Errorf("error %q, want it to say there is no such session", err)
	}
}
