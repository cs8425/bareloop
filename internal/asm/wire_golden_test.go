package asm

// Wire byte contract for ContentPart. The referee here is a literal, not an implementation:
// llama.cpp reads part.at("text") for every part and answers a request that dropped the key
// with 500 "key 'text' not found", and the KV cache is keyed by the exact prefix bytes, so a
// part that re-marshals differently between turns re-prefills the whole conversation.
//
// The literals exist because the marshalling implementation is changing: the v1 MarshalJSON
// costs encoding/json v2 a full reformat+clone of every part — the daemon's largest
// allocation source — and the v2-native MarshalJSONTo(*jsontext.Encoder) that replaces it
// must produce these same bytes. The moment the method exists, the cross-check below arms
// itself and judges it against the table and against Marshal, directly.

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"reflect"
	"testing"
)

var goldenParts = []struct {
	name string
	part ContentPart
	want string
}{
	{"text", ContentPart{Type: PartTypeText, Text: "hello, world"},
		`{"type":"text","text":"hello, world"}`},
	// The key llama.cpp 500s without: an empty text part still spells "text".
	{"text-empty", ContentPart{Type: PartTypeText},
		`{"type":"text","text":""}`},
	// An unset type fills in as text.
	{"type-default", ContentPart{Text: "x"},
		`{"type":"text","text":"x"}`},
	{"image", ContentPart{Type: PartTypeImage, ImageURL: &ImageURL{URL: "data:image/png;base64,QUJD"}},
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}`},
	// The image shape never carries a text key, and a set Text is not its output either.
	{"image-ignores-text", ContentPart{Type: PartTypeImage, Text: "ignored", ImageURL: &ImageURL{URL: "u"}},
		`{"type":"image_url","image_url":{"url":"u"}}`},
	// The escaping regime is part of the byte contract, and it is measured, not assumed:
	// json.Marshal in this build escapes < > & (the v1-compatible default of the v2 engine),
	// so the wire bytes of any text carrying them are the escaped form — today and after
	// MarshalJSONTo arrives. An encoder written with a naked jsontext default (no HTML
	// escape) would silently differ here; this line is the tripwire.
	{"html-trio", ContentPart{Type: PartTypeText, Text: "<a> & </a>"},
		`{"type":"text","text":"\u003ca\u003e \u0026 \u003c/a\u003e"}`},
	// Measured, not assumed: this build's v1-compat layer escapes U+2028 as well — the JS
	// pair rides alongside the HTML trio here, which plain v1 never did. That sentence used
	// to claim the opposite from memory; the second tripwire in one sitting is the argument
	// for writing every side of this fence from the build. Both pins flip to raw in the
	// native-marshal commit, where the policy becomes: nothing beyond JSON's own rules.
	{"u2028-passthrough", ContentPart{Type: PartTypeText, Text: "line\u2028sep"},
		`{"type":"text","text":"line\u2028sep"}`},
	{"escapers", ContentPart{Type: PartTypeText, Text: "quote\" back\\ slash\n nl\t tab"},
		`{"type":"text","text":"quote\" back\\ slash\n nl\t tab"}`},
	{"unicode-passthrough", ContentPart{Type: PartTypeText, Text: "你看到了啥?"},
		`{"type":"text","text":"你看到了啥?"}`},
}

// TestContentPartWireGolden pins every shape to its bytes, through the one path every
// caller uses: json.Marshal.
func TestContentPartWireGolden(t *testing.T) {
	for _, c := range goldenParts {
		raw, err := json.Marshal(c.part)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if string(raw) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, raw, c.want)
		}
	}
}

// TestPayloadContentBytesGolden pins the whole content-array shape the way it rides a real
// message: [image, text] for a picture with a caption. This is the KV prefix — if these
// bytes drift, the provider's cache starts over for every turn that ever carried a picture.
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

// TestMarshalJSONToMatchesMarshal is the armed cross-check: it looks for the v2-native
// marshaler, and when present calls it directly — the way encoding/json v2 will — and
// demands byte equality with the golden table for every shape. Before the method exists it
// skips: the golden table above already binds the current path, and an absent alternative
// has nothing to compare against. The encoder is built with the escaping options MEASURED
// on this build's json.Marshal (HTML trio and the JS pair — both pinned by the golden table
// above) — comparing against a naked jsontext default would judge the method against an
// escaping regime no caller of this path uses. The native-marshal commit flips this to the
// naked default, which is exactly when the golden table earns its keep again.
func TestMarshalJSONToMatchesMarshal(t *testing.T) {
	partType := reflect.TypeOf(ContentPart{})
	encType := reflect.TypeOf((*jsontext.Encoder)(nil))
	errType := reflect.TypeOf((*error)(nil)).Elem()

	if pv := reflect.New(partType); !pv.MethodByName("MarshalJSONTo").IsValid() &&
		!reflect.ValueOf(ContentPart{}).MethodByName("MarshalJSONTo").IsValid() {
		t.Skip("ContentPart has no MarshalJSONTo yet; the golden table binds the current path")
	}

	encode := func(p ContentPart) ([]byte, error) {
		pv := reflect.New(partType)
		pv.Elem().Set(reflect.ValueOf(p))
		m := pv.MethodByName("MarshalJSONTo")
		if !m.IsValid() {
			m = reflect.ValueOf(p).MethodByName("MarshalJSONTo")
		}
		if mt := m.Type(); mt.NumIn() != 1 || mt.In(0) != encType || mt.NumOut() != 1 || !mt.Out(0).Implements(errType) {
			t.Fatalf("MarshalJSONTo must be func(*jsontext.Encoder) error, got %v", mt)
		}
		var buf bytes.Buffer
		// No Flush: this encoder writes its tokens through (it has no buffering contract),
		// which is the same reason MarshalJSONTo receives the engine's own encoder.
		// Both options because this build's json.Marshal is measured to escape the HTML trio
		// AND U+2028/29 — the stand-in must run the policy it is being compared against.
		enc := jsontext.NewEncoder(&buf, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
		if out := m.Call([]reflect.Value{reflect.ValueOf(enc)}); !out[0].IsNil() {
			return nil, out[0].Interface().(error)
		}
		return buf.Bytes(), nil
	}

	for _, c := range goldenParts {
		via, err := encode(c.part)
		if err != nil {
			t.Fatalf("%s: MarshalJSONTo: %v", c.name, err)
		}
		if string(via) != c.want {
			t.Errorf("%s: MarshalJSONTo writes different bytes than the golden:\n got %s\nwant %s", c.name, via, c.want)
		}
	}
}
