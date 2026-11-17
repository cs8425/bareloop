package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// Remove is the only route that deletes a conversation, and until now it deleted it whatever the session
// was doing: a DELETE that landed mid-run closed the store under the run and took the directory with it,
// which surfaced as a nil-pointer panic in a goroutine nobody had asked a question about — and the guard
// around a run printed the value without a stack, so the log said nothing more than "invalid memory
// address or nil pointer dereference". These tests are the two halves of the fix: a busy session is
// refused and loses nothing, and an idle one goes completely, because a guard that refused everything
// would pass the first test too.

// busyParkedSession is the same fixture TestUnloadRefusesWhileASessionIsBusy uses, for the same reason: a
// parked wait is the cheapest way to be a non-idle session with nothing on a network. The wake is a run
// with an unanswered call (park.go), and this one is not due for another four hours.
func busyParkedSession(t *testing.T, m *Manager, id string) *Session {
	t.Helper()
	s, err := m.Create(context.Background(), Config{
		ID:           id,
		BuiltinTools: []string{"wait", "get_datetime"},
		LLM:          llm.Config{BaseURL: "http://127.0.0.1:1/v1"},
	})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
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
	return s
}

// TestRemoveRefusesWhileASessionIsBusy is the guard. The assertion that matters is not the error: it is
// that a refusal cost nothing, because the caller's next request is the stop and the one after that is
// the delete they meant.
func TestRemoveRefusesWhileASessionIsBusy(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	t.Cleanup(m.Close)
	s := busyParkedSession(t, m, "rm-busy")
	dir := m.dir("rm-busy")

	err := m.Remove("rm-busy")
	if err == nil {
		t.Fatal("a busy session was deleted out from under its own run")
	}
	if !strings.Contains(err.Error(), PhaseRunning) {
		t.Errorf("error %q does not name the phase, which is the caller's next question", err)
	}
	if !strings.Contains(err.Error(), "stop") {
		t.Errorf("error %q does not say what to do about it", err)
	}
	if _, still := m.Peek("rm-busy"); !still {
		t.Fatal("a refused delete cost the caller the session, and deleting it is the next request")
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("the directory is gone (%v), so the refusal deleted the conversation it refused to delete", statErr)
	}
	if n := len(s.path()); n != 1 {
		t.Errorf("the path is %d entries, want the one entry the fixture wrote", n)
	}
}

// TestRemoveTakesAnIdleSessionAndItsDirectory is the half a blanket guard could fake: an idle session
// goes, the directory goes with it, and the row goes with that.
func TestRemoveTakesAnIdleSessionAndItsDirectory(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	t.Cleanup(m.Close)
	if _, err := m.Create(context.Background(), Config{
		ID:  "rm-idle",
		LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	dir := m.dir("rm-idle")

	if err := m.Remove("rm-idle"); err != nil {
		t.Fatalf("remove an idle session: %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("the directory is still there (%v), so Remove deleted the row and left the conversation", statErr)
	}
	for _, sum := range m.List() {
		if sum.ID == "rm-idle" {
			t.Fatal("the session is still listed after its directory was removed")
		}
	}
	// Asking twice is not an error: a client that retries a delete it already made should not have to know
	// that. Unload answers the same question with an error on purpose (it cannot tell a typo from a
	// session it never had, and silence about a typo is how a script unloads nothing for an hour), and the
	// two routes sit side by side — so the asymmetry is pinned here rather than left to be rediscovered
	// when someone "harmonises" one of them.
	if err := m.Remove("rm-idle"); err != nil {
		t.Errorf("removing a session that is already gone: %v, want the leniency a delete is expected to have", err)
	}
	if _, err := m.Unload("rm-idle"); err == nil {
		t.Error("Unload answered a deleted id with ok, and it is the route that distinguishes a typo from an absent one")
	}
}
