package ir

import (
	"strings"
	"time"
	"unicode/utf8"
)

// MaxTurnRows is the ceiling on how many turns one turn list carries, and TurnsOf is the only place it
// is applied.
//
// Not named MaxTurns, whatever the two words suggest: the agent package already owns that name for how
// many times one run may ask the model a question, which has nothing to do with how many rows a list
// may hold.
//
// The ceiling exists so that "list the turns of this session" has an answer with a size whatever the
// session has grown into: a row is an id, a kind, a timestamp and a sentence, so the response is
// bounded by how many turns there are rather than by how much was typed. Asking for more than the
// ceiling is not an error — the question has an answer, just not all of it — so the list comes back
// full and marked clamped, with HasOlder saying the path continues before its first row.
const MaxTurnRows = 500

// MaxTurnSnippet is how many runes of a first line a snippet may carry. Runes and not bytes because
// the transcripts kept here are Chinese: a byte budget cuts a character in half and hands the caller
// a replacement character to draw.
const MaxTurnSnippet = 120

// Turn is one turn of the path, reduced to what a ruler needs: where to jump, when it happened, and
// what it says. Nothing else travels — a ruler over a session of hundreds of turns is a list of
// handles, and the entry behind a handle is one request away.
type Turn struct {
	// ID is the entry to jump to: the first entry of the turn. It is a legal page boundary by
	// construction, because the walk below stops only where IsTurnStart says a page may begin (or at
	// the start of the path), so a client that jumps here and then pages around this id never splits
	// a tool call from its result.
	ID string `json:"id"`
	// Kind is that entry's kind: "user", or "compaction" for a summary, which starts a turn of its
	// own and draws as a banner instead of a message.
	Kind string `json:"kind"`
	// Ts is that entry's timestamp. Which day a turn happened on is what the transcript groups by.
	Ts time.Time `json:"ts"`
	// Snippet is the first line of the entry, cut at MaxTurnSnippet runes with an ellipsis added where
	// it was cut. Empty means the entry has no text at all; a message that is only an attachment
	// reports the attachment's name, or "image" when it has none, because an attachment is something
	// the user sent and a blank row would claim it was not.
	Snippet string `json:"snippet"`
}

// TurnList is the turns of a path, taken from the newest end when they do not all fit.
type TurnList struct {
	// Turns are in path order, oldest first — the order the transcript draws them in.
	Turns []Turn
	// Total is how many turns the whole path holds, clamped list or not.
	Total int
	// HasOlder marks a path that continues before the first turn of this list.
	HasOlder bool
	// Limit is the ceiling in force: the size the caller asked for, or MaxTurnRows when it asked for no
	// particular size. Clamped marks a request for more than the ceiling.
	Limit   int
	Clamped bool
}

// TurnsOf lists one row per turn of a path in root-to-tip order.
//
// limit counts turns, taken from the newest end; zero asks for as many as the ceiling allows. Taking
// them from the newest end is not a preference but the same direction the transcript itself opens in,
// so the rows a clamped list drops are the ones nobody was looking at.
//
// Nothing here can fail: a turn is a position in the path, so there is no id to resolve and no
// combination of arguments to refuse.
func TurnsOf(path []*Entry, limit int) *TurnList {
	l := &TurnList{Limit: MaxTurnRows, Turns: []Turn{}}
	if limit > 0 {
		l.Limit = limit
		if limit > MaxTurnRows {
			l.Limit = MaxTurnRows
			l.Clamped = true
		}
	}
	if len(path) == 0 {
		// why answered with no turns rather than one row: turnStarts always offers position 0 as a
		// boundary because every path has to be cuttable, and a path with nothing on it has no entry to
		// put in that row — a row with no id would be a jump target no client can draw.
		return l
	}
	starts := turnStarts(path)
	l.Total = len(starts)
	if len(starts) <= l.Limit {
		l.Turns = turnRows(path, starts)
		return l
	}
	// Whole turns are dropped from the old end and never cut in half: what comes back is a suffix of
	// the turn list, so it still concatenates with the turns above it.
	l.Turns = turnRows(path, starts[len(starts)-l.Limit:])
	l.HasOlder = true
	return l
}

