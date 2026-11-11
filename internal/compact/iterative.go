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
// Accepts (<= summaryCap% of n_ctx), so the chain cannot quietly grow.
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

// ChunkMaxTokens is max_tokens for one call of the chain — SummaryCap itself, the one number
// that sizes the single summary call too: a chain is that question asked about one part of the
// conversation at a time, and the spec asks nothing more of it. A summary written over parts is
// therefore not allowed to be thinner than the one a single call would have written, and it is not
// allowed to be thicker either: the cap that decides whether the summary is worth keeping is the
// same number applied to the final answer, never to an intermediate one — the two used to be
// conflated here, and the quarter of the window this clamped to before that charged the window for
// its own answer twice. On an 8k window the clamp landed below one honest round of thinking: the
// model spent all 2048 completion tokens reasoning and returned no summary text at all.
func (p Params) ChunkMaxTokens(nctx int) int {
	return p.SummaryCap(nctx)
}

// ChunkFit is where a part of the chain stops: the threshold position, threshold% × n_ctx — the same
// number that decided this conversation needs a summary at all, so a part is "the conversation as the
// loop would have sent it, up to the message that tipped it past the threshold".
//
// What it used to be, and why that is not coming back: n_ctx − (max_tokens + max_tokens/2 + an
// instruction allowance), where the half was a guess at how big the previous part's summary would be.
// The previous summary is not a guess — it is text the caller is holding — and reserving for it here
// is how a chain ended up with parts smaller than the window had to offer.
func (p Params) ChunkFit(nctx int) int {
	return p.TriggerLevel(nctx)
}

// Ruler is the size used for cutting.
//
// Why a ruler at all: the size table an assembler builds is keyed by entry id and priced for walking
// one boundary, while cutting a region asks for one entry at a time. The numbers are the same — the
// measurement each entry carries (ir.Entry.Tokens, written by asm.Stamp where a server's own two
// prompt counts made the difference) — and a ruler is simply the reader of them that does not need a
// payload to ask. Only where no server ever counted an entry — the unsent tail of a path, a server
// that reports no usage at all — does an estimate stand on its own (spec §0: a real number where
// there is one, an estimate only for what the server has never seen).
type Ruler struct {
	structuredCap int
}

// NewRuler makes a ruler for a payload whose assembler capped structuredContent at structuredCap.
//
// The cap must be the one the assembler used (asm.Built.StructuredCap): pricing a capped tool
// document at its stored size is the same mistake in the other direction.
func NewRuler(structuredCap int) *Ruler {
	return &Ruler{structuredCap: structuredCap}
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

// Tokens is one entry's size: the number a server counted for it where one counted anything, and an
// estimate only for the entries no request ever carried. A measured zero is kept: a reply the server
// says it generated in no tokens is not going to grow the next request, and pretending otherwise is
// how one long answer reads as a short one.
//
// A floor of 1 is kept for an estimated entry with content: the cutting below walks entries one at a
// time, and an entry that measures 0 never ends a part, so a region of nothing-but-zero entries would
// produce no part at all and the summary would answer "nothing to summarise" over a conversation that
// plainly exists.
func (r *Ruler) Tokens(e *ir.Entry) int {
	if e == nil {
		return 0
	}
	if e.Tokens > 0 {
		return e.Tokens
	}
	n := r.raw(e)
	if n < 1 && (e.Content != "" || len(e.ToolCalls) > 0 || len(e.Parts) > 0 || len(e.Structured) > 0) {
		n = 1
	}
	return n
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

// NeedsIterative is the conservative form of a simpler rule: split only when the request goes over the
// model's window. It takes the size of the one request the single call would put on the wire — the
// caller counts it (agent.Session.summaryRequestSize) because only the caller can see the payload, the
// instruction and the entries that were never sent in the same view.
//
// What it deliberately does NOT look at is the region's priced sum: the ruler overestimates tool
// results (§3.9.1's 20x), and that is exactly how a conversation that fits one request ended up being
// folded in serial parts. Over n_ctx means "this call would be rejected", and the chain is what makes
// a rejected-by-size request into a summary anyway.
//
// The reactive form — send it, split when the provider answers 400 — is the rule this approximates.
// It needs a new error class first: 400 is currently filed "never retry" (§3.9's retry table), so an
// over-long summary would end as an ordinary failure instead of a split.
func NeedsIterative(oneRequest, nctx int) bool { return nctx > 0 && oneRequest > nctx }

// TextTokens prices a string with the reference ruler: four bytes to a token, which is the same
// fallback SummarySize uses when a server counted nothing. An instruction and a summary are therefore
// measured with one stick — the number is only ever compared against a window, never billed.
func TextTokens(s string) int { return (len(s) + 3) / 4 }

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
