package compact

// The kept tail is max(budget, the newest MinKeptMessages) — spec §2.1. budget is keepBudget%
// of the window and MinKeptMessages is a floor, so the two can demand opposite things from
// the same tail, and these cases pin who wins: the floor, rounded outward to a legal boundary, with
// the plan still reporting the budget it exceeded.
//
// There is no manual argument: an automatic and a manual compaction of the same path get the same
// plan, and a test cannot accidentally pin the two apart.

import (
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

const selNCtx = 1000

// selPath is four user messages of exactly 100 tokens each (txt makes content of `chars` bytes and
// an estimate is bytes/4), so the path totals 400 tokens, every boundary in it is legal, and the
// only variables left are the cap and the floors.
func selPath() []*ir.Entry {
	path := make([]*ir.Entry, 4)
	for i := range path {
		path[i] = txt("m"+string(rune('0'+i)), "user", 400)
	}
	return path
}

// selToolPath is [user user tool user user]: asking to keep 3 of them puts the cut on a tool
// result, which is where "round outward, never inward" is decided.
func selToolPath() []*ir.Entry {
	roles := []string{"user", "user", "tool", "user", "user"}
	path := make([]*ir.Entry, len(roles))
	for i, role := range roles {
		path[i] = txt("t"+string(rune('0'+i)), role, 400)
	}
	return path
}

// selNoBoundaryPath is [tool tool user user], where no legal boundary exists far enough back to
// honour a floor of 3 — the cut does not exist, and Select must say so rather than pick one.
func selNoBoundaryPath() []*ir.Entry {
	roles := []string{"tool", "tool", "user", "user"}
	path := make([]*ir.Entry, len(roles))
	for i, role := range roles {
		path[i] = txt("n"+string(rune('0'+i)), role, 400)
	}
	return path
}

// selSizes goes through the assembler's own sizing code rather than inventing a table.
func selSizes(path []*ir.Entry) *asm.Sizes {
	return asm.DeriveSizes(&asm.Built{Entries: path}, 0)
}

func TestSelectTailIsTheGreaterOfCapAndFloor(t *testing.T) {
	cases := []struct {
		threshold, keepBudget, minSummarize, minUserTurns, minKeptMsgs int
		ok                                                             bool
		cut, kept, summarised, replaced, budget, keptUsers             int
		empty, split                                                   bool
		boundary                                                       string
		reason                                                         Reason
		what                                                           string
	}{
		// A keepBudget of 0 caps the tail at nothing. Every case below lives on that contradiction.
		{50, 0, 50, 2, 0, true, 4, 0, 400, 4, 0, 0, true, true, "", ReasonOK,
			"floor 0: nothing demands a tail, so the budget gets an empty one (compact everything)"},
		{50, 0, 50, 0, 1, true, 3, 100, 300, 3, 0, 1, false, false, "m3", ReasonOK,
			"floor 1 beats the 0 budget: tail 100 tokens over a budget of 0, reported as such"},
		{50, 0, 50, 2, 1, true, 3, 100, 300, 3, 0, 1, false, false, "m3", ReasonOK,
			"minKeptUserTurns pushes the cut into phase 2 without calling it a split: m3 is still a user boundary"},
		{50, 0, 50, 0, 2, true, 2, 200, 200, 2, 0, 2, false, false, "m2", ReasonOK,
			"floor 2: the budget is raised to the cost of exactly two messages"},
		{50, 0, 50, 0, 4, false, 0, 0, 0, 0, 0, 0, false, false, "", ReasonNoBoundary,
			"floor == the whole path means summarise nothing, so nothing runs"},
		{50, 0, 50, 0, 9, false, 0, 0, 0, 0, 0, 0, false, false, "", ReasonNoBoundary,
			"floor beyond the path is the same refusal, not a panic and not a full keep"},
		{50, 0, 350, 0, 1, false, 0, 0, 0, 0, 0, 0, false, false, "", ReasonNoBoundary,
			"the floor raises the tail cap, it does not lower minSummarizeTokens: a region worth 300 of 400 still refuses at 350"},
		{50, 0, 401, 2, 0, false, 0, 0, 0, 0, 0, 0, false, false, "", ReasonTooSmall,
			"the whole path is below minSummarizeTokens: the reason names the knob to lower, not a boundary"},
		// The 350/401 pair is the boundary between the two refusals: 350 still leaves one cut worth having
		// (so the floors and the boundaries are what refuse), 401 leaves none (so it is the knob to change).
		// A budget that already fits more than the floor must keep the budget's answer, not shrink to N.
		{65, 25, 50, 2, 0, true, 2, 200, 200, 2, 250, 2, false, false, "m2", ReasonOK,
			"control: a real budget cuts on a turn boundary (unchanged behaviour)"},
		{65, 25, 50, 2, 1, true, 2, 200, 200, 2, 250, 2, false, false, "m2", ReasonOK,
			"floor 1 under a 250-token budget changes nothing: the floor is a floor, not a target"},
		// Where the floor cut is not a legal boundary it moves outward — selToolPath covers that below.
	}
	for _, c := range cases {
		p := Params{
			ThresholdPct: c.threshold, KeepBudgetPct: intp(c.keepBudget),
			MinSummarizeTokens: c.minSummarize, MinKeptUserTurns: c.minUserTurns,
			MinKeptMessages: c.minKeptMsgs,
		}
		path := selPath()
		plan, ok := Select(path, selSizes(path), selNCtx, p)
		if plan == nil {
			t.Fatalf("%s: nil plan", c.what)
		}
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v (reason %q)", c.what, ok, c.ok, plan.Reason)
		}
		if !c.ok {
			if plan.Reason != c.reason {
				t.Errorf("%s: reason = %q, want %q", c.what, plan.Reason, c.reason)
			}
			continue
		}
		if plan.Reason != ReasonOK {
			t.Errorf("%s: usable plan carries reason %q", c.what, plan.Reason)
		}
		if plan.Cut != c.cut || plan.Replaced != c.replaced {
			t.Errorf("%s: cut = %d / replaced = %d, want %d / %d", c.what, plan.Cut, plan.Replaced, c.cut, c.replaced)
		}
		if plan.KeptTokens != c.kept || plan.SummarizeTokens != c.summarised {
			t.Errorf("%s: kept = %d, summarised = %d; want %d / %d", c.what, plan.KeptTokens, plan.SummarizeTokens, c.kept, c.summarised)
		}
		if plan.KeptUserTurns != c.keptUsers {
			t.Errorf("%s: kept user turns = %d, want %d", c.what, plan.KeptUserTurns, c.keptUsers)
		}
		if plan.Empty != c.empty || plan.Split != c.split {
			t.Errorf("%s: empty = %v, split = %v; want %v / %v", c.what, plan.Empty, plan.Split, c.empty, c.split)
		}
		// Budget is the configured cap, not the raised one: KeptTokens > Budget is how the log and the
		// inspector show that the floor is what the tail is made of.
		if plan.Budget != c.budget {
			t.Errorf("%s: budget = %d, want %d", c.what, plan.Budget, c.budget)
		}
		switch {
		case c.boundary == "" && plan.BoundaryID != nil:
			t.Errorf("%s: boundary = %q, want the field absent", c.what, *plan.BoundaryID)
		case c.boundary != "" && plan.BoundaryID == nil:
			t.Errorf("%s: boundary absent, want %q", c.what, c.boundary)
		case c.boundary != "" && *plan.BoundaryID != c.boundary:
			t.Errorf("%s: boundary = %q, want %q", c.what, *plan.BoundaryID, c.boundary)
		}
	}
}

