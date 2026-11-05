package ir

// Tests for the page cutter. The property that matters is not any one page but that a walk of pages
// reproduces the path: every page head is a legal boundary, and neighbouring pages neither repeat an
// entry nor skip one. Cutting by count instead of by turn breaks exactly that, quietly — the browser
// ends up with a tool result whose card it never drew.

import (
	"fmt"
	"testing"
)

// msgs builds a path of t turns, each one user message followed by n entries that must stay attached
// to it (an assistant answer, a tool result, …).
func msgs(t, n int) []*Entry {
	var out []*Entry
	add := func(kind, role string) {
		out = append(out, &Entry{ID: posID(len(out)), Kind: kind, Role: role})
	}
	for i := 0; i < t; i++ {
		add(KindUser, "user")
		for j := 0; j < n; j++ {
			if j%2 == 0 {
				add(KindAssistant, "assistant")
			} else {
				add(KindTool, "tool")
			}
		}
	}
	return out
}

// posID names an entry by its position in the path, so a test can say which entries it expected
// without spelling out the log's own ids.
func posID(i int) string { return fmt.Sprintf("%04d", i) }

func TestWholePathIsTheDefault(t *testing.T) {
	path := msgs(5, 3)
	for _, o := range []PageOptions{{}, {All: true}} {
		p, err := PageOf(path, o)
		if err != nil {
			t.Fatalf("PageOf(%+v): %v", o, err)
		}
		if len(p.Entries) != len(path) || p.HasOlder || p.HasNewer || p.Truncated {
			t.Errorf("%+v: got %d entries, older=%v newer=%v truncated=%v, want all %d of the path",
				o, len(p.Entries), p.HasOlder, p.HasNewer, p.Truncated, len(path))
		}
		if p.Limit != 0 || p.Total != len(path) {
			t.Errorf("got limit %d total %d, want 0 and %d", p.Limit, p.Total, len(path))
		}
	}
}

