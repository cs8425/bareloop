package asm

// The assembler is the only place a payload is built, so everything it decides has to be a
// function of (Payload, Options): what a provider accepts (an interrupted reasoning-only assistant
// message does not, and sending it back bricks the session), how an image looks (one encoding,
// everywhere), and how the summary is spliced in (exactly once, with role user).

import (
	"encoding/json"
	"strings"
	"testing"

	"bareloop/internal/ir"
)

func u(content string) *ir.Entry {
	return &ir.Entry{ID: "u", Kind: ir.KindUser, Role: "user", Content: content}
}

func a(content string) *ir.Entry {
	return &ir.Entry{ID: "a", Kind: ir.KindAssistant, Role: "assistant", Content: content}
}

func boolPtr(b bool) *bool { return &b }

func roleOf(m Message) string { return m.Role }

// TestInterruptedAssistantSurvivesOnlyOnTheWire: an interrupted turn can leave an assistant entry
// with reasoning and nothing else. Reasoning is not message content, so sending it is a guaranteed
// 400 from llama.cpp ("Assistant message must contain either 'content' or 'tool_calls'") that then
// poisons every later turn. The entry stays in the log; it just must not reach the provider.
func TestInterruptedAssistantSurvivesOnlyOnTheWire(t *testing.T) {
	dead := &ir.Entry{ID: "dead", Kind: ir.KindAssistant, Role: "assistant", Reasoning: "hmm, let me think", Interrupted: true}
	b := Assemble(Payload{Kept: []*ir.Entry{u("one"), dead, u("two"), a("ok")}}, Options{})
	if len(b.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (the reasoning-only assistant must be dropped): %+v", len(b.Messages), b.Messages)
	}
	if len(b.Messages) != len(b.Entries) {
		t.Fatalf("Messages and Entries must stay parallel: %d vs %d", len(b.Messages), len(b.Entries))
	}
	for _, e := range b.Entries {
		if e.ID == "dead" {
			t.Fatal("the dropped entry must not be in Entries either")
		}
	}
	for _, m := range b.Messages {
		if m.Role == "assistant" && m.Content == nil && len(m.ToolCalls) == 0 {
			t.Fatalf("an empty assistant message reached the wire: %+v", m)
		}
	}
	// An assistant message that only has reasoning but does have an image is sendable.
	img := &ir.Entry{ID: "ai", Kind: ir.KindAssistant, Role: "assistant", Reasoning: "look",
		Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: "AAA"}}}
	if got := len(Assemble(Payload{Kept: []*ir.Entry{img}}, Options{}).Messages); got != 1 {
		t.Fatalf("an assistant message carrying an image must be sent, got %d messages", got)
	}
}

// TestReasoningIsOneSwitchForEveryone: the flag is per-payload, so a prefix cannot drift between
// the main send and the summary call (spec §5.4 #3).
func TestReasoningIsOneSwitchForEveryone(t *testing.T) {
	e := a("answer")
	e.Reasoning = "because"
	on := Assemble(Payload{Kept: []*ir.Entry{e}}, Options{}).Messages[0]
	if on.ReasoningContent != "because" {
		t.Fatalf("reasoning must be sent by default, got %q", on.ReasoningContent)
	}
	off := Assemble(Payload{Kept: []*ir.Entry{e}}, Options{SendReasoning: boolPtr(false)}).Messages[0]
	if off.ReasoningContent != "" {
		t.Fatalf("SendReasoning=false must drop the channel, got %q", off.ReasoningContent)
	}
	if off.Content != "answer" {
		t.Fatal("dropping reasoning must not touch the content")
	}
}

