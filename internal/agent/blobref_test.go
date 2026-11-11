package agent

// The resident-shape contract of the blob window: a session holding a screenshot
// keeps a hash, not the pixels; the ruler (Payload) still shows exactly what would go
// on the wire — base64 and all — and what the session remembers sending is the
// reference, so the remembered payload costs its text and nothing more.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

func TestWindowKeepsReferencesAndPayloadShowsBytes(t *testing.T) {
	s, _ := chainFixture(t, "blob-window", 4096)
	img := bytes.Repeat([]byte("screenshot-pixel"), 409) // ~6.5 KB of raw bytes
	b64 := base64.StdEncoding.EncodeToString(img)
	e := &ir.Entry{Kind: ir.KindUser, Role: "user", Content: "what does this show?",
		Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: b64, Name: "shot.png"}}}
	if _, err := s.appendEntry(e); err != nil {
		t.Fatalf("append: %v", err)
	}
	sum := sha256.Sum256(img)
	sha := hex.EncodeToString(sum[:])

	// The window reads references.
	kept, _, err := s.window()
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	var win *ir.Entry
	for _, k := range kept {
		if k.ID == e.ID {
			win = k
		}
	}
	if win == nil {
		t.Fatal("the window never carried the entry")
	}
	if got := win.Parts[0].Data; got != ir.RefPrefix+sha {
		t.Errorf("window holds %q…; want the reference %s%s…", clipForTest(got, 40), ir.RefPrefix, sha[:16])
	}

	// The ruler shows the wire: expanded, byte-for-byte the data URL it always was.
	pl, err := s.Payload()
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	docs, err := json.Marshal(pl["messages"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := "data:image/png;base64," + b64; !strings.Contains(string(docs), want) {
		t.Error("the payload ruler shows no data URL for the image")
	}
	if strings.Contains(string(docs), ir.RefPrefix+sha) {
		t.Error("the payload ruler leaked a reference where bytes are owed")
	}

	// What the loop remembers sending is what it held before expanding: the
	// reference. A remembered screenshot is 71 bytes, not a megabyte.
	built, _, tools, err := s.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s.rememberSent(built.All(), asm.EntryIDs(built), tools)
	sent, _, _ := s.lastSentPayload()
	mem, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(mem), ir.RefPrefix+sha) {
		t.Error("the remembered payload lost the reference")
	}
	if strings.Contains(string(mem), "data:image/png;base64,") {
		t.Error("the remembered payload holds base64; the resident shape is the reference")
	}
}