// TestSelectFloorCutsAroundToolSpans runs the same search on paths where the floor's own cut is not
// a legal boundary: it must move outward (keeping more than N) or refuse — moving inward would keep
// fewer than the floor asked for, and inventing a boundary inside a tool span is not on the table.
func TestSelectFloorCutsAroundToolSpans(t *testing.T) {
	p := Params{ThresholdPct: 50, KeepBudgetPct: intp(0), MinSummarizeTokens: 50, MinKeptMessages: 3}

	// [user user tool user user]: keeping 3 would cut before a tool result, so 4 stay.
	path := selToolPath()
	plan, ok := Select(path, selSizes(path), selNCtx, p)
	if !ok {
		t.Fatalf("tool span: ok = false, want a cut that keeps 4 (reason %q)", plan.Reason)
	}
	if plan.Cut != 1 || plan.Replaced != 1 {
		t.Errorf("tool span: cut = %d, replaced = %d, want 1 / 1", plan.Cut, plan.Replaced)
	}
	if plan.KeptTokens != 400 || plan.SummarizeTokens != 100 {
		t.Errorf("tool span: kept = %d, summarised = %d, want 400 / 100", plan.KeptTokens, plan.SummarizeTokens)
	}
	if plan.KeptUserTurns != 3 {
		t.Errorf("tool span: kept user turns = %d, want 3", plan.KeptUserTurns)
	}
	if plan.Budget != 0 {
		t.Errorf("tool span: budget = %d, want the configured 0 so kept > budget stays visible", plan.Budget)
	}
	if plan.Split || plan.Empty {
		t.Errorf("tool span: split = %v, empty = %v, want false / false (the cut is a user boundary)", plan.Split, plan.Empty)
	}
	if plan.BoundaryID == nil || *plan.BoundaryID != "t1" {
		t.Errorf("tool span: boundary = %v, want t1", plan.BoundaryID)
	}

	// [tool tool user user]: no legal boundary leaves 3 entries, so the cut does not exist.
	path = selNoBoundaryPath()
	if plan, ok := Select(path, selSizes(path), selNCtx, p); ok || plan.Reason != ReasonNoBoundary {
		t.Errorf("no legal boundary: ok = %v reason = %q, want false / %q", ok, plan.Reason, ReasonNoBoundary)
	}
}

