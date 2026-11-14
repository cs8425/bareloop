package asm

// Wire byte contract for ContentPart and Message. The referee here is a literal, not an
// implementation:
// llama.cpp reads part.at("text") for every part and answers a request that dropped the key
// with 500 "key 'text' not found", and the KV cache is keyed by the exact prefix bytes, so a
// part that re-marshals differently between turns re-prefills the whole conversation.
//
// Two columns because two regimes exist, and both are measured on this build, not inherited
// from memory:
//
//   - native: what encoding/json/v2 writes through MarshalJSONTo — the wire bytes encodeBody
//     sends — escaping nothing beyond JSON's own rules. This column is the hot-cache contract.
//   - compat: what the v1 API (json.Marshal) additionally demands here: the HTML trio (< > &)
//     AND the JS pair (U+2028/29), the latter being this build's compat layer going beyond what
//     plain v1 ever did. That finding arrived twice as a red test, which is why every side of
//     this fence is a literal.
//
// MarshalJSONTo is the highway, MarshalJSON the GOEXPERIMENT=nojsonv2 fallback; both are pinned
// so neither route can rot unnoticed, and every test below calls its subject directly — no
// reflection, no skipping.

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"strings"
	"testing"
)

var goldenParts = []struct {
	name   string
	part   ContentPart
	native string // v2-native: JSON's own escaping only — the wire
	compat string // v1-compat layer of json.Marshal on this build
}{
	{"text", ContentPart{Type: PartTypeText, Text: "hello, world"},
		`{"type":"text","text":"hello, world"}`,
		`{"type":"text","text":"hello, world"}`},
	// The key llama.cpp 500s without: an empty text part still spells "text".
	{"text-empty", ContentPart{Type: PartTypeText},
		`{"type":"text","text":""}`,
		`{"type":"text","text":""}`},
	// An unset type fills in as text.
	{"type-default", ContentPart{Text: "x"},
		`{"type":"text","text":"x"}`,
		`{"type":"text","text":"x"}`},
	{"image", ContentPart{Type: PartTypeImage, ImageURL: &ImageURL{URL: "data:image/png;base64,QUJD"}},
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}`,
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}`},
	// The image shape never carries a text key, and a set Text is not its output either.
	{"image-ignores-text", ContentPart{Type: PartTypeImage, Text: "ignored", ImageURL: &ImageURL{URL: "u"}},
		`{"type":"image_url","image_url":{"url":"u"}}`,
		`{"type":"image_url","image_url":{"url":"u"}}`},
	// The two regimes part ways here, and only here: the wire ships the logical characters,
	// the v1-compat layer ships six bytes per one.
	{"html-trio", ContentPart{Type: PartTypeText, Text: "<a> & </a>"},
		`{"type":"text","text":"<a> & </a>"}`,
		`{"type":"text","text":"\u003ca\u003e \u0026 \u003c/a\u003e"}`},
	{"u2028-passthrough", ContentPart{Type: PartTypeText, Text: "line sep"},
		`{"type":"text","text":"line` + "\u2028" + `sep"}`,
		`{"type":"text","text":"line\u2028sep"}`},
	// JSON's own escapers are mandatory in both regimes — these bytes are the agreement that
	// "no escaping" never meant "no quoting".
	{"escapers", ContentPart{Type: PartTypeText, Text: "quote\" back\\ slash\n nl\t tab"},
		`{"type":"text","text":"quote\" back\\ slash\n nl\t tab"}`,
		`{"type":"text","text":"quote\" back\\ slash\n nl\t tab"}`},
	{"unicode-passthrough", ContentPart{Type: PartTypeText, Text: "你看到了啥?"},
		`{"type":"text","text":"你看到了啥?"}`,
		`{"type":"text","text":"你看到了啥?"}`},
}

// TestMarshalJSONToNativeBytes judges the highway: MarshalJSONTo on a naked jsontext encoder —
// the policy jsonv2.Marshal (the wire) runs — must produce the native column byte for byte.
func TestMarshalJSONToNativeBytes(t *testing.T) {
	for _, c := range goldenParts {
		var buf bytes.Buffer
		if err := c.part.MarshalJSONTo(jsontext.NewEncoder(&buf)); err != nil {
			t.Fatalf("%s: MarshalJSONTo: %v", c.name, err)
		}
		// jsontext terminates each top-level value with a newline (its NDJSON habit) — here
		// the test harness makes the part top-level, while on the wire a part is always a
		// member of a bigger document and jsonv2.Marshal owns the document's termination.
		// Trim the harness's terminator and compare the value bytes, which are the contract.
		got := strings.TrimSuffix(buf.String(), "\n")
		if got != c.native {
			t.Errorf("%s: native bytes:\n got %s\nwant %s", c.name, got, c.native)
		}
	}
}

