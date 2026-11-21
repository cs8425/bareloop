package compact

// A 413 is a fact about bytes, so the round it triggers plans in bytes: Params.WireBudgetBytes moves the
// kept-tail cap from "keepBudget% of the window" to "keepBudget% of what the path weighs". The live
// symptom this pins is a rescue that kept working and kept failing: three compactions in ninety seconds,
// each one replacing the shallowest legal position, the request parked at 95 MB under a 102 MB wall the
// whole time — because "the lightest tail the token cap allows, vetoed only by the wall" is by
// construction a tail with no headroom, and the turn that triggered it is still growing.
//
// The fixture stands in for a path of images without dragging a blob store in: DeriveSizes takes a
// stamped Tokens as measured (Size.Real), so an entry can cost 50 tokens and carry 400 KB, which is the
// entire disagreement between the two rulers (the provider billed the real ones at 505 tokens for
// 3.7 MB). As in the other selection tests, every byte number below is read out of the size table.

import (
	"strings"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

// wireEntry stamps tok tokens onto bytes of content: the image-shaped entry.
func wireEntry(id, role string, tok, bytes int) *ir.Entry {
	return &ir.Entry{ID: id, Kind: role, Role: role, Content: strings.Repeat("x", bytes), Tokens: tok}
}

// wirePath is [user light, assistant light, user fat, assistant fat, user light]: 250 tokens and
// roughly 800 KB, with the weight in the middle — so a token cap stops the cut at the third entry while
// a byte cap has to walk past the fat ones, and both still find a legal user boundary.
func wirePath() []*ir.Entry {
	return []*ir.Entry{
		wireEntry("x0", "user", 50, 400),
		wireEntry("x1", "assistant", 50, 400),
		wireEntry("x2", "user", 50, 400_000),
		wireEntry("x3", "assistant", 50, 400_000),
		wireEntry("x4", "user", 50, 400),
	}
}

func TestTheByteCapSizesTheKeptTailInsteadOfTheTokenCap(t *testing.T) {
	path := wirePath()
	sizes := asm.DeriveSizes(&asm.Built{Entries: path}, 0)
	pre := sizes.WirePrefixes()
	n := len(path)
	kept := func(j int) int { return pre[n] - pre[j] }
	total := kept(0)

	// The token answer for this path, with the byte cap switched off: 25% of a 1000-token window is 250
	// tokens, which the whole path fits inside, so the cut stops at the first user position whose region
	// is worth summarising — entry x2, carrying 400 KB of the payload with it.
	tok := Params{KeepBudgetPct: intp(25), MinSummarizeTokens: 50}
	control, ok := Select(path, sizes, selNCtx, tok)
	if !ok || control.Cut != 2 || control.WireBudget != 0 {
		t.Fatalf("control: cut = %d wireBudget = %d (ok %v, reason %q), want cut 2 and no byte cap",
			control.Cut, control.WireBudget, ok, control.Reason)
	}
	if control.KeptWire != kept(2) {
		t.Errorf("control: KeptWire = %d, want the weight of everything from x2 (%d)", control.KeptWire, kept(2))
	}

	// The same path, the same 25%, counted in bytes: a fifth of what the path weighs. x2 and x3 cannot
	// both stay, so the cut walks to x4 — deeper, still a user boundary, still not a split.
	byteBudget := total / 5
	p := tok
	p.WireBudgetBytes = byteBudget
	plan, ok := Select(path, sizes, selNCtx, p)
	if !ok {
		t.Fatalf("byte cap %d: no plan (reason %q)", byteBudget, plan.Reason)
	}
	if plan.Cut != 4 || plan.Split || plan.Empty {
		t.Errorf("byte cap %d: cut = %d split = %v empty = %v, want cut 4 with a user boundary",
			byteBudget, plan.Cut, plan.Split, plan.Empty)
	}
	if plan.WireBudget != byteBudget {
		t.Errorf("byte cap %d: the plan reports %d, so the reader cannot tell which ruler ran", byteBudget, plan.WireBudget)
	}
	if want := kept(4); plan.KeptWire != want {
		t.Errorf("byte cap %d: kept wire = %d, want the weight of x4 alone (%d)", byteBudget, plan.KeptWire, want)
	}
	if plan.Cut <= control.Cut {
		t.Errorf("the byte cap did not cut deeper than the token cap it replaced: %d vs %d", plan.Cut, control.Cut)
	}

	// A cap the newest entry alone cannot satisfy leaves no tail at all: compact everything. This is the
	// answer a token cap would never give, and the reason the 413 round has headroom afterwards.
	p.WireBudgetBytes = kept(n-1) - 1
	plan, ok = Select(path, sizes, selNCtx, p)
	if !ok || !plan.Empty || plan.Cut != n {
		t.Fatalf("a cap smaller than the newest entry: cut = %d empty = %v (ok %v, reason %q), want the empty tail",
			plan.Cut, plan.Empty, ok, plan.Reason)
	}
}

// TestAFloorStillOutranksTheByteCap keeps the half of the bargain the wall test pinned for MaxWireBytes:
// MinKeptMessages is the user saying how little raw history they will accept, and when the byte cap
// demands less than that, the floor raises the cap outward rather than being traded away. The tail then
// legitimately weighs more than the cap, which is only reportable because the cap is in the plan.
func TestAFloorStillOutranksTheByteCap(t *testing.T) {
	path := wirePath()
	sizes := asm.DeriveSizes(&asm.Built{Entries: path}, 0)
	pre := sizes.WirePrefixes()
	n := len(path)

	p := Params{KeepBudgetPct: intp(25), MinSummarizeTokens: 50, MinKeptMessages: 3}
	p.WireBudgetBytes = (pre[n] - pre[0]) / 5
	plan, ok := Select(path, sizes, selNCtx, p)
	if !ok {
		t.Fatalf("the byte cap refused a round the floor should have saved (reason %q)", plan.Reason)
	}
	if plan.Cut != 2 {
		t.Errorf("cut = %d, want 2: floor 3 demands three kept entries, so the byte cap must be raised outward", plan.Cut)
	}
	if want := pre[n] - pre[2]; plan.KeptWire != want {
		t.Errorf("kept wire = %d, want the floor's own tail %d", plan.KeptWire, want)
	}
	if plan.KeptWire <= p.WireBudgetBytes {
		t.Errorf("the raised cap did not exceed the byte cap, so this case proves nothing: kept %d cap %d",
			plan.KeptWire, p.WireBudgetBytes)
	}
}