// TestImageIsAlwaysADataURL checks both the shape strict servers want and the text-only fallback,
// which must never leak base64.
func TestImageIsAlwaysADataURL(t *testing.T) {
	img := ir.Part{Type: ir.PartImage, MimeType: "image/jpeg", Data: "QUJDRA==", Name: "photo.jpg"}
	e := &ir.Entry{ID: "i", Kind: ir.KindUser, Role: "user", Content: "what is in this picture?", Parts: []ir.Part{img}}

	b := Assemble(Payload{Kept: []*ir.Entry{e}}, Options{})
	parts, ok := b.Messages[0].Content.([]ContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want a 2-part array (image, text)", b.Messages[0].Content)
	}
	if parts[0].Type != PartTypeImage || parts[0].ImageURL == nil ||
		!strings.HasPrefix(parts[0].ImageURL.URL, "data:image/jpeg;base64,") {
		t.Fatalf("image part = %#v", parts[0])
	}
	if parts[0].Text != "" {
		t.Fatal("an image part must not carry a text field")
	}
	if parts[1].Type != PartTypeText || parts[1].Text != "what is in this picture?" {
		t.Fatalf("text part = %#v", parts[1])
	}
	// The text part must always be marshalled with its key: llama.cpp reads part.at("text") for
	// every part and answers 500 "key 'text' not found" when it is missing.
	raw, err := json.Marshal(ContentPart{Type: PartTypeText})
	if err != nil || string(raw) != `{"type":"text","text":""}` {
		t.Fatalf("empty text part marshalled as %s, err %v", raw, err)
	}

	textOnly := Assemble(Payload{Kept: []*ir.Entry{e}}, Options{SendImages: boolPtr(false)})
	note, isText := textOnly.Messages[0].Content.(string)
	if !isText {
		t.Fatalf("with sendImages off content must be a string, got %#v", textOnly.Messages[0].Content)
	}
	if !strings.Contains(note, "image/jpeg") || !strings.Contains(note, "NOT sent") {
		t.Fatalf("the note must name the image and say the pixels were not sent: %q", note)
	}
	if !strings.Contains(note, "what is in this picture?") {
		t.Fatal("the real text of the message must survive next to the note")
	}
	all, _ := json.Marshal(textOnly.All())
	if strings.Contains(string(all), "QUJDRA==") || strings.Contains(string(all), "image_url") {
		t.Fatalf("sendImages off must cost neither the request nor the context: %s", all)
	}
}

// TestDataURLIsTheSameEverywhere: exotic mime types pass through, and a part with no declared type
// still produces a valid URL.
func TestDataURLIsTheSameEverywhere(t *testing.T) {
	if got := DataURL(ir.Part{MimeType: "image/webp", Data: "AAA"}); got != "data:image/webp;base64,AAA" {
		t.Fatalf("webp = %q", got)
	}
	if got := DataURL(ir.Part{Data: "AAA"}); got != "data:application/octet-stream;base64,AAA" {
		t.Fatalf("no mime = %q", got)
	}
}

func call(id string) ir.ToolCall {
	return ir.ToolCall{ID: id, Name: "fs_list", Args: json.RawMessage(`{}`)}
}

// TestDanglingToolCallGetsASyntheticResult is required by every chat template: a tool call inside
// the payload must be answered.
func TestDanglingToolCallGetsASyntheticResult(t *testing.T) {
	as := a("let me look")
	as.ToolCalls = []ir.ToolCall{call("c1")}
	b := Assemble(Payload{Kept: []*ir.Entry{u("hi"), as, u("and then?")}}, Options{})
	if b.Synthetic != 1 {
		t.Fatalf("Synthetic = %d, want 1", b.Synthetic)
	}
	if len(b.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (assistant, synthesised result, user): %+v", len(b.Messages), b.Messages)
	}
	tool := b.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "c1" {
		t.Fatalf("synthetic result = %+v", tool)
	}
	txt, _ := tool.Content.(string)
	if !strings.Contains(txt, "interrupted") {
		t.Fatalf("the synthetic result should say what happened: %q", txt)
	}
	if b.Messages[1].Role != "assistant" || b.Messages[3].Role != "user" {
		t.Fatal("the synthesised result must sit right after the assistant message that asked for it")
	}
}

