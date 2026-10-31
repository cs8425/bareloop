package asm

import (
	"encoding/json"
	"strings"
	"testing"

	"bareloop/internal/ir"
)

const structuredRaw = `{"n":1}`

// structuredEntry is a tool result as the loop stores it: the server's prose in Content, its JSON
// in the entry's own field, byte for byte as it arrived.
func structuredEntry(content string) *ir.Entry {
	return &ir.Entry{ID: "0000002", Kind: ir.KindTool, Role: "tool", ToolCallID: "c1",
		Server: "mcpstub", Content: content, Structured: json.RawMessage(structuredRaw)}
}

// structuredPayload is the smallest honest payload: repair() drops a tool result whose call is not
// in it, so the assistant turn that asked for "c1" has to travel with the answer.
func structuredPayload(e *ir.Entry) Payload {
	return Payload{Kept: []*ir.Entry{
		{ID: "0000001", Kind: "assistant", Role: "assistant",
			ToolCalls: []ir.ToolCall{{ID: "c1", Name: "structured", Server: e.Server}}},
		e,
	}}
}

// toolMessage picks the tool result out of an assembled payload, whatever repair() did around it.
func toolMessage(t *testing.T, b *Built) Message {
	t.Helper()
	for _, m := range b.Messages {
		if m.Role == "tool" {
			return m
		}
	}
	t.Fatalf("no tool message among %d messages: repair() dropped the result", len(b.Messages))
	return Message{}
}

// contentText flattens whatever content() put on the wire: a string, or a parts array once the
// entry carries an image.
func contentText(t *testing.T, v any) string {
	t.Helper()
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []ContentPart:
		var sb strings.Builder
		for _, p := range c {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	t.Fatalf("unexpected content type %T", v)
	return ""
}

// TestFoldStructuredModes is the four words, spelled out on the three shapes an MCP result arrives
// in: prose plus JSON, prose only, JSON only. The last one is where fallback has to write the
// marker itself, and drop has to write nothing at all.
func TestFoldStructuredModes(t *testing.T) {
	const marker = StructuredMarker
	const raw = structuredRaw
	for _, tc := range []struct {
		name   string
		in     string
		noJSON bool
		mode   StructuredMode
		want   string
	}{
		{"auto/folds_under_the_text", "a line", false, StructuredAuto, "a line\n" + marker + "\n" + raw},
		{"auto/never_twice", "a line\n" + raw, false, StructuredAuto, "a line\n" + raw},
		{"auto/blank_text_still_counts_as_text", "  \n", false, StructuredAuto, "  \n\n" + marker + "\n" + raw},
		{"fallback/text_wins", "a line", false, StructuredFallback, "a line"},
		{"fallback/no_text_uses_the_json", "", false, StructuredFallback, marker + "\n" + raw},
		{"always/pastes_it_again", "a line\n" + raw, false, StructuredAlways, "a line\n" + raw + "\n" + marker + "\n" + raw},
		{"always/no_text", "", false, StructuredAlways, marker + "\n" + raw},
		{"drop/text_only", "a line", false, StructuredDrop, "a line"},
		{"drop/nothing_to_send", "", false, StructuredDrop, ""},
		{"unset_mode_is_auto", "a line", false, StructuredMode(""), "a line\n" + marker + "\n" + raw},
		{"unknown_mode_adds_nothing", "a line", false, StructuredMode("nonsense"), "a line"},
		{"no_json_is_left_alone", "a line", true, StructuredAlways, "a line"},
		{"no_json_and_no_text_stays_empty", "", true, StructuredAlways, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := structuredEntry(tc.in)
			if tc.noJSON {
				e.Structured = nil
			}
			out := foldStructured(e, tc.mode)
			if out.Content != tc.want {
				t.Errorf("content = %q, want %q", out.Content, tc.want)
			}
			// The entry is the log's and the UI's, not the assembler's: a fold that wrote
			// through it would change what a session already stored.
			if e.Content != tc.in {
				t.Errorf("the entry was rewritten: %q", e.Content)
			}
		})
	}
}

// TestStructuredTargetsFor is the per-server map: one server's prose is worth its tokens and
// another's is not, and the zero value has to mean the documented default.
func TestStructuredTargetsFor(t *testing.T) {
	ts := StructuredTargets{
		Default:  StructuredDrop,
		ByServer: map[string]StructuredMode{"mcpstub": StructuredAlways},
	}
	if got := ts.For("mcpstub"); got != StructuredAlways {
		t.Errorf("named server = %q, want always", got)
	}
	if got := ts.For("mcp-vroot"); got != StructuredDrop {
		t.Errorf("unnamed server = %q, want the session default drop", got)
	}
	if got := (&StructuredTargets{}).For("anything"); got != StructuredAuto {
		t.Errorf("zero targets = %q, want auto", got)
	}
	// nil is every existing caller: Options{} carries no targets at all.
	var none *StructuredTargets
	if got := none.For("anything"); got != StructuredAuto {
		t.Errorf("nil targets = %q, want auto", got)
	}
}

