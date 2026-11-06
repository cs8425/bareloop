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
	"bareloop/internal/llm"
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

// Ruler is the size used for cutting.
//
// Two numbers get confused here, and this type exists to keep them apart. What the provider counted,
// reply by reply, is on the entry: every assistant entry carries the usage of the request that produced
// it (ir.Entry.Usage), and the difference between one reply's prompt count and the next, less the
// answer in between, is exactly how much the conversation grew on the wire in between - a real number,
// per entry, with no estimate in it anywhere. What one entry costs on its own is a different question,
// which no server answers, because an entry does not have one: a prompt is a prefix, and its cost
// belongs to the request.
//
// Why the size table cannot do this job: asm.DeriveSizes reads that same usage, gets the arithmetic
// right, and then attributes a whole segment's growth to that segment's LAST entry while zeroing the
// others - the right table for walking one boundary, where only the cumulative sum is ever read. Used
// as the size of ONE entry it prices the same text twice, and a cut made from it was measured: a region
// reported as 314,524 tokens for a payload the server had just measured at 184,383, and serial parts of
// 4,378 / 4,416 / 80,641 prompt tokens, because one entry carrying a whole request's spike becomes its
// own part while everything measured at zero piles up in the last one.
//
// So the ruler keeps the measured numbers and fixes the attribution: a segment's measured growth is
// spread over the entries that caused it, weighted by how big each one looks, and an assistant entry
// costs what the server said it generated. Only where the server never counted anything - the unsent
// tail of a path, or a server that reports no usage at all - does an estimate stand on its own, and even
// that one is calibrated against the request total when a total is known (spec §0: a real number where
// there is one, an estimate only for what the server has never seen).
type Ruler struct {
	structuredCap int
	// marginal is what the server counted, per entry: one segment's measured growth shared over the
	// entries that made it, and an assistant reply's own completion tokens. Keyed by pointer, because a
	// region is a slice of these very entries, so no id lookup is needed to agree about which entry is
	// being sized.
	marginal map[*ir.Entry]int
	// k scales the entries the server never counted; 1 when nothing was ever measured either.
	k float64
}

// NewRuler reads the sizes out of the payload the request was built from: the growth the server
// measured between its own replies, and an estimate only for what it never counted.
//
// structuredCap must be the ceiling the assembler used (asm.Built.StructuredCap): pricing a capped
// tool document at its stored size is the same mistake in the other direction.
func NewRuler(all []*ir.Entry, structuredCap, realTotal int) *Ruler {
	r := &Ruler{structuredCap: structuredCap, marginal: make(map[*ir.Entry]int, len(all)), k: 1}
	raw := make([]int, len(all))
	for i, e := range all {
		raw[i] = r.raw(e)
	}

	// Anchors are the replies the server measured. Between two of them the prompt counts say exactly
	// how much the conversation grew, which is the number a cut needs; the cached prefix is already
	// inside PromptTokens (ir.Usage), so CacheTokens is not added to it.
	prevPrompt, prevAnswer, start := -1, 0, 0
	for i, e := range all {
		if e == nil || e.Kind != ir.KindAssistant || e.Usage == nil || !e.Usage.HasPrompt {
			continue
		}
		attrib := e.Usage.PromptTokens
		if prevPrompt >= 0 {
			attrib = e.Usage.PromptTokens - prevPrompt - prevAnswer
		}
		if attrib < 0 {
			// The path moved between the two requests (a rollback, a deleted entry), so these two numbers
			// are not about the same conversation. Leave that segment to the estimates rather than let a
			// negative number cancel the size of a part.
			attrib = -1
		}
		if attrib >= 0 {
			spread(r.marginal, all[start:i], raw[start:i], attrib)
		}
		answer := 0
		if e.Usage.HasCompletion {
			answer = e.Usage.CompletionTokens
		}
		r.marginal[e] = answer
		prevPrompt, prevAnswer, start = e.Usage.PromptTokens, answer, i+1
	}

	// realTotal is the prompt size the server counted for the request that carried `all`. It calibrates
	// the estimates only when nothing was counted per entry, because when the replies are measured the
	// total is derivable from them and adds nothing: summing the measured entries IS the last request's
	// context. Scaling the unsent tail down to make that sum land on the total would price the very
	// message that is about to be sent at nothing.
	if len(r.marginal) == 0 && realTotal > 0 {
		est := 0
		for _, n := range raw {
			est += n
		}
		if est > 0 {
			r.k = float64(realTotal) / float64(est)
		}
	}
	return r
}

