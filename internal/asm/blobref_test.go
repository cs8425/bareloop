package asm

// Tests for the reference path: entries whose image parts name the blob table
// ("blob:<sha>") instead of carrying base64. The promise they hold is one number and
// one byte string: a note quotes the same "~N KB" the base64 was always counted from,
// and an expanded payload is byte-identical to the payload the base64 would have built.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"bareloop/internal/ir"
)

// fakeBlobs answers the way the store does: base64 worked from the row's bytes, size
// worked from the column that keeps it. fails names the shas whose rows are gone —
// the shape of a corrupted store, which both doors must refuse rather than invent.
type fakeBlobs struct {
	raw   map[string][]byte
	mimes map[string]string
	fails map[string]bool
	gets  int // point reads, counted so "assembling reads no blob" is a fact, not a hope
	sizes int
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{raw: map[string][]byte{}, mimes: map[string]string{}, fails: map[string]bool{}}
}

func (f *fakeBlobs) Size(sha string) (int, error) {
	f.sizes++
	raw, ok := f.raw[sha]
	if !ok || f.fails[sha] {
		return 0, fmt.Errorf("blob %s is not in the store", sha)
	}
	return len(raw), nil
}

func (f *fakeBlobs) Get(sha string) (string, string, error) {
	f.gets++
	raw, ok := f.raw[sha]
	if !ok || f.fails[sha] {
		return "", "", fmt.Errorf("blob %s is not in the store", sha)
	}
	return base64.StdEncoding.EncodeToString(raw), f.mimes[sha], nil
}

func refImage(sha, mime, name string) ir.Part {
	return ir.Part{Type: ir.PartImage, MimeType: mime, Data: ir.RefPrefix + sha, Name: name}
}

func image(b64, mime, name string) ir.Part {
	return ir.Part{Type: ir.PartImage, MimeType: mime, Data: b64, Name: name}
}

func marshalAll(t *testing.T, msgs []Message) []byte {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// The cross-shape identity this whole design lives on: a reference window assembled
// and expanded is byte-for-byte the payload a base64 window assembles directly, so no
// KV cache can tell the two apart and no session reprefills because its daemon got
// leaner.
func TestExpandedReferencePayloadIsByteIdenticalToBytesPayload(t *testing.T) {
	raw := bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 7}, 700)
	b64 := base64.StdEncoding.EncodeToString(raw)
	const sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	keptB64 := []*ir.Entry{
		{ID: "1", Role: "user", Content: "look", Parts: []ir.Part{image(b64, "image/png", "pic.png"), {Type: ir.PartText, Text: "and"}}},
	}
	keptRef := []*ir.Entry{
		{ID: "1", Role: "user", Content: "look", Parts: []ir.Part{refImage(sha, "image/png", "pic.png"), {Type: ir.PartText, Text: "and"}}},
	}

	fake := newFakeBlobs()
	fake.raw[sha] = raw
	fake.mimes[sha] = "image/png"

	builtB64 := Assemble(Payload{System: "sys", Kept: keptB64}, Options{})
	builtRef := Assemble(Payload{System: "sys", Kept: keptRef}, Options{Blobs: fake})

	// Assembling a reference window under a vision session reads no blob at all:
	// the placeholder URL goes onto the wire's draft verbatim.
	if fake.gets != 0 || fake.sizes != 0 {
		t.Errorf("assembling over references asked the blob table %d gets, %d sizes; wanted none", fake.gets, fake.sizes)
	}
	part := builtRef.Messages[0].Content.([]ContentPart)[0]
	if part.ImageURL == nil || part.ImageURL.URL != ir.RefPrefix+sha {
		t.Errorf("reference assembly put %q in the URL; wanted the reference verbatim", part.ImageURL)
	}

	msgs, err := ExpandMessages(builtRef.All(), fake)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if want, got := marshalAll(t, builtB64.All()), marshalAll(t, msgs); !bytes.Equal(want, got) {
		t.Errorf("expanded reference payload differs from the base64 payload:\n want %s\n got  %s", want, got)
	}
	if fake.gets != 1 {
		t.Errorf("expansion read the blob %d times; wanted one read per image", fake.gets)
	}
	// The input is never rewritten: what the session keeps holding is the reference.
	if p := builtRef.Messages[0].Content.([]ContentPart)[0]; p.ImageURL.URL != ir.RefPrefix+sha {
		t.Errorf("ExpandMessages rewrote the caller's message: %q", p.ImageURL.URL)
	}
}

