package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"bareloop/internal/ir"
	"bareloop/internal/llm"
	"bareloop/internal/store"
)

// The fork tests build their trees at the store level rather than through turns: what a
// fork has to get right is the *shape* — parents, siblings, compaction boundaries, tip —
// and a shape made of appendEntry calls can only ever be the straight line the tip walks,
// which is exactly the one shape that would not catch a broken remap. So the fixture seeds
// the store with explicit ids and edges, unloads the session, and lets Fork read what the
// disk says.

func forkSrc(t *testing.T, m *Manager, id string, entries []*ir.Entry, tip string) {
	t.Helper()
	if _, err := m.Create(context.Background(), Config{
		ID:  id,
		LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"},
	}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	if _, err := m.Unload(id); err != nil {
		t.Fatalf("unload %s: %v", id, err)
	}
	l, err := store.Open(filepath.Join(m.Root(), id), false)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	defer l.Close()
	for _, e := range entries {
		cp := *e
		if err := l.AppendEntry(&cp, false); err != nil {
			t.Fatalf("append %s: %v", e.ID, err)
		}
	}
	if tip != "" {
		if err := l.SetTip(tip); err != nil {
			t.Fatalf("set tip on %s: %v", id, err)
		}
	}
}

func replay(t *testing.T, m *Manager, id string) *store.State {
	t.Helper()
	l, err := store.Open(filepath.Join(m.Root(), id), false)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	defer l.Close()
	st, err := l.Replay()
	if err != nil {
		t.Fatalf("replay %s: %v", id, err)
	}
	return st
}

func user(id, parent, content string) *ir.Entry {
	return &ir.Entry{ID: id, ParentID: parent, Kind: ir.KindUser, Role: "user", Content: content}
}

func assistant(id, parent, content string) *ir.Entry {
	return &ir.Entry{ID: id, ParentID: parent, Kind: ir.KindAssistant, Role: "assistant", Content: content}
}

func compaction(id, parent, bid string) *ir.Entry {
	b := bid
	return &ir.Entry{
		ID: id, ParentID: parent, Kind: ir.KindCompaction, Role: "user", Content: "[Conversation summary]",
		Compaction: &ir.CompactionMeta{BoundaryID: &b, Replaced: 2},
	}
}

// shoppingTree is the shape a week of honest use leaves behind: a line, a regenerate with
// two siblings off the second answer, and one sibling chosen and carried on — with a
// compaction whose boundary is the very first message.
//
//	s1 u ── s2 a ── s3 u ──┬─ s4 a
//	                       ├─ s5 a
//	                       └─ s6 a ── s7 cmp(b→s1) ── s8 u
func shoppingTree() []*ir.Entry {
	return []*ir.Entry{
		user("s1", "", "remember the list"),
		assistant("s2", "s1", "noted"),
		user("s3", "s2", "repeat it"),
		assistant("s4", "s3", "the list"),
		assistant("s5", "s3", "the list again"),
		assistant("s6", "s3", "the list, third"),
		compaction("s7", "s6", "s1"),
		user("s8", "s7", "and now?"),
	}
}

func TestForkPathRemapsIDsParentsAndBoundaries(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	forkSrc(t, m, "src", shoppingTree(), "s8")

	_, n, err := m.Fork(context.Background(), "src", ForkOptions{ID: "dst"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if n != 6 { // the path s1 s2 s3 s6 s7 s8 — one sibling survives, two stay behind
		t.Fatalf("copied %d entries, want 6", n)
	}
	src, dst := replay(t, m, "src"), replay(t, m, "dst")
	if len(dst.Entries) != 6 {
		t.Fatalf("dst has %d entries, want 6", len(dst.Entries))
	}
	for old := range src.Entries {
		if _, leaked := dst.Entries[old]; leaked {
			t.Fatalf("source id %q leaked into the copy", old)
		}
	}
	path := ir.Path(dst.Entries, dst.Tip)
	if len(path) != 6 {
		t.Fatalf("dst path is %d entries, want 6: %v", len(path), idsOf(path))
	}
	// Every parent resolves; every old id is gone from the payload too.
	for _, e := range path {
		if e.ParentID != "" {
			if _, ok := dst.Entries[e.ParentID]; !ok {
				t.Fatalf("entry %s: parent %q missing from the copy", e.ID, e.ParentID)
			}
		}
	}
	// The compaction came along and is not Broken: the boundary resolved through the map.
	cmp, c := ir.Effective(dst.Entries, path)
	if c == nil {
		t.Fatalf("no compaction on the copied path")
	}
	if c.Compaction == nil || c.Compaction.Broken {
		t.Fatalf("the copied compaction reads Broken — the boundary remap failed")
	}
	if len(cmp) == 0 {
		t.Fatalf("effective path after compaction is empty")
	}
	// The boundary names the copy's first entry, not the source's "s1".
	if *c.Compaction.BoundaryID != path[0].ID {
		t.Fatalf("boundary %q is not the copy's first entry %q", *c.Compaction.BoundaryID, path[0].ID)
	}
}

func TestForkCutBeforeTheCompactionLeavesItBehind(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	forkSrc(t, m, "src", shoppingTree(), "s8")

	// s2 is the answer before the compaction exists; a path fork there must not smuggle
	// the summary (or its boundary) in.
	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{ID: "cut", At: "s2"}); err != nil {
		t.Fatalf("fork at s2: %v", err)
	}
	st := replay(t, m, "cut")
	path := ir.Path(st.Entries, st.Tip)
	if len(path) != 2 {
		t.Fatalf("path is %d entries, want 2: %v", len(path), idsOf(path))
	}
	for _, e := range path {
		if e.IsCompaction() {
			t.Fatalf("a compaction rode along on a path that predates it")
		}
	}
}

func TestForkFullCopiesEveryBranch(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	forkSrc(t, m, "src", shoppingTree(), "s8")

	if _, n, err := m.Fork(context.Background(), "src", ForkOptions{ID: "full", Full: true}); err != nil {
		t.Fatalf("full fork: %v", err)
	} else if n != 8 {
		t.Fatalf("full fork copied %d, want 8", n)
	}
	src, dst := replay(t, m, "src"), replay(t, m, "full")

	// Same shape: the multiset of child-counts matches, entry for entry.
	if fmt.Sprint(shape(src.Entries)) != fmt.Sprint(shape(dst.Entries)) {
		t.Fatalf("tree shapes differ:\nsrc %v\ndst %v", shape(src.Entries), shape(dst.Entries))
	}
	// Every branch resolves: a compaction on any path of the copy must not read Broken.
	for id, e := range dst.Entries {
		if !e.IsCompaction() {
			continue
		}
		path := ir.Path(dst.Entries, id)
		_, c := ir.Effective(dst.Entries, path)
		if c == nil || c.Compaction == nil || c.Compaction.Broken {
			t.Fatalf("compaction %s reads Broken in the full copy", id)
		}
	}
	// The tip follows the source's, through the map.
	srcTip, dstTip := src.Entries[src.Tip], dst.Entries[dst.Tip]
	if srcTip == nil || dstTip == nil || srcTip.Content != dstTip.Content {
		t.Fatalf("tip content differs: %v vs %v", srcTip, dstTip)
	}
}

func TestForkTwiceDivergesAndTheSourceKeepsTalking(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	forkSrc(t, m, "src", shoppingTree()[:5], "s5") // two siblings exist, tip at s5

	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{ID: "a"}); err != nil {
		t.Fatalf("fork a: %v", err)
	}
	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{ID: "b", Full: true}); err != nil {
		t.Fatalf("fork b: %v", err)
	}
	// The source answers one more turn after both copies were made...
	s, err := m.Get(context.Background(), "src")
	if err != nil {
		t.Fatalf("get src: %v", err)
	}
	if _, err := s.appendEntry(assistant("late", "", "I kept talking")); err != nil {
		t.Fatalf("append to src: %v", err)
	}
	// ...and neither copy feels it. The path fork (tip s5: s1 s2 s3 s5) has four entries,
	// the full fork all five, and both keep whatever they were born with.
	before := replay(t, m, "src")
	if len(before.Entries) != 6 {
		t.Fatalf("source should have 6 entries now, got %d", len(before.Entries))
	}
	for id, want := range map[string]int{"a": 4, "b": 5} {
		if got := len(replay(t, m, id).Entries); got != want {
			t.Fatalf("copy %q has %d entries, want the %d it was born with", id, got, want)
		}
	}
}