// TestParseStructuredMode is the accept/reject table, including the empty string ("not set") and
// the two words someone reaches for first.
func TestParseStructuredMode(t *testing.T) {
	for _, tc := range []struct {
		s     string
		want  StructuredMode
		isErr bool
	}{
		{"", StructuredAuto, false},
		{"auto", StructuredAuto, false},
		{"AUTO", StructuredAuto, false},
		{" fallback ", StructuredFallback, false},
		{"always", StructuredAlways, false},
		{"drop", StructuredDrop, false},
		{"off", "", true},
		{"true", "", true},
		{"none", "", true},
		{"fallbacks", "", true},
	} {
		got, err := ParseStructuredMode(tc.s)
		if tc.isErr {
			if err == nil {
				t.Errorf("ParseStructuredMode(%q) = %q, want an error naming the four words", tc.s, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseStructuredMode(%q): %v", tc.s, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseStructuredMode(%q) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// TestAssembleFoldsPerMode is where the mode has to show up: the tool message on the wire.
// Assemble hands back the caller's own *ir.Entry values, so the same assertions also pin the
// reason the fold returns a copy — four modes over one entry, and the stored text never moves.
func TestAssembleFoldsPerMode(t *testing.T) {
	const marker = StructuredMarker
	for _, tc := range []struct {
		mode StructuredMode
		want string
	}{
		{StructuredAuto, "a line\n" + marker + "\n" + structuredRaw},
		{StructuredFallback, "a line"},
		{StructuredAlways, "a line\n" + marker + "\n" + structuredRaw},
		{StructuredDrop, "a line"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			e := structuredEntry("a line")
			b := Assemble(structuredPayload(e), Options{Structured: &StructuredTargets{Default: tc.mode}})
			if got := contentText(t, toolMessage(t, b).Content); got != tc.want {
				t.Errorf("wire content = %q, want %q", got, tc.want)
			}
			if b.Entries[len(b.Entries)-1].Content != "a line" {
				t.Errorf("assembly rewrote the entry it was given: %q", b.Entries[len(b.Entries)-1].Content)
			}
			if string(b.Entries[len(b.Entries)-1].Structured) != structuredRaw {
				t.Error("assembly took the JSON away from the entry")
			}
		})
	}

	// No targets at all is what every caller that knows nothing about the option passes.
	e := structuredEntry("a line")
	b := Assemble(structuredPayload(e), Options{})
	if got := contentText(t, toolMessage(t, b).Content); !strings.Contains(got, marker) {
		t.Errorf("Options{} sent %q, want auto behaviour (the JSON folded in)", got)
	}

	// A per-server word beats the session default, and the server name on the entry is the key.
	targets := &StructuredTargets{
		Default:  StructuredAlways,
		ByServer: map[string]StructuredMode{"mcpstub": StructuredDrop},
	}
	e = structuredEntry("a line")
	b = Assemble(structuredPayload(e), Options{Structured: targets})
	if got := contentText(t, toolMessage(t, b).Content); got != "a line" {
		t.Errorf("per-server drop = %q, want the text alone", got)
	}

	other := structuredEntry("a line")
	other.Server = "mcp-vroot"
	b = Assemble(structuredPayload(other), Options{Structured: targets})
	if got := contentText(t, toolMessage(t, b).Content); !strings.Contains(got, marker) {
		t.Errorf("another server = %q, want the session default always", got)
	}
}

// TestEstimateCountsStructured: the estimator is a ceiling, so the raw JSON counts whether or not
// this session's mode sends it. Under-counting a large structuredContent is how a request walks
// into the pre-send guard instead of the compaction trigger that would have made room for it.
func TestEstimateCountsStructured(t *testing.T) {
	plain := &ir.Entry{Role: "tool", Content: "ok"}
	big := structuredEntry("ok")
	big.Structured = json.RawMessage(strings.Repeat("x", 4000))
	if got, base := Estimate(big), Estimate(plain); got <= base {
		t.Errorf("Estimate with the JSON = %d, want more than the %d without it", got, base)
	}
	if got, base := entryBytes(big), entryBytes(plain); got <= base {
		t.Errorf("entryBytes with the JSON = %d, want more than the %d without it", got, base)
	}
}
