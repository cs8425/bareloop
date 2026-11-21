package asm

// A generation failure leaves behind an assistant entry that is entirely our own words:
//
//	[generation failed: llm: 413 Payload Too Large: (empty body) [44 msgs, 100.3 MiB sent, 6 tools]]
//
// It has to be an entry — the turn needs something on the path to answer it, or the loop asks the same
// question forever — but it is a byte report, and handing it back as one of the model's turns credits
// the model with a sentence it never said. So: on the path, off the wire. ir.Entry.Notice is the mark,
// and the drop lives in repair() where Entries and Messages are appended together, which is what keeps
// EntryIDs, HeadLen and the size table agreeing with what actually goes out rather than drifting a
// message to the left of it.

import (
	"strings"
	"testing"

	"bareloop/internal/ir"
)

func notice(id string) *ir.Entry {
	return &ir.Entry{
		ID:      id,
		Kind:    ir.KindAssistant,
		Role:    "assistant",
		Content: "[generation failed: llm: 413 Payload Too Large: (empty body)]",
		Finish:  "error",
		Label:   "llm error",
		Notice:  true,
	}
}

func TestTheFailureNoticeIsNotOneOfTheModelsTurns(t *testing.T) {
	b := Assemble(Payload{Kept: []*ir.Entry{u("q1"), notice("f"), u("q2"), a("ok")}}, Options{})
	if len(b.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (the notice must not reach the wire): %+v", len(b.Messages), b.Messages)
	}
	if len(b.Messages) != len(b.Entries) {
		t.Fatalf("Messages and Entries must stay parallel: %d vs %d", len(b.Messages), len(b.Entries))
	}
	for _, id := range EntryIDs(b) {
		if id == "f" {
			t.Fatal("the notice is still in EntryIDs: anything that slices the payload by id now reads one message to the left")
		}
	}
	// The size table is built from the same Entries, so a dropped notice is not priced into a payload
	// that does not carry it: the byte arithmetic and the wire are one account, not two.
	if s := DeriveSizes(b, 0); len(s.order) != 3 {
		t.Fatalf("sizes priced %d entries for a %d-message payload", len(s.order), len(b.Messages))
	}
}

// A failed turn that had already asked for a tool stays on the wire. The guard is not decoration:
// dropping an entry with calls would orphan the results that follow it, and repair() pairs a result to
// a call it can no longer see.
func TestAFailedTurnThatCalledAToolIsStillSent(t *testing.T) {
	e := notice("f2")
	e.ToolCalls = []ir.ToolCall{{ID: "c1", Name: "fs_view"}}
	tool := &ir.Entry{ID: "t", Kind: ir.KindTool, Role: "tool", ToolCallID: "c1", Tool: "fs_view", Content: "the picture"}
	b := Assemble(Payload{Kept: []*ir.Entry{u("q"), e, tool}}, Options{})
	if len(b.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: an entry with tool calls must not be dropped: %+v", len(b.Messages), b.Messages)
	}
	if b.Synthetic != 0 {
		t.Fatalf("repair() synthesised %d result(s) for a call that is on the wire: %+v", b.Synthetic, b.Messages)
	}
}

// The other side of the line: a stream that died halfway leaves the words the model did say plus our
// note about how it stopped. Those words are conversation, so the entry goes out — which is why the
// mark is on the entry and not derived from "it has an error label".
func TestAHalfSpokenFailureIsStillConversation(t *testing.T) {
	half := &ir.Entry{
		ID:      "half",
		Kind:    ir.KindAssistant,
		Role:    "assistant",
		Content: "the answer begins here\n\n[stream error: connection reset]",
		Label:   "llm error",
	}
	b := Assemble(Payload{Kept: []*ir.Entry{u("q"), half}}, Options{})
	if len(b.Messages) != 2 {
		t.Fatalf("messages = %d, want 2: half a sentence the model said belongs in the conversation", len(b.Messages))
	}
	text, _ := b.Messages[1].Content.(string)
	if !strings.Contains(text, "the answer begins here") {
		t.Fatalf("the model's own words are missing from the wire: %+v", b.Messages[1])
	}
}
