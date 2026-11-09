package ir

import (
	"sort"
	"time"
)

// Meta is the resident shape of one entry: where it sits in the tree and what it
// is, with none of what it says. Everything a session can answer without putting
// bytes in front of a model — the tree, the path, the pending list, the phases,
// the park's "is the tip an unanswered call" — is a question about spines, and
// the spine costs tens of bytes per entry rather than the content's tens of
// kilobytes. The saying is read back from the store for the stretch a moment
// actually needs: the window of the next payload, the page somebody asked for.
//
// It mirrors the spine of Entry deliberately and does not embed it: the point of
// the split is that nothing content-shaped can join the resident set by accident.
type Meta struct {
	ID       string
	ParentID string
	Kind     string
	Ts       time.Time
	// Compaction is set only on a compaction entry: the boundary, the summary's
	// price, the broken flag. That is the one fact below the spine the tree needs
	// (Effective resolves a boundary against it), and it is why the replay of the
	// spine inflates those few payloads and no others — compaction rows grow with
	// the number of summaries, not with the length of the conversation.
	Compaction *CompactionMeta
}

// IsCompaction says whether this entry is a compaction summary (spec §3.6).
func (m *Meta) IsCompaction() bool { return m.Kind == KindCompaction }

// PathMeta is Path over the resident spine: the root-to-id sequence, terminating
// on a missing parent exactly as Path does.
func PathMeta(entries map[string]*Meta, id string) []*Meta {
	seen := map[string]bool{}
	var out []*Meta
	cur := id
	for cur != "" {
		if seen[cur] {
			break // corrupt cycle
		}
		seen[cur] = true
		e, ok := entries[cur]
		if !ok {
			break
		}
		out = append(out, e)
		cur = e.ParentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// EffectiveMeta is Effective over the resident spine, rule for rule (spec §3.2):
// only the last compaction entry is active, the boundary resolves against the
// whole path, and a boundary that cannot be found fails open with Broken marked.
// The kept slice it returns names entries by spine; the content is what
// EffectiveMeta's caller reads back for exactly those ids.
func EffectiveMeta(entries map[string]*Meta, path []*Meta) ([]*Meta, *Meta) {
	idx := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].IsCompaction() {
			idx = i
			break
		}
	}
	if idx < 0 {
		return path, nil
	}
	cmp := path[idx]
	meta := cmp.Compaction
	if meta == nil {
		meta = &CompactionMeta{}
		cmp.Compaction = meta
	}
	meta.Broken = false
	withoutOlder := func(from int) []*Meta {
		kept := make([]*Meta, 0, len(path)-from)
		for _, e := range path[from:] {
			if e.IsCompaction() {
				continue
			}
			kept = append(kept, e)
		}
		return kept
	}
	if meta.BoundaryID == nil {
		return withoutOlder(idx + 1), cmp
	}
	bid := *meta.BoundaryID
	for i := range path {
		if path[i].ID == bid {
			return withoutOlder(i), cmp
		}
	}
	meta.Broken = true
	all := make([]*Meta, 0, len(path)-1)
	for _, e := range path {
		if e != cmp {
			all = append(all, e)
		}
	}
	return all, cmp
}

// TreeOrderMeta is TreeOrder over the resident spine, rule for rule (a parentless
// orphans-as-roots, ids sorted among siblings, a corrupt cycle entered once) —
// answering for the whole tree what PathMeta answers for one chain. It returns
// ids, which is what a full-shaped fork or export wants before any payload has
// been read: the order first, the content one row at a time after, is the shape
// the single-connection store insists on.
func TreeOrderMeta(entries map[string]*Meta) []string {
	kids := make(map[string][]string, len(entries))
	var roots []string
	for id, m := range entries {
		if p := m.ParentID; p == "" || entries[p] == nil {
			roots = append(roots, id)
		} else {
			kids[p] = append(kids[p], id)
		}
	}
	sort.Strings(roots)
	for _, v := range kids {
		sort.Strings(v)
	}
	out := make([]string, 0, len(entries))
	visited := make(map[string]bool, len(entries))
	var walk func(string)
	walk = func(id string) {
		if visited[id] {
			return // a cycle would have no end; the first visitor wins
		}
		visited[id] = true
		out = append(out, id)
		for _, k := range kids[id] {
			walk(k)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// WindowRewriter is the window's stand-alone rewrite in streaming shape — the
// answer to "what does this window row need before it leaves?", handed one row at
// a time with its position, for a window of exactly these ids:
//
//   - the head entry roots the new session (its parent is one of the entries the
//     summary replaced, by construction);
//   - the summary whose boundary names a row the window dropped re-aims at the
//     window's head — naming the same slice with an id the copy carries — and a
//     boundary that already lands inside the window is nobody's to move;
//   - the summary that was broken at resolution (brokenCmp, empty unless the
//     boundary failed on the source's own path) passes through untouched: fail
//     open travels as it stands, the copy standing broken on the receiving
//     machine exactly as it stands broken here.
//
// This is the export-side rewrite the ctx file has always owed, lifted to the ir
// layer so the ctx fork pays the same rule from the same code — one window
// semantics, two delivery vehicles.
func WindowRewriter(ids []string, brokenCmp string) func(i int, e *Entry) *Entry {
	on := make(map[string]bool, len(ids))
	for _, id := range ids {
		on[id] = true
	}
	head := ""
	if len(ids) > 0 {
		head = ids[0]
	}
	return func(i int, e *Entry) *Entry {
		if i == 0 && e.ParentID != "" {
			rooted := *e
			rooted.ParentID = ""
			e = &rooted
		}
		if e.ID == brokenCmp || !e.IsCompaction() {
			return e
		}
		c := e.Compaction
		if c == nil || c.BoundaryID == nil || on[*c.BoundaryID] {
			return e
		}
		m := *c
		bid := head
		m.BoundaryID = &bid
		m.Broken = false
		rewritten := *e
		rewritten.Compaction = &m
		return &rewritten
	}
}