// TestTrailingUnansweredToolCallIsLegal: an interrupted turn may end on a tool call. Inventing a
// result for it would rewrite history the model never saw.
func TestTrailingUnansweredToolCallIsLegal(t *testing.T) {
	as := a("let me look")
	as.ToolCalls = []ir.ToolCall{call("c1")}
	b := Assemble(Payload{Kept: []*ir.Entry{u("hi"), as}}, Options{})
	if b.Synthetic != 0 || len(b.Messages) != 2 {
		t.Fatalf("Synthetic = %d, messages = %d, want 0 and 2", b.Synthetic, len(b.Messages))
	}
	// What the model said next to the call is part of the history: both the text and the call go out.
	if txt, _ := b.Messages[1].Content.(string); txt != "let me look" {
		t.Fatalf("content = %#v, want the text to survive next to the tool call", b.Messages[1].Content)
	}
	if len(b.Messages[1].ToolCalls) != 1 {
		t.Fatalf("tool_calls were dropped: %+v", b.Messages[1])
	}

	// A tool-call-only message must omit the key instead of sending "": an empty string is what a
	// strict parser reads as "no content at all", and it costs tokens for no information.
	quiet := a("")
	quiet.ToolCalls = []ir.ToolCall{call("c2")}
	q := Assemble(Payload{Kept: []*ir.Entry{quiet}}, Options{}).Messages[0]
	if q.Content != nil {
		t.Fatalf("content = %#v, want nil for a tool-call-only message", q.Content)
	}
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"content"`) {
		t.Fatalf("the content key must be omitted entirely: %s", raw)
	}
	if !strings.Contains(string(raw), `"tool_calls"`) {
		t.Fatalf("the call itself disappeared: %s", raw)
	}
}

// TestOrphanToolResultIsDropped: the call is out of the payload (folded into a summary), so the
// result would be a message with no owner.
func TestOrphanToolResultIsDropped(t *testing.T) {
	res := &ir.Entry{ID: "t", Kind: ir.KindTool, Role: "tool", ToolCallID: "gone", Content: "output"}
	b := Assemble(Payload{Kept: []*ir.Entry{u("hi"), res}}, Options{})
	if len(b.Messages) != 1 || b.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want only the user message", b.Messages)
	}
}

// TestSummaryIsSplicedOnceWithUserRole is spec §3.6/§3.7, in both directions: exactly one copy of
// the summary text, role user, and the caller's entry is not mutated.
func TestSummaryIsSplicedOnceWithUserRole(t *testing.T) {
	s := &ir.Entry{ID: "s", Kind: ir.KindCompaction, Role: "user", Content: "[Conversation summary — earlier messages were compacted]\n- user: old"}
	kept := []*ir.Entry{u("new"), a("fine")}
	b := Assemble(Payload{System: "sys", Summary: s, Kept: kept}, Options{})
	if len(b.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(b.Messages))
	}
	if b.Messages[0].Role != "user" || b.Messages[0].Content != s.Content {
		t.Fatalf("summary message = %+v", b.Messages[0])
	}
	if strings.Count(strings.Join([]string{b.Messages[0].Content.(string)}, "|"), "earlier messages were compacted") != 1 {
		t.Fatal("the summary text must appear exactly once")
	}
	if roleOf(b.Messages[1]) != "user" || roleOf(b.Messages[2]) != "assistant" {
		t.Fatalf("kept suffix was reordered: %+v", b.Messages)
	}
	if b.Summary == nil || b.Summary.ID != "s" || b.Summary.Role != "user" {
		t.Fatalf("Built.Summary = %+v", b.Summary)
	}
	if s.Role != "user" {
		t.Fatal("Assemble must copy, never mutate the caller's entry")
	}
	// The system head comes first on the wire and nowhere else.
	all := b.All()
	if len(all) != 4 || all[0].Role != "system" || all[0].Content != "sys" {
		t.Fatalf("All() = %+v", all)
	}
	if len(Assemble(Payload{Kept: kept}, Options{}).All()) != 2 {
		t.Fatal("an empty system head must not add a message")
	}
}