// TestContentPartWireGolden pins the v1 API path (json.Marshal → engine → MarshalJSONTo under
// the compat escaping policy) to the compat column. The wire no longer runs this path; the
// compat column keeps the API honest for every caller still holding json.Marshal.
func TestContentPartWireGolden(t *testing.T) {
	for _, c := range goldenParts {
		raw, err := json.Marshal(c.part)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if string(raw) != c.compat {
			t.Errorf("%s:\n got %s\nwant %s", c.name, raw, c.compat)
		}
	}
}

// TestMarshalJSONFallbackBytes calls the v1 method directly — the code a GOEXPERIMENT=nojsonv2
// build runs — and pins it to the same compat column, so the escape hatch cannot drift from
// what the compat layer produces today.
func TestMarshalJSONFallbackBytes(t *testing.T) {
	for _, c := range goldenParts {
		raw, err := c.part.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: MarshalJSON: %v", c.name, err)
		}
		if string(raw) != c.compat {
			t.Errorf("%s: fallback bytes:\n got %s\nwant %s", c.name, raw, c.compat)
		}
	}
}

// TestPayloadContentBytesGolden pins the whole content-array shape the way it rides a real
// message: [image, text] for a picture with a caption. This is the KV prefix — if these
// bytes drift, the provider's cache starts over for every turn that ever carried a picture.
// The content carries no escapable characters, so the bytes are regime-independent on
// purpose: this is the one prefix test that reads identically under both engines.
func TestPayloadContentBytesGolden(t *testing.T) {
	msgs := []Message{{Role: "user", Content: []ContentPart{
		{Type: PartTypeImage, ImageURL: &ImageURL{URL: "data:image/jpeg;base64,QUJDRA=="}},
		{Type: PartTypeText, Text: "what is in this picture?"},
	}}}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJDRA=="}},{"type":"text","text":"what is in this picture?"}]}]`
	if string(raw) != want {
		t.Fatalf("\n got %s\nwant %s", raw, want)
	}
}

// goldenMessages pins the Message twins the same way goldenParts pins ContentPart — and for
// the same reason, arrived at one level up: the 2026-11-13 dogfood 400 ("All non-assistant
// messages must contain 'content'") was the engine's omitempty deleting the key from a tool
// result whose content was "", measured by probe before Message marshalled by hand. The
// empty-content row below is why this table exists; before the hand-written marshal it was
// the wire's idea of a joke: a tool message that ships without content.
var goldenMessages = []struct {
	name   string
	msg    Message
	native string
	compat string
}{
	{"user-text", Message{Role: "user", Content: "hi"},
		`{"role":"user","content":"hi"}`,
		`{"role":"user","content":"hi"}`},
	// The dogfood lesson: "" is a value, not an absence. This row is the whole reason.
	{"tool-empty-content", Message{Role: "tool", Content: "", ToolCallID: "c1"},
		`{"role":"tool","content":"","tool_call_id":"c1"}`,
		`{"role":"tool","content":"","tool_call_id":"c1"}`},
	// nil is the one way to omit the key — the assistant that only carries calls.
	{"assistant-tool-calls-only", Message{Role: "assistant", ToolCalls: []ToolCallOut{
		{ID: "c1", Type: "function", Function: FunctionCall{Name: "ls", Arguments: "{}"}}},
	},
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}}]}`,
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{}"}}]}`},
	// Field order is the struct declaration order, spelled out by the fullest shape.
	{"tool-result-named", Message{Role: "tool", Content: "out", ToolCallID: "c1", Name: "mcp-x"},
		`{"role":"tool","content":"out","tool_call_id":"c1","name":"mcp-x"}`,
		`{"role":"tool","content":"out","tool_call_id":"c1","name":"mcp-x"}`},
	{"assistant-reasoning", Message{Role: "assistant", Content: "t", ReasoningContent: "think"},
		`{"role":"assistant","content":"t","reasoning_content":"think"}`,
		`{"role":"assistant","content":"t","reasoning_content":"think"}`},
	{"user-parts", Message{Role: "user", Content: []ContentPart{{Type: PartTypeText, Text: "x"}}},
		`{"role":"user","content":[{"type":"text","text":"x"}]}`,
		`{"role":"user","content":[{"type":"text","text":"x"}]}`},
	// The two regimes part ways exactly where ContentPart's table says they do.
	{"html-trio", Message{Role: "user", Content: "<a> & </a>"},
		`{"role":"user","content":"<a> & </a>"}`,
		`{"role":"user","content":"\u003ca\u003e \u0026 \u003c/a\u003e"}`},
}

