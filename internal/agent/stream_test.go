// Two promises about one streamed reply, both learned the hard way from a run stopped between two
// argument fragments (2026-11-05, a Stop landing inside fs_list's arguments):
//
//  1. What reaches the log has to be storable. A tool call's arguments are kept as the provider
//     wrote them (json.RawMessage), so a half-written object is not a field that fails to parse —
//     it is a record encoding/json refuses outright, and the refusal drops the entire reply.
//  2. What the browser was shown has to end. The hub remembers a session's recent events so a page
//     that reloads mid-run can catch up, forgetting a stream's fragments only when the frame that
//     closes them arrives. A stream that ends without that frame is replayed to every later reload:
//     a tool card forever marked running, with nothing behind it in the log.
//
// The first belongs to generate's accumulation, the second to every exit generate has.
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// TestCompleteArgsAcceptsOnlyAWholeValue is the gate in front of the log. The two halves of the
// decision: empty is a call with no arguments — legal, and what llama.cpp sends for fs_unload —
// while anything else must be a value, because ir.ToolCall.Args is sent back out as it was stored.
func TestCompleteArgsAcceptsOnlyAWholeValue(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"path":"internal","depth":2}`, true},
		{`{}`, true},
		{" \n", true}, // no arguments at all, with the whitespace one fragment leaves behind
		{``, true},
		{`{"depth":5,"path":"internal"`, false}, // what a Stop between two fragments leaves
		{`{"path":`, false},
		{`{"a":1}{"b":2}`, false}, // trailing garbage is not one value
		{`not json`, false},
	} {
		if got := completeArgs(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("completeArgs(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestAHalfWrittenCallIsWhatLosesTheReply keeps the reason for that gate on the record. The bytes are
// the ones this daemon actually logged, from data/test-1: a reply holding one finished call and one
// cut in the middle of its arguments.
func TestAHalfWrittenCallIsWhatLosesTheReply(t *testing.T) {
	whole := ir.ToolCall{ID: "b", Name: "fs_read", Args: json.RawMessage(`{"path":"internal"}`)}
	half := ir.ToolCall{ID: "a", Name: "fs_list", Args: json.RawMessage(`{"depth":5,"path":"internal"`)}

	if _, err := json.Marshal(&ir.Entry{Kind: ir.KindAssistant, Role: "assistant", ToolCalls: []ir.ToolCall{whole}}); err != nil {
		t.Fatalf("a reply whose calls are all whole must store, got: %v", err)
	}
	// This is the error the daemon logged while the reply was being committed, and with it the whole
	// assistant message — prose, reasoning, usage, the finished call — never reached the log.
	if _, err := json.Marshal(&ir.Entry{Kind: ir.KindAssistant, Role: "assistant",
		ToolCalls: []ir.ToolCall{half, whole}}); err == nil {
		t.Error("a half-written call marshalled, so generate has no reason to drop one")
	} else if !strings.Contains(err.Error(), "json: error calling MarshalJSON") {
		t.Logf("marshal error: %v", err) // shape changed upstream; the comment above may need it
	}
}

// TestACutCallSaysSoItself is about what the cut leaves behind. The sentence is stored with the
// entry, which means it is read twice: by whoever opens the transcript, and by the model on the next
// payload — both of them otherwise told, by silence, that the call was never asked for.
func TestACutCallSaysSoItself(t *testing.T) {
	line := cutOffNote("fs_list")
	if !strings.Contains(line, "fs_list") {
		t.Errorf("the note does not name the call it cut: %s", line)
	}
	if !strings.Contains(line, "not made") {
		t.Errorf("the note does not say the call did not run: %s", line)
	}
	if empty := cutOffNote(""); !strings.Contains(empty, "a tool call") || strings.Contains(empty, "  ") {
		t.Errorf("a call whose name never arrived should still read as a sentence, got: %s", empty)
	}
}

// TestAGenerateThatReturnsNothingClosesItsStream is the second promise, on the exit that used to
// break it: the request never answered, so there is no entry to commit and no later frame to hang
// the closing off. One frame, and it names the stream that opened — the hub drops a session's
// fragments on it, and a page that reloads afterwards is left with the transcript instead.
func TestAGenerateThatReturnsNothingClosesItsStream(t *testing.T) {
	var (
		mu     sync.Mutex
		frames []Event
	)
	m := NewManager(t.TempDir(), false, Config{}, func(e Event) {
		mu.Lock()
		frames = append(frames, e)
		mu.Unlock()
	}, nil)
	s, err := m.Create(context.Background(), Config{ID: "cut-short", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A context already stopped is what a Stop leaves behind on the way into the request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entry, streamID, aborted := s.generate(ctx, &asm.Built{System: "test"}, nil, 1)
	if entry != nil {
		t.Fatalf("a generate whose request never went out returned %+v", entry)
	}
	if !aborted {
		t.Error("a stopped context returned aborted=false, so the loop will call it a finished answer")
	}

	closes := 0
	mu.Lock()
	defer mu.Unlock()
	for _, e := range frames {
		if e.Type != EvtDelta || e.Delta == nil || !e.Delta.Done {
			continue
		}
		closes++
		if e.Delta.StreamID != streamID {
			t.Errorf("frame %d closes stream %q, want %q", closes, e.Delta.StreamID, streamID)
		}
	}
	if closes != 1 {
		t.Errorf("generate returned without a reply after emitting %d closing frames, want exactly 1;"+
			" the stream it opened is still open in the hub, and every page that reloads will replay it"+
			" as a tool call that never finishes", closes)
	}
}
