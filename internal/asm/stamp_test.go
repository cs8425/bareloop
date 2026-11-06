package asm

import (
	"strings"
	"testing"

	"bareloop/internal/ir"
)

// The measuring tests: what a server counted becomes a number on the entry it counted, and nothing
// else. The ruler that reads those numbers is tested in internal/compact.

func stampedTxt(id, kind string, chars int) *ir.Entry {
	return &ir.Entry{ID: id, Kind: kind, Role: kind, Content: strings.Repeat("x", chars)}
}

// countedReply is a reply a server counted: it was handed `prompt` tokens, `cache` of them reused, and
// it answered with `completion`. Everything between one counted reply and the next has then been
// priced by the server itself, which is the only per-entry number anybody is ever going to get.
func countedReply(id string, prompt, cache, completion int) *ir.Entry {
	e := stampedTxt(id, "assistant", 500)
	e.Usage = &ir.Usage{PromptTokens: prompt, CacheTokens: cache, CompletionTokens: completion, HasPrompt: true, HasCompletion: true}
	return e
}

// TestStampSplitsMeasuredGrowthBetweenTheEntriesThatCausedIt is the live failure, reduced to five
// entries: a tool result that estimates at ten thousand tokens inside a stretch of conversation the
// provider measured at three hundred. Whatever number a part is cut against, it has to be the one that
// was counted — and the growth belongs to the entries between the two replies, not to whichever one
// happens to be last, which is what a boundary table does and what makes one entry into a part of its
// own while the rest measure nothing.
func TestStampSplitsMeasuredGrowthBetweenTheEntriesThatCausedIt(t *testing.T) {
	tool := stampedTxt("t1", "tool", 40000)
	late := stampedTxt("u2", "user", 500)
	all := []*ir.Entry{
		stampedTxt("u1", "user", 500),
		countedReply("a1", 1000, 900, 200),
		tool,
		late,
		countedReply("a2", 1500, 1000, 50),
	}
	if got := Stamp(all, 0, 0); got == 0 {
		t.Fatal("nothing was measured on a path the server had counted twice")
	}

	// A reply costs what it generated: the tokens it will be charged for when it is sent back.
	if got := all[1].Tokens; got != 200 {
		t.Fatalf("the reply costs %d tokens, want the 200 it generated", got)
	}
	// The two entries between the counted replies are worth the 300 tokens the growth measured at —
	// both of them, in the proportion they look like they cost.
	grown := tool.Tokens + late.Tokens
	if grown != 300 {
		t.Fatalf("the entries between the counted replies are worth %d tokens, want the 300 that were counted", grown)
	}
	if late.Tokens < 1 {
		t.Fatalf("the growth landed on one entry after all: %d and %d", tool.Tokens, late.Tokens)
	}
	if tool.Tokens >= 300 {
		t.Fatalf("the huge tool result was handed the whole growth (%d of 300)", tool.Tokens)
	}
	if est := Estimate(tool, 0); tool.Tokens >= est {
		t.Fatalf("the entry still costs its estimate: measured %d, estimated %d", tool.Tokens, est)
	}
	// And the whole path adds up to what the server counted for it: its last prompt plus that reply.
	total := 0
	for _, e := range all {
		total += e.Tokens
	}
	if want := 1500 + 50; total != want {
		t.Fatalf("the measured path adds up to %d tokens, want the %d the server counted for it", total, want)
	}
}

// TestStampLeavesTheUnsentTailUnmeasured: the entries no request has ever carried have no number, and
// an invented one is worse than the estimate — the unsent tail is usually the huge message that is the
// reason a summary is being asked for, and measuring it by scaling down to the last request's total
// would price it at nothing.
func TestStampLeavesTheUnsentTailUnmeasured(t *testing.T) {
	tail := stampedTxt("u9", "user", 40000)
	all := []*ir.Entry{stampedTxt("u1", "user", 500), countedReply("a1", 1000, 900, 100), tail}
	Stamp(all, 0, 0)
	if tail.Tokens != 0 {
		t.Fatalf("the unsent tail was given a number: %d", tail.Tokens)
	}
	if got, want := Estimate(tail, 0), 40000/4; got < want/2 {
		t.Fatalf("the estimate that stands in for it is %d, near %d", got, want)
	}
}

// TestStampIgnoresAnArithmeticThatNoLongerDescribesThePath: prompt counts are only a difference when
// they are about the same conversation. Time travel, a deleted entry or a rollback makes the second
// number smaller than the first, and the result is not a size — it is a subtraction between two
// unrelated requests, and a negative one is the worst possible size for a part.
func TestStampIgnoresAnArithmeticThatNoLongerDescribesThePath(t *testing.T) {
	mid := stampedTxt("t1", "tool", 4000)
	after := countedReply("a2", 500, 0, 20)
	all := []*ir.Entry{stampedTxt("u1", "user", 500), countedReply("a1", 1000, 900, 100), mid, after}
	Stamp(all, 0, 0)
	if mid.Tokens != 0 {
		t.Fatalf("a stretch whose arithmetic went negative was measured at %d tokens", mid.Tokens)
	}
	if after.Tokens != 20 {
		t.Fatalf("a reply costs what it generated even after a rollback: %d", after.Tokens)
	}
}