// spread shares one segment's measured growth over the entries of that segment, weighted by how big
// each one looks, because that is the only question an estimate is still good for: not how many tokens
// an entry is worth on its own, but which of them the tokens were spent on.
//
// The sum is preserved exactly, with the integer division's remainder going to the largest entry, for
// two reasons: a ruler that loses a few tokens per segment is the same bug in miniature, and a ruler
// that leaves the remainder on the last entry is the bug this function replaces.
func spread(dst map[*ir.Entry]int, seg []*ir.Entry, raw []int, attrib int) {
	if len(seg) == 0 || len(seg) != len(raw) {
		return
	}
	total := 0
	for _, n := range raw {
		if n > 0 {
			total += n
		}
	}
	given, big := 0, -1
	for i, e := range seg {
		if e == nil {
			continue
		}
		n := attrib / len(seg)
		if total > 0 {
			n = attrib * raw[i] / total
		}
		if n < 1 && raw[i] > 0 {
			n = 1 // an entry that exists is not free, whatever the division says
		}
		dst[e] = n
		given += n
		if big < 0 || raw[i] > raw[big] {
			big = i
		}
	}
	if rest := attrib - given; rest > 0 && big >= 0 && seg[big] != nil {
		dst[seg[big]] += rest
	}
}

// raw is the uncapped estimate: what the entry will cost on the wire, including a structuredContent
// document up to the ceiling the assembler honours. A nil ruler asks the assembler for its default
// ceiling, which is what the tests and any caller that knows nothing about the payload want.
func (r *Ruler) raw(e *ir.Entry) int {
	if e == nil {
		return 0
	}
	if r == nil {
		return asm.Estimate(e, 0)
	}
	return asm.Estimate(e, r.structuredCap)
}

// Tokens is one entry's size: what the server counted for it where it counted anything, and a
// calibrated estimate only where it did not. A floor of 1 is kept for an estimated entry with content:
// the cutting below walks entries one at a time, and an entry that measures 0 never ends a part, so a
// region of nothing-but-zero entries would produce no part at all and the summary would answer
// "nothing to summarise" over a conversation that plainly exists.
func (r *Ruler) Tokens(e *ir.Entry) int {
	if e == nil {
		return 0
	}
	if r == nil {
		return r.raw(e)
	}
	// A measured zero is kept: a reply the server says it generated in no tokens is not going to grow
	// the next request, and pretending otherwise is how one long answer reads as a short one.
	if v, ok := r.marginal[e]; ok {
		return v
	}
	n := r.raw(e)
	if r.k == 1 {
		return n
	}
	out := int(float64(n) * r.k)
	if out < 1 && n > 0 {
		out = 1
	}
	return out
}

// RegionTokens is the ruler's size of a region — the number that decides whether one summary call
// can carry it (§ NeedsIterative) and where the parts are cut. It is NOT the size table's sum, and
// it is the whole reason those two stopped being the same number.
func RegionTokens(region []*ir.Entry, ruler *Ruler) int {
	total := 0
	for _, e := range region {
		total += ruler.Tokens(e)
	}
	return total
}

// NeedsIterative reports whether the region cannot be summarised in one call. It is the automatic
// fallback: normal compaction keeps its prefix-stable, cache-friendly shape.
func NeedsIterative(region []*ir.Entry, ruler *Ruler, nctx int, p Params) bool {
	fit := p.ChunkFit(nctx)
	if fit <= 0 || len(region) == 0 {
		return false
	}
	return RegionTokens(region, ruler) > fit
}