// turnRows builds one row per turn start. starts holds positions in path, ascending.
func turnRows(path []*Entry, starts []int) []Turn {
	out := make([]Turn, 0, len(starts))
	for _, i := range starts {
		e := path[i]
		if e == nil {
			// why skipped rather than trusted: turnStarts walks the path asking IsTurnStart, which reads
			// a nil entry as "not a turn start", so the only position that can hold one is 0 — and a
			// row with no id would be a jump target no client can draw.
			continue
		}
		out = append(out, Turn{ID: e.ID, Kind: e.Kind, Ts: e.Ts, Snippet: snippetOf(e)})
	}
	return out
}

// TurnsSpine is TurnsOf over the spine, for a session that keeps its conversation
// on disk: the rows' handles, kinds and timestamps come off the spine, and the one
// lookup function is asked for exactly the turn-start entries the kept rows will
// quote — the snippets and nothing else. A lookup that cannot name an id (an entry
// deleted between the cut and the read) answers the row with an empty snippet: the
// ruler's jump target is the id, and the id is still true.
func TurnsSpine(path []*Meta, limit int, contents func(ids []string) (map[string]*Entry, error)) (*TurnList, error) {
	l := &TurnList{Limit: MaxTurnRows, Turns: []Turn{}}
	if limit > 0 {
		l.Limit = limit
		if limit > MaxTurnRows {
			l.Limit = MaxTurnRows
			l.Clamped = true
		}
	}
	if len(path) == 0 {
		// why answered with no turns rather than one row: turnStarts always offers position 0 as a
		// boundary because every path has to be cuttable, and a path with nothing on it has no entry to
		// put in that row — a row with no id would be a jump target no client can draw.
		return l, nil
	}
	starts := turnStartsM(path)
	l.Total = len(starts)
	if len(starts) > l.Limit {
		// Whole turns are dropped from the old end and never cut in half: what comes back is a suffix
		// of the turn list, so it still concatenates with the turns above it.
		starts = starts[len(starts)-l.Limit:]
		l.HasOlder = true
	}
	ids := make([]string, 0, len(starts))
	for _, i := range starts {
		ids = append(ids, path[i].ID)
	}
	snips, err := contents(ids)
	if err != nil {
		return nil, err
	}
	for k, i := range starts {
		m := path[i]
		row := Turn{ID: m.ID, Kind: m.Kind, Ts: m.Ts}
		if e, ok := snips[ids[k]]; ok && e != nil {
			row.Snippet = snippetOf(e)
		}
		l.Turns = append(l.Turns, row)
	}
	return l, nil
}

// snippetOf returns the first line of what an entry has to say, cut at MaxTurnSnippet runes.
func snippetOf(e *Entry) string {
	// TrimSpace first: what it removes is what a pasted prompt brings along (a leading blank line,
	// trailing spaces), and a snippet that begins with whitespace draws as an empty tooltip.
	text := strings.TrimSpace(e.Content)
	if text == "" {
		text = partText(e.Parts)
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		// \r survives the cut and is trimmed with the spaces: it is the other half of a pair typed on
		// Windows, and left at the end of a line it draws as a stray box.
		text = strings.TrimRight(text[:i], " \t\r")
	}
	if len(text) > MaxTurnSnippet {
		// Bytes as a cheap "definitely short enough" test first: UTF-8 never takes fewer bytes than
		// runes. The walk then stops the moment the budget is spent, because a first line with no
		// newline in it can be as long as the message.
		off, seen := 0, 0
		for off < len(text) && seen < MaxTurnSnippet {
			_, w := utf8.DecodeRuneInString(text[off:])
			off += w
			seen++
		}
		// The ellipsis says the line went on. Without it a snippet that stops mid-word reads as if the
		// message had ended there.
		if off < len(text) {
			text = text[:off] + "…"
		}
	}
	return text
}

// partText finds what an entry has to say when its content is empty: the first text part, or the
// name of the first image, or that there is an image. A multi-part tool result keeps its text in the
// parts and never in content; a user message keeps its text in content and its images in parts.
func partText(parts []Part) string {
	for _, p := range parts {
		if t := strings.TrimSpace(p.Text); p.Type == PartText && t != "" {
			return t
		}
	}
	for _, p := range parts {
		if p.Type == PartImage {
			if p.Name != "" {
				return p.Name
			}
			// The name of the kind is the word a caller would draw for it, so the row says what is
			// there instead of inventing a sentence about it.
			return PartImage
		}
	}
	return ""
}
