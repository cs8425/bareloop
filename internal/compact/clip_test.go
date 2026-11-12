package compact

import (
	"strings"
	"testing"

	"bareloop/internal/ir"
)

func TestClipEntryLeavesSmallEntriesAlone(t *testing.T) {
	small := txt("s", "user", 100)
	got, clipped := ClipEntry(small, 1000)
	if clipped || got != small {
		t.Fatal("a short entry must come back untouched")
	}
	if _, clipped := ClipEntry(txt("e", "user", 0), 1000); clipped {
		t.Fatal("an empty entry has nothing to clip")
	}
	big := txt("b", "user", 40000)
	out, clipped := ClipEntry(big, 500)
	if !clipped {
		t.Fatal("expected clipping")
	}
	if !strings.Contains(out.Content, "does not fit the model window") {
		t.Fatalf("missing the omission marker: %s", out.Content[:200])
	}
	if out.Role != big.Role || out.ID != big.ID || out.Kind != big.Kind {
		t.Fatal("the copy must keep the identity of the entry")
	}
	if big.Content == out.Content || len(big.Content) <= len(out.Content) {
		t.Fatal("the entry in the log must never be rewritten")
	}
}

func TestClipOversizedPassesThroughWhatFits(t *testing.T) {
	a, b := txt("a", "user", 100), txt("b", "user", 40000)
	c, d := txt("c", "user", 100), txt("d", "user", 100)
	before := b.Content

	out, clipped := ClipOversized([]*ir.Entry{a, b, c, d}, 500)
	if clipped != 1 {
		t.Fatalf("clipped = %d, want exactly the one oversized entry", clipped)
	}
	if len(out) != 4 {
		t.Fatalf("the region must come back the same length: %d", len(out))
	}
	if out[0] != a || out[2] != c || out[3] != d {
		t.Fatal("an entry that fits must come back as itself, not a copy")
	}
	if !strings.Contains(out[1].Content, "omitted") || len(out[1].Content) >= len(before) {
		t.Fatal("the oversized entry did not come back clipped")
	}
	if b.Content != before {
		t.Fatal("the entry in the log must never be rewritten")
	}
	if !strings.HasPrefix(out[1].Content, "abcd abcd ") {
		t.Fatal("the head of the message must survive the clip")
	}

	if _, clipped := ClipOversized([]*ir.Entry{a}, 0); clipped != 0 {
		t.Fatal("a budget of nothing clips nothing")
	}
}
