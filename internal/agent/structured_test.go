package agent

// Where a tool's structuredContent ends up is decided in two places: toolOutput decides what the
// log gets (the JSON in its own field, never pasted into the text), and the session config decides
// what the model gets (per server, else session default, else auto). Both are pinned here.

import (
	"context"
	"encoding/json"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// TestToolOutputKeepsStructuredApart: the JSON used to be appended to the text when the text did not
// already contain it. That wrote the same bytes twice for a server that pretty-prints, and it made
// "did this server send me the same thing twice?" unanswerable from the log alone. The assembler
// composes that now; the entry stores the two channels apart.
func TestToolOutputKeepsStructuredApart(t *testing.T) {
	raw := json.RawMessage(`{"ok":true}`)
	text, parts, structured := toolOutput(&mcp.CallResult{
		Content:    []mcp.ContentBlock{{Type: "text", Text: "a line"}},
		Structured: raw,
	})
	if text != "a line" {
		t.Fatalf("content = %q, want exactly the text block", text)
	}
	if string(structured) != string(raw) {
		t.Fatalf("structured = %s, want the bytes as they arrived", structured)
	}
	if len(parts) != 0 {
		t.Fatalf("parts = %+v, want none", parts)
	}
	if text, _, structured := toolOutput(nil); text != "" || structured != nil {
		t.Fatal("a call that never answered has neither text nor JSON")
	}
}

// TestTextDoesNotSubstituteStructured is the mcp half of the same rule: with no text block, Text()
// is empty. Anything else would decide the mode inside the client.
func TestTextDoesNotSubstituteStructured(t *testing.T) {
	res := &mcp.CallResult{Structured: json.RawMessage(`{"ok":true}`)}
	if got := res.Text(); got != "" {
		t.Fatalf("Text() = %q, want empty so the assembler can choose", got)
	}
}

// TestStructuredTargetsPrecedence is server > session > auto. An unparseable value counts as unset:
// this runs on every payload, and a hand-edited config must not be able to stop a session answering
// (Create and UpdateConfig refuse to store one).
func TestStructuredTargetsPrecedence(t *testing.T) {
	cfg := Config{
		StructuredContent: "always",
		MCP: []mcp.Config{
			{Name: "quiet", StructuredContent: "drop"},
			{Name: "loud"},
			{Name: "bogus", StructuredContent: "alway"},
		},
	}
	tt := structuredTargets(cfg)
	for server, want := range map[string]asm.StructuredMode{
		"quiet":  asm.StructuredDrop,   // the server's own word wins
		"loud":   asm.StructuredAlways, // no override: the session speaks
		"bogus":  asm.StructuredAlways, // unreadable is unset, not an error at this point
		"absent": asm.StructuredAlways, // a server the config never mentioned
	} {
		if got := tt.For(server); got != want {
			t.Fatalf("For(%q) = %q, want %q", server, got, want)
		}
	}
	none := structuredTargets(Config{})
	if got := none.For("any"); got != asm.StructuredAuto {
		t.Fatalf("a session that says nothing must be auto, got %q", got)
	}
}

// TestStructuredModeIsValidatedWhereItIsWritten is why the words are checked at Create and at PATCH:
// "auto" is the default, so an unvalidated typo is indistinguishable from a silently ignored setting.
func TestStructuredModeIsValidatedWhereItIsWritten(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{StructuredContent: "fallback"}, nil, nil)

	if got := m.MergeDefaults(Config{ID: "s"}); got.StructuredContent != "fallback" {
		t.Fatalf("a session that says nothing must inherit the daemon's, got %q", got.StructuredContent)
	}

	if _, err := m.Create(context.Background(), Config{ID: "sbad",
		LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"},
		MCP: []mcp.Config{{Name: "x", StructuredContent: "sometimes"}}}); err == nil {
		t.Fatal("a server-level typo must be refused")
	}

	s, err := m.Create(context.Background(), Config{ID: "smode", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config().StructuredContent; got != "fallback" {
		t.Fatalf("Create must keep the inherited mode, got %q", got)
	}

	if err := s.UpdateConfig(context.Background(), Config{StructuredContent: "nope"}); err == nil {
		t.Fatal("a PATCHed typo must be refused")
	}
	if got := s.Config().StructuredContent; got != "fallback" {
		t.Fatalf("the refused patch changed the mode: %q", got)
	}
	if err := s.UpdateConfig(context.Background(), Config{StructuredContent: "drop"}); err != nil {
		t.Fatal(err)
	}
	// An unrelated patch must not reset it: that body simply does not mention the field.
	if err := s.UpdateConfig(context.Background(), Config{MaxTurns: 3}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().MaxTurns; got != 3 {
		t.Fatalf("the unrelated patch did not land either (MaxTurns = %d), so the assertion below proves nothing", got)
	}
	if got := s.Config().StructuredContent; got != "drop" {
		t.Fatalf("an unrelated patch reset the mode: %q", got)
	}
}
