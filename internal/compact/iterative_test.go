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

// TestChunkFitLeavesRoomForTheAnswer is the whole point of the chain: instruction + partial summary
// + part + max_tokens must stay inside n_ctx, otherwise the summary call dies exactly like the turn.
func TestChunkFitLeavesRoomForTheAnswer(t *testing.T) {
	for _, nctx := range []int{2048, 8192, 40960, 262144} {
		for _, keep := range []int{10, 20, 50} {
			p := Params{ThresholdPct: 70, KeepTargetPct: keep, SummaryMaxPct: 70}
			fit, out := p.ChunkFit(nctx), p.ChunkMaxTokens(nctx)
			if fit <= 0 {
				t.Fatalf("nctx %d keep %d: fit %d must be positive", nctx, keep, fit)
			}
			if used := fit + out + out/2 + InstructionAllowance; used > nctx {
				t.Fatalf("nctx %d keep %d: one request could cost %d tokens (fit %d + answer %d + partial %d + instruction %d)",
					nctx, keep, used, fit, out, out/2, InstructionAllowance)
			}
		}
	}
	if got := (Params{}).ChunkFit(0); got != 0 {
		t.Fatalf("no window means no plan, got %d", got)
	}
}

func TestNeedsIterativeOnlyWhenItReallyDoesNotFit(t *testing.T) {
	p := Params{ThresholdPct: 70, KeepTargetPct: 20, SummaryMaxPct: 70}
	fit := p.ChunkFit(40960)
	small := region(4)
	if RegionTokens(small, nil) > fit {
		t.Fatalf("fixture is too big to test the negative case: %d > %d", RegionTokens(small, nil), fit)
	}
	if NeedsIterative(small, nil, 40960, p) {
		t.Fatal("a region that fits must not switch to the chain (it would pay several calls for nothing)")
	}
	if !NeedsIterative(region(400), nil, 40960, p) {
		t.Fatal("a region larger than one request must switch to the chain")
	}
	if NeedsIterative(region(400), nil, 0, p) {
		t.Fatal("without n_ctx there is no plan")
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

func TestChunkRequestCarriesThePreviousSummary(t *testing.T) {
	p := Params{ThresholdPct: 70, KeepTargetPct: 20, SummaryMaxPct: 70, Temperature: 0.2, Marker: DefaultMarker}
	call := p.ChunkRequest(40960, region(2), "", 1, 3, false, "")
	if len(call.Messages) != 2 || call.Messages[0].Role != "system" || call.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v", call.Messages)
	}
	sys, _ := call.Messages[0].Content.(string)
	if !strings.Contains(sys, "part 1 of 3") {
		t.Fatalf("the model must know it is reading part of a longer conversation: %s", sys)
	}
	if call.MaxTokens != p.ChunkMaxTokens(40960) {
		t.Fatalf("max_tokens = %d, want the acceptance cap %d", call.MaxTokens, p.ChunkMaxTokens(40960))
	}
	next := p.ChunkRequest(40960, region(2), "PARTIAL SUMMARY SO FAR", 2, 3, false, "keep the file names")
	body, _ := next.Messages[1].Content.(string)
	if !strings.Contains(body, "PARTIAL SUMMARY SO FAR") {
		t.Fatal("the chain only works if the previous answer is handed to the next call")
	}
	if !strings.Contains(body, "New part of the conversation") {
		t.Fatal("the model must be able to tell the partial summary from the new part")
	}
	sys2, _ := next.Messages[0].Content.(string)
	if !strings.Contains(sys2, "keep the file names") {
		t.Fatal("the user's instruction must reach every part")
	}
	if single := p.ChunkRequest(40960, region(2), "", 1, 1, false, ""); strings.Contains(single.Messages[0].Content.(string), "part 1 of 1") {
		t.Fatal("a single part needs no counter")
	}
}

func TestChunkInstructionNote(t *testing.T) {
	if got := ChunkInstructionNote(1, false); got != "" {
		t.Fatalf("one clean part needs no note, got %q", got)
	}
	got := ChunkInstructionNote(4, true)
	if !strings.Contains(got, "4 serial parts") || !strings.Contains(got, "beginning and end") {
		t.Fatalf("note = %q", got)
	}
}