// Chunk is one instalment of the region.
type Chunk struct {
	Entries []*ir.Entry
	// Start is where Entries begins inside the region PlanChunks was handed. The parts are
	// contiguous by construction, and the caller needs the offset to cut the payload's messages at
	// the same boundary — entry number k is message number k, once the system head is accounted for.
	Start int
	// Tokens is the ruler's size of what this part will actually carry, so a log line can put it
	// next to the prompt tokens the server reported. When the two disagree by 20x, that disagreement
	// is the bug, and it used to be invisible.
	Tokens int
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
func PlanChunks(region []*ir.Entry, ruler *Ruler, nctx int, p Params) []Chunk {
	fit := p.ChunkFit(nctx)
	if fit <= 0 || len(region) == 0 {
		return nil
	}
	var out []Chunk
	for i := 0; i < len(region); {
		if t := ruler.Tokens(region[i]); t > fit {
			clip, cut := ClipEntry(region[i], fit)
			out = append(out, Chunk{Entries: []*ir.Entry{clip}, Start: i, Tokens: ruler.Tokens(clip), Clipped: cut})
			i++
			continue
		}
		start, sum, end := i, 0, i
		for end < len(region) {
			t := ruler.Tokens(region[end])
			if end > start && sum+t > fit {
				break
			}
			sum += t
			end++
		}
		if end >= len(region) {
			out = append(out, Chunk{Entries: region[start:end], Start: start, Tokens: sum})
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
		// The pull-back makes the sum a lie about the part that ships, and the number is only ever
		// read out of a log line next to a real one, so it is recomputed over what is kept.
		sum = 0
		for _, e := range region[start:cut] {
			sum += ruler.Tokens(e)
		}
		out = append(out, Chunk{Entries: region[start:cut], Start: start, Tokens: sum})
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

// ChunkInput is what one call of the chain carries. The type exists because the shape is the whole
// point: a summary request that does not begin with the bytes the loop already sent pays for the
// conversation again, and the measurement says so — a chain of three parts over a 184,383-token
// payload reused 0 prompt tokens on every call, on a server that had just reused 171,147 of that
// same payload for the turn before it.
type ChunkInput struct {
	// Head is the payload's system head, alone. Every part after the first opens with it, byte for
	// byte, because it is the only thing two parts of one chain can share.
	Head []asm.Message
	// Prefix is part 1 only: the payload messages up to and including this part — the spec's
	// [cmp0?, ...msgX, summary_prompt]. It is the chunk AND the cache hit, so nothing may go in
	// front of it: not a system message, not the instruction, not a re-rendered transcript.
	Prefix []asm.Message
	// Chunk is this part's payload messages, for every part after the first.
	Chunk []asm.Message
	// Tools is exactly the array the loop sends. A chat template renders the tool definitions at the
	// start of the prompt, so a missing or reordered array forks at token 0 — which is how a summary
	// asked with no tools becomes a different prompt from the turn it is summarising.
	Tools []llm.Tool
	// Prev is the running summary the previous call answered with ("" for part 1).
	Prev string
	// Index and Total name this part. The sentence naming them belongs at the TAIL of the request
	// (see ChunkRequest): the same words in the head stop the parts from sharing a prefix.
	Index, Total int
	Split        bool
	// Extra is the caller's own instruction — a manual summary's text, or a retry's "your last
	// attempt was N tokens and the cap is M".
	Extra string
}

// ChunkRequest builds one call of the chain:
//
//	part 1   [ ...the payload prefix... ][user: instruction]
//	part k   [system head][user: the running summary][ ...this part's messages... ][user: instruction]
//
// The messages are the payload's own messages rather than a rendering of the entries, for two
// reasons that are the same reason: a rendering does not match the bytes the provider cached, and a
// rendering of an entry prints only its text — so every tool result whose answer travels in
// structuredContent arrived as nothing at all, while the ruler that cut the part had counted those
// bytes as tokens.
//
// prev matters twice over: the model must fold the new part into the running summary instead of
// summarising only the part, and it must keep that summary short, or the next call has no room left
// for its part.
func (p Params) ChunkRequest(nctx int, in ChunkInput) SummaryCall {
	msgs := make([]asm.Message, 0, len(in.Head)+len(in.Prefix)+len(in.Chunk)+2)
	if len(in.Prefix) > 0 {
		msgs = append(msgs, in.Prefix...)
	} else {
		msgs = append(msgs, in.Head...)
		if strings.TrimSpace(in.Prev) != "" {
			// The running summary travels with the user role, exactly as a committed one does (§3.7).
			msgs = append(msgs, asm.Message{Role: "user",
				Content: "Summary of everything handled so far:\n\n" + in.Prev})
		}
		msgs = append(msgs, in.Chunk...)
	}
	// The instruction is the LAST message, so everything before it is shared with the request whose
	// cache this call is trying to reuse.
	instr := p.SummaryInstruction(in.Split)
	if in.Total > 1 {
		instr += fmt.Sprintf("\n\nThis is part %d of %d of one long conversation that does not fit the model window in one request. %s",
			in.Index, in.Total, "Fold it together with the partial summary you are given and answer with the complete summary so far - not just this part. Keep the running summary under a third of the length of what you have read so far, because your answer is fed into the next request.")
	}
	if add := strings.TrimSpace(in.Extra); add != "" {
		instr += "\n\nAdditional instruction from the user: " + add
	}
	msgs = append(msgs, asm.Message{Role: "user", Content: instr})
	return SummaryCall{
		Messages:    msgs,
		Tools:       in.Tools,
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
