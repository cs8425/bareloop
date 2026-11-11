package llm

// Body-level golden: the hot-cache contract in one byte string. The provider keys its prompt
// cache on the token stream produced from exactly these bytes, so anything that shifts them —
// a field order, an escaping regime, a map iteration — re-prefills every live session. The
// asm package pins ContentPart (including its U+2028 passthrough, written there with visible
// escapes); this pins the whole envelope: chatRequest's declared field order, the v2-native
// no-escaping wire, the Extra-merge sort (top level and Extra values only — see
// TestEncodeBodyExtraMergeRawPassThrough), and determinism across calls (every turn
// re-sends the prefix, so the same request must encode identically twice). No invisible
// characters in goldens — every byte here is one a human can proofread.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"bareloop/internal/asm"
)

func TestEncodeBodyStructOrder(t *testing.T) {
	p := &openai{cfg: Config{Model: "m1"}}
	req := Request{
		Messages: []asm.Message{
			{Role: "system", Content: "you are <helpful> & kind"},
			{Role: "user", Content: []asm.ContentPart{
				{Type: asm.PartTypeImage, ImageURL: &asm.ImageURL{URL: "data:image/png;base64,QUJD"}},
				{Type: asm.PartTypeText, Text: "see: <b>&</> tail"},
			}},
		},
		Tools: []Tool{{Name: "ls", Description: "list <dir>", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
	}
	body, err := p.encodeBody(req, false)
	if err != nil {
		t.Fatalf("encodeBody: %v", err)
	}
	// Struct-declared field order (model, messages, tools, stream), the raw trio unescaped,
	// and no "max_tokens":0 — v2's omitempty never dropped the zero (measured), omitzero does.
	want := `{"model":"m1","messages":[{"role":"system","content":"you are <helpful> & kind"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},{"type":"text","text":"see: <b>&</> tail"}]}],"tools":[{"type":"function","function":{"name":"ls","description":"list <dir>","parameters":{"type":"object","properties":{}}}}],"stream":false}`
	if string(body) != want {
		t.Fatalf("\n got %s\nwant %s", body, want)
	}
}

func TestEncodeBodyExtraMergeSorted(t *testing.T) {
	p := &openai{cfg: Config{Model: "m1"}}
	req := Request{
		Messages: []asm.Message{{Role: "user", Content: "x"}},
		Extra:    map[string]any{"top_k": 1, "nested": []any{1.5, true, nil}},
	}
	body, err := p.encodeBody(req, true)
	if err != nil {
		t.Fatalf("encodeBody: %v", err)
	}
	// Sorted map keys decide the order only where maps exist: the top level and inside
	// Extra values. Nested wire objects keep the bytes the structs wrote — a message rides
	// as {"role":…, "content":…}, struct order, identical to the no-Extra path. (Before
	// the single-pass rewrite the merge round-tripped everything through map[string]any,
	// and this golden read {"content":…, "role":…}; TestEncodeBodyExtraMergeRawPassThrough
	// pins exactly that drift and its bounds. Two facts keep this safe, and both belong to
	// the contract: JSON consumers parse objects by key (member order is not data), and a
	// session's Extra comes from fixed config, so it rides this one path every turn and its
	// KV prefix stays byte-identical — the no-Extra path has its own (struct-order) bytes,
	// pinned by TestEncodeBodyStructOrder above, and the two are now the same bytes.)
	// Numbers print shortest-form — also a prefix fact.
	want := `{"messages":[{"role":"user","content":"x"}],"model":"m1","nested":[1.5,true,null],"stream":true,"stream_options":{"include_usage":true},"top_k":1}`
	if string(body) != want {
		t.Fatalf("\n got %s\nwant %s", body, want)
	}
	again, err := p.encodeBody(req, true)
	if err != nil {
		t.Fatalf("encodeBody second call: %v", err)
	}
	if string(again) != string(body) {
		t.Fatalf("not deterministic:\nfirst  %s\nsecond %s", body, again)
	}
}

// TestEncodeBodyExtraMergeRawPassThrough bounds the one-time drift the single-pass
// rewrite traded for its savings, by asserting against the retired three-pass merge
// (mergeExtraTriplePass, kept as the oracle). Both encoders eat the same request; the
// new one must equal this golden byte string — schema members and tool-call arguments
// ride as stored, messages in struct order, Extra values sorted, top level sorted —
// and the oracle must differ from it in exactly two ways: every nested object decoded
// to sorted keys ({"function":… comes before "id"), and RawMessage schema members
// re-sorted. Nothing else moved: strings, numbers, escaping, and the Extra values are
// provably identical because they appear in both outputs byte for byte. A session that
// had Extra set re-prefills once after the upgrade (its prefix is oracle-shaped) and
// is new-shape stable every turn after.
func TestEncodeBodyExtraMergeRawPassThrough(t *testing.T) {
	p := &openai{cfg: Config{Model: "m1"}}
	schema := json.RawMessage(`{"type":"object","properties":{"z":{"type":"string"},"a":{}}}`)
	const args = `{"zeta":1,"alpha":{"y":2,"b":1}}`
	extra := map[string]any{"model": "over", "chat_template_kwargs": map[string]any{"thinking": true, "b_mode": false}}
	tools := []Tool{{Name: "ls", Parameters: schema}}
	messages := []asm.Message{{Role: "assistant", ToolCalls: []asm.ToolCallOut{
		{ID: "c1", Type: "function", Function: asm.FunctionCall{Name: "ls", Arguments: args}},
	}}}
	req := Request{Messages: messages, Tools: tools, Extra: extra}

	body, err := p.encodeBody(req, true)
	if err != nil {
		t.Fatalf("encodeBody: %v", err)
	}
	want := `{"chat_template_kwargs":{"b_mode":false,"thinking":true},"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"ls","arguments":"{\"zeta\":1,\"alpha\":{\"y\":2,\"b\":1}}"}}]}],"model":"over","stream":true,"stream_options":{"include_usage":true},"tools":[{"type":"function","function":{"name":"ls","parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{}}}}}]}`
	if string(body) != want {
		t.Fatalf("\n got %s\nwant %s", body, want)
	}

	triple, err := mergeExtraTriplePass(chatRequest{
		Model: "m1", Messages: messages, Tools: []toolDefJSON{{Type: "function", Function: functionDef{Name: "ls", Parameters: schema}}},
		Stream: true, StreamOptions: &streamOptions{IncludeUsage: true},
	}, extra)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if bytes.Equal(triple, body) {
		t.Fatalf("expected the documented drift against the oracle, got one shape:\n%s", triple)
	}
	// The oracle canonicalized nested objects to sorted keys — inside the tool_calls
	// element the function member sorts first, before id and type.
	if !strings.Contains(string(triple), `[{"function":{"arguments":"{\"zeta`) {
		t.Fatalf("oracle should sort nested message objects:\n%s", triple)
	}
	// …and re-sorted RawMessage schema members, the one place the new path deliberately
	// stops trusting and passes stored bytes through.
	if !strings.Contains(string(triple), `"properties":{"a":{},"z":{"type":"string"}}`) {
		t.Fatalf("oracle should re-sort schema members:\n%s", triple)
	}
}