// A text-only note quotes the attachment's size from the base64's length. A reference
// does not carry the length — it carries a size the store answers raw — and the two
// integer chains must agree at every size, not just usually.
func TestReferenceNoteQuotesTheSameKB(t *testing.T) {
	fake := newFakeBlobs()
	for n := 0; n <= 15000; n++ {
		raw := make([]byte, n)
		sha := fmt.Sprintf("%064x", n)
		fake.raw[sha] = raw
		want := kbWord(image(base64.StdEncoding.EncodeToString(raw), "image/png", "x"), nil)
		got := kbWord(refImage(sha, "image/png", "x"), fake)
		if got != want {
			t.Fatalf("raw size %d: base64 path says %s KB, reference path says %s", n, want, got)
		}
	}
	for _, n := range []int{600_000, 1_103_297, 5_000_001, 17_000_003} {
		raw := make([]byte, n)
		sha := fmt.Sprintf("big%061d", n%1000000)
		fake.raw[sha] = raw
		want := kbWord(image(base64.StdEncoding.EncodeToString(raw), "image/png", "x"), nil)
		got := kbWord(refImage(sha, "image/png", "x"), fake)
		if got != want {
			t.Errorf("raw size %d: base64 path says %s KB, reference path says %s", n, want, got)
		}
	}
	// A size nobody can answer is said as a question, never as a number.
	fake.fails["missing"] = true
	if got := kbWord(refImage("missing", "image/png", "x"), fake); got != "?" {
		t.Errorf("unreadable blob: KB = %q; wanted ?", got)
	}
	if got := kbWord(refImage("anything", "image/png", "x"), nil); got != "?" {
		t.Errorf("no resolver: KB = %q; wanted ?", got)
	}
}

// A reference without bytes to price it still says so in its own note rather than
// inventing a size.
func TestAssembleReferenceNoteWithoutResolver(t *testing.T) {
	e := &ir.Entry{ID: "1", Role: "user", Parts: []ir.Part{refImage("abc", "image/png", "pic.png")}}
	b := Assemble(Payload{Kept: []*ir.Entry{e}}, Options{SendImages: boolPtr(false)})
	text, ok := b.Messages[0].Content.(string)
	if !ok || !strings.Contains(text, "~? KB)") {
		t.Errorf("note without a resolver: %q; wanted it to say ~? KB", b.Messages[0].Content)
	}
}

func TestExpandMessagesRefusalsAndPassthrough(t *testing.T) {
	fake := newFakeBlobs()
	fake.raw["good"] = []byte("pixel")
	fake.mimes["good"] = "image/gif"
	fake.fails["gone"] = true

	// A message with no reference is returned standing: string content, text
	// parts, and a data URL that is already a data URL all pass untouched.
	plain := []Message{
		{Role: "system", Content: "head"},
		{Role: "user", Content: []ContentPart{{Type: PartTypeText, Text: "t"}}},
		{Role: "user", Content: []ContentPart{{Type: PartTypeImage, ImageURL: &ImageURL{URL: "data:image/png;base64,AAA"}}}},
	}
	got, err := ExpandMessages(plain, fake)
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	if b, want := marshalAll(t, got), marshalAll(t, plain); string(b) != string(want) {
		t.Errorf("passthrough changed the messages:\n want %s\n got  %s", want, b)
	}
	if fake.gets != 0 {
		t.Errorf("passthrough read the blob table %d times", fake.gets)
	}

	// A stored mime of "" follows the assembler's fallback, the shape DataURL has
	// always produced for a part that carried no mime.
	msgs, err := ExpandMessages([]Message{{Role: "user", Content: []ContentPart{
		{Type: PartTypeImage, ImageURL: &ImageURL{URL: ir.RefPrefix + "good"}},
	}}}, fake)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	url := msgs[0].Content.([]ContentPart)[0].ImageURL.URL
	if want := "data:image/gif;base64," + base64.StdEncoding.EncodeToString([]byte("pixel")); url != want {
		t.Errorf("expanded URL = %q; wanted %q", url, want)
	}

	// A blob that cannot be read refuses the send, naming itself.
	_, err = ExpandMessages([]Message{{Role: "user", Content: []ContentPart{
		{Type: PartTypeImage, ImageURL: &ImageURL{URL: ir.RefPrefix + "gone"}},
	}}}, fake)
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("missing blob: err = %v; wanted a refusal naming the sha", err)
	}

	// A reference with no resolver at all is the same refusal, not a silent drop.
	_, err = ExpandMessages([]Message{{Role: "user", Content: []ContentPart{
		{Type: PartTypeImage, ImageURL: &ImageURL{URL: ir.RefPrefix + "good"}},
	}}}, nil)
	if err == nil {
		t.Error("no resolver: expand returned no error; wanted a refusal")
	}
}
