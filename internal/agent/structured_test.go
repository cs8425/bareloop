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

// TestStructuredMaxIsValidatedWhereItIsWritten is the mode's rule again, for the number: a stored
// config must not be able to lie, and a negative ceiling does exactly that — it is what gets typed for
// "no limit" and what the assembler reads back as the default. So that is refused at Create and at
// PATCH, a ceiling of any size is not, and a PATCH that does not carry one is not a change to it.
func TestStructuredMaxIsValidatedWhereItIsWritten(t *testing.T) {
	const base = "http://127.0.0.1:1/v1"
	m := NewManager(t.TempDir(), false, Config{StructuredMaxBytes: 40000}, nil, nil)

	// The one thing this pins that the mode above does not: the ceiling is not inherited from the
	// daemon. The mode is, because it is a word about a server; this is a token budget, and the
	// number only has to be looked up in one place — the session's own config. The assertion is here
	// so that "make the two fields behave alike" has to be a decision rather than an oversight.
	if got := m.MergeDefaults(Config{ID: "s"}).StructuredMaxBytes; got != 0 {
		t.Fatalf("a fresh session was handed %d bytes by MergeDefaults, want 0 so the package default %d applies", got, asm.DefaultStructuredMaxBytes)
	}

	// The one refused shape, and it is refused because nothing after the write can reveal it: a negative
	// says "no limit" to whoever typed it and says the default to the assembler.
	for _, bad := range []int{-1, -8192} {
		if _, err := m.Create(context.Background(), Config{ID: "sbad", LLM: llm.Config{BaseURL: base}, StructuredMaxBytes: bad}); err == nil {
			t.Fatalf("a ceiling of %d bytes was stored; it promises no limit and delivers the default", bad)
		}
	}

	s, err := m.Create(context.Background(), Config{ID: "smax", LLM: llm.Config{BaseURL: base}, StructuredMaxBytes: 30000})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config().StructuredMaxBytes; got != 30000 {
		t.Fatalf("Create stored a ceiling of %d, want 30000", got)
	}
	// The number is only worth storing if it reaches the assembler, which is where the cut happens.
	if got := s.cfgOptions().StructuredMaxBytes; got != 30000 {
		t.Fatalf("cfgOptions passed %d to the assembler, want the session's own 30000", got)
	}

	// An unrelated patch leaves the ceiling alone, and a patch carrying one changes it.
	if err := s.UpdateConfig(context.Background(), Config{MaxTurns: 3}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().StructuredMaxBytes; got != 30000 {
		t.Fatalf("an unrelated patch reset the ceiling to %d", got)
	}
	if err := s.UpdateConfig(context.Background(), Config{StructuredMaxBytes: 8192}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().StructuredMaxBytes; got != 8192 {
		t.Fatalf("after PATCH the ceiling is %d, want 8192", got)
	}
	if err := s.UpdateConfig(context.Background(), Config{StructuredMaxBytes: -1}); err == nil {
		t.Fatal("a PATCHed negative ceiling was accepted: it stores a number the assembler reads as the default")
	}
	if got := s.Config().StructuredMaxBytes; got != 8192 {
		t.Fatalf("the refused patch changed the ceiling to %d", got)
	}
	// A ceiling small enough to starve the model is somebody else's call. It is obvious in the first
	// tool result, this endpoint is how it gets undone, and refusing it here would only lock the door.
	if err := s.UpdateConfig(context.Background(), Config{StructuredMaxBytes: 16}); err != nil {
		t.Fatalf("a ceiling of 16 bytes was refused: %v", err)
	}
	if got := s.Config().StructuredMaxBytes; got != 16 {
		t.Fatalf("the small ceiling did not land: %d", got)
	}
}
