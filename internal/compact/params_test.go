package compact

// The four numbers the whole feature rests on are three percentages of n_ctx and one comparison.
// They are also the part most likely to be "improved" into a wrong shape by accident, so they are
// pinned here against the live configuration (a llama.cpp server with n_ctx = 40960) and against
// the big-window value that used to be configured (262144).

import "testing"

func TestBudgetSummaryCapTriggerLevelArePercentagesOfNCtx(t *testing.T) {
	p := Params{ThresholdPct: 70, KeepTargetPct: 50, SummaryMaxPct: 70}
	cases := []struct {
		nctx                     int
		trigger, cap, budget, mx int
		what                     string
	}{
		{40960, 28672, 20480, 8192, 28672, "the current server window"},
		{262144, 183500, 131072, 52428, 183500, "a 256k window"},
		{1000, 700, 500, 200, 700, "a tiny window (integer division rounds down)"},
	}
	for _, c := range cases {
		if got := p.TriggerLevel(c.nctx); got != c.trigger {
			t.Errorf("%s: TriggerLevel(%d) = %d, want %d", c.what, c.nctx, got, c.trigger)
		}
		if got := p.SummaryCap(c.nctx); got != c.cap {
			t.Errorf("%s: SummaryCap(%d) = %d, want %d", c.what, c.nctx, got, c.cap)
		}
		if got := p.Budget(c.nctx); got != c.budget {
			t.Errorf("%s: Budget(%d) = %d, want %d (threshold - keepTarget)", c.what, c.nctx, got, c.budget)
		}
		if got := p.SummaryMaxTokens(c.nctx); got != c.mx {
			t.Errorf("%s: SummaryMaxTokens(%d) = %d, want %d", c.what, c.nctx, got, c.mx)
		}
	}
}

// TestBudgetIsTheDifferenceNotAReservation locks spec §2.1: the budget is exactly
// (threshold - keepTarget)% of the window. Adding a hidden reservation on top makes the observed
// kept size stop matching what the user set, which is how this feature becomes un-debuggable.
func TestBudgetIsTheDifferenceNotAReservation(t *testing.T) {
	if got := (Params{ThresholdPct: 70, KeepTargetPct: 20}).Budget(40960); got != 20480 {
		t.Fatalf("70/20 budget = %d, want 20480", got)
	}
	if got := (Params{ThresholdPct: 70, KeepTargetPct: 20}).SummaryCap(40960); got != 8192 {
		t.Fatalf("70/20 summary cap = %d, want 8192", got)
	}
	if got := (Params{ThresholdPct: 3, KeepTargetPct: 1}).TriggerLevel(40960); got != 1228 {
		t.Fatalf("3%% of 40960 = %d, want 1228", got)
	}
}

// TestThresholdNotAboveKeepTargetBudgetsZero pins the arithmetic of a configuration the settings
// dialog lets a user reach. A zero budget does NOT mean "never compact": it means the only cut
// that fits replaces every recorded message (spec §2.1), and select_test.go pins what stops it.
func TestThresholdNotAboveKeepTargetBudgetsZero(t *testing.T) {
	for _, p := range []Params{{ThresholdPct: 50, KeepTargetPct: 50}, {ThresholdPct: 40, KeepTargetPct: 50}} {
		if got := p.Budget(40960); got != 0 {
			t.Errorf("threshold %d <= keepTarget %d: budget = %d, want 0", p.ThresholdPct, p.KeepTargetPct, got)
		}
	}
}

// TestNothingIsInventedWithoutAnNCtx: no window means no budget, no cap, no trigger level.
func TestNothingIsInventedWithoutAnNCtx(t *testing.T) {
	p := Params{ThresholdPct: 70, KeepTargetPct: 50, SummaryMaxPct: 70}
	for _, nctx := range []int{0, -1} {
		if p.TriggerLevel(nctx) != 0 || p.SummaryCap(nctx) != 0 || p.Budget(nctx) != 0 || p.SummaryMaxTokens(nctx) != 0 {
			t.Fatalf("n_ctx %d must produce no numbers at all", nctx)
		}
	}
}

// TestShouldTriggerIsStrictAndNeverGuesses covers spec §1.3.
func TestShouldTriggerIsStrictAndNeverGuesses(t *testing.T) {
	p := Params{Auto: true, ThresholdPct: 70, KeepTargetPct: 50}
	const nctx = 40960
	level := p.TriggerLevel(nctx) // 28672
	cases := []struct {
		used int
		good bool
		auto bool
		ctx  int
		want bool
		what string
	}{
		{level - 1, true, true, nctx, false, "below the trigger"},
		{level, true, true, nctx, false, "exactly at the trigger (must be strictly above)"},
		{level + 1, true, true, nctx, true, "one over the trigger"},
		{10 * nctx, true, false, nctx, false, "auto compaction is off (T1)"},
		{10 * nctx, false, true, nctx, false, "no real measurement: not guessing"},
		{10 * nctx, true, true, 0, false, "n_ctx unknown"},
	}
	for _, c := range cases {
		pp := p
		pp.Auto = c.auto
		if got := pp.ShouldTrigger(c.ctx, c.used, c.good); got != c.want {
			t.Errorf("%s: ShouldTrigger(%d, %d, %v) = %v, want %v", c.what, c.ctx, c.used, c.good, got, c.want)
		}
	}
}

