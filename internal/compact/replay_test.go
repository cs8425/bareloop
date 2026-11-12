package compact

import (
	"strings"
	"testing"

	"bareloop/internal/asm"
)

// TestTippingPlansAgainstThePrefixThatTips is the replay round's planning path: the conversation
// cut at the entry whose estimate first pushes it past the line the trigger compares against —
// the message that "made it overflow" in the one-at-a-time story this replays. Nothing beyond the
// tip exists for the round: the size table it plans against stops there too, which is what stops
// Select from walking a suffix the round never asked about.
func TestTippingPlansAgainstThePrefixThatTips(t *testing.T) {
	path := region(10)
	sizes := asm.DeriveSizes(&asm.Built{Entries: path}, 50)
	totals := make([]int, len(path)+1)
	totals[0] = sizes.Head()
	for i, e := range path {
		totals[i+1] = totals[i] + sizes.Tokens(e.ID)
	}
	// tip so that the running total crosses after exactly 4 entries.
	level := totals[3] + (totals[4]-totals[3])/2

	virt, vsizes := Tipping(path, sizes, level)
	if got, want := strings.Join(ids(virt), ","), "a,b,c,d"; got != want {
		t.Fatalf("the virtual path is %s, want the prefix that first crosses the line", got)
	}
	if vsizes.Total() != totals[4] {
		t.Fatalf("the planning table totals %d, want the tipped prefix with its head: %d", vsizes.Total(), totals[4])
	}
	suf, order := vsizes.Suffix()
	if len(order) != 4 || suf[0] != totals[4]-sizes.Head() {
		t.Fatalf("Select walks suffix[0]=%d over %d entries; the tipped prefix costs %d without its head",
			suf[0], len(order), totals[4]-sizes.Head())
	}
	// The full table is a separate read of the same prices, not a mutated one.
	if sizes.Total() != totals[len(path)] {
		t.Fatal("Tipping must not rewrite the full size table")
	}
}

// TestTippingLetsAnUntippedPathThrough keeps the ordinary compaction ordinary: a path whose whole
// estimate stays under the line — every conversation that triggers off a real usage number rather
// than an estimate overshoot — is planned whole, with the caller's own table.
func TestTippingLetsAnUntippedPathThrough(t *testing.T) {
	path := region(10)
	sizes := asm.DeriveSizes(&asm.Built{Entries: path}, 50)

	virt, vsizes := Tipping(path, sizes, sizes.Total())
	if len(virt) != len(path) || vsizes != sizes {
		t.Fatal("a path that never crosses the line is the planning path")
	}
	if virt, vsizes := Tipping(path, sizes, 0); len(virt) != len(path) || vsizes != sizes {
		t.Fatal("no line means no cut")
	}
	if virt, _ := Tipping(nil, sizes, 1); virt != nil {
		t.Fatal("an empty path has nothing to tip")
	}
}
