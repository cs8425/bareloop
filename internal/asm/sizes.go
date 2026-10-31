package asm

// Per-message sizing (spec §2.4).
//
// Servers only report counts per request / per assistant turn, so real per-message sizes
// are obtained by subtracting adjacent turns:
//
//	size(assistant i)                = predicted_n[i]
//	real total of a non-assistant segment
//	                                 = (prompt_n[i+1]+cache_n[i+1]) - (prompt_n[i]+cache_n[i]) - predicted_n[i]
//	first segment                    = prompt_n[0] + cache_n[0]
//	system head                      = 0 (already inside the first segment)
//	the unsent tail (after the last assistant) -> no real value -> bytes/4 estimate
//
// A negative segment total means "no data" (typically the previous in-loop compaction
// shrank the prompt), NOT zero: those entries stay absent so consumers fall back to
// estimates instead of showing a fake ~0.

import "bareloop/internal/ir"

// Size is one entry's derived size.
type Size struct {
	Tokens int  // real when Real, otherwise the bytes/4 estimate
	Real   bool // covered by server data
	Bytes  int  // helper column only
}

// Sizes is the size table of one built payload.
type Sizes struct {
	per   map[string]Size
	order []string
}

// DeriveSizes computes the size table from the payload plus the real usage recorded on the
// assistant entries it contains.
func DeriveSizes(b *Built) *Sizes {
	s := &Sizes{per: map[string]Size{}}
	// Mark all entries as estimates first (also covers the unsent tail).
	for _, e := range b.Entries {
		s.per[e.ID] = Size{Tokens: Estimate(e), Real: false, Bytes: entryBytes(e)}
		s.order = append(s.order, e.ID)
	}

	// Anchors = assistant entries with a real prompt count. Everything between two anchors
	// is one segment; its total is attributed to the segment's last entry.
	prevSum := -1
	prevPredicted := 0
	windowStart := 0
	for i, e := range b.Entries {
		if !(e.Kind == ir.KindAssistant && e.Usage != nil && e.Usage.HasPrompt) {
			continue
		}
		// One prompt number: under the ir.Usage semantics the cached prefix is already inside
		// PromptTokens, so adding CacheTokens here would double it.
		segSum := e.Usage.PromptTokens
		var attrib int
		if prevSum < 0 {
			attrib = segSum
		} else {
			attrib = segSum - prevSum - prevPredicted
		}
		// Attribute the segment (entries windowStart..i-1) to its last entry.
		if attrib >= 0 && i > windowStart {
			last := b.Entries[i-1]
			s.per[last.ID] = Size{Tokens: attrib, Real: true, Bytes: entryBytes(last)}
			for j := windowStart; j < i-1; j++ {
				id := b.Entries[j].ID
				s.per[id] = Size{Tokens: 0, Real: true, Bytes: entryBytes(b.Entries[j])}
			}
		} else if i > windowStart {
			// No data: keep the estimates for this window (do not write real zeros).
			for j := windowStart; j < i; j++ {
				id := b.Entries[j].ID
				s.per[id] = Size{Tokens: Estimate(b.Entries[j]), Real: false, Bytes: entryBytes(b.Entries[j])}
			}
		}
		pred := 0
		if e.Usage.HasCompletion {
			pred = e.Usage.CompletionTokens
		}
		s.per[e.ID] = Size{Tokens: pred, Real: true, Bytes: entryBytes(e)}
		prevSum = segSum
		prevPredicted = pred
		windowStart = i + 1
	}
	return s
}

// Tokens returns the size used for the boundary walk (real when available, else estimate).
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

// Order returns the payload entry ids in send order.
func (s *Sizes) Order() []string { return s.order }

// Total is the telescoping sum over the payload. Use it only for the boundary walk: after
// an in-loop compaction it inflates relative to the real usage (spec §2.4), so display
// "before/after" numbers must come from the real contextUsed instead.
func (s *Sizes) Total() int {
	n := 0
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
	n += len(e.Structured)
	return n
}
