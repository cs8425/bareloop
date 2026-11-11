package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bareloop/internal/ir"
	"bareloop/internal/store"
)

// Fork copies a session and returns the copy: three shapes, one primitive.
//
// The primitive is a copied set: pick a subset of the source entries, hand every one of
// them a fresh id in the new store, and rewrite each internal reference — the parent edge,
// and the compaction boundary that hides behind it — through the old→new map. A fork that
// kept the source's ids could not be told apart from its source by anything but its directory,
// and two trees that quote each other's entry ids are one tree with a split personality;
// ids being session-local, the remap costs a map and buys the independence the copy is for.
//
// The three shapes differ only in the subset:
//
//   - path fork (the default): the path root..at, at defaulting to the source tip, copied
//     as a straight line. Everything off that path — the abandoned siblings of a regenerate,
//     the rewound branches — stays behind. This is the deliberate shrink: a compaction is
//     the model guessing what it may summarize, this is a person deciding what they will not
//     carry. The source keeps its full tree either way, so deciding wrong costs a re-fork.
//
//   - ctx fork (At "ctx", the same reserved word the export's ?path=ctx answers): the
//     window the next request carries — the active summary and the entries its boundary
//     keeps — stood on its own by the one window rule the ctx export pays from
//     (ir.WindowRewriter). The daily job's move: hand today's working context to a new
//     session and let the long history stay where it is.
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
// The copy streams, because the resident spine already knows which ids travel and in what
// order (the tree is asked its shape without a payload being read for it): one entry is
// read, remapped, and written before the next is named, and image bytes travel by sha —
// copied from blob table to blob table without ever passing through the base64 layer a
// decode-then-re-encode round trip would rebuild from those very bytes. Nothing here
// needs the source's whole tree in RAM, and nothing here would be legal with the store's
// one-connection rule if it tried: every read is a self-contained statement.
//
// The copy writes through the store's own import transaction, not through
// Session.appendEntry: appendEntry is a conversation move — it advances the tip and
// publishes — and a fork is a transcript being born, not a turn being taken. The
// transaction adds the promise the row-by-row handle never made: rows, blobs and tip
// commit together or not at all, so no half copy outlives its failure. The new
// session is unloaded before Fork returns, so
// the next Get replays what was written rather than trusting an in-memory snapshot of an
// empty store.

