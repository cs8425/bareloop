package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"bareloop/internal/ir"
	"bareloop/internal/store"
)

// Fork copies a session and returns the copy: two shapes, one primitive.
//
// The primitive is a copied set: pick a subset of the source entries, hand every one of
// them a fresh id in the new store, and rewrite each internal reference — the parent edge,
// and the compaction boundary that hides behind it — through the old→new map. A fork that
// kept the source's ids could not be told apart from its source by anything but its directory,
// and two trees that quote each other's entry ids are one tree with a split personality;
// ids being session-local, the remap costs a map and buys the independence the copy is for.
//
// The two shapes differ only in the subset:
//
//   - path fork (the default): the path root..at, at defaulting to the source tip, copied
//     as a straight line. Everything off that path — the abandoned siblings of a regenerate,
//     the rewound branches — stays behind. This is the deliberate shrink: a compaction is
//     the model guessing what it may summarize, this is a person deciding what they will not
//     carry. The source keeps its full tree either way, so deciding wrong costs a re-fork.
//
//   - full fork (Full): the whole tree, every branch, tip following the source's. A clone to
//     play in while the original stays untouched — a config experiment, a rewind that should
//     not burn the real history.
//
// The compaction invariant is what makes the path fork safe without any bookkeeping: a
// BoundaryID names an entry that was an ancestor of its compaction at commit time (that is
// how the compactor writes it), so any path that contains the compaction contains its
// boundary too. A boundary that still fails to resolve in the copy keeps its old id rather
// than getting an invented one: it reads as Broken and fails open, the same honest answer
// the resolver gives a hand-edited log.
//
// The copy writes through the store handle, not through Session.appendEntry: appendEntry
// is a conversation move — it advances the tip and publishes — and a fork is a transcript
// being born, not a turn being taken. The new session is unloaded before Fork returns, so
// the next Get replays what was written rather than trusting an in-memory snapshot of an
// empty store.

// ForkOptions says what to copy. The zero value forks the source's current path to
// its tip under a caller-supplied id.
type ForkOptions struct {
	// ID is the new session's id. Required: the daemon never invents ids (same rule as
	// Create — a directory named after the clock is not something anybody asked for).
	ID string
	// At cuts the path at that entry (default: the source tip). Ignored when Full.
	At string
	// Full copies every branch instead of one path.
	Full bool
	// Name overrides the display name; empty follows the source ("X (fork)").
	Name string
}

// Fork creates the copy and returns its id and the number of entries written.
func (m *Manager) Fork(ctx context.Context, srcID string, o ForkOptions) (string, int, error) {
	if o.ID == "" {
		return "", 0, errors.New("agent: a session id is required to fork")
	}
	if err := validID(o.ID); err != nil {
		return "", 0, err
	}
	src, err := m.Get(ctx, srcID)
	if err != nil {
		return "", 0, err
	}
	// Replay is the source's truth as of now, taken off the store rather than the running
	// session's snapshot: a fork of a busy session copies whatever whole state it opened
	// with, and no half-appended turn can hide in a Replay.
	st, err := src.Log().Replay()
	if err != nil {
		return "", 0, err
	}
	set, tipSrc, err := forkSet(st, o)
	if err != nil {
		return "", 0, err
	}
	if len(set) == 0 {
		return "", 0, fmt.Errorf("agent: session %q has nothing to fork", srcID)
	}

	cfg := src.Config()
	cfg.ID = o.ID
	// A fork announces itself in the list; an explicit name wins, and a source with no
	// name of its own gets no " (fork)" appended to nothing.
	if o.Name != "" {
		cfg.Name = o.Name
	} else if cfg.Name != "" {
		cfg.Name += " (fork)"
	}
	// Create owns the id and the validation (pricing, structured modes, policy) and refuses
	// an existing directory — the fork inherits all of that rather than re-deriving it.
	dst, err := m.Create(ctx, cfg)
	if err != nil {
		return "", 0, err
	}
	written := 0
	// Anything short of a finished copy leaves a directory that lists as a session: the
	// half copy is not a fork anyone could defend, so the failed attempt is undone here,
	// in the one place that knows how much it made.
	defer func() {
		if written == len(set) {
			return
		}
		if err := m.Remove(o.ID); err != nil {
			m.logf("fork %q: could not undo the failed copy (%d entries in): %v", o.ID, written, err)
		}
	}()

	log := dst.Log()
	ids := make(map[string]string, len(set))
	for _, e := range set {
		ne := *e
		nid := log.NextID()
		ids[e.ID] = nid
		ne.ID = nid
		if ne.ParentID != "" {
			ne.ParentID = ids[ne.ParentID] // set order is parent-before-child, so this is mapped
		}
		if c := ne.Compaction; c != nil {
			nc := *c
			if c.BoundaryID != nil {
				if mapped, ok := ids[*c.BoundaryID]; ok {
					nb := mapped
					nc.BoundaryID = &nb
				}
			}
			ne.Compaction = &nc // a fresh meta: writing through the source's pointer would edit the source
		}
		if err := log.AppendEntry(&ne, false); err != nil {
			return "", 0, fmt.Errorf("agent: fork %q: %w", o.ID, err)
		}
		written++
	}
	if err := log.SetTip(ids[tipSrc]); err != nil {
		return "", 0, fmt.Errorf("agent: fork %q: setting the tip: %w", o.ID, err)
	}
	// The opened session still carries its empty snapshot; drop it so the next reader
	// replays the copy from disk.
	if _, err := m.Unload(o.ID); err != nil {
		return "", 0, err
	}
	return o.ID, len(set), nil
}