// TestSelectRefusesWithoutAWindowOrAPath pins the two refusals that no floor can rescue: they are
// the only cases where nothing can run at all, manual included (spec §3.10).
func TestSelectRefusesWithoutAWindowOrAPath(t *testing.T) {
	p := Params{ThresholdPct: 50, MinSummarizeTokens: 50}
	path := selPath()

	if plan, ok := Select(path, selSizes(path), 0, p); ok || plan.Reason != ReasonNCTXUnknown {
		t.Errorf("no n_ctx: ok = %v reason = %q, want false / %q", ok, plan.Reason, ReasonNCTXUnknown)
	}
	if plan, ok := Select(nil, selSizes(nil), selNCtx, p); ok || plan.Reason != ReasonTooSmall {
		t.Errorf("empty path: ok = %v reason = %q, want false / %q", ok, plan.Reason, ReasonTooSmall)
	}
}

// TestFloorCutRoundsOutward covers the helper directly, including the two edges the table above can
// only reach through the whole search.
func TestFloorCutRoundsOutward(t *testing.T) {
	path := selToolPath() // [user user tool user user]
	if got := floorCut(path, 1); got != 4 {
		t.Errorf("keep 1: cut = %d, want 4 (the last entry is a user message)", got)
	}
	if got := floorCut(path, 3); got != 1 {
		t.Errorf("keep 3: cut = %d, want 1 (outward past the tool result, never inward)", got)
	}
	if got := floorCut(path, 0); got != 5 {
		t.Errorf("keep 0: cut = %d, want 5 (len(path): the empty tail is the answer)", got)
	}
	if got := floorCut(selNoBoundaryPath(), 3); got != -1 {
		t.Errorf("no legal boundary that far back: cut = %d, want -1", got)
	}
}

