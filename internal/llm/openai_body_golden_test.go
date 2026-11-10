package llm

// Body-level golden: the hot-cache contract in one byte string. The provider keys its prompt
// cache on the token stream produced from exactly these bytes, so anything that shifts them —
// a field order, an escaping regime, a map iteration — re-prefills every live session. The
// asm package pins ContentPart (including its U+2028 passthrough, written there with visible
// escapes); this pins the whole envelope: chatRequest's declared field order, the v2-native
// no-escaping wire, the Extra-merge key sort, and determinism across calls (every turn
// re-sends the prefix, so the same request must encode identically twice). No invisible
// characters in goldens — every byte here is one a human can proofread.

import (
	"encoding/json"
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
	// Sorted map keys decide the order here — and "here" is every level: the round trip
	// through map[string]any canonicalizes NESTED objects too, so the message itself comes
	// back as {"content":…, "role":…}, not in struct order. Two facts make this safe, and
	// both belong to this contract: JSON consumers parse objects by key (member order is
	// not data), and a session's Extra comes from fixed config, so it rides this one path
	// every turn and its KV prefix stays byte-identical — the no-Extra path has its own
	// (struct-order) bytes, pinned by TestEncodeBodyStructOrder above.
	// Numbers print shortest-form, floats survive the round trip — also prefix facts.
	want := `{"messages":[{"content":"x","role":"user"}],"model":"m1","nested":[1.5,true,null],"stream":true,"stream_options":{"include_usage":true},"top_k":1}`
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
