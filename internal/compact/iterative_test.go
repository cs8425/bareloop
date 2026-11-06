package compact

// Tests for the serial summary chain: the invariant that makes it work is that no call ever asks
// for more than the window can carry, that nothing is lost between the parts, that a part never
// starts with an orphan tool result, and that clipping a message never touches the stored entry.

import (
	"strings"
	"testing"

	"bareloop/internal/ir"
)

func txt(id, role string, chars int) *ir.Entry {
	return &ir.Entry{ID: id, Kind: role, Role: role, Content: strings.Repeat("abcd ", chars/5)}
}

func ids(es []*ir.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func region(n int) []*ir.Entry {
	out := make([]*ir.Entry, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, txt(string(rune('a'+i)), role, 500))
	}
	return out
}

// TestAPartIsFilledToTheThresholdPosition is the whole definition of the chain: a part stops where the
// conversation would have triggered a summary anyway (threshold% × n_ctx), and it asks for the same
// answer the single call asks for. The arithmetic this replaces — n_ctx − (max + max/2 + an allowance)
// — reserved a guess at the previous part's summary on every call, which is how a chain ended up with
// parts far smaller than the window offered and an answer budget the model could not think inside.
func TestAPartIsFilledToTheThresholdPosition(t *testing.T) {
	for _, nctx := range []int{2048, 8192, 40960, 262144} {
		p := Params{ThresholdPct: 70, KeepTargetPct: 50, SummaryMaxPct: 70}
		if got, want := p.ChunkFit(nctx), p.TriggerLevel(nctx); got != want {
			t.Fatalf("nctx %d: a part fills to %d, want the threshold position %d", nctx, got, want)
		}
		// The cap on the answer belongs to summaryMax, not to the acceptance cap of the final summary:
		// a summary written over three parts must not be thinner than the one call would have written.
		if got, want := p.ChunkMaxTokens(nctx), p.SummaryMaxTokens(nctx); got != want {
			t.Fatalf("nctx %d: a part may answer with %d tokens, the single call may use %d", nctx, got, want)
		}
	}
	if got := (Params{}).ChunkFit(0); got != 0 {
		t.Fatalf("no window means no plan, got %d", got)
	}
}

// TestNeedsIterativeOnlyWhenTheRequestDoesNotFit fixes the boundary: over n_ctx splits, at or under it
// goes out as one call. The 6924-of-8192 case is the live session that got folded into two serial parts
// for nothing — it fitted, it was sent, it came back with a summary; the region's priced sum is what
// said otherwise, and it is no longer in the room.
func TestNeedsIterativeOnlyWhenTheRequestDoesNotFit(t *testing.T) {
	if NeedsIterative(6924, 8192) {
		t.Fatal("a request that fits must be sent: the chain pays several calls to summarise less")
	}
	if NeedsIterative(8192, 8192) {
		t.Fatal("exactly the window is not over it")
	}
	if !NeedsIterative(8193, 8192) {
		t.Fatal("one token over the window is a request the provider turns down")
	}
	if NeedsIterative(100000, 0) {
		t.Fatal("without a configured window there is no ceiling to exceed")
	}
}

// TestPlanChunksCoversTheRegionInOrder guards the expensive failure: a chunk plan that silently
// drops entries would summarise part of the conversation and forget the rest.
func TestPlanChunksCoversTheRegionInOrder(t *testing.T) {
	p := Params{ThresholdPct: 70, KeepTargetPct: 20, SummaryMaxPct: 70}
	const nctx = 40960
	fit := p.ChunkFit(nctx)
	r := region(300)
	chunks := PlanChunks(r, nil, nctx, p)
	if len(chunks) < 2 {
		t.Fatalf("expected several parts, got %d", len(chunks))
	}
	var got []string
	for i, c := range chunks {
		if len(c.Entries) == 0 {
			t.Fatalf("part %d is empty", i+1)
		}
		if !c.Clipped && RegionTokens(c.Entries, nil) > fit {
			t.Fatalf("part %d carries %d tokens, one request fits %d", i+1, RegionTokens(c.Entries, nil), fit)
		}
		if i > 0 && !LegalBoundary(c.Entries[0]) {
			t.Fatalf("part %d starts with %s: the message that asked for it is in the previous part",
				i+1, c.Entries[0].Role)
		}
		got = append(got, ids(c.Entries)...)
	}
	want := ids(r)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("coverage changed:\n got %v\nwant %v", got, want)
	}
	if len(PlanChunks(nil, nil, nctx, p)) != 0 {
		t.Fatal("an empty region has no parts")
	}
	if len(PlanChunks(region(3), nil, 40960, p)) != 1 {
		t.Fatal("a region that fits is one part")
	}
}

// TestPlanChunksNeverAbandonsOneOversizedEntry: a single pasted file is the case that bricks a
// session, so the planner clips it instead of giving up on it.
func TestPlanChunksNeverAbandonsOneOversizedEntry(t *testing.T) {
	p := Params{ThresholdPct: 70, KeepTargetPct: 20, SummaryMaxPct: 70}
	const nctx = 40960
	fit := p.ChunkFit(nctx)
	huge := txt("huge", "user", fit*8)
	before := huge.Content
	chunks := PlanChunks([]*ir.Entry{txt("a", "user", 100), huge, txt("b", "user", 100)}, nil, nctx, p)
	clipped := 0
	for _, c := range chunks {
		if c.Clipped {
			clipped++
		}
	}
	if clipped != 1 {
		t.Fatalf("clipped parts = %d, want 1", clipped)
	}
	if huge.Content != before {
		t.Fatal("the entry in the log must never be rewritten")
	}
	for _, c := range chunks {
		for _, e := range c.Entries {
			if e.ID == "huge" {
				if !strings.Contains(e.Content, "omitted") {
					t.Fatal("a clipped part must say so, otherwise the model invents the middle")
				}
				if len(e.Content) >= len(before) {
					t.Fatalf("clip did nothing: %d vs %d", len(e.Content), len(before))
				}
				if !strings.HasPrefix(e.Content, "abcd abcd ") {
					t.Fatal("the head of the message must survive the clip")
				}
			}
		}
	}
}

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
}

// TestChunkRequestCarriesThePreviousSummary moved to iterative_cache_test.go, where the shape of a
// chain call is tested as the cache question it is: which bytes a part begins with is the whole reason
// the chain is affordable, and the wording it used to assert on is now only half the contract.

func TestChunkInstructionNote(t *testing.T) {
	if got := ChunkInstructionNote(1, false); got != "" {
		t.Fatalf("one clean part needs no note, got %q", got)
	}
	got := ChunkInstructionNote(4, true)
	if !strings.Contains(got, "4 serial parts") || !strings.Contains(got, "beginning and end") {
		t.Fatalf("note = %q", got)
	}
}