// TestPagesWalkBackIntoThePath is the acceptance property: whatever the page size, walking back with
// Before=oldest hands over every entry of the path exactly once and never splits a turn.
func TestPagesWalkBackIntoThePath(t *testing.T) {
	path := msgs(9, 3) // nine turns of four
	full := idsOf(path)
	for _, limit := range []int{1, 2, 3, 4, 5, 7, 8, 13, 36, 1000} {
		var walk []string
		before := ""
		for pages := 0; ; pages++ {
			if pages > len(path) {
				t.Fatalf("limit %d: the walk never ends", limit)
			}
			p, err := PageOf(path, PageOptions{Limit: limit, Before: before})
			if err != nil {
				t.Fatalf("limit %d before %q: %v", limit, before, err)
			}
			// An empty page in this walk is a bug, not a state: the loop stops as soon as a page has no
			// older side, so `before` is never an id that leaves nothing before it.
			if len(p.Entries) == 0 {
				t.Fatalf("limit %d before %q: the walk got an empty page", limit, before)
			}
			head := p.Entries[0]
			// A page head is a turn start, or the first entry of the path: position 0 is always a
			// legal boundary, even when the log does not open with a user message.
			if head.ID != path[0].ID && !IsTurnStart(head) {
				t.Fatalf("limit %d: page head %s (%s) is neither the path start nor a turn start",
					limit, head.ID, head.Kind)
			}
			if !p.Truncated && len(p.Entries) > limit {
				t.Fatalf("limit %d: page of %d entries is not marked truncated", limit, len(p.Entries))
			}
			if !p.HasOlder && head.ID != path[0].ID {
				t.Fatalf("limit %d: page from %s claims nothing older but does not start the path",
					limit, head.ID)
			}
			if !p.HasNewer && p.Entries[len(p.Entries)-1].ID != path[len(path)-1].ID {
				t.Fatalf("limit %d: page claims nothing newer but does not end the path", limit)
			}
			for i := len(p.Entries) - 1; i >= 0; i-- {
				walk = append(walk, p.Entries[i].ID) // pages arrive newest first
			}
			if !p.HasOlder {
				break
			}
			before = head.ID
		}
		reverse(walk)
		if !equal(walk, full) {
			t.Errorf("limit %d: the walk covers %d entries, want the whole path (%d)", limit, len(walk), len(full))
		}
	}
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func TestPageIsClampedAndSaysSo(t *testing.T) {
	path := msgs(300, 1) // 600 entries, 300 turns of two
	p, err := PageOf(path, PageOptions{Limit: 5000})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !p.Clamped || p.Limit != MaxPageEntries {
		t.Errorf("got clamped=%v limit %d, want true and %d", p.Clamped, p.Limit, MaxPageEntries)
	}
	if len(p.Entries) == 0 || len(p.Entries) > MaxPageEntries {
		t.Errorf("got %d entries, want between 1 and %d", len(p.Entries), MaxPageEntries)
	}
	if p.Truncated {
		t.Error("turns of two cannot overflow a 200-entry page")
	}
	if !p.HasOlder || p.HasNewer {
		t.Errorf("got older=%v newer=%v, want the newest page of a longer path", p.HasOlder, p.HasNewer)
	}
}

// TestAnchorAlwaysComesBackWhole: any entry asked for as an anchor arrives inside a page that begins
// on a legal boundary — including an entry in the middle of a turn, which drags its whole turn along.
func TestAnchorAlwaysComesBackWhole(t *testing.T) {
	path := msgs(20, 3)
	for i := range path {
		p, err := PageOf(path, PageOptions{Limit: 12, Anchor: path[i].ID})
		if err != nil {
			t.Fatalf("anchor %s: %v", path[i].ID, err)
		}
		found := false
		for _, e := range p.Entries {
			if e.ID == path[i].ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("anchor %s (position %d) is not in the page it asked for", path[i].ID, i)
		}
		if head := p.Entries[0]; head.ID != path[0].ID && !IsTurnStart(head) {
			t.Fatalf("anchor %s: page head %s is neither the path start nor a turn start", path[i].ID, head.ID)
		}
		// And the user message that opens the anchored turn is on the page too, which is what keeps a
		// tool card from arriving without the question it answers.
		want := path[(i/4)*4].ID
		found = false
		for _, e := range p.Entries {
			if e.ID == want {
				found = true
			}
		}
		if !found {
			t.Errorf("anchor %s: page is missing the turn's user message %s", path[i].ID, want)
		}
	}
}

// TestAnchorOnATurnBiggerThanAPage: the turn is larger than the page it was asked into. It comes back
// whole and the page says so — splitting it would put a tool call on one page and its result on the
// next, which is two pages that can neither of them draw.
func TestAnchorOnATurnBiggerThanAPage(t *testing.T) {
	path := msgs(2, 500) // two turns of 501
	first, last := len(path)/2, len(path)-1
	p, err := PageOf(path, PageOptions{Limit: 10, Anchor: path[last].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !p.Truncated {
		t.Error("a turn larger than the page must be reported as truncated")
	}
	if !equal(idsOf(p.Entries), idsOf(path[first:])) {
		t.Errorf("got %d entries, want exactly the anchored turn (%d)", len(p.Entries), len(path)-first)
	}
	if !p.HasOlder || p.HasNewer {
		t.Errorf("got older=%v newer=%v, want the anchored turn and everything older still out of it", p.HasOlder, p.HasNewer)
	}
}

// TestBeforeInsideATurn: the turn named by before belongs to the newer page, so an id in the middle of
// a turn ends the older page at that turn's start instead of repeating half of it.
func TestBeforeInsideATurn(t *testing.T) {
	path := msgs(3, 3) // 12 entries, turns at 0, 4, 8
	p, err := PageOf(path, PageOptions{Limit: 10, Before: path[6].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !equal(idsOf(p.Entries), idsOf(path[:4])) {
		t.Errorf("got %v, want only the first turn", idsOf(p.Entries))
	}
	if p.HasOlder || !p.HasNewer {
		t.Errorf("got older=%v newer=%v, want false and true", p.HasOlder, p.HasNewer)
	}
}

func TestBeforeTheFirstTurnIsEmpty(t *testing.T) {
	path := msgs(3, 3)
	p, err := PageOf(path, PageOptions{Limit: 4, Before: path[1].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if len(p.Entries) != 0 || p.HasOlder || !p.HasNewer || p.Total != len(path) {
		t.Errorf("got %d entries older=%v newer=%v total %d, want none, false, true, %d",
			len(p.Entries), p.HasOlder, p.HasNewer, p.Total, len(path))
	}
}

// TestBeforeWithoutALimitStillCuts is the regression: an early "no limit means the whole path" return
// sat above the lookup, so a `before` with no `limit` was accepted and then ignored — a whole path
// where a cut was asked for, and a 200 where an unknown id should have been a 400. What is checked of
// anchors here is the id alone, because an anchor with no limit is refused on its own account
// (TestAnchorNeedsALimit).
func TestBeforeWithoutALimitStillCuts(t *testing.T) {
	path := msgs(3, 3) // 12 entries, turns at 0, 4, 8
	p, err := PageOf(path, PageOptions{Before: path[8].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !equal(idsOf(p.Entries), idsOf(path[:8])) {
		t.Errorf("got %d entries, want the 8 older than the third turn", len(p.Entries))
	}
	if p.HasOlder || !p.HasNewer || p.Limit != 0 {
		t.Errorf("got older=%v newer=%v limit %d, want false, true and 0 (nothing was asked for)",
			p.HasOlder, p.HasNewer, p.Limit)
	}
	if _, err := PageOf(path, PageOptions{Before: "not here"}); err == nil {
		t.Error("an unknown before without a limit must still be refused")
	}
	// A limit goes with the anchor because an anchor without one is refused on its own account
	// (TestAnchorNeedsALimit); here the size is deliberately present, so the only thing left to refuse
	// is the id.
	if _, err := PageOf(path, PageOptions{Limit: 5, Anchor: "not here"}); err == nil {
		t.Error("an unknown anchor must still be refused")
	}
}

// TestAnchorNeedsALimit: an anchor is a place in a page, and with no size asked for the page would
// grow until it held the path — so the request that measured nothing is refused rather than answered
// with the whole transcript.
func TestAnchorNeedsALimit(t *testing.T) {
	path := msgs(3, 3)
	p, err := PageOf(path, PageOptions{Limit: 4, Anchor: path[8].ID})
	if err != nil {
		t.Fatalf("PageOf with a size: %v", err)
	}
	if len(p.Entries) >= len(path) {
		t.Fatalf("a page of 4 returned the whole path (%d entries)", len(p.Entries))
	}
	if _, err := PageOf(path, PageOptions{Anchor: path[8].ID}); err == nil {
		t.Error("anchor with no limit: want a refusal, got a page")
	}
}

// TestIDsAreRefusedOnAnEmptyPath: nowhere on the path is nowhere, whatever the path looks like.
func TestIDsAreRefusedOnAnEmptyPath(t *testing.T) {
	if _, err := PageOf(nil, PageOptions{Before: "0000001"}); err == nil {
		t.Error("before on an empty path: want an error, got none")
	}
	if _, err := PageOf(nil, PageOptions{After: "0000001"}); err == nil {
		t.Error("after on an empty path: want an error, got none")
	}
	if _, err := PageOf(nil, PageOptions{Limit: 5, Anchor: "0000001"}); err == nil {
		t.Error("anchor on an empty path: want an error, got none")
	}
}

// TestCompactionIsAPageBoundary: a compaction record travels with role user and renders as a card of
// its own, so a page may begin right at it.
func TestCompactionIsAPageBoundary(t *testing.T) {
	path := []*Entry{
		entry("1", "", KindUser, "user", "q"),
		entry("2", "1", KindAssistant, "assistant", "a"),
		compaction("3", "2", "1"),
		entry("4", "3", KindUser, "user", "q2"),
		entry("5", "4", KindAssistant, "assistant", "a2"),
	}
	if !IsTurnStart(path[2]) {
		t.Fatal("a compaction record is not a turn start")
	}
	p, err := PageOf(path, PageOptions{Limit: 2, Anchor: "4"})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !equal(idsOf(p.Entries), []string{"4", "5"}) {
		t.Errorf("got %v, want the last turn only", idsOf(p.Entries))
	}
	if !p.HasOlder {
		t.Error("the compaction and what it summarised are still older")
	}
}

// TestPathWithNoUserMessage: position 0 is a boundary even when nothing else is, so the path comes
// back as one page rather than as nothing.
func TestPathWithNoUserMessage(t *testing.T) {
	path := msgs(1, 4)[1:] // an answer and three more, with the question cut off
	for _, o := range []PageOptions{{Limit: 1}, {Limit: 2}} {
		p, err := PageOf(path, o)
		if err != nil {
			t.Fatalf("PageOf(%+v): %v", o, err)
		}
		if len(p.Entries) != len(path) || !p.Truncated {
			t.Errorf("%+v: got %d entries truncated=%v, want all %d and truncated",
				o, len(p.Entries), p.Truncated, len(path))
		}
		if p.HasOlder || p.HasNewer {
			t.Errorf("%+v: got older=%v newer=%v, want the only page there is", o, p.HasOlder, p.HasNewer)
		}
	}
}

func TestEmptyPath(t *testing.T) {
	p, err := PageOf(nil, PageOptions{Limit: 10})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if len(p.Entries) != 0 || p.HasOlder || p.HasNewer || p.Total != 0 {
		t.Errorf("got %+v, want an empty page", p)
	}
}

// TestPagesWalkForwardIntoThePath is the same acceptance property walked the other way, which is what
// a window that recycles what it has scrolled past depends on: starting from an entry the caller is
// standing on, each page begins where the last one ended, no entry arrives twice, none is skipped, and
// the last page ends the path.
func TestPagesWalkForwardIntoThePath(t *testing.T) {
	path := msgs(9, 3) // nine turns of four
	for _, limit := range []int{1, 2, 3, 4, 5, 7, 8, 13, 36, 1000} {
		// A client passes the newest id it holds, which is a turn edge, so the walk starts from the
		// very first entry: the turn holding it belongs to what the caller has, and the expected cover
		// is everything from the second turn on.
		after := path[0].ID
		want := idsOf(path[4:])
		var walk []string
		for pages := 0; ; pages++ {
			if pages > len(path) {
				t.Fatalf("limit %d: the walk never ends", limit)
			}
			p, err := PageOf(path, PageOptions{Limit: limit, After: after})
			if err != nil {
				t.Fatalf("limit %d after %q: %v", limit, after, err)
			}
			// As in the walk backwards, an empty page here is a bug rather than a state: the loop only
			// asks again when the page it got said something newer exists.
			if len(p.Entries) == 0 {
				t.Fatalf("limit %d after %q: the walk got an empty page", limit, after)
			}
			head, tail := p.Entries[0], p.Entries[len(p.Entries)-1]
			if !IsTurnStart(head) {
				t.Fatalf("limit %d: page head %s (%s) is not a turn start", limit, head.ID, head.Kind)
			}
			// And a page that is not the last one ends on a turn edge, so the next page can begin on a
			// legal boundary instead of in the middle of a tool call and its result.
			if p.HasNewer && !IsTurnStart(path[indexOf(path, tail.ID)+1]) {
				t.Fatalf("limit %d: page tail %s is not a turn edge", limit, tail.ID)
			}
			if !p.HasOlder {
				t.Fatalf("limit %d: a page from %s claims nothing older", limit, head.ID)
			}
			if !p.Truncated && len(p.Entries) > limit {
				t.Fatalf("limit %d: page of %d entries is not marked truncated", limit, len(p.Entries))
			}
			if !p.HasNewer && tail.ID != path[len(path)-1].ID {
				t.Fatalf("limit %d: page claims nothing newer but ends at %s", limit, tail.ID)
			}
			for _, e := range p.Entries {
				walk = append(walk, e.ID) // pages arrive oldest first going forwards
			}
			if !p.HasNewer {
				break
			}
			after = tail.ID
		}
		if !equal(walk, want) {
			t.Errorf("limit %d: the walk covers %d entries, want the %d from the second turn on", limit, len(walk), len(want))
		}
	}
}

// TestAfterInsideATurn: the turn named by after belongs to the page the caller already has, so an id
// in the middle of a turn starts the newer page at that turn's end rather than repeating half of it.
func TestAfterInsideATurn(t *testing.T) {
	path := msgs(3, 3) // 12 entries, turns at 0, 4, 8
	p, err := PageOf(path, PageOptions{Limit: 10, After: path[6].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !equal(idsOf(p.Entries), idsOf(path[8:])) {
		t.Errorf("got %v, want only the last turn", idsOf(p.Entries))
	}
	if !p.HasOlder || p.HasNewer {
		t.Errorf("got older=%v newer=%v, want true and false", p.HasOlder, p.HasNewer)
	}
}

// TestAfterTheLastTurnIsEmpty is the other end of the path: standing inside the final turn leaves
// nothing newer, and a page of nothing is not a reason to hand back the whole transcript. Like the
// same test for before, it is what a client sees when it asks for more than the tip has.
func TestAfterTheLastTurnIsEmpty(t *testing.T) {
	path := msgs(3, 3)
	p, err := PageOf(path, PageOptions{Limit: 4, After: path[9].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if len(p.Entries) != 0 || !p.HasOlder || p.HasNewer || p.Total != len(path) {
		t.Errorf("got %d entries older=%v newer=%v total %d, want none, true, false, %d",
			len(p.Entries), p.HasOlder, p.HasNewer, p.Total, len(path))
	}
}

// TestAfterWithoutALimitStillCuts: "everything newer than here" is a question with an answer, so the
// cut happens with no ceiling, and an id off the path is refused rather than answered with the path.
func TestAfterWithoutALimitStillCuts(t *testing.T) {
	path := msgs(3, 3) // 12 entries, turns at 0, 4, 8
	p, err := PageOf(path, PageOptions{After: path[3].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !equal(idsOf(p.Entries), idsOf(path[4:])) {
		t.Errorf("got %d entries, want the 8 newer than the first turn", len(p.Entries))
	}
	if !p.HasOlder || p.HasNewer || p.Limit != 0 {
		t.Errorf("got older=%v newer=%v limit %d, want true, false and 0 (nothing was asked for)",
			p.HasOlder, p.HasNewer, p.Limit)
	}
	if _, err := PageOf(path, PageOptions{After: "not here"}); err == nil {
		t.Error("an unknown after with no limit must still be refused")
	}
}

// TestAfterOnATurnBiggerThanAPage: the turn that follows is larger than the page asked into, so it
// comes back whole and says so — the same exception the anchor and before walks make.
func TestAfterOnATurnBiggerThanAPage(t *testing.T) {
	path := msgs(2, 500) // two turns of 501
	first := len(path) / 2
	p, err := PageOf(path, PageOptions{Limit: 10, After: path[0].ID})
	if err != nil {
		t.Fatalf("PageOf: %v", err)
	}
	if !p.Truncated {
		t.Error("a turn larger than the page must be reported as truncated")
	}
	if !equal(idsOf(p.Entries), idsOf(path[first:])) {
		t.Errorf("got %d entries, want exactly the next turn (%d)", len(p.Entries), len(path)-first)
	}
	if !p.HasOlder || p.HasNewer {
		t.Errorf("got older=%v newer=%v, want the turn ahead and the tip inside it", p.HasOlder, p.HasNewer)
	}
}

func TestRejectedRequests(t *testing.T) {
	path := msgs(3, 3)
	for name, o := range map[string]PageOptions{
		"before and anchor together": {Before: "1", Anchor: "2"},
		"all with a limit":           {All: true, Limit: 5},
		"all with before":            {All: true, Before: "1"},
		"all with anchor":            {All: true, Anchor: "1"},
		"before off the path":        {Before: "not here"},
		"anchor off the path":        {Limit: 5, Anchor: "not here"},
		"all with after":             {All: true, After: "1"},
		"before and after together":  {Before: "1", After: "2"},
		"after and anchor together":  {After: "1", Anchor: "2"},
		"after off the path":         {After: "not here"},
		"anchor with no size":        {Anchor: "2"},
	} {
		if _, err := PageOf(path, o); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}
