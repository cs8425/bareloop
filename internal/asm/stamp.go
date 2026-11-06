package asm

// Measuring what each entry costs to send.
//
// The server counts one number per request. Two requests that differ by one message differ by that
// message's cost, so walking the path in the order the requests were actually made turns every
// prompt count into a measurement of the entries between it and the count before it. This is the
// only place that subtraction happens, and it writes its answer on the entry — which is the whole
// point: a later compaction rewrites the front of the conversation, and the cost of the messages
// that survived it does not become unknown again.

import "bareloop/internal/ir"

// noPartner marks a walk that has not reached a counted request yet.
const noPartner = -1

// Stamp measures the entries of a path that a server has counted, in the order the requests were
// made, and writes each entry's own token cost on it. Entries already measured are left alone, and
// entries no request has ever carried (an unsent tail, a branch nobody asked) stay unmeasured for
// the estimate to speak for. headFloor is the estimated size of the system prompt and tool schemas,
// which sit in front of every counted prompt and belong to no entry; structuredCap is the assembler's
// cap, so an entry's estimate is weighted the way it would be sent.
//
// It returns how many entries it measured, which is how a caller can tell a first pass from a no-op.
func Stamp(path []*ir.Entry, headFloor, structuredCap int) int {
	stamped := 0
	partner, start := noPartner, 0
	var partnerPrompt, partnerAnswer int
	// unbase says the last prompt that was counted was counted against a payload this walk never saw,
	// so it has no front to subtract from. It is true after a compaction and until one more request has
	// been counted: from there the differences are ordinary again.
	unbase := false
	for i, e := range path {
		if e == nil {
			continue
		}
		if e.IsCompaction() {
			// A summary's own cost is the completion the summariser was charged for, which the record of
			// the compaction already carries (CompactionMeta.SummarySize, in tokens) — so it is read here
			// like any other measured number rather than estimated from the length of the text.
			if e.Tokens == 0 && e.Compaction != nil && e.Compaction.SummarySize > 0 {
				e.Tokens = e.Compaction.SummarySize
				stamped++
			}
			// And this is where one telescope ends and another begins: what a later prompt contains is
			// the summary and what was kept, not the conversation above this line, so subtracting across
			// it would measure nothing at all.
			partner, start, unbase = noPartner, i+1, true
			partnerPrompt, partnerAnswer = 0, 0
			continue
		}
		if !counted(e) {
			continue
		}
		answer := 0
		if e.Usage.HasCompletion {
			answer = e.Usage.CompletionTokens
		}
		// What this request added to the prompt is the cost of everything sent since the count before
		// it — and whatever that stretch has already been measured at is not owed twice.
		stretch := path[start:i]
		owed := e.Usage.PromptTokens - headFloor
		switch {
		case partner >= 0:
			owed = e.Usage.PromptTokens - partnerPrompt - partnerAnswer
		case unbase:
			owed = -1
		}
		owed -= measured(stretch)
		if owed >= 0 {
			stamped += spread(stretch, owed, structuredCap)
		}
		if e.Tokens == 0 && answer > 0 {
			e.Tokens = answer
			stamped++
		}
		partner, partnerPrompt, partnerAnswer = i, e.Usage.PromptTokens, answer
		start, unbase = i+1, false
	}
	return stamped
}

// counted reports whether a server counted this entry's request, which is what makes it the far
// end of a subtraction.
func counted(e *ir.Entry) bool {
	return e.Kind == ir.KindAssistant && e.Usage != nil && e.Usage.HasPrompt
}

// measured is what a stretch of entries already costs, measured or not.
func measured(stretch []*ir.Entry) int {
	n := 0
	for _, e := range stretch {
		if e != nil {
			n += e.Tokens
		}
	}
	return n
}

// spread writes owed tokens across the entries of a stretch that have no number yet, weighted by
// what each one looks like it costs. The weight is only the split of a measured total: it never
// invents the total, and every entry with something in it is left worth at least a token.
func spread(stretch []*ir.Entry, owed, structuredCap int) int {
	weight := make([]int, len(stretch))
	has := make([]bool, len(stretch))
	stampable := 0
	total := 0
	for i, e := range stretch {
		if e == nil || e.Tokens > 0 {
			continue // already counted, or nothing to write on
		}
		content := e.Content != "" || len(e.ToolCalls) > 0 || len(e.Parts) > 0 || len(e.Structured) > 0
		if !content {
			continue
		}
		has[i] = true
		weight[i] = Estimate(e, structuredCap)
		if weight[i] < 1 {
			weight[i] = 1
		}
		stampable++
		total += weight[i]
	}
	if stampable == 0 || owed <= 0 {
		return 0
	}
	remaining := owed
	big, bigWeight := 0, 0
	for i := range stretch {
		if !has[i] {
			continue
		}
		n := owed * weight[i] / total
		if n < 1 {
			n = 1
		}
		if n > remaining {
			n = remaining
		}
		stretch[i].Tokens = n
		remaining -= n
		if weight[i] > bigWeight {
			big, bigWeight = i, weight[i]
		}
	}
	// The remainders of an integer split are real tokens too: they belong to the heaviest entry of
	// the stretch rather than nowhere.
	if remaining > 0 && big >= 0 {
		stretch[big].Tokens += remaining
	}
	return stampable
}
