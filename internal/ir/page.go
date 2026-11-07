package ir

import (
	"errors"
	"fmt"
)

// MaxPageEntries is the ceiling on how many entries one page may carry.
//
// The transcript is the expensive surface, and its cost grows with what is on it: forcing a full
// reflow of one measured 0.14 ms with 221 characters of content and 52.88 ms once it had reached
// 214 000 pixels of height. A page the size of a whole session is exactly what paging is meant to
// avoid. This is a ceiling and not a default: PageOf's own zero value answers "no limit" — the
// library's answer — while the transcript endpoint caps every URL at this ceiling or below.
const MaxPageEntries = 200

// DefaultPageEntries is what the transcript endpoint answers a request that names no size at
// all: a page, never the path. "No query string" was never anybody's promise to pay for the
// whole session — one session's unparameterised read measured 73 MB, images inline — and a
// server should not be able to be talked into an unbounded transfer by saying nothing, nor by
// naming a place without a size (a bare before/after gets this page too). No name the endpoint
// reads asks for the whole path any more — the one that did is gone with its option, before the
// project had anyone to disappoint; the only whole-path answer left is the cost panel's shape
// (`?usage=1`), whose rows are bytes of arithmetic rather than bytes of conversation.
// The number is the web UI's page size; when the two move, one of them is lying about its page.
const DefaultPageEntries = 40

// IsTurnStart reports whether a boundary may fall immediately before e. This is the one rule the
// page cutter and the compaction cutter share — compact decides "did this cut split a turn" with
// the same test — so it lives next to the entries rather than twice in two packages.
//
// A user message begins a turn, and so does a compaction record: it carries role user and renders as
// a banner of its own. An assistant message or a tool result begins nothing, which is why a cut may
// not land before one: between a tool call and its result, one card would end up on each side.
func IsTurnStart(e *Entry) bool {
	switch {
	case e == nil:
		return false
	case e.Kind == KindCompaction:
		return true
	case e.Role == "":
		// Logged before Role was recorded, so the kind is the role — the same reading compact uses.
		return e.Kind == KindUser
	default:
		return e.Role == "user"
	}
}

// PageOptions selects a page from a path. The zero value asks for the whole path.
type PageOptions struct {
	// Limit counts entries, taken from the newest end. Zero or less means no limit, which is not the
	// same as no request: Before is honoured either way, an Anchor is refused without one.
	Limit int
	// Before is exclusive: the page ends before that entry, which is how a client loads the next
	// older page — it passes back the oldest id it is already holding. Without a Limit this still
	// cuts: "everything older than here" is a question with an answer.
	Before string
	// Anchor asks for a page around that entry, for a jump target that sits far from both ends.
	// Unlike Before it is a place and not a cut, so it is refused without a Limit: a page around an
	// entry with no size is the whole path, and the caller that named an entry did not want that.
	Anchor string
	// After is the mirror of Before: exclusive, and the page it selects begins with the turn that
	// follows the one holding the entry named. The id a client passes is the newest one it is still
	// holding, so nothing it already has is ever handed back to it a second time. Like Before it cuts
	// with no Limit, because "everything newer than here" is a question with an answer.
	After string
}

// Page is a run of whole turns cut out of a path.
type Page struct {
	// Entries is a subslice of the path handed to PageOf, which copies nothing: a caller that lets
	// these pointers escape the lock has to copy them itself (Session.Path already does).
	Entries []*Entry
	// Total is the length of the path the page was cut from.
	Total int
	// HasOlder and HasNewer say the path continues on that side of the page.
	HasOlder bool
	HasNewer bool
	// Limit is the page size the caller asked for, or 0 when it asked for none — which still honours
	// Before, because that cuts the path rather than sizing the page. When Clamped it is the ceiling
	// that was applied instead of the number asked for.
	Limit int
	// Clamped marks a request for more than MaxPageEntries.
	Clamped bool
	// Truncated marks a page that had to carry a single turn larger than the page it was cut for.
	// Saying so beats cutting the turn: half a tool call on one page and its result on the next is
	// not a page either half can draw.
	Truncated bool
}