// TestInLoopIsOnUnlessExplicitlyDisabled covers the pointer semantics of T2: a missing key is not
// a false, so a config that only sets auto keeps in-loop compaction available.
func TestInLoopIsOnUnlessExplicitlyDisabled(t *testing.T) {
	yes := true
	no := false
	if !(Params{}).UseInLoop() {
		t.Fatal("an omitted inLoop must mean on")
	}
	if !(Params{InLoop: &yes}).UseInLoop() {
		t.Fatal("inLoop true must be on")
	}
	if (Params{InLoop: &no}).UseInLoop() {
		t.Fatal("an explicit inLoop false must be off")
	}
}

// TestDefaultsMatchTheDocumentedTable (spec §7): auto is off, in-loop is on, and the marker is the
// one that later tells a summary entry apart in the log.
func TestDefaultsMatchTheDocumentedTable(t *testing.T) {
	d := DefaultParams()
	if d.Auto {
		t.Fatal("spec §1.1: automatic compaction is off by default")
	}
	if !d.UseInLoop() {
		t.Fatal("spec §1.1: in-loop compaction is on by default")
	}
	if d.ThresholdPct != 70 || d.KeepTargetPct != 50 || d.MinKeptUserTurns != 4 || d.MinSummarizeTokens != 2000 ||
		d.SummaryMaxPct != 70 || d.MaxAttempts != 3 {
		t.Fatalf("defaults drifted: %+v", d)
	}
	if d.Marker != DefaultMarker || DefaultMarker == "" {
		t.Fatalf("marker = %q", d.Marker)
	}
	if d.SummaryMode != ModePrefixStable {
		t.Fatalf("summary mode = %q, want %q", d.SummaryMode, ModePrefixStable)
	}
}

// TestNormaliseFillsBlanksAndKeepsChoices pins Normalise's two halves: a field whose 0 would be
// destructive falls back to its default, while a field whose 0 is the answer — the two floors — keeps
// it, and a negative on those clamps to 0 rather than to the shipped default.
func TestNormaliseFillsBlanksAndKeepsChoices(t *testing.T) {
	n := Params{}.Normalise()
	if n.ThresholdPct != 70 || n.KeepTargetPct != 50 || n.MinSummarizeTokens != 2000 || n.SummaryMaxPct != 70 ||
		n.MaxAttempts != 3 || n.Marker != DefaultMarker || n.Mode != "threshold-target" || n.SummaryMode != ModePrefixStable {
		t.Fatalf("zero value did not normalise to the defaults: %+v", n)
	}
	if n.MinKeptMessages != 0 {
		t.Fatalf("minKeptMessages default must stay 0 (compact everything allowed), got %d", n.MinKeptMessages)
	}
	// why its own case: the two floors are the only fields where 0 is a real answer, so a negative has to
	// land on 0 — landing on the default would enforce a floor the config never asked for.
	neg := Params{MinKeptUserTurns: -3, MinKeptMessages: -3}.Normalise()
	if neg.MinKeptUserTurns != 0 || neg.MinKeptMessages != 0 {
		t.Fatalf("negative floors must clamp to 0 (no floor), got %+v", neg)
	}
	kept := Params{ThresholdPct: 90, KeepTargetPct: 10, MinKeptUserTurns: 0, MinKeptMessages: 6,
		MinSummarizeTokens: 1, SummaryMaxPct: 30, MaxAttempts: 1, SummaryMode: ModeTranscript, Marker: "X"}.Normalise()
	if kept.ThresholdPct != 90 || kept.KeepTargetPct != 10 || kept.MinKeptMessages != 6 || kept.MinSummarizeTokens != 1 ||
		kept.SummaryMaxPct != 30 || kept.MaxAttempts != 1 || kept.SummaryMode != ModeTranscript || kept.Marker != "X" {
		t.Fatalf("explicit values were overwritten: %+v", kept)
	}
	if got := (Params{SummaryMode: "gibberish"}).Normalise().SummaryMode; got != ModePrefixStable {
		t.Fatalf("unknown summary mode = %q, want the default", got)
	}
	if got := (Params{MaxAttempts: -1}).Normalise().MaxAttempts; got != -1 {
		t.Fatalf("maxAttempts -1 means forever, got %d", got)
	}
}
