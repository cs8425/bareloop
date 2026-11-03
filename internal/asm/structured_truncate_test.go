package asm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"bareloop/internal/ir"
)

// docOfSize is a JSON-shaped document of exactly n bytes (n >= 14). Its middle is one long run of
// "x" so that a cut's position is knowable from a byte count alone and a test can name the exact
// prefix the model should have been given.
func docOfSize(n int) string {
	const overhead = len(`{"items":["`) + len(`"]}`)
	if n < overhead+1 {
		n = overhead + 1
	}
	return `{"items":["` + strings.Repeat("x", n-overhead) + `"]}`
}

// foldWith assembles one tool result under a mode and a ceiling and answers what went on the wire,
// plus the payload it came from.
func foldWith(t *testing.T, prose, raw string, mode StructuredMode, maxBytes int) (string, *Built) {
	t.Helper()
	e := structuredEntry(prose)
	e.Structured = json.RawMessage(raw)
	b := Assemble(structuredPayload(e), Options{
		Structured:         &StructuredTargets{Default: mode},
		StructuredMaxBytes: maxBytes,
	})
	return contentText(t, toolMessage(t, b).Content), b
}

// TestTruncatedResultSaysWhatWasCut pins the whole shape of a cut result: the server's prose, the
// marker, exactly the first maxBytes of the document, and one note naming what is missing. The
// assertion is on the full string rather than on substrings because the wording is what stops the
// model inventing the rest — changing it should cost whoever changed it a failing test.
func TestTruncatedResultSaysWhatWasCut(t *testing.T) {
	raw := docOfSize(5000)
	got, _ := foldWith(t, "prose", raw, StructuredAlways, 1000)

	want := "prose\n" + StructuredMarker + "\n" + raw[:1000] + truncationNote(1000, len(raw))
	if got != want {
		t.Errorf("wire is %d bytes, want %d: %s", len(got), len(want), got[len(got)-120:])
	}
	if !strings.Contains(got, fmt.Sprintf("of a %d-byte document", len(raw))) {
		t.Errorf("the note does not name the original size: %s", got[len(got)-200:])
	}
	if !strings.Contains(got, "will not parse") {
		t.Errorf("the note does not say the fragment is incomplete: %s", got[len(got)-200:])
	}
	if strings.Contains(got, raw[len(raw)-100:]) {
		t.Error("the tail of the document survived the cut")
	}
}

// TestNoteReadsAsTheDocumentedSentence pins the wording character for character against the sentence
// README quotes. Every other test here compares the wire against truncationNote itself, so rewording the
// note keeps them all green while the documented promise goes stale. The doubled bracket is the mistake
// this exists for: StructuredMarker arrives with its own brackets already on it.
func TestNoteReadsAsTheDocumentedSentence(t *testing.T) {
	got := truncationNote(16384, 10485760)
	want := "\n[structuredContent truncated: the first 16384 bytes of a 10485760-byte document are shown " +
		"above, and it is missing its last 10469376 bytes. What is above is cut off mid-document and will " +
		"not parse; answer only from what is shown, and say what you cannot see.]"
	if got != want {
		t.Errorf("the note is %q, not the sentence README quotes", got)
	}
}

// TestStoredEntryKeepsTheWholeDocument: the ceiling is a wire decision. The entry, the log behind it
// and the UI keep every byte, which is what makes the setting reversible and, in phase two, readable
// again.
func TestStoredEntryKeepsTheWholeDocument(t *testing.T) {
	raw := docOfSize(5000)
	_, b := foldWith(t, "prose", raw, StructuredAlways, 1000)

	var tool *ir.Entry
	for _, e := range b.Entries {
		if e.Kind == ir.KindTool {
			tool = e
		}
	}
	if tool == nil {
		t.Fatal("no tool entry in the payload")
	}
	if len(tool.Structured) != len(raw) {
		t.Errorf("the payload carries %d stored bytes, want all %d", len(tool.Structured), len(raw))
	}
	if tool.Content != "prose" {
		t.Errorf("the entry's own text was rewritten: %q", tool.Content)
	}
	if b.StructuredCap != 1000 {
		t.Errorf("Built.StructuredCap = %d, want the ceiling the payload was built with", b.StructuredCap)
	}
}

