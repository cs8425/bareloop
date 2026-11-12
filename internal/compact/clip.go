package compact

// Reading a message that does not fit the window.
//
// A summary is asked about bytes, and every so often one of those bytes is a pasted document bigger
// than the model window it must be summarised inside. The compactor cannot ask about it in one piece
// and cannot pretend it was read; the honest answer is to put the beginning and the end in front of
// the model and say out loud, in the request itself, that the middle was not sent. (The serial chain
// this file used to be part of clipped its parts for the same reason; the chain is gone — the
// compactor replays itself over shorter virtual paths instead — and these two helpers are all of it
// that outlived it.)

import (
	"fmt"
	"strings"

	"bareloop/internal/ir"
)

// TextTokens prices a string with the reference ruler: four bytes to a token, which is the same
// fallback SummarySize uses when a server counted nothing. An instruction and a summary are therefore
// measured with one stick — the number is only ever compared against a window, never billed.
func TextTokens(s string) int { return (len(s) + 3) / 4 }

// ClipEntry shortens one oversized entry to roughly budgetTokens worth of text, keeping its head
// and tail - the beginning of a pasted document and its end carry more signal than its middle.
// The marker inside is not cosmetic: the summary must be able to say that a part was omitted,
// otherwise the model invents what it thinks was there.
func ClipEntry(e *ir.Entry, budgetTokens int) (*ir.Entry, bool) {
	if e == nil || budgetTokens <= 0 {
		return e, false
	}
	text := e.Content
	if strings.TrimSpace(text) == "" {
		return e, false
	}
	limit := budgetTokens * 4 // chars, at the same 4-bytes-per-token ratio the estimator uses
	r := []rune(text)
	if len(r) <= limit {
		return e, false
	}
	head := limit * 3 / 5
	tail := limit - head
	if tail < 1 {
		tail = 1
	}
	marker := fmt.Sprintf("\n[... %d characters omitted: this single message does not fit the model window, so the summariser could not read its middle ...]\n", len(r)-head-tail)
	c := *e // shallow copy: the entry in the log is never rewritten
	c.Content = string(r[:head]) + marker + string(r[len(r)-tail:])
	// The transcript renders attachments as placeholders anyway; keeping them costs nothing here.
	return &c, true
}

// ClipOversized clips every entry of a region that ClipEntry can shrink below budgetTokens, and
// reports how many were. It is the flattening pass of the last resort: when even a request cut from
// the payload's own bytes would not fit the window (one message larger than the window is enough),
// the region goes to the summariser as a flat transcript and no entry may ride in it whole just
// because it is long — the request that is refused summarises nothing.
func ClipOversized(region []*ir.Entry, budgetTokens int) ([]*ir.Entry, int) {
	out := make([]*ir.Entry, 0, len(region))
	clipped := 0
	for _, e := range region {
		c, did := ClipEntry(e, budgetTokens)
		if did {
			clipped++
		}
		out = append(out, c)
	}
	return out, clipped
}