// TestStampNeverMeasuresAcrossACompaction: the first request after a compaction was counted against a
// payload of summary plus kept messages, so its prompt says nothing about the conversation before the
// record — and the summary itself is not a mystery either, because the record carries what the
// summariser was charged for.
func TestStampNeverMeasuresAcrossACompaction(t *testing.T) {
	summary := &ir.Entry{
		ID: "cmp", Kind: ir.KindCompaction, Role: "user", Content: strings.Repeat("s", 4000),
		Compaction: &ir.CompactionMeta{SummarySize: 700},
	}
	tool := stampedTxt("t1", "tool", 4000)
	all := []*ir.Entry{
		stampedTxt("u1", "user", 500),
		countedReply("a1", 1000, 900, 100),
		summary,
		tool,
		countedReply("a2", 900, 0, 20), // smaller than the prompt before: a different payload entirely
	}
	Stamp(all, 0, 0)

	if summary.Tokens != 700 {
		t.Fatalf("the summary was priced at %d tokens, want the 700 the summariser was charged for it", summary.Tokens)
	}
	if got := all[0].Tokens; got != 1000 {
		t.Fatalf("the entries before the record lost their measurement: %d", got)
	}
	if tool.Tokens != 0 {
		t.Fatalf("the record was subtracted across: the tool was measured at %d tokens", tool.Tokens)
	}
	if all[4].Tokens != 20 {
		t.Fatalf("a reply is still its own answer across a compaction: %d", all[4].Tokens)
	}
}

// TestStampMeasuresOnlyWhatIsNew is the ordinary case, turn by turn: the numbers already on the entries
// are the ones a server counted, so the next request's difference has to be what it actually added —
// not the whole growth again, which would price the same tokens twice every time the path is assembled.
func TestStampMeasuresOnlyWhatIsNew(t *testing.T) {
	all := []*ir.Entry{stampedTxt("u1", "user", 500), countedReply("a1", 1000, 900, 100)}
	Stamp(all, 0, 0)
	first := all[0].Tokens
	if first == 0 {
		t.Fatal("the first request measured nothing")
	}

	// The next turn arrives, and with it a prompt count that says the two new entries cost 300.
	tool := stampedTxt("t1", "tool", 4000)
	user := stampedTxt("u2", "user", 500)
	all = append(all, tool, user, countedReply("a2", 1400, 1000, 30))
	Stamp(all, 0, 0)

	if all[0].Tokens != first {
		t.Fatalf("an entry measured last turn was re-measured: %d, was %d", all[0].Tokens, first)
	}
	if got := tool.Tokens + user.Tokens; got != 300 {
		t.Fatalf("the new entries are worth %d tokens, want the 300 the difference counted", got)
	}
	// A third pass over the same path is a no-op: measurement is not a running total.
	grown := tool.Tokens + user.Tokens
	Stamp(all, 0, 0)
	if tool.Tokens+user.Tokens != grown {
		t.Fatalf("assembling the same path twice changed a measured number: %d, was %d", tool.Tokens+user.Tokens, grown)
	}
}

// TestDeriveSizesCountsTheHeadOnce is why the floor is a number the caller hands in: the system prompt
// and the tool schemas are in front of every request and are nobody's message, so no subtraction can
// produce them and no entry should carry them twice.
func TestDeriveSizesCountsTheHeadOnce(t *testing.T) {
	one := stampedTxt("u1", "user", 4000)
	one.Tokens = 300
	two := stampedTxt("t1", "tool", 4000)
	sizes := DeriveSizes(&Built{Entries: []*ir.Entry{one, two}, StructuredCap: 0}, 500)

	if got := sizes.Total(); got != 500+300+Estimate(two, 0) {
		t.Fatalf("the payload is %d tokens: want the 500 of head, the 300 that were counted, and the estimate of the entry nobody counted", got)
	}
	if row, ok := sizes.Get(one.ID); !ok || !row.Real || row.Tokens != 300 {
		t.Fatalf("a measured entry is not reported as measured: %+v", row)
	}
	if row, ok := sizes.Get(two.ID); !ok || row.Real {
		t.Fatalf("an unmeasured entry claims a server counted it: %+v", row)
	}
	if !sizes.AnyEstimate() {
		t.Fatal("a payload with an unmeasured entry reported itself as entirely measured")
	}
}
