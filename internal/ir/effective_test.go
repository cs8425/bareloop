package ir

// Tests for the two functions that decide what actually goes on the wire. Both were broken in
// ways that were invisible from the outside: a compaction that summarised everything and still
// sent everything (boundary resolved only after the entry), and a stale real-usage number that
// made every compaction followed by a redundant one.

import "testing"

func chain(entries ...*Entry) map[string]*Entry {
	m := make(map[string]*Entry, len(entries))
	for _, e := range entries {
		m[e.ID] = e
	}
	return m
}

func entry(id, parent, kind, role, content string) *Entry {
	return &Entry{ID: id, ParentID: parent, Kind: kind, Role: role, Content: content}
}

func usage(prompt int) *Usage {
	return &Usage{PromptTokens: prompt, HasPrompt: true}
}

// compaction builds a compaction entry whose boundary points backwards at bid, which is what the
// commit actually writes (spec §3.8 step 1: parent = last entry of the pre-compaction path).
func compaction(id, parent, bid string) *Entry {
	e := entry(id, parent, KindCompaction, "user", "[Conversation summary]")
	id2 := bid
	e.Compaction = &CompactionMeta{BoundaryID: &id2}
	return e
}

// pathOf walks the map the way the session does.
func pathOf(m map[string]*Entry, tip string) []*Entry { return Path(m, tip) }

func idsOf(list []*Entry) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBoundaryPointsBackwardsAtTheFirstKeptMessage is the regression: the boundary id recorded at
// commit time necessarily points at an entry *before* the compaction entry. Resolving it only in
// the suffix after the entry made every compaction "broken", which failed open and sent the whole
// history plus the summary (twice).
func TestBoundaryPointsBackwardsAtTheFirstKeptMessage(t *testing.T) {
	m := chain(
		entry("1", "", KindUser, "user", "old question"),
		entry("2", "1", KindAssistant, "assistant", "old answer"),
		entry("3", "2", KindUser, "user", "keep me"),
		entry("4", "3", KindAssistant, "assistant", "keep me too"),
		compaction("5", "4", "3"),
		entry("6", "5", KindUser, "user", "new question"),
	)
	path := pathOf(m, "6")
	kept, cmp := Effective(m, path)
	if cmp == nil || cmp.ID != "5" {
		t.Fatalf("active compaction = %v, want 5", cmp)
	}
	if cmp.Compaction != nil && cmp.Compaction.Broken {
		t.Fatalf("boundary 3 is on the path; Effective must not mark it broken")
	}
	want := []string{"3", "4", "6"}
	if got := idsOf(kept); !equal(got, want) {
		t.Fatalf("kept = %v, want %v", got, want)
	}
	for _, e := range kept {
		if e.IsCompaction() {
			t.Fatalf("the compaction entry must never be in kept (the caller splices its text in front): %v", idsOf(kept))
		}
	}
}

// TestNilBoundaryMeansEmptyKept pins the deliberate "summarize everything" state.
func TestNilBoundaryMeansEmptyKept(t *testing.T) {
	c := entry("3", "2", KindCompaction, "user", "[Conversation summary]")
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		entry("2", "1", KindAssistant, "assistant", "b"),
		c,
		entry("4", "3", KindUser, "user", "after"),
	)
	kept, cmp := Effective(m, pathOf(m, "4"))
	if cmp == nil {
		t.Fatal("expected the compaction entry back")
	}
	if got := idsOf(kept); !equal(got, []string{"4"}) {
		t.Fatalf("kept = %v, want [4]", got)
	}
}

// TestBrokenBoundaryFailsOpenWithoutDuplicatingTheSummary: an id that is not on the path means we
// do not know what was folded away, so everything is sent (spec §2.5) - except the compaction
// entry itself, because its content is what the caller splices in front. Keeping it here sends
// the summary twice, which is what made a broken session cost more than no compaction at all.
func TestBrokenBoundaryFailsOpenWithoutDuplicatingTheSummary(t *testing.T) {
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		entry("2", "1", KindAssistant, "assistant", "b"),
		compaction("3", "2", "deleted-id"),
		entry("4", "3", KindUser, "user", "after"),
	)
	kept, cmp := Effective(m, pathOf(m, "4"))
	if cmp == nil || cmp.Compaction == nil || !cmp.Compaction.Broken {
		t.Fatal("a boundary id that is not on the path must be reported as broken")
	}
	want := []string{"1", "2", "4"}
	if got := idsOf(kept); !equal(got, want) {
		t.Fatalf("kept = %v, want %v (everything, minus the compaction entry)", got, want)
	}
}