// PageOf cuts a turn-atomic page out of a path in root-to-tip order.
//
// The unit is the whole turn, so the page boundaries are always legal ones: two neighbouring pages
// concatenate back into the path with no gap and no overlap, and no page begins in the middle of a
// turn. Asking for the newest N and then asking with Before set to the oldest id that page returned
// is the same walk a reader would do with a scrollbar. After is that walk in the other direction:
// forwards from a page's newestId, which is what a window that recycles what it has scrolled past
// has to ask for.
func PageOf(path []*Entry, o PageOptions) (*Page, error) {
	// Every combination check lives here rather than in the HTTP layer, so that one rule about what
	// the parameters mean has one home and is testable without a server.
	if o.Before != "" && o.Anchor != "" {
		return nil, errors.New("ir: before and anchor cannot be combined")
	}
	if o.After != "" && (o.Before != "" || o.Anchor != "") {
		// Two named places are two stretches of the path, and the request that means one but is
		// answered with the other is the one a pager writes when it grows its window in both
		// directions at once. Refused rather than given a precedence.
		return nil, errors.New("ir: after cannot be combined with before or anchor")
	}
	// why refused rather than answered with no ceiling: an anchored page with no size grows until it
	// has swallowed the path, so the answer to "centre a page of nothing in particular here" is the
	// whole transcript — the one thing paging exists to keep out of a response.
	if o.Anchor != "" && o.Limit <= 0 {
		return nil, errors.New("ir: anchor needs a limit: an anchor is a place in a page, not a page")
	}
	// The three ids are resolved once, here, so that "not on this path" is refused whatever else the
	// request looked like — including an empty path, where there is nowhere for any of them to be, and
	// a before or after with no limit, where an answer of the whole path would be a guess at what was
	// asked for.
	bi, ai, afi := -1, -1, -1
	if o.Before != "" {
		if bi = indexOf(path, o.Before); bi < 0 {
			return nil, fmt.Errorf("ir: before entry %q is not on the path", o.Before)
		}
	}
	if o.After != "" {
		if afi = indexOf(path, o.After); afi < 0 {
			return nil, fmt.Errorf("ir: after entry %q is not on the path", o.After)
		}
	}
	if o.Anchor != "" {
		if ai = indexOf(path, o.Anchor); ai < 0 {
			return nil, fmt.Errorf("ir: anchor entry %q is not on the path", o.Anchor)
		}
	}
	n := len(path)
	p := &Page{Total: n, Entries: path}
	if n == 0 {
		return p, nil
	}
	// A page is still cut when no size was asked for. "Everything before this entry" is a request, and
	// an id that is not on the path is an error whoever typed it — neither is a reason to answer with
	// the whole path, which is what an early "no limit means everything" return here would have done.
	// limit is the ceiling in force; p.Limit reports what the caller asked for instead (0 = none).
	limit := o.Limit
	if limit <= 0 {
		limit = n
	} else {
		if limit > MaxPageEntries {
			limit = MaxPageEntries
			p.Clamped = true
		}
		p.Limit = limit
	}

	starts := turnStarts(path)
	// edge is one past the last entry of turn k.
	edge := func(k int) int {
		if k+1 < len(starts) {
			return starts[k+1]
		}
		return n
	}
	lo, hi := 0, n
	switch {
	case o.Anchor != "":
		// Two cursors, because the page grows in both directions from the anchored turn and one cursor
		// walking outwards would move the far edge back in when the near one stepped out.
		ol, ne := turnAt(starts, ai), turnAt(starts, ai)
		lo, hi = starts[ol], edge(ne)
		// The anchored turn always comes along; what is left of the page is shared between the two
		// sides, older side first and by half, so the anchor stays roughly centred. If that turn is
		// already bigger than the page then room is negative and neither loop below fires, which is the
		// right answer: hand over the turn and mark the page truncated.
		room := (limit - (hi - lo)) / 2
		for ol > 0 && lo-starts[ol-1] <= room {
			room -= lo - starts[ol-1]
			ol--
			lo = starts[ol]
		}
		for ne+1 < len(starts) && edge(ne+1)-lo <= limit {
			ne++
			hi = edge(ne)
		}
	case o.After != "":
		// The turn holding `after` is the one the caller is already standing on, whatever the id points
		// at, so the page begins at the next turn and whole turns are added towards the tip. Walking
		// this with a page's newestId reproduces the path forwards with no gap and no overlap — the
		// mirror of the `before` walk, and the one a window that recycles behind itself depends on.
		k := turnAt(starts, afi) + 1
		if k < len(starts) {
			lo, hi = starts[k], edge(k)
			for k+1 < len(starts) && edge(k+1)-lo <= limit {
				k++
				hi = edge(k)
			}
		} else {
			// after lies inside the last turn: nothing newer than it exists.
			lo, hi = n, n
		}
	case o.Before != "":
		// The turn containing before belongs to the newer page whatever the id points at, so the
		// page ends at the turn edge at or before it. Then whole turns are added towards the past.
		k := turnAt(starts, bi) - 1
		if k >= 0 {
			hi = starts[k+1]
			for k > 0 && hi-starts[k-1] <= limit {
				k--
			}
			lo = starts[k]
		} else {
			// before lies inside the first turn: nothing older than it exists.
			lo, hi = 0, 0
		}
	default:
		k := len(starts) - 1
		hi = n
		for k > 0 && hi-starts[k-1] <= limit {
			k--
		}
		lo = starts[k]
	}
	p.Entries = path[lo:hi]
	p.HasOlder = lo > 0
	p.HasNewer = hi < n
	p.Truncated = hi-lo > limit
	return p, nil
}

// turnStarts lists the positions in path a page may begin at: the start of every turn, plus position
// zero. A page may always begin at the start of the path even when the log opens with something that
// is not a user message, and treating that prefix as one group is what keeps every path cuttable.
func turnStarts(path []*Entry) []int {
	starts := []int{0}
	for i, e := range path {
		if i > 0 && IsTurnStart(e) {
			starts = append(starts, i)
		}
	}
	return starts
}

// turnAt returns the index in starts of the turn that holds position i. starts is ascending and
// always holds 0, so the answer always exists.
func turnAt(starts []int, i int) int {
	lo, hi := 0, len(starts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if starts[mid] <= i {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// indexOf finds an entry of this path by id, or -1 when the path does not contain it. Off-path
// branches are not in a path, so an id from another branch is reported the same way.
func indexOf(path []*Entry, id string) int {
	if id == "" {
		return -1
	}
	for i, e := range path {
		if e != nil && e.ID == id {
			return i
		}
	}
	return -1
}
