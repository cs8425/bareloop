package compact

// Serial (rolling) summarisation of a region that does not fit the model window in one piece.
//
// Why this exists: the summary request is an ordinary completion, so it is bound by the same
// n_ctx as the turn it is trying to shrink. A session that grew past the window therefore reaches
// a deadlock - too big to send, and too big to summarise - which turns one oversized message into
// a permanently unusable session (observed: a 185k-token region on a 40960-token server, where
// even a manual compaction answered "request (185164 tokens) exceeds the available context size").
//
// The fix is to summarise in series, the way a person would read a long document in instalments:
//
//	part 1                -> cmp1
//	cmp1 + part 2          -> cmp2
//	cmp2 + part 3          -> cmp3        ... until the parts run out
//
// Each call is bounded by the window: instruction + previous partial + chunk + max_tokens <= n_ctx
// (that budget is ChunkFit). Only the region the plan already chose to replace is cut into parts;
// the kept suffix is never touched. Acceptance is unchanged: the final summary must satisfy
// Accepts (<= keepTarget% of n_ctx), so the chain cannot quietly grow.
//
// Serial on purpose. This is a local-first tool: the same GPU answers every call, so K parallel
// summary calls do not finish K times faster - they queue, and each one competes for the KV cache
// the next one is reusing. A parallel map-reduce variant (summarise the parts independently, then
// merge the parts) is therefore left as an explicit option rather than the default:
//
//	summaryMode: "map-reduce"   // NOT IMPLEMENTED YET - runs the serial chain and says so in the log
//
// It is worth implementing only when the summary model is a different, remote endpoint, where the
// calls really do run in parallel.