// TestOnlyTheNewestCompactionIsActive is the rolling summary (spec §3.6): older summaries live
// inside the newest one, and the active boundary may even point at an older compaction entry.
func TestOnlyTheNewestCompactionIsActive(t *testing.T) {
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		entry("2", "1", KindAssistant, "assistant", "b"),
		compaction("3", "2", "1"), // older, superseded
		entry("4", "3", KindUser, "user", "c"),
		compaction("5", "4", "2"), // newer, boundary points before the older entry
		entry("6", "5", KindUser, "user", "d"),
	)
	kept, cmp := Effective(m, pathOf(m, "6"))
	if cmp == nil || cmp.ID != "5" {
		t.Fatalf("active compaction = %v, want 5", cmp)
	}
	if cmp.Compaction != nil && cmp.Compaction.Broken {
		t.Fatal("boundary 2 is on the path")
	}
	want := []string{"2", "4", "6"}
	if got := idsOf(kept); !equal(got, want) {
		t.Fatalf("kept = %v, want %v (neither compaction entry may be sent)", got, want)
	}
}

// TestBrokenIsRecomputedAndNeverTrustedFromStorage: Broken is a diagnostic computed per resolve.
func TestBrokenIsRecomputedAndNeverTrustedFromStorage(t *testing.T) {
	c := compaction("3", "2", "1")
	c.Compaction.Broken = true // a hand-edited log claiming the worst
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		entry("2", "1", KindAssistant, "assistant", "b"),
		c,
	)
	_, cmp := Effective(m, pathOf(m, "3"))
	if cmp.Compaction != nil && cmp.Compaction.Broken {
		t.Fatal("a valid boundary must clear a stored Broken flag")
	}
}

// TestLastUsageStopsAtTheActiveCompaction is the "will not chain-compact" rule (spec §1.5). The
// newest real usage on the path priced the payload that the compaction just replaced; handing it
// back made the next turn compact again (deterministically), and let the pre-send guard refuse a
// payload that had already shrunk.
func TestLastUsageStopsAtTheActiveCompaction(t *testing.T) {
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		&Entry{ID: "2", ParentID: "1", Kind: KindAssistant, Role: "assistant", Content: "b", Usage: usage(200)},
		compaction("3", "2", "1"),
	)
	path := pathOf(m, "3")
	if u := LastUsage(path, true); u != nil {
		t.Fatalf("usage before the boundary must read as unavailable, got %+v", u)
	}
	// With a broken boundary nothing was folded away, so the number is still the wire's price.
	if u := LastUsage(path, false); u == nil || u.ContextUsed() != 200 {
		t.Fatalf("broken boundary must keep using the real number, got %+v", u)
	}
	// The first completed turn after the compaction restores the real number.
	m["4"] = &Entry{ID: "4", ParentID: "3", Kind: KindAssistant, Role: "assistant", Content: "c", Usage: usage(300)}
	if u := LastUsage(pathOf(m, "4"), true); u == nil || u.ContextUsed() != 300 {
		t.Fatalf("usage after the boundary must be used, got %+v", u)
	}
}

// TestUsageWithoutPromptIsNotReal covers the HasPrompt flag: a server that reports nothing must
// not be mistaken for a server that reports zero, because 0 would satisfy every threshold.
func TestUsageWithoutPromptIsNotReal(t *testing.T) {
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		&Entry{ID: "2", ParentID: "1", Kind: KindAssistant, Role: "assistant", Content: "b", Usage: &Usage{PromptTokens: 999}},
	)
	if u := LastUsage(pathOf(m, "2"), false); u != nil {
		t.Fatalf("HasPrompt=false must not count as a measurement: %+v", u)
	}
	if (&Usage{PromptTokens: 5, HasPrompt: true}).ContextUsed() != 5 {
		t.Fatal("a prompt with no completion is just the prompt")
	}
	// The one-semantics invariant: the cached prefix is a *part* of PromptTokens, so it must not be
	// counted twice. A turn that reused 900 of 1000 prompt tokens and wrote 50 occupies 1050, not 1950.
	if got := (&Usage{PromptTokens: 1000, CacheTokens: 900, CompletionTokens: 50, HasPrompt: true}).ContextUsed(); got != 1050 {
		t.Fatalf("cached tokens must not be added to a prompt that already includes them: got %d, want 1050", got)
	}
	if (&Usage{}).ContextUsed() != 0 || (*Usage)(nil).ContextUsed() != 0 {
		t.Fatal("no prompt means no occupancy claim")
	}
}

// TestPathWalksToTheRootAndSurvivesACycle keeps the branch/corner cases of Path honest: a missing
// parent returns the visible prefix, a cycle terminates.
func TestPathWalksToTheRootAndSurvivesACycle(t *testing.T) {
	m := chain(
		entry("1", "", KindUser, "user", "a"),
		entry("2", "1", KindAssistant, "assistant", "b"),
		entry("3", "2", KindUser, "user", "c"),
	)
	if got := idsOf(pathOf(m, "3")); !equal(got, []string{"1", "2", "3"}) {
		t.Fatalf("path = %v, want [1 2 3]", got)
	}
	m["loop"] = entry("loop", "back", KindUser, "user", "x")
	m["back"] = entry("back", "loop", KindUser, "user", "y")
	if got := pathOf(m, "loop"); len(got) != 2 {
		t.Fatalf("a cycle must terminate, got %v", idsOf(got))
	}
	if got := pathOf(m, "missing"); len(got) != 0 {
		t.Fatalf("unknown tip must give an empty path, got %v", idsOf(got))
	}
}
