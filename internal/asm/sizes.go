package asm

// Per-entry sizing (spec §2.4).
//
// A server counts tokens per request, never per message, so a message's own cost is only ever a
// difference between two of its counts — and a difference is a fact about one message, not about
// the conversation that happened to precede it. That is what makes the number worth keeping:
// Stamp measures each entry once, along the path in the order the requests were made, and writes
// the result on the entry (ir.Entry.Tokens). Anything reading sizes afterwards — the boundary
// walk, a compaction's "after", a replay round's plan — takes the number that was counted instead
// of re-deriving it from a payload that may since have been rewritten by another compaction.
//
// One piece is never counted as itself: the system prompt and the tool schemas sit in front of
// every request, so the first request's prompt contains them and no subtraction can separate them
// from the first message. The caller who can see both hands that floor in as headFloor; it is an
// estimate, it is kept separate from every entry, and it is the only estimated part of a Total.
//
// The Wire column answers a question no token column can: what does this entry cost in **bytes**,
// which is what a server refuses with 413 before it has read a single token. It is an estimate by
// summing lengths, and its one non-obvious case is an image: an entry carries a 69-byte reference
// where the wire carries megabytes of base64, so the reference is priced from the blob's stored byte
// count (RefSizes) rather than from its own length. Bytes, the older column, stays what it always
// was — the entry's weight in the log — because those two numbers are used for two different things
// and neither should be quietly redefined as the other.

import (
	"strings"

	"bareloop/internal/ir"
)

// TokensOf is the bytes/4 estimate of text nobody has counted.
func TokensOf(s string) int { return (len(s) + 3) / 4 }

// Size is one entry's derived size.
type Size struct {
	Tokens int  // measured when Real, otherwise the bytes/4 estimate
	Real   bool // counted by a server
	Bytes  int  // helper column only
	// Wire is the entry's estimated weight in a request body — the column a byte ceiling is cut
	// against. Estimated (see entryWire), never measured: no server reports the size of a body it
	// refused to read.
	Wire int
}

// RefSizes answers what a stored blob weighs in raw bytes, by the content address its bytes spell.
// The store answers from a size column and moves no bytes (store.Log.BlobSize), which is the entire
// reason this is affordable to ask once per reference while deriving a table. A caller may cache
// forever: a sha's byte count is a fact about the universe, not about a row.
//
// found=false — a blob this store has never seen — prices the reference at nothing. That is not the
// table pretending an oversized attachment is free; it is a payload that cannot be sent at all, and
// ExpandMessages is the code that says so out loud when the bytes are owed.
type RefSizes interface {
	Size(sha string) (bytes int, found bool)
}

// Sizes is the size table of one built payload.
type Sizes struct {
	per   map[string]Size
	order []string
	floor int // the head no request count attributes: system prompt and tool schemas
	// missing counts the image references no resolver could price, which is what makes a Wire sum a
	// floor rather than an estimate: the bytes are not small, they are unknown.
	missing int
}