// TestRaisingTheCeilingKeepsThePrefix is the KV-cache claim: what a smaller ceiling shows is a prefix
// of what a bigger one shows, so changing the ceiling does not rewrite the start of every past tool
// result — and the same document assembled twice sends the same bytes.
func TestRaisingTheCeilingKeepsThePrefix(t *testing.T) {
	raw := docOfSize(20000)
	head := "prose\n" + StructuredMarker + "\n"
	small, _ := foldWith(t, "prose", raw, StructuredAlways, 1000)
	again, _ := foldWith(t, "prose", raw, StructuredAlways, 1000)
	big, _ := foldWith(t, "prose", raw, StructuredAlways, 2000)

	if small != again {
		t.Error("assembling the same document twice produced different bytes")
	}
	if !strings.HasPrefix(small, head+raw[:1000]) || !strings.HasPrefix(big, head+raw[:2000]) {
		t.Fatal("the two payloads are not the heads this test claims to compare")
	}
	if !strings.HasPrefix(big, strings.TrimSuffix(small, truncationNote(1000, len(raw)))) {
		t.Error("the smaller ceiling is not a prefix of the bigger one")
	}
}

// TestCutNeverSplitsARune runs the cut across every alignment inside a three-byte rune. Half a
// character at the end of a fragment reads as mojibake to the model and can be refused outright by a
// strict provider, so the head is always backed off to a rune boundary — and the note has to name
// the number of bytes actually kept, not the ceiling that was asked for.
func TestCutNeverSplitsARune(t *testing.T) {
	raw := `{"v":"` + strings.Repeat("哈", 3000) + `"}`
	for cut := 900; cut <= 1010; cut++ {
		got, _ := foldWith(t, "prose", raw, StructuredAlways, cut)
		if !utf8.ValidString(got) {
			t.Fatalf("ceiling %d sent invalid UTF-8", cut)
		}
		kept := headWithin(raw, cut)
		want := "prose\n" + StructuredMarker + "\n" + kept + truncationNote(len(kept), len(raw))
		if got != want {
			t.Fatalf("ceiling %d: kept %d bytes, which is not the rune-aligned head", cut, len(kept))
		}
		if len(kept) == cut && cut < len(raw) && !utf8.RuneStart(raw[cut]) {
			t.Fatalf("ceiling %d cut in the middle of a rune", cut)
		}
	}
}

// TestDropNeedsNoCeiling: drop already sends nothing, and a ceiling must not put the JSON back.
func TestDropNeedsNoCeiling(t *testing.T) {
	got, _ := foldWith(t, "prose", docOfSize(5000), StructuredDrop, 1000)
	if got != "prose" {
		t.Errorf("drop = %q, want the prose alone", got)
	}
}

// TestFallbackWithoutTextIsStillCapped: with no prose at all the JSON is the whole message, which is
// the easiest way for one server to cost a window on its own.
func TestFallbackWithoutTextIsStillCapped(t *testing.T) {
	raw := docOfSize(5000)
	got, _ := foldWith(t, "", raw, StructuredFallback, 1000)
	want := StructuredMarker + "\n" + raw[:1000] + truncationNote(1000, len(raw))
	if got != want {
		t.Errorf("fallback without text is %d bytes, want %d", len(got), len(want))
	}
}

// TestAutoWithTheDocumentAlreadyInTheText: when a server puts its JSON in the prose itself, auto
// appends nothing — and the prose is left exactly as it arrived, ceiling or no ceiling. That text is
// the tool result proper, and shortening it silently is a different decision with different
// consequences; the remaining hole is recorded in _state, and this test is what keeps it a decision
// rather than an oversight.
func TestAutoWithTheDocumentAlreadyInTheText(t *testing.T) {
	raw := docOfSize(5000)
	got, _ := foldWith(t, "here: "+raw, raw, StructuredAuto, 1000)
	if got != "here: "+raw {
		t.Errorf("auto sent %d bytes, want the prose untouched at %d", len(got), len(raw)+6)
	}
	if strings.Contains(got, "truncated") {
		t.Error("a fold that appended nothing has nothing to say about truncation")
	}
}

// TestDocumentUnderTheCeilingIsUntouched is the regression guard for every session that never hits
// this: no note, no marker change, byte for byte what the assembler did before ceilings existed.
func TestDocumentUnderTheCeilingIsUntouched(t *testing.T) {
	raw := docOfSize(5000)
	got, _ := foldWith(t, "prose", raw, StructuredAlways, 100000)
	if got != "prose\n"+StructuredMarker+"\n"+raw {
		t.Errorf("wire = %q, want prose, marker and the document", got)
	}
}

