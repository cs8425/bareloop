package compact

// The four numbers the whole feature rests on are three percentages of n_ctx and one comparison.
// They are also the part most likely to be "improved" into a wrong shape by accident, so they are
// pinned here against the live configuration (a llama.cpp server with n_ctx = 40960) and against
// the big-window value that used to be configured (262144).

import "testing"

func TestBudgetSummaryCapTriggerLevelArePercentagesOfNCtx(t *testing.T) {
	p := Params{ThresholdPct: 70, SummaryCapPct: 70}
	cases := []struct {
		nctx                 int
		trigger, cap, budget int
		what                 string
	}{
		{40960, 28672, 28672, 8192, "the current server window"},
		{262144, 183500, 183500, 52428, "a 256k window"},
		{1000, 700, 700, 200, "a tiny window (integer division rounds down)"},
	}
	for _, c := range cases {
		if got := p.TriggerLevel(c.nctx); got != c.trigger {
			t.Errorf("%s: TriggerLevel(%d) = %d, want %d", c.what, c.nctx, got, c.trigger)
		}
		if got := p.SummaryCap(c.nctx); got != c.cap {
			t.Errorf("%s: SummaryCap(%d) = %d, want %d", c.what, c.nctx, got, c.cap)
		}
		if got := p.Budget(c.nctx); got != c.budget {
			t.Errorf("%s: Budget(%d) = %d, want %d (keepBudget, defaulted)", c.what, c.nctx, got, c.budget)
		}
	}
}

// TestBudgetIsKeepBudgetNotADerivation locks the decoupled §2.1: the tail's budget is keepBudget%
// of the window, the user's own number. It used to be the trigger minus the summary cap, which
// meant tightening the summary's cap silently resized the tail — the two settings answer
// different questions and the test pins that they no longer share arithmetic.
func TestBudgetIsKeepBudgetNotADerivation(t *testing.T) {
	if got := (Params{ThresholdPct: 70}).Budget(40960); got != 8192 {
		t.Fatalf("an absent keepBudget must land on the default 20%%, got %d", got)
	}
	// Same keepBudget, different summaryCap: the budget does not move. And keepBudget moves it
	// alone — the summary's number and the tail's budget answer two questions and share no arithmetic.
	a := Params{ThresholdPct: 70, SummaryCapPct: 50, KeepBudgetPct: intp(30)}
	b := Params{ThresholdPct: 70, SummaryCapPct: 90, KeepBudgetPct: intp(30)}
	if a.Budget(40960) != 12288 || b.Budget(40960) != 12288 {
		t.Fatalf("summaryCap must not move the budget: %d %d", a.Budget(40960), b.Budget(40960))
	}
	if got := (Params{ThresholdPct: 70, SummaryCapPct: 50, KeepBudgetPct: intp(10)}).Budget(40960); got != 4096 {
		t.Fatalf("keepBudget 10 must set the budget to 4096, got %d", got)
	}
	// And the summary's one number answers only to summaryCap — acceptance and max_tokens alike.
	if got := a.SummaryCap(40960); got != 20480 {
		t.Fatalf("summaryCap 50 must cap at 20480, got %d", got)
	}
	if got := (Params{ThresholdPct: 3}).TriggerLevel(40960); got != 1228 {
		t.Fatalf("3%% of 40960 = %d, want 1228", got)
	}
}