// DeriveSizes reads the size table of a payload: every entry's own measured number where a server
// counted one (see Stamp), an estimate of its bytes where nobody has. headFloor is the estimate of
// the system prompt and tool schemas in front of it, which no entry can carry.
//
// The optional RefSizes is what lets the Wire column price image references honestly; without it
// every reference prices at its own 69 bytes, which is the answer for a payload of text and a
// forty-thousand-fold lie for one carrying a screenshot. At most one resolver is read.
func DeriveSizes(b *Built, headFloor int, refs ...RefSizes) *Sizes {
	var r RefSizes
	if len(refs) > 0 {
		r = refs[0]
	}
	s := &Sizes{per: map[string]Size{}, floor: headFloor}
	for _, e := range b.Entries {
		wire, missing := entryWire(e, b, r)
		s.missing += missing
		size := Size{Tokens: e.Tokens, Real: e.Tokens > 0, Bytes: entryBytes(e), Wire: wire}
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

// View is the same price table read through a shorter payload: the same per-entry numbers and the
// same head, ordered by path instead of by the payload this table was derived from. It exists for
// planning against a virtual path — the prefix a replay round pretends the conversation is made of
// (compact.Tipping) — where walking the boundary on the full path's totals would plan a cut that
// never happens. The two tables share one map and never write it; an id that is not in the table
// prices to 0 exactly as it does on the full view.
func (s *Sizes) View(path []*ir.Entry) *Sizes {
	v := &Sizes{per: s.per, floor: s.floor}
	v.order = make([]string, 0, len(path))
	for _, e := range path {
		if e != nil {
			v.order = append(v.order, e.ID)
		}
	}
	return v
}

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

// WireTotal is the entries' share of a request body, in bytes: what a server counts before it
// refuses a body for being a body too large. The envelope in front of it — model name, sampling
// fields, tool schemas, the system prompt — is nobody's message and is measured by the package that
// owns the serialized shape (llm.Request); adding the two is the estimate to compare against a
// ceiling.
func (s *Sizes) WireTotal() int {
	n := 0
	for _, id := range s.order {
		n += s.per[id].Wire
	}
	return n
}

// WirePrefix is the same sum over the first i entries in send order, which is the question a byte
// ceiling asks: stop before entry i and the body is the envelope plus this. Monotone by
// construction, so a caller may walk it outward from the front and stop at the first i that no
// longer fits.
func (s *Sizes) WirePrefix(i int) int {
	if i > len(s.order) {
		i = len(s.order)
	}
	n := 0
	for _, id := range s.order[:i] {
		n += s.per[id].Wire
	}
	return n
}

// WirePrefixes is the whole walk at once: prefixes[i] = WirePrefix(i), for a caller deciding where
// to cut by looking at every position rather than one.
func (s *Sizes) WirePrefixes() []int {
	pre := make([]int, len(s.order)+1)
	for i, id := range s.order {
		pre[i+1] = pre[i] + s.per[id].Wire
	}
	return pre
}

// MissingRefs is how many image references in this payload no resolver could price. It is a tripwire
// rather than a routine count: the log refuses to write an entry whose blob is not in the store
// (store.go, tested in store_test.go), so a healthy session's own payload always answers zero. What
// it does catch is a payload that arrived by another door — rows imported without their bytes — and
// in that case the number is not "small request" but "this body cannot be sent at all", which is why
// it travels beside the sum instead of inside it.
func (s *Sizes) MissingRefs() int { return s.missing }

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

// entryWire estimates what one entry adds to a request body, by summing the lengths of everything
// that goes on it. It deliberately does not try to be a serialization: the JSON scaffolding around
// a message (its role, the field names, the commas) is a constant per message and belongs with the
// envelope's fixed cost, which the llm package measures by marshaling its own struct — the same
// struct that goes out — rather than letting this function count punctuation twice and get it wrong
// twice.
//
// Two things are counted that a look at `Content` alone misses, and both were measured rather than
// guessed. A thinking model's reasoning channel rides back out on the next request whenever the
// payload's switch is on (`Built.SendReasoning`, on by default), and it is routinely the largest part
// of an assistant turn — on a live session it was three quarters of the difference between the
// payload's log weight and its body size. And a tool call's arguments are a JSON document smuggled
// inside a JSON string, so their quotes arrive doubled (jsonQuoteCost).
//
// The one length that is not the answer on its own is an image part's. A part whose data is
// "blob:<sha>" occupies 69 bytes in the log and 4/3rds of the blob's raw bytes on the wire inside a
// data URL; a part whose data is already base64 occupies that many bytes both places. Priced from
// the log column, an entry holding a 2 MB screenshot claims 69 bytes; the 413 that arrives for it
// disagrees by a factor of forty thousand, which is the whole reason this column exists.
//
// The second result counts the references that could not be priced at all, so that a caller adding
// these sums knows whether it is holding an estimate or a floor.
//
// What is deliberately not counted is a message's own scaffolding and the escaping of message
// content. Measured against `llm.BodySizer` on live payloads the whole omission runs to 1.5% of the
// body for a small one and 8% for a tool-heavy one, and what is left of it is dominated by tool
// results whose content is itself quote-ridden text. That residual is the margin a caller keeps
// between this sum and a byte ceiling — and it is why the decision to send is taken from the exact
// body (`llm.BodySizer.BodyBytes` of the request about to go out) while this column is trusted only
// with the relative question of where to cut.
func entryWire(e *ir.Entry, b *Built, refs RefSizes) (int, int) {
	n := len(e.Content)
	if b.SendReasoning {
		n += len(e.Reasoning)
	}
	for _, tc := range e.ToolCalls {
		n += len(tc.Name) + len(tc.Args) + jsonQuoteCost(tc.Args)
	}
	missing := 0
	for _, p := range e.Parts {
		n += len(p.Text)
		if p.Data != "" {
			bytes, unresolved := partWire(p, refs)
			n += bytes
			missing += unresolved
		}
	}
	if raw := len(e.Structured); raw > 0 {
		// structuredWireBytes is the assembler's own arithmetic on what a tool result's JSON puts on
		// the wire, cap and truncation note included. It prices the ceiling, so a server whose mode is
		// "never" is over-counted here by at most that ceiling: an estimate that guesses high about a
		// byte wall fails safe, and the alternative is to consult the per-server mode table for a
		// number this table never gets to decide.
		n += structuredWireBytes(raw, b.StructuredCap)
	}
	return n, missing
}

// jsonQuoteCost is what JSON string escaping adds to s. There are two tiers: a character with a short
// escape (a quote, a backslash, \n, \t, ...) costs one more byte, and a control character without one
// costs five more (0x01 is written \u0001). It is counted in one place only — a tool call's
// arguments — because that is the one place a payload embeds structured text inside a string: the
// arguments go to the server quoted, and a document full of field names is measurably bigger on the
// wire than in the log. Content is not scanned the same way on purpose: doing so would walk every
// byte of the conversation each time a size table is derived, and content is rarely the part that
// doubles.
func jsonQuoteCost(s []byte) int {
	extra := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\', '\n', '\r', '\t', '\b', '\f':
			extra++
		default:
			if s[i] < 0x20 {
				extra += 5
			}
		}
	}
	return extra
}

// partWire is one attachment part's wire weight, and whether its payload had to be left out of it:
// the data URL the assembler wraps the bytes in (counted by asking that one function what it would
// produce for empty data, so this cannot drift from it), plus the payload itself — and the payload's
// size is the blob's, not the reference's.
func partWire(p ir.Part, refs RefSizes) (int, int) {
	n := len(dataURL(p.MimeType, ""))
	if sha, isRef := strings.CutPrefix(p.Data, ir.RefPrefix); isRef {
		if refs != nil {
			if raw, ok := refs.Size(sha); ok && raw > 0 {
				return n + 4*((raw+2)/3), 0 // base64: four characters for every three bytes
			}
		}
		return n, 1
	}
	return n + len(p.Data), 0
}