// forkSet picks the subset in parent-before-child order and names the entry the copy's tip
// should be. Path mode walks at's own ancestry (the error for an unknown id happens there);
// full mode hands over the whole tree.
func forkSet(st *store.State, o ForkOptions) ([]*ir.Entry, string, error) {
	if o.Full {
		return treeOrder(st.Entries), st.Tip, nil
	}
	at := o.At
	if at == "" {
		at = st.Tip
	}
	if at == "" {
		return nil, "", nil
	}
	var rev []*ir.Entry
	seen := make(map[string]bool, len(st.Entries))
	for cur := at; cur != ""; {
		if seen[cur] {
			return nil, "", fmt.Errorf("agent: fork: entry %q: parent cycle", cur)
		}
		seen[cur] = true
		e, ok := st.Entries[cur]
		if !ok {
			return nil, "", fmt.Errorf("agent: fork: entry %q is not in this session", cur)
		}
		rev = append(rev, e)
		cur = e.ParentID
	}
	set := make([]*ir.Entry, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		set = append(set, rev[i])
	}
	return set, at, nil
}

// treeOrder lists every entry parent-before-child. Children walk in id order, and ids are
// handed out by the store's seq counter, so id order is the source's append order: the copy
// numbers its entries in the same timeline the source lived them. An entry whose parent is
// missing walks as a root — that should not happen, and if it ever does, a copy that kept
// the odd entry beats one that silently dropped it.
func treeOrder(entries map[string]*ir.Entry) []*ir.Entry {
	kids := make(map[string][]*ir.Entry, len(entries))
	var roots []*ir.Entry
	for _, e := range entries {
		if p := e.ParentID; p == "" || entries[p] == nil {
			roots = append(roots, e)
		} else {
			kids[p] = append(kids[p], e)
		}
	}
	byID := func(v []*ir.Entry) func(i, j int) bool {
		return func(i, j int) bool { return v[i].ID < v[j].ID }
	}
	sort.Slice(roots, byID(roots))
	for _, v := range kids {
		sort.Slice(v, byID(v))
	}
	out := make([]*ir.Entry, 0, len(entries))
	visited := make(map[string]bool, len(entries))
	var walk func(*ir.Entry)
	walk = func(e *ir.Entry) {
		if visited[e.ID] {
			return // a cycle would have no end; the first visitor wins
		}
		visited[e.ID] = true
		out = append(out, e)
		for _, k := range kids[e.ID] {
			walk(k)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}