// TestKeepBudgetZeroIsTheAnswerNotABrokenValue pins the pointer semantics: an explicit 0 asks for
// the compact-everything tail (head + summary only) and survives Normalise, while a negative —
// which no configuration means — clamps to the same 0. A zero budget does NOT mean "never
// compact": it means the only cut that fits replaces every recorded message (spec §2.1), and
// select_test.go pins what stops it.
func TestKeepBudgetZeroIsTheAnswerNotABrokenValue(t *testing.T) {
	for _, v := range []*int{intp(0), intp(-5)} {
		p := Params{ThresholdPct: 70, KeepBudgetPct: v}
		if got := p.Budget(40960); got != 0 {
			t.Errorf("keepBudget %d: budget = %d, want 0", *v, got)
		}
		if got := *p.Normalise().KeepBudgetPct; got != 0 {
			t.Errorf("keepBudget %d must survive Normalise as 0, got %d", *v, got)
		}
	}
	if got := *(Params{ThresholdPct: 70}).Normalise().KeepBudgetPct; got != 20 {
		t.Fatalf("an absent keepBudget must normalise to the default 20, got %d", got)
	}
}

// TestNothingIsInventedWithoutAnNCtx: no window means no budget, no cap, no trigger level.
func TestNothingIsInventedWithoutAnNCtx(t *testing.T) {
	p := Params{ThresholdPct: 70, SummaryCapPct: 70}
	for _, nctx := range []int{0, -1} {
		if p.TriggerLevel(nctx) != 0 || p.SummaryCap(nctx) != 0 || p.Budget(nctx) != 0 {
			t.Fatalf("n_ctx %d must produce no numbers at all", nctx)
		}
	}
}

// TestShouldTriggerIsStrictAndNeverGuesses covers spec §1.3.
func TestShouldTriggerIsStrictAndNeverGuesses(t *testing.T) {
	p := Params{Auto: true, ThresholdPct: 70}
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
	if d.ThresholdPct != 70 || d.SummaryCapPct != 70 || d.MinKeptUserTurns != 4 || d.MinSummarizeTokens != 2000 ||
		d.MaxAttempts != 3 || d.KeepBudgetPct == nil || *d.KeepBudgetPct != 20 {
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
	if n.ThresholdPct != 70 || n.SummaryCapPct != 70 || n.MinSummarizeTokens != 2000 ||
		n.MaxAttempts != 3 || n.Marker != DefaultMarker || n.Mode != "threshold-target" || n.SummaryMode != ModePrefixStable {
		t.Fatalf("zero value did not normalise to the defaults: %+v", n)
	}
	if n.MinKeptMessages != 0 {
		t.Fatalf("minKeptMessages default must stay 0 (compact everything allowed), got %d", n.MinKeptMessages)
	}
	// why its own case: the two floors and keepBudget are the only fields where 0 is a real answer, so a
	// negative has to land on 0 — landing on the default would enforce an answer the config never asked for.
	neg := Params{MinKeptUserTurns: -3, MinKeptMessages: -3, KeepBudgetPct: intp(-3)}.Normalise()
	if neg.MinKeptUserTurns != 0 || neg.MinKeptMessages != 0 || *neg.KeepBudgetPct != 0 {
		t.Fatalf("negative floors and keepBudget must clamp to 0, got %+v", neg)
	}
	kept := Params{ThresholdPct: 90, SummaryCapPct: 30, KeepBudgetPct: intp(33), MinKeptUserTurns: 0, MinKeptMessages: 6,
		MinSummarizeTokens: 1, MaxAttempts: 1, SummaryMode: ModeTranscript, Marker: "X"}.Normalise()
	if kept.ThresholdPct != 90 || kept.SummaryCapPct != 30 || kept.KeepBudgetPct == nil || *kept.KeepBudgetPct != 33 ||
		kept.MinKeptMessages != 6 || kept.MinSummarizeTokens != 1 ||
		kept.MaxAttempts != 1 || kept.SummaryMode != ModeTranscript || kept.Marker != "X" {
		t.Fatalf("explicit values were overwritten: %+v", kept)
	}
	if got := (Params{SummaryMode: "gibberish"}).Normalise().SummaryMode; got != ModePrefixStable {
		t.Fatalf("unknown summary mode = %q, want the default", got)
	}
	if got := (Params{MaxAttempts: -1}).Normalise().MaxAttempts; got != -1 {
		t.Fatalf("maxAttempts -1 means forever, got %d", got)
	}
}
