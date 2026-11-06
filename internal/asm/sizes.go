package asm

// Per-entry sizing (spec §2.4).
//
// A server counts tokens per request, never per message, so a message's own cost is only ever a
// difference between two of its counts — and a difference is a fact about one message, not about
// the conversation that happened to precede it. That is what makes the number worth keeping:
// Stamp measures each entry once, along the path in the order the requests were made, and writes
// the result on the entry (ir.Entry.Tokens). Anything reading sizes afterwards — the boundary
// walk, a compaction's "after", a chunk plan — takes the number that was counted instead of
// re-deriving it from a payload that may since have been rewritten by another compaction.
//
// One piece is never counted as itself: the system prompt and the tool schemas sit in front of
// every request, so the first request's prompt contains them and no subtraction can separate them
// from the first message. The caller who can see both hands that floor in as headFloor; it is an
// estimate, it is kept separate from every entry, and it is the only estimated part of a Total.

import "bareloop/internal/ir"

// TokensOf is the bytes/4 estimate of text nobody has counted.
func TokensOf(s string) int { return (len(s) + 3) / 4 }

// Size is one entry's derived size.
type Size struct {
	Tokens int  // measured when Real, otherwise the bytes/4 estimate
	Real   bool // counted by a server
	Bytes  int  // helper column only
}

// Sizes is the size table of one built payload.
type Sizes struct {
	per   map[string]Size
	order []string
	floor int // the head no request count attributes: system prompt and tool schemas
}

// DeriveSizes reads the size table of a payload: every entry's own measured number where a server
// counted one (see Stamp), an estimate of its bytes where nobody has. headFloor is the estimate of
// the system prompt and tool schemas in front of it, which no entry can carry.
func DeriveSizes(b *Built, headFloor int) *Sizes {
	s := &Sizes{per: map[string]Size{}, floor: headFloor}
	for _, e := range b.Entries {
		size := Size{Tokens: e.Tokens, Real: e.Tokens > 0, Bytes: entryBytes(e)}
		if !size.Real {
			size.Tokens = Estimate(e, b.StructuredCap)
		}
		s.per[e.ID] = size
		s.order = append(s.order, e.ID)
	}
	return s
}

// Tokens returns the size used for the boundary walk (measured when there is one, else estimate).
func (s *Sizes) Tokens(id string) int {
	if v, ok := s.per[id]; ok {
		return v.Tokens
	}
	return 0
}

// Get returns the full size row.
func (s *Sizes) Get(id string) (Size, bool) {
	v, ok := s.per[id]
	return v, ok
}

// Head is the estimated floor in front of every payload.
func (s *Sizes) Head() int { return s.floor }

// Order returns the payload entry ids in send order.
func (s *Sizes) Order() []string { return s.order }

// Total is the whole payload: the measured entries, the estimated entries nobody has counted yet,
// and the estimated head. The head is what it was counted for: after a compaction the entries that
// survived carry their own numbers and the deleted ones are simply gone, so the total moves with
// the conversation instead of standing where the last request left it.
func (s *Sizes) Total() int {
	n := s.floor
	for _, id := range s.order {
		n += s.per[id].Tokens
	}
	return n
}

// Suffix returns suffixTokens[i] = sum of tokens of entries i..end, plus the ids.
func (s *Sizes) Suffix() ([]int, []string) {
	suf := make([]int, len(s.order)+1)
	for i := len(s.order) - 1; i >= 0; i-- {
		suf[i] = suf[i+1] + s.Tokens(s.order[i])
	}
	return suf, s.order
}

// AnyEstimate reports whether any entry used the estimate (UI shows "est.").
func (s *Sizes) AnyEstimate() bool {
	for _, v := range s.per {
		if !v.Real {
			return true
		}
	}
	return false
}

func entryBytes(e *ir.Entry) int {
	n := len(e.Content)
	for _, tc := range e.ToolCalls {
		n += len(tc.Name) + len(tc.Args)
	}
	for _, p := range e.Parts {
		n += len(p.Text) + len(p.Data)
	}
	// Stored bytes include a tool's structuredContent, mode or no mode: it is in the log either way.
	// This column is therefore the weight of the entry in the log, which a capped structuredContent
	// can legitimately exceed — the number that follows what goes on the wire is the token estimate,
	// which is the one the guard and the compaction cut read.
	n += len(e.Structured)
	return n
}