func TestForkRefusesWhatItCannotDefend(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	forkSrc(t, m, "src", shoppingTree(), "s8")
	forkSrc(t, m, "bare", nil, "")

	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{}); err == nil ||
		!strings.Contains(err.Error(), "id is required") {
		t.Fatalf("fork without an id: %v", err)
	}
	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{ID: "x", At: "nope"}); err == nil ||
		!strings.Contains(err.Error(), "not in this session") {
		t.Fatalf("fork at an unknown entry: %v", err)
	}
	if _, _, err := m.Fork(context.Background(), "bare", ForkOptions{ID: "x"}); err == nil ||
		!strings.Contains(err.Error(), "nothing to fork") {
		t.Fatalf("fork of an empty session: %v", err)
	}
	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{ID: "x"}); err != nil {
		t.Fatalf("first fork: %v", err)
	}
	if _, _, err := m.Fork(context.Background(), "src", ForkOptions{ID: "x"}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("fork onto a taken id: %v", err)
	}
	// A refused fork leaves no half directory: "x" exists exactly once, whole.
	if got := len(replay(t, m, "x").Entries); got != 6 {
		t.Fatalf("the refused fork disturbed x: %d entries", got)
	}
}

// shape is the multiset of child counts — tree shape without any name in it, which is the
// only comparison two trees with disjoint id spaces allow.
func shape(entries map[string]*ir.Entry) []int {
	kids := map[string]int{}
	for _, e := range entries {
		kids[e.ParentID]++
	}
	v := make([]int, 0, len(kids))
	for _, n := range kids {
		v = append(v, n)
	}
	sort.Ints(v)
	return v
}

func idsOf(path []*ir.Entry) []string {
	out := make([]string, 0, len(path))
	for _, e := range path {
		out = append(out, e.ID)
	}
	return out
}