// TestEstimateFollowsTheCeilingAndNeverUnderrunsTheWire holds both directions that matter. Pricing
// the whole stored document is the bug this file exists to fix: after a restart — which forgets the
// real usage — a 10 MB result would read as megabytes of tokens, and the pre-send guard would refuse
// a payload that is really sixteen thousand bytes. Under-counting the bytes actually sent is the bug
// the old comment in Estimate warned about, so it is asserted against the assembled wire itself.
func TestEstimateFollowsTheCeilingAndNeverUnderrunsTheWire(t *testing.T) {
	raw := docOfSize(1_000_000)
	e := structuredEntry("prose")
	e.Structured = json.RawMessage(raw)

	capped := Estimate(e, 1000)
	if capped*4 > 100_000 {
		t.Errorf("a document of %d bytes capped to 1000 estimated at %d tokens: the ceiling never reached the estimator", len(raw), capped)
	}
	whole := Estimate(e, 10_000_000)
	if whole <= capped {
		t.Errorf("a ceiling of ten megabytes estimated %d tokens, no more than the 1000-byte one at %d", whole, capped)
	}

	for _, maxBytes := range []int{1024, 4096, 0, DefaultStructuredMaxBytes} {
		wire, _ := foldWith(t, "prose", raw, StructuredAlways, maxBytes)
		// Two floor divisions by four can drop a few bytes, which is the only slack allowed here.
		if got := Estimate(e, maxBytes); got*4 < len(wire)-8 {
			t.Errorf("ceiling %d: the estimate is %d tokens (%d bytes) but %d bytes went on the wire",
				maxBytes, got, got*4, len(wire))
		}
	}
}

// TestZeroMeansTheDefault: the assembler, the fold and the estimator have to read "unset" the same
// way, or the numbers the guard trusts stop describing the payload it is about to send.
func TestZeroMeansTheDefault(t *testing.T) {
	if got := (Options{}).structuredMax(); got != DefaultStructuredMaxBytes {
		t.Errorf("Options{} ceiling = %d, want the default %d", got, DefaultStructuredMaxBytes)
	}
	if got := (Options{StructuredMaxBytes: -1}).structuredMax(); got != DefaultStructuredMaxBytes {
		t.Errorf("a hand-edited negative ceiling = %d, want the default rather than no limit", got)
	}
	if a, b := structuredWireBytes(1_000_000, 0), structuredWireBytes(1_000_000, DefaultStructuredMaxBytes); a != b {
		t.Errorf("the estimator reads 0 as %d bytes and the default as %d", a, b)
	}
	_, built := foldWith(t, "prose", docOfSize(DefaultStructuredMaxBytes*3), StructuredAlways, 0)
	if built.StructuredCap != DefaultStructuredMaxBytes {
		t.Errorf("the payload ceiling = %d, want %d", built.StructuredCap, DefaultStructuredMaxBytes)
	}
}

// TestTruncationNoteStaysInsideItsCeiling keeps structuredNoteBytes honest: it is the allowance the
// estimator trusts, so no note may outgrow it — not the thin ones, and not the widest note that can
// exist, which is the case the allowance cannot be measured from a single call (three counts, never all
// three widest at once).
func TestTruncationNoteStaysInsideItsCeiling(t *testing.T) {
	for _, kept := range []int{1, 1024, DefaultStructuredMaxBytes} {
		for _, total := range []int{kept + 1, 1 << 20, 1 << 40, 1 << 62} {
			if n := len(truncationNote(kept, total)); n > structuredNoteBytes {
				t.Errorf("a note of %d bytes outgrew the %d-byte allowance measured from it", n, structuredNoteBytes)
			}
		}
	}
	// The widest note that can actually exist, and the reason the allowance is not measured from one call:
	// a shown head and a missing tail that are both nineteen digits long. No (kept, total) pair used by
	// the loop above produces that, which is exactly how an allowance comes to be under the note it bounds.
	wide := truncationNote(1_000_000_000_000_000_000, 2_000_000_000_000_000_000)
	if len(wide) > structuredNoteBytes {
		t.Errorf("a note of %d bytes outgrew the %d-byte allowance: %s", len(wide), structuredNoteBytes, wide)
	}
}

// TestValidateStructuredMax pins the whole of the door: 0 is "unset", any positive count is a
// ceiling somebody is allowed to want, and only the value that would mean the opposite of what it
// says is refused. There is no floor to test for, and that is the assertion too — a small ceiling is
// a visible, reversible choice, while a negative is what gets written for "no limit".
func TestValidateStructuredMax(t *testing.T) {
	for _, ok := range []int{0, 1, 16, 1024, DefaultStructuredMaxBytes, 1 << 40} {
		if err := ValidateStructuredMax(ok); err != nil {
			t.Errorf("%d bytes refused: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, -16384} {
		if err := ValidateStructuredMax(bad); err == nil {
			t.Errorf("%d was stored; a negative byte count is not a request to send everything", bad)
		}
	}
}
