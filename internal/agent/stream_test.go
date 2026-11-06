// Promises about the frames that end a thing — a streamed reply, a tool run — the first two learned the hard
// way from a run stopped between two argument fragments (2026-11-05, a Stop landing inside fs_list's
// arguments):
//
//  1. What reaches the log has to be storable. A tool call's arguments are kept as the provider
//     wrote them (json.RawMessage), so a half-written object is not a field that fails to parse —
//     it is a record encoding/json refuses outright, and the refusal drops the entire reply.
//  2. What the browser was shown has to end. The hub remembers a session's recent events so a page
//     that reloads mid-run can catch up, forgetting a stream's fragments only when the frame that
//     closes them arrives. A stream that ends without that frame is replayed to every later reload:
//     a tool card forever marked running, with nothing behind it in the log.
//  3. When the log refuses what the browser was already shown, the frame that ends the thing has to say so
//     (2026-11-06). A refusal leaves a buffer of text, or a card that has been spinning since the call, and
//     each reads as exactly what it read as a moment ago: an answer, a call in flight. Only the closing frame
//     can correct it, because only those frames carry the id the thing is keyed by — an error event can shout
//     at the page, but not at one node.
//
// The first belongs to generate's accumulation, the second to every exit generate has, the third to the two
// places a commit can fail.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestARefusedReplyClosesItsStreamSayingSo is the frame a page gets when storage refuses a reply that was
// already streamed out (the "commit assistant entry" way out of a turn). What the page was shown is
// unreadable either way — the Done is what makes the hub forget the fragments, so no reload brings them
// back — but the page that watched the text arrive keeps holding a message that looks finished, and only
// the closing delta can tell it otherwise, because a delta names the stream and an error event does not.
// So the promise pinned here is that the reason and the Done travel on one frame: a reason without a Done
// would arrive mid-stream and close a buffer still being written into, and a Done without a reason is the
// silence this replaces.
func TestARefusedReplyClosesItsStreamSayingSo(t *testing.T) {
	var (
		mu     sync.Mutex
		frames []Event
	)
	m := NewManager(t.TempDir(), false, Config{}, func(e Event) {
		mu.Lock()
		frames = append(frames, e)
		mu.Unlock()
	}, nil)
	s, err := m.Create(context.Background(), Config{ID: "refused", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	s.emit(Event{Type: EvtDelta, Delta: &Delta{StreamID: "refused-t1-1", Content: "here is the answer"}})
	s.closeStreamErr("refused-t1-1", errors.New("no space left on device"))

	mu.Lock()
	defer mu.Unlock()
	var (
		closes  int
		reasons []Delta
	)
	for _, e := range frames {
		if e.Type != EvtDelta || e.Delta == nil || e.Delta.StreamID != "refused-t1-1" {
			continue
		}
		if e.Delta.Done {
			closes++
		}
		if e.Delta.Error != "" {
			reasons = append(reasons, *e.Delta)
		}
	}
	if closes != 1 {
		t.Errorf("a refused reply closed its stream %d times, want exactly 1:%s", closes, describeFrames(frames))
	}
	if len(reasons) != 1 {
		t.Fatalf("the refused stream carried %d reason(s), want 1 — a page holding that text has to be told it was"+
			" thrown away, in the one frame that reaches it:%s", len(reasons), describeFrames(frames))
	}
	r := reasons[0]
	if !r.Done {
		t.Errorf("the reason travels on a frame that does not close the stream: a page reads a Done as the end, so"+
			" this one lands mid-stream — and the hub, which forgets a stream's fragments on the Done, keeps the"+
			" refused fragments for every later reload: %+v", r)
	}
	if !strings.Contains(r.Error, "no space left on device") {
		t.Errorf("the closing frame says %q, which is not the reason", r.Error)
	}

	// Both wire shapes: the client reads the reason at delta.error, and a client that has never heard of
	// the field must receive exactly the frame it received before — no key, not an empty one.
	said, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(said) != `{"streamId":"refused-t1-1","done":true,"error":"no space left on device"}` {
		t.Errorf("the frame a page reads is not this one: %s", said)
	}
	plain, err := json.Marshal(Delta{StreamID: "x-t1-1", Done: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "error") {
		t.Errorf("an ordinary goodbye grew a key of its own: %s", plain)
	}
}

// TestARefusedToolResultStillEndsItsCard is promise three on the other frame that ends a thing. A tool card
// is keyed by the run's id and only a tool_result names it, so when the path refuses a result the one frame
// that could say so is the one the loop used not to send: the card kept reading "running", forever, next to
// an error event about a run that had stopped. Pinned here: the card gets its terminal frame, it says error
// and why, and it invents no entry for the card to link to.
func TestARefusedToolResultStillEndsItsCard(t *testing.T) {
	var (
		mu     sync.Mutex
		frames []Event
	)
	m := NewManager(t.TempDir(), false, Config{}, func(e Event) {
		mu.Lock()
		frames = append(frames, e)
		mu.Unlock()
	}, nil)
	s, err := m.Create(context.Background(), Config{ID: "refused-tool", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	run := &RunInfo{ID: "call-1", CallID: "call-1", Server: "stub", Tool: "fs_list", Status: "running"}
	s.emit(Event{Type: EvtToolCall, Run: run})
	s.refuseToolResult(run, errors.New("no space left on device"))

	mu.Lock()
	defer mu.Unlock()
	var (
		ended  *RunInfo
		toasts int
		toast  string
	)
	for _, e := range frames {
		switch {
		case e.Type == EvtToolResult && e.Run != nil:
			if ended != nil {
				t.Errorf("one refused call ended its card twice")
			}
			ended = e.Run
		case e.Type == EvtError:
			toasts++
			toast = e.Info
		}
	}
	if ended == nil {
		t.Fatalf("a refused tool result ended no card — the one still on screen reads running forever:%s", describeFrames(frames))
	}
	if ended.Status != "error" {
		t.Errorf("the card a refusal ended says %q, not error", ended.Status)
	}
	if !strings.Contains(ended.Error, "commit tool result") || !strings.Contains(ended.Error, "no space left on device") {
		t.Errorf("the card says %q, which is not the reason it was refused", ended.Error)
	}
	if ended.EntryID != "" {
		t.Errorf("the frame points the card at %s, an entry this refusal never wrote", ended.EntryID)
	}
	if toasts != 1 || !strings.Contains(toast, "no space left on device") {
		t.Errorf("the run got %d error event(s) (%q): the card and the operator are two readers, each needs the sentence",
			toasts, toast)
	}

	body, err := json.Marshal(ended)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"status":"error"`) {
		t.Errorf("the frame a card reads does not end it: %s", body)
	}
	if strings.Contains(string(body), "entryId") {
		t.Errorf("a call with no entry grew a link anyway: %s", body)
	}
}

// TestAStreamedReplyNamesItsStream is the identity that lets a browser finish the job 2026-11-05
// started: a live buffer is keyed by its stream id and nothing else, so the frame that commits an
// assistant entry has to name it. The rule that replaces it — "the newest buffer is the one this reply
// wrote itself into" — is a guess, and the guess drew one reply twice when a late Done revived a buffer
// the entry had already taken over. A run with a tool call in it is scripted rather than one plain
// reply, because half of this assertion is about the entries that must NOT carry a stream.
func TestAStreamedReplyNamesItsStream(t *testing.T) {
	var (
		mu     sync.Mutex
		frames []Event
	)
	m := NewManager(t.TempDir(), false, Config{}, func(e Event) {
		mu.Lock()
		frames = append(frames, e)
		mu.Unlock()
	}, nil)
	cfg := Config{ID: "named"}
	cfg.BuiltinTools = []string{"wait", "get_datetime"}
	cfg.LLM = llm.Config{BaseURL: "http://127.0.0.1:1/v1"}
	s, err := m.Create(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	script := []turn{
		gatedToolCalls("call-1", "get_datetime", `{}`),
		says("the date was on its way"),
	}
	f := &fakeProvider{script: script}
	s.provider = f
	t.Cleanup(func() { _ = s.Close() })

	send(t, s, "what time is it")
	f.waitForRequest(t, 1)
	f.gate(t, 1).release()
	waitForIdle(t, s)

	mu.Lock()
	defer mu.Unlock()

	opened := map[string]int{}
	done := map[string]int{}
	for i, e := range frames {
		if e.Type != EvtDelta || e.Delta == nil {
			continue
		}
		opened[e.Delta.StreamID] = i
		if e.Delta.Done {
			done[e.Delta.StreamID] = i
		}
	}
	if len(opened) < 2 {
		t.Fatalf("the run streamed %d stream(s), want one per assistant reply:%s", len(opened), describeFrames(frames))
	}

	claimed := map[string]string{}
	for i, e := range frames {
		if e.Type != EvtEntry || e.Entry == nil {
			continue
		}
		if e.Entry.Kind != ir.KindAssistant {
			if e.StreamID != "" {
				t.Errorf("frame %d: a %s entry names stream %q. Only a streamed reply has an id to lend:"+
					" a tool result that claims a buffer would be drawn into it and steal its identity",
					i, e.Entry.Kind, e.StreamID)
			}
			continue
		}
		if e.StreamID == "" {
			t.Errorf("frame %d: the assistant entry %s names no stream, so the browser is left guessing"+
				" which buffer this reply wrote itself into:%s", i, e.Entry.ID, describeFrames(frames))
			continue
		}
		if _, ok := opened[e.StreamID]; !ok {
			t.Errorf("frame %d: entry %s names stream %q, which no delta ever came on", i, e.Entry.ID, e.StreamID)
		}
		if other, dup := claimed[e.StreamID]; dup {
			t.Errorf("stream %q is claimed by %s and by %s: upgrading in place would draw two messages in one node",
				e.StreamID, other, e.Entry.ID)
		}
		claimed[e.StreamID] = e.Entry.ID
		// The order the upgrade depends on: the entry lands while its stream is still open, and the Done
		// that retires the buffer's live state comes after it. Reverse the two and a page reloading
		// mid-run closes the buffer before the entry that would have inherited it arrives — the empty
		// shell this file's other test was written about.
		if d, ok := done[e.StreamID]; ok && d < i {
			t.Errorf("frame %d: entry %s arrives after the Done that closed stream %q (frame %d)",
				i, e.Entry.ID, e.StreamID, d)
		}
	}
	if len(claimed) < 2 {
		t.Errorf("%d assistant entries named a stream, want both of the run's replies to:%s", len(claimed), describeFrames(frames))
	}
}

// TestStreamIDIsOmittedWhenNothingStreamed pins the wire shape. A client that has never heard of the
// field must receive exactly the frame it received before — no key, not an empty one — and a client that
// has must be able to join an entry to its deltas with a string compare, which only works while the two
// spell the key identically.
func TestStreamIDIsOmittedWhenNothingStreamed(t *testing.T) {
	quiet, err := json.Marshal(Event{Type: EvtEntry, Entry: &ir.Entry{ID: "0000001", Kind: ir.KindUser}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(quiet), "streamId") {
		t.Errorf("an entry that never streamed grew the key anyway: %s", quiet)
	}

	loud, err := json.Marshal(Event{Type: EvtEntry, Entry: &ir.Entry{ID: "0000002", Kind: ir.KindAssistant}, StreamID: "named-t1-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(loud), `"streamId":"named-t1-1"`) {
		t.Errorf("a streamed reply does not name its stream: %s", loud)
	}

	d, err := json.Marshal(Delta{StreamID: "named-t1-1", Done: true})
	if err != nil || !strings.Contains(string(d), `"streamId":"named-t1-1"`) {
		t.Errorf("Delta spells its stream id differently, so the join cannot be made: %s (%v)", d, err)
	}
}

// describeFrames is one line per event, for a failure about order to be readable at all.
func describeFrames(frames []Event) string {
	var b strings.Builder
	for i, e := range frames {
		switch {
		case e.Entry != nil:
			fmt.Fprintf(&b, "\n  %d %s %s stream=%q", i, e.Type, e.Entry.Kind, e.StreamID)
		case e.Delta != nil:
			fmt.Fprintf(&b, "\n  %d delta %s done=%v err=%q", i, e.Delta.StreamID, e.Delta.Done, e.Delta.Error)
		default:
			fmt.Fprintf(&b, "\n  %d %s", i, e.Type)
		}
	}
	return b.String()
}