// ForkOptions says what to copy. The zero value forks the source's current path to
// its tip under a caller-supplied id.
type ForkOptions struct {
	// ID is the new session's id. Required: the daemon never invents ids (same rule as
	// Create — a directory named after the clock is not something anybody asked for).
	ID string
	// At cuts the path at that entry (default: the source tip); the reserved word
	// "ctx" forks the window the next request carries instead. Ignored when Full.
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
	// The shape questions are answered by the resident spine — which ids travel, in
	// what order, where the copy's tip stands. The orders are parent-before-child by
	// construction, so the remap below always names an id it has already issued.
	// (A path cut at a corrupt parent cycle walks to the cycle and stops, the same
	// truncation every other path read applies; the spine itself ends there.)
	var ids []string
	var rw func(int, *ir.Entry) *ir.Entry
	tipSrc := ""
	switch at := strings.TrimSpace(o.At); {
	case o.Full:
		ids = src.TreeIDs()
		if info, err := src.Log().Read(); err == nil {
			tipSrc = info.Tip
		}
	case strings.EqualFold(at, "ctx"):
		var brokenCmp string
		ids, brokenCmp = src.WindowIDs()
		rw = ir.WindowRewriter(ids, brokenCmp)
		if len(ids) > 0 {
			tipSrc = ids[len(ids)-1]
		}
	default:
		ids = src.PathIDs(at)
		if len(ids) == 0 {
			if at == "" {
				return "", 0, fmt.Errorf("agent: session %q has nothing to fork", srcID)
			}
			return "", 0, fmt.Errorf("agent: fork: entry %q is not in this session", at)
		}
		tipSrc = ids[len(ids)-1]
	}
	if len(ids) == 0 {
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
	// an existing directory — the fork inherits all of that rather than re-deriving it. The
	// MCP list travels as the daemon's own copy, which is what keeps a fork legal under
	// mcpLock: the copy introduces no server the source did not already run.
	dst, err := m.create(ctx, cfg, false)
	if err != nil {
		return "", 0, err
	}
	srcLog, log := src.Log(), dst.Log()
	// The copy runs as one living transaction, which is what makes "how much of
	// it landed" a settled fact instead of a forensics question: rows, blobs and
	// the tip appear together or not at all, and anything short of Finish is
	// undone by Abort plus the half-copy removal below.
	//
	// How big can "all of it" get, and what bites first in one transaction?
	// Nothing that scales with the row count: each row's INSERT binds six
	// parameters however many rows the copy writes — SQLite's variable limit is
	// 32,766 since 3.32, and because per-row statements are never concatenated,
	// the statement text never grows either; the per-string/blob ceiling
	// (about 1 GB) is a single blob's problem, not the transaction's; and the
	// uncommitted pages live in SQLite's own page cache, spilling to its temp
	// store, not onto this process's heap. What the copy does grow is the WAL,
	// up to the checkpoint, and one final commit. In short the ceiling is the
	// disk, not the engine — and the largest fork anyone has run (a thousand-
	// entry session with images) sits orders of magnitude under all of it.
	// The source's own store is read through its own handle (a different Log,
	// a different lock), so streaming interleaves freely with the transaction.
	it, err := log.ImportStream()
	if err != nil {
		return "", 0, fmt.Errorf("agent: fork %q: opening the copy transaction: %w", o.ID, err)
	}
	written := 0
	// Anything short of a committed copy leaves a directory that lists as a
	// session: the half copy is not a fork anyone could defend, so the failed
	// attempt is undone here, in the one place that knows how much it made —
	// the transaction first (its handle is open on that directory), then the
	// directory itself.
	committed := false
	defer func() {
		if committed {
			return
		}
		if aerr := it.Abort(); aerr != nil {
			m.logf("fork %q: could not roll back the copy transaction: %v", o.ID, aerr)
		}
		if rerr := m.Remove(o.ID); rerr != nil {
			m.logf("fork %q: could not undo the failed copy (%d entries in): %v", o.ID, written, rerr)
		}
	}()
	mapped := make(map[string]string, len(ids))
	copied := make(map[string]bool)
	for _, id := range ids {
		// One row at a time, references kept as references: the entry travels the
		// store's own cheap shape and never its images.
		found, err := srcLog.Contents([]string{id}, false)
		if err != nil {
			return "", 0, fmt.Errorf("agent: fork %q: reading %s: %w", o.ID, id, err)
		}
		e, ok := found[id]
		if !ok {
			// Deleted mid-copy: stopping is honest, a silently missing line is a
			// parent walk no reader will take.
			return "", 0, fmt.Errorf("agent: fork %q: entry %s left the source mid-copy", o.ID, id)
		}
		if rw != nil {
			e = rw(written, e)
		}
		ne := *e
		nid := it.NextID()
		mapped[e.ID] = nid
		ne.ID = nid
		if ne.ParentID != "" {
			ne.ParentID = mapped[ne.ParentID] // stream order is parent-before-child, so this is mapped
		}
		if c := ne.Compaction; c != nil {
			nc := *c
			if c.BoundaryID != nil {
				if mappedID, ok := mapped[*c.BoundaryID]; ok {
					nb := mappedID
					nc.BoundaryID = &nb
				}
			}
			ne.Compaction = &nc // a fresh meta: writing through the source's pointer would edit the source
		}
		// The image bytes move before the payload that names them — the store
		// refuses a reference to a sha it has never seen — and they move as bytes,
		// from blob table to blob table, never through the base64 layer that the
		// write side would only undo to these very bytes.
		for _, p := range ne.Parts {
			sha, isRef := strings.CutPrefix(p.Data, store.BlobPrefix)
			if p.Type != ir.PartImage || !isRef || sha == "" || copied[sha] {
				continue
			}
			b, mime, found, err := srcLog.BlobBytes(sha)
			if err != nil {
				return "", 0, fmt.Errorf("agent: fork %q: blob %s: %w", o.ID, sha, err)
			}
			if !found {
				return "", 0, fmt.Errorf("agent: fork %q: entry %s names blob %s, which is not in the source store", o.ID, id, sha)
			}
			if err := it.PutBlob(sha, mime, b); err != nil {
				return "", 0, fmt.Errorf("agent: fork %q: putting blob %s: %w", o.ID, sha, err)
			}
			copied[sha] = true
		}
		if err := it.Entry(&ne); err != nil {
			return "", 0, fmt.Errorf("agent: fork %q: %w", o.ID, err)
		}
		written++
	}
	// Finish commits the tip with the rows and answers two questions the manual
	// path never asked: the tip stands among the copied rows, and every parent
	// the copy carries was copied. (A source whose tip nobody could read — the
	// full shape's rare stumble — gets the last row copied as tip, which is
	// where the log's own replay would stand the session anyway.)
	if err := it.Finish(mapped[tipSrc]); err != nil {
		return "", 0, fmt.Errorf("agent: fork %q: committing the copy: %w", o.ID, err)
	}
	committed = true
	// The opened session still carries its empty snapshot; drop it so the next reader
	// replays the copy from disk.
	if _, err := m.Unload(o.ID); err != nil {
		return "", 0, err
	}
	return o.ID, written, nil
}