// TestTheByteBoundOnlyPushesTheCutDeeper is the wall's only entry into the plan, and these cases pin
// what it can and cannot do. The budget speaks tokens, the wall speaks bytes, and the two disagree
// loudest on images — a few hundred tokens for several megabytes — so a path of screenshots can be
// inside its token budget while over its ceiling. MaxWireBytes may then push the cut deeper; it may
// not pull one shallower, and it never outranks a floor. selPath's entries are identical, so the
// numbers below are read out of the size table rather than written by hand.
func TestTheByteBoundOnlyPushesTheCutDeeper(t *testing.T) {
	path := selPath()
	sizes := selSizes(path)
	pre := sizes.WirePrefixes()
	n := len(path)
	kept := func(j int) int { return pre[n] - pre[j] }

	// The token answer for this path: a 250-token tail cap and no floors past minSummarizeTokens.
	tok := Params{ThresholdPct: 65, KeepBudgetPct: intp(25), MinSummarizeTokens: 50}
	if plan, ok := Select(path, sizes, selNCtx, tok); !ok || plan.Cut != 2 {
		t.Fatalf("control: token-only cut = %d (ok %v, reason %q), want 2", plan.Cut, ok, plan.Reason)
	}

	cases := []struct {
		maxWire int
		cut     int
		empty   bool
		what    string
	}{
		{0, 2, false, "no ceiling known: bytes have no vote and the token answer stands"},
		{1 << 30, 2, false, "a wall with room to spare does not move the token answer"},
		{kept(2), 2, false, "a tail weighing exactly the budget fits: the bound is over, not at-or-over"},
		{kept(2) - 1, 3, false, "one byte over and the cut goes a position deeper"},
		{kept(3) - 1, 4, true, "a wall nothing but an empty tail can hold compact everything"},
		{1, n, true, "a wall no message can cross is still a cut, not a refusal"},
	}
	for _, c := range cases {
		p := tok
		p.MaxWireBytes = c.maxWire
		plan, ok := Select(path, sizes, selNCtx, p)
		if !ok || plan.Cut != c.cut || plan.Empty != c.empty {
			t.Errorf("%s: cut = %d empty = %v (ok %v, reason %q), want cut %d empty %v",
				c.what, plan.Cut, plan.Empty, ok, plan.Reason, c.cut, c.empty)
		}
		if ok && plan.Cut < 2 {
			t.Errorf("%s: the byte bound pulled the cut shallower than the token answer, which it may not do", c.what)
		}
	}
}

// TestAFloorStillOutranksTheByteBound pins the other half of the bargain: the wall asks for a deeper
// cut, but MinKeptMessages is the user saying how little raw history they will accept, and a request
// that cannot be made to fit without breaking that still has to answer with the floors' refusal
// rather than quietly trading the floor for bytes.
func TestAFloorStillOutranksTheByteBound(t *testing.T) {
	path := selPath()
	sizes := selSizes(path)
	pre := sizes.WirePrefixes()
	n := len(path)

	p := Params{ThresholdPct: 65, KeepBudgetPct: intp(25), MinSummarizeTokens: 50, MinKeptMessages: 3}
	// Control: without the wall this path compacts at the floor's own answer — the floor raises the tail
	// cap to the cost of three entries, and the search takes the shallowest legal cut that fits it, so the
	// boundary sits at 1. Whatever the case below then refuses can only be refused by the floor meeting
	// the wall, not by the floor alone.
	if plan, ok := Select(path, sizes, selNCtx, p); !ok || plan.Cut != 1 {
		t.Fatalf("control: floor 3 without a wall should cut at 1, got %d (ok %v, reason %q)",
			plan.Cut, ok, plan.Reason)
	}
	p.MaxWireBytes = pre[n] - pre[n-1] - 1 // fits only if the tail is emptied, which floor 3 forbids
	plan, ok := Select(path, sizes, selNCtx, p)
	if ok {
		t.Fatalf("a floor of 3 was walked past to satisfy bytes: cut = %d empty = %v", plan.Cut, plan.Empty)
	}
	if plan.Reason != ReasonNoBoundary {
		t.Fatalf("refusal should come from the floors, got reason %q", plan.Reason)
	}
}