import (
	"fmt"
	"strings"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

// Extra summary modes. ModeIterative forces the serial chain even when everything would fit; the
// automatic fallback picks it whenever the region does not fit one call (NeedsIterative).
const (
	// ModeIterative is the serial rolling summary described above.
	ModeIterative = "iterative"
	// ModeMapReduce is reserved for the parallel variant (see the note above). Until it exists
	// it behaves like ModeIterative and logs one line, so a config can be written ahead of time.
	ModeMapReduce = "map-reduce"
	// InstructionAllowance is what one chunk call reserves for the instruction and the transcript
	// scaffolding ("Conversation to compress:", the part counter, ...). Generous on purpose: this
	// number is subtracted from the window on every call.
	InstructionAllowance = 1200
)

// ChunkMaxTokens is max_tokens for one call of the chain: the acceptance cap, because an answer
// longer than that is thrown away by Accepts anyway, and never more than a quarter of the window -
// the part has to fit next to it.
func (p Params) ChunkMaxTokens(nctx int) int {
	if nctx <= 0 {
		return 0
	}
	out := p.SummaryCap(nctx)
	if quarter := nctx / 4; out > quarter {
		out = quarter
	}
	if out < 64 {
		out = 64
	}
	return out
}

// ChunkFit is how much conversation one call of the chain may carry:
// n_ctx - max_tokens - (the previous partial summary, asked to stay at most half the cap) - the
// instruction allowance. The floors below only apply when they still fit that arithmetic: a fit
// that quietly exceeds the window is the exact bug this function exists to prevent.
func (p Params) ChunkFit(nctx int) int {
	if nctx <= 0 {
		return 0
	}
	out := p.ChunkMaxTokens(nctx)
	rest := out + out/2 + InstructionAllowance
	fit := nctx - rest
	if fit < nctx/8 && nctx/8+rest <= nctx {
		fit = nctx / 8
	}
	if fit < 512 && 512+rest <= nctx {
		fit = 512
	}
	if fit < 0 {
		return 0
	}
	return fit
}

// entryTokens is the size used for cutting: the server-derived number when the size table has one,
// otherwise the assembler's estimate (the same estimate the guard uses).
func entryTokens(e *ir.Entry, sizes *asm.Sizes) int {
	if e == nil {
		return 0
	}
	if sizes != nil {
		if t := sizes.Tokens(e.ID); t > 0 {
			return t
		}
	}
	return asm.Estimate(e)
}

// RegionTokens is the size of a region under the same rule.
func RegionTokens(region []*ir.Entry, sizes *asm.Sizes) int {
	total := 0
	for _, e := range region {
		total += entryTokens(e, sizes)
	}
	return total
}

// NeedsIterative reports whether the region cannot be summarised in one call. It is the automatic
// fallback: normal compaction keeps its prefix-stable, cache-friendly shape.
func NeedsIterative(region []*ir.Entry, sizes *asm.Sizes, nctx int, p Params) bool {
	fit := p.ChunkFit(nctx)
	if fit <= 0 || len(region) == 0 {
		return false
	}
	return RegionTokens(region, sizes) > fit
}

// Chunk is one instalment of the region.
type Chunk struct {
	Entries []*ir.Entry
	// Clipped marks a chunk that contains an entry shortened by ClipEntry.
	Clipped bool
}

// PlanChunks cuts the region into serial parts of at most ChunkFit tokens.
//
// Two rules the cut must respect:
//   - a part never starts with a tool result, because the assistant message that asked for it
//     would live in the previous part (the same legality rule as the compaction boundary, §2.2);
//   - a single entry bigger than the fit is not abandoned: it is clipped, because "one pasted
//     200k-token file" is exactly the case that bricks a session.
func PlanChunks(region []*ir.Entry, sizes *asm.Sizes, nctx int, p Params) []Chunk {
	fit := p.ChunkFit(nctx)
	if fit <= 0 || len(region) == 0 {
		return nil
	}
	var out []Chunk
	for i := 0; i < len(region); {
		if t := entryTokens(region[i], sizes); t > fit {
			clip, cut := ClipEntry(region[i], fit)
			out = append(out, Chunk{Entries: []*ir.Entry{clip}, Clipped: cut})
			i++
			continue
		}
		start, sum, end := i, 0, i
		for end < len(region) {
			t := entryTokens(region[end], sizes)
			if end > start && sum+t > fit {
				break
			}
			sum += t
			end++
		}
		if end >= len(region) {
			out = append(out, Chunk{Entries: region[start:end]})
			break
		}
		// Pull back to a legal start for the next part: the part must not begin with a tool result.
		cut := end
		for cut > start+1 && !LegalBoundary(region[cut]) {
			cut--
		}
		if cut <= start {
			cut = start + 1 // nothing legal inside: carry one entry anyway, progress beats purity
		}
		out = append(out, Chunk{Entries: region[start:cut]})
		i = cut
	}
	return out
}

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

// ChunkRequest builds one call of the chain: [system: instruction][user: partial summary + part].
//
// prev is the summary produced by the previous call ("" for the first). The wording matters twice
// over: the model must fold the new part into the partial summary instead of summarising only the
// part, and it must keep the running summary short, or the next call has no room left for its part.
func (p Params) ChunkRequest(nctx int, chunk []*ir.Entry, prev string, index, total int, split bool, extra string) SummaryCall {
	instr := p.SummaryInstruction(split)
	if total > 1 {
		instr += fmt.Sprintf("\n\nThis is part %d of %d of one long conversation that does not fit the model window in one request. %s",
			index, total, "Fold it together with the partial summary you are given and answer with the complete summary so far - not just this part. Keep the running summary under a third of the length of what you have read so far, because your answer is fed into the next request.")
	}
	if add := strings.TrimSpace(extra); add != "" {
		instr += "\n\nAdditional instruction from the user: " + add
	}
	var sb strings.Builder
	if strings.TrimSpace(prev) != "" {
		sb.WriteString("Summary of everything handled so far:\n\n")
		sb.WriteString(prev)
		sb.WriteString("\n\nNew part of the conversation to fold in:\n\n")
	} else {
		sb.WriteString("Conversation to compress:\n\n")
	}
	sb.WriteString(Transcript(chunk))
	return SummaryCall{
		Messages: []asm.Message{
			{Role: "system", Content: instr},
			{Role: "user", Content: sb.String()},
		},
		MaxTokens:   p.ChunkMaxTokens(nctx),
		Temperature: p.Temperature,
	}
}

// ChunkInstructionNote is what the compaction entry says about the method, so that a later reader
// of the log can tell a summary of everything from a summary of the parts the model could read.
func ChunkInstructionNote(chunks int, clipped bool) string {
	if chunks <= 1 && !clipped {
		return ""
	}
	n := fmt.Sprintf("summarised in %d serial parts", chunks)
	if clipped {
		n += ", one or more oversized messages were read only at their beginning and end"
	}
	return n
}