func TestMessageMarshalJSONToNativeBytes(t *testing.T) {
	for _, c := range goldenMessages {
		var buf bytes.Buffer
		if err := c.msg.MarshalJSONTo(jsontext.NewEncoder(&buf)); err != nil {
			t.Fatalf("%s: MarshalJSONTo: %v", c.name, err)
		}
		got := strings.TrimSuffix(buf.String(), "\n")
		if got != c.native {
			t.Errorf("%s: native bytes:\n got %s\nwant %s", c.name, got, c.native)
		}
	}
}

func TestMessageWireGolden(t *testing.T) {
	for _, c := range goldenMessages {
		raw, err := json.Marshal(c.msg)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if string(raw) != c.compat {
			t.Errorf("%s:\n got %s\nwant %s", c.name, raw, c.compat)
		}
	}
}

func TestMessageMarshalJSONFallbackBytes(t *testing.T) {
	for _, c := range goldenMessages {
		raw, err := c.msg.MarshalJSON()
		if err != nil {
			t.Fatalf("%s: MarshalJSON: %v", c.name, err)
		}
		if string(raw) != c.compat {
			t.Errorf("%s: fallback bytes:\n got %s\nwant %s", c.name, raw, c.compat)
		}
	}
}

// TestMessageShapeRefusals pins the refusals next to the acceptances, because a provider
// conversation is only half the contract: on the llama.cpp server that produced the 2026-11-13
// 400, a tool message without the content key answers 400 "All non-assistant messages must
// contain 'content'" while the same message with content "" answers 200 — the key, not the
// value, is what dies. The D card (assistant pairing tool_calls with an empty content)
// happened to pass that same server, and the shape rule refuses it anyway: the assembler
// cannot produce it, so a caller holding one bypassed the choke point, and "this one server
// tolerated it once" is not a promise worth a poisoned session against the next.
func TestMessageShapeRefusals(t *testing.T) {
	call := []ToolCallOut{{ID: "c1", Type: "function", Function: FunctionCall{Name: "ls", Arguments: "{}"}}}
	for _, c := range []struct {
		name string
		msg  Message
		want string // substring of the refusal
	}{
		{"tool-no-content", Message{Role: "tool", ToolCallID: "c1"}, "must carry content"},
		{"tool-no-call-id", Message{Role: "tool", Content: "out"}, "tool_call_id"},
		{"assistant-calls-empty-content", Message{Role: "assistant", Content: "", ToolCalls: call}, "empty content"},
		{"user-no-content", Message{Role: "user"}, "must carry content"},
		{"system-no-content", Message{Role: "system"}, "must carry content"},
	} {
		var buf bytes.Buffer
		err := c.msg.MarshalJSONTo(jsontext.NewEncoder(&buf))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: MarshalJSONTo refused with %v, want %q", c.name, err, c.want)
		}
		_, err = c.msg.MarshalJSON()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: MarshalJSON refused with %v, want %q", c.name, err, c.want)
		}
	}
	// And what the rules let through stays through on both twins: the empty-but-present
	// shapes measured 200 against the fault's own server.
	for _, c := range []Message{
		{Role: "tool", Content: "", ToolCallID: "c1"}, // the measured-200 C card
		{Role: "user", Content: ""},                   // empty is still a message
		{Role: "assistant", Content: ""},              // empty alone (no calls) — the rule only refuses empty+calls
		{Role: "developer", Content: nil},             // unrecognised roles shape-check as nothing
		{Role: "assistant", ToolCalls: []ToolCallOut{{ID: "c1", Type: "function", Function: FunctionCall{Name: "ls", Arguments: "{}"}}}},
	} {
		if err := c.checkShape(); err != nil {
			t.Errorf("%s: refused a shape that should travel: %v", c.Role, err)
		}
	}
}
