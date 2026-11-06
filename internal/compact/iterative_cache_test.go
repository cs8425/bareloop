package compact

// The shape of a serial chain is a cache question, not a wording question.
//
// Measured on a live session, on a 262k-window llama.cpp that was reusing almost everything: the
// loop's own turn reused 171,147 of its 184,383 prompt tokens, and the three-part summary chain that
// ran moments later reused 0 — on every part. It was cutting the right region and asking the right
// question. It was simply asking it in a request that began with nothing the provider had ever seen,
// so every part paid a full prefill of a conversation the server already held.
//
// Everything below is one half of the answer, the half a unit test can hold: a part must BEGIN with
// bytes that are already cached, and the difference between one part and the next must live at the
// END. The other half — a ruler that prices the region the way the summariser will read it — is at the
// bottom of this file, because the two failures are the same failure measured twice.

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

func chainParams() Params {
	return Params{ThresholdPct: 70, KeepTargetPct: 20, SummaryMaxPct: 70, Temperature: 0.2, Marker: DefaultMarker}
}

func testHead() []asm.Message {
	return []asm.Message{{Role: "system", Content: "You are a test assistant. Be brief."}}
}

func testTools() []llm.Tool {
	return []llm.Tool{
		{Name: "fs_read", Description: "read a file"},
		{Name: "fs_grep", Description: "search files"},
	}
}

func textOf(m asm.Message) string {
	s, _ := m.Content.(string)
	return s
}

func names(ts []llm.Tool) string {
	out := make([]string, 0, len(ts))
	for _, tl := range ts {
		out = append(out, tl.Name)
	}
	return strings.Join(out, ",")
}

// dump is for the one failure worth reading: which message of the call is which.
func dump(msgs []asm.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString("\n  " + strconv.Itoa(i) + " " + m.Role + " " + clip(textOf(m), 90))
	}
	return b.String()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TestPartOneIsThePayloadWithTheInstructionAppended is the spec's first request of a chain:
//
//	[cmp0?, ...msgX, summary_prompt] => cmp1
//
// which is only a cache hit if the prefix is the payload's own messages, in order, with nothing added
// in front of them. The instruction is appended and the tools come along, because a chat template
// renders the tool definitions before the first message — an array left out forks at token 0.
func TestPartOneIsThePayloadWithTheInstructionAppended(t *testing.T) {
	p := chainParams()
	payload := []asm.Message{
		{Role: "system", Content: "You are a test assistant. Be brief."},
		{Role: "user", Content: "the first question"},
		{Role: "assistant", Content: "the first answer"},
	}
	tools := testTools()
	call := p.ChunkRequest(40960, ChunkInput{
		Head: payload[:1], Prefix: payload, Tools: tools,
		Index: 1, Total: 3,
	})
	if len(call.Messages) != len(payload)+1 {
		t.Fatalf("part 1 must be the payload plus one instruction, got %d messages:%s", len(call.Messages), dump(call.Messages))
	}
	for i := range payload {
		if call.Messages[i].Role != payload[i].Role || !reflect.DeepEqual(call.Messages[i].Content, payload[i].Content) {
			t.Fatalf("message %d is not the payload's own (got %q, want %q): anything inserted here is a prompt the provider never cached:%s",
				i, textOf(call.Messages[i]), textOf(payload[i]), dump(call.Messages))
		}
	}
	last := call.Messages[len(call.Messages)-1]
	if last.Role != "user" || !strings.Contains(textOf(last), "part 1 of 3") {
		t.Fatalf("the instruction must be the LAST message and must name the part, got %s %q", last.Role, textOf(last))
	}
	if !reflect.DeepEqual(call.Tools, tools) {
		t.Fatalf("tools = %s, want the loop's own %s: a summary asked with no tools is a different prompt from the turn it is summarising",
			names(call.Tools), names(tools))
	}
	if call.MaxTokens != p.ChunkMaxTokens(40960) {
		t.Fatalf("max_tokens = %d, want the acceptance cap %d", call.MaxTokens, p.ChunkMaxTokens(40960))
	}
}

// TestPrefixAlreadyCarriesTheHead: part 1 is given both the head and the prefix, because the caller
// hands the same two halves to every part of the chain. The prefix wins whole — a second copy of the
// system message, or the head placed ahead of the payload, is the prefix rule broken.
func TestPrefixAlreadyCarriesTheHead(t *testing.T) {
	p := chainParams()
	payload := append(testHead(), asm.Message{Role: "user", Content: "the oldest message"})
	other := []asm.Message{{Role: "system", Content: "A DIFFERENT SYSTEM PROMPT"}}
	call := p.ChunkRequest(40960, ChunkInput{Head: other, Prefix: payload, Index: 1, Total: 2})
	if got := textOf(call.Messages[0]); got != textOf(payload[0]) {
		t.Fatalf("part 1 opened with %q instead of the payload's own first message: %s", clip(got, 60), dump(call.Messages))
	}
	if len(call.Messages) != len(payload)+1 {
		t.Fatalf("the head was inserted as well as the prefix: %d messages:%s", len(call.Messages), dump(call.Messages))
	}
}

// TestLaterPartsShareTheHeadAndDifferOnlyInTheTail is the spec's second and third request:
//
//	[cmp1, ...msgY, summary_prompt] => cmp2
//	[cmp2, ...msgZ, summary_prompt] => cmp3
//
// Parts after the first cannot share the conversation — that is what a chain is — so the only prefix
// left is the system message, and the running summary is what makes one part's answer the next part's
// bytes. Anything that varies between parts (the counter, the wording about instalments) therefore
// belongs in the last message and nowhere else.
func TestLaterPartsShareTheHeadAndDifferOnlyInTheTail(t *testing.T) {
	p := chainParams()
	head := testHead()
	tools := testTools()
	partA := []asm.Message{{Role: "user", Content: "the second instalment of the conversation"}}
	partB := []asm.Message{{Role: "assistant", Content: "the third instalment of the conversation"}}

	c1 := p.ChunkRequest(40960, ChunkInput{Head: head, Prefix: append(testHead(),
		asm.Message{Role: "user", Content: "the first instalment"}), Tools: tools, Index: 1, Total: 3})
	c2 := p.ChunkRequest(40960, ChunkInput{Head: head, Chunk: partA, Prev: "SUMMARY OF THE FIRST INSTALMENT", Tools: tools, Index: 2, Total: 3})
	c3 := p.ChunkRequest(40960, ChunkInput{Head: head, Chunk: partB, Prev: "SUMMARY OF THE FIRST TWO INSTALMENTS", Tools: tools, Index: 3, Total: 3})

	for i, c := range []SummaryCall{c1, c2, c3} {
		if len(c.Messages) == 0 {
			t.Fatalf("part %d asks nothing", i+1)
		}
		if c.Messages[0].Role != "system" || textOf(c.Messages[0]) != textOf(head[0]) {
			t.Fatalf("part %d does not open with the system message every part opens with: %+v", i+1, c.Messages[0])
		}
		if !reflect.DeepEqual(c.Tools, tools) {
			t.Fatalf("part %d dropped the tools (%s)", i+1, names(c.Tools))
		}
	}
	// The counter sentence may only ever live in the message the parts cannot share.
	for i, c := range []SummaryCall{c1, c2, c3} {
		for j, m := range c.Messages[:len(c.Messages)-1] {
			if strings.Contains(textOf(m), " of 3 ") {
				t.Fatalf("part %d carries the instalment counter at message %d, ahead of the instruction: %s",
					i+1, j, clip(textOf(m), 120))
			}
		}
	}
	// The running summary is a message of its own, with the role a committed summary travels with, and
	// it must be told apart from the part that follows it.
	if !strings.Contains(textOf(c2.Messages[1]), "SUMMARY OF THE FIRST INSTALMENT") {
		t.Fatalf("the chain only works if the previous answer is handed to the next call:%s", dump(c2.Messages))
	}
	if !reflect.DeepEqual(c2.Messages[2], partA[0]) {
		t.Fatalf("part 2 is not carrying its own message itself: %+v", c2.Messages[2])
	}
	if tail := textOf(c2.Messages[len(c2.Messages)-1]); !strings.Contains(tail, "part 2 of 3") {
		t.Fatalf("the model must know which instalment it is folding in: %s", clip(tail, 120))
	}
	if tail := textOf(c3.Messages[len(c3.Messages)-1]); !strings.Contains(tail, "part 3 of 3") {
		t.Fatalf("part 3 must count itself too: %s", clip(tail, 120))
	}
	// Folding, not summarising-only-this-part, is the one instruction the chain cannot lose.
	if tail := textOf(c3.Messages[len(c3.Messages)-1]); !strings.Contains(tail, "not just this part") {
		t.Fatal("a part that is summarised alone throws every earlier part away")
	}
}

// TestExtraInstructionReachesEveryPartAndASinglePartNeedsNoCounter keeps the two wording promises the
// chain has always made, now that they live in the tail of the request.
func TestExtraInstructionReachesEveryPartAndASinglePartNeedsNoCounter(t *testing.T) {
	p := chainParams()
	head := testHead()
	part := []asm.Message{{Role: "user", Content: "some conversation"}}
	first := p.ChunkRequest(40960, ChunkInput{Head: head, Prefix: append(testHead(), part...), Index: 1, Total: 2, Extra: "keep the file names"})
	if got := textOf(first.Messages[len(first.Messages)-1]); !strings.Contains(got, "keep the file names") {
		t.Fatal("the user's own instruction must reach every part, not only the first")
	}
	next := p.ChunkRequest(40960, ChunkInput{Head: head, Chunk: part, Prev: "earlier summary", Index: 2, Total: 2, Extra: "keep the file names"})
	if got := textOf(next.Messages[len(next.Messages)-1]); !strings.Contains(got, "keep the file names") {
		t.Fatal("the user's own instruction must reach every part, not only the first")
	}
	single := p.ChunkRequest(40960, ChunkInput{Head: head, Chunk: part, Index: 1, Total: 1})
	if got := textOf(single.Messages[len(single.Messages)-1]); strings.Contains(got, "part 1 of 1") {
		t.Fatalf("a single part needs no counter: %s", clip(got, 120))
	}
}

// TestThePartIsTheMessageItselfNotARenderingOfIt: the chain used to flatten each part into
// "[role] text", which cost two things at once — the bytes no longer matched what the provider had
// cached, and everything a message carries besides its text (a tool call, the reasoning channel, a
// tool result folded from structuredContent) vanished from the request while the ruler that cut the
// part had already charged those bytes as tokens.
func TestThePartIsTheMessageItselfNotARenderingOfIt(t *testing.T) {
	p := chainParams()
	part := []asm.Message{{
		Role:             "assistant",
		ReasoningContent: "I thought about this for a while",
		ToolCalls: []asm.ToolCallOut{{ID: "call_1", Type: "function",
			Function: asm.FunctionCall{Name: "fs_read", Arguments: `{"path":"a.go"}`}}},
	}}
	call := p.ChunkRequest(40960, ChunkInput{Head: testHead(), Chunk: part, Prev: "PREV", Index: 2, Total: 2})
	var found bool
	for i := range call.Messages {
		if reflect.DeepEqual(call.Messages[i], part[0]) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the part's own assistant message is not in the request — it was re-rendered:%s", dump(call.Messages))
	}
	if found && len(call.Messages) > 0 {
		for i := range call.Messages {
			if call.Messages[i].ReasoningContent != "" && len(call.Messages[i].ToolCalls) == 0 {
				t.Fatal("thinking arrived without the tool calls it belonged to")
			}
		}
	}
}

// ─── the ruler ────────────────────────────────────────────────────────────────

// The ruler is a reader. What an entry costs is measured once, by asm.Stamp, from the difference
// between two prompt counts a server made, and it lives on the entry until then (ir.Entry.Tokens);
// how that measurement is worked out, split and kept honest is internal/asm/stamp_test.go. What is
// left for a ruler is the two ends: take the number that was counted, and for the entries no request
// ever carried, estimate the way the assembler would send them.

// TestRulerTakesTheNumberThatWasCounted: an entry the server priced at 300 tokens is not 10,000 tokens
// because four bytes makes a token, and it is not the estimate of a document the assembler may not even
// send. The estimate decides where a part is cut, so the wrong one buys a summariser call over a
// conversation that fits in one.
func TestRulerTakesTheNumberThatWasCounted(t *testing.T) {
	measured := txt("t1", "tool", 40000)
	measured.Tokens = 300
	blank := txt("u2", "user", 500)

	r := NewRuler(0)
	if got := r.Tokens(measured); got != 300 {
		t.Fatalf("a measured entry = %d tokens, want the 300 that were counted for it (it estimates at %d)",
			got, asm.Estimate(measured, 0))
	}
	if got, want := r.Tokens(blank), asm.Estimate(blank, 0); got != want {
		t.Fatalf("an entry nobody counted = %d tokens, want its estimate %d", got, want)
	}
	var none *Ruler
	if got, want := none.Tokens(blank), asm.Estimate(blank, 0); got != want {
		t.Fatalf("a nil ruler = %d tokens, want the estimate %d it has always been", got, want)
	}
	// An entry no request ever carried is not free because the ones beside it were counted: it is the
	// huge thing about to be sent, and it is the reason the summary is being asked for.
	if RegionTokens([]*ir.Entry{measured, blank}, r) != 300+asm.Estimate(blank, 0) {
		t.Fatalf("the region did not add a measurement to an estimate: %d", RegionTokens([]*ir.Entry{measured, blank}, r))
	}
}

// TestRulerPricesAStructuredDocumentAtTheCeilingTheAssemblerHonours: an entry whose answer travels in
// structuredContent costs on the wire what the assembler is willing to send, not what the log stored.
// Pricing the stored document is how one chatty MCP server reads as a hundred-thousand-token region —
// and how a chain gets cut that the summariser then fills with something else.
func TestRulerPricesAStructuredDocumentAtTheCeilingTheAssemblerHonours(t *testing.T) {
	doc := json.RawMessage(strings.Repeat("{}", 20000)) // 40 KB of structuredContent
	e := &ir.Entry{ID: "t", Kind: ir.KindTool, Role: "tool", Content: "ok", Structured: doc}
	capped := NewRuler(16384).Tokens(e)
	wide := NewRuler(200000).Tokens(e)
	if capped >= wide {
		t.Fatalf("a 16 KB ceiling and a 200 KB ceiling priced the same entry alike: %d vs %d", capped, wide)
	}
	if stored := len(doc) / 4; capped >= stored {
		t.Fatalf("the entry was priced at %d tokens, which is the stored document (%d) and not the capped one the assembler will send", capped, stored)
	}
}

// TestNeedsIterativeIsBlindToThePricedRegion is the live failure, reduced. The region's estimated sum
// said "fold this into a chain of near-zero-reuse calls" while the prompt the server had counted
// fitted one request — the first is a guess about bytes, the second is a fact about a request. The
// decision now takes the fact and nothing else: a priced sum cannot reach it at all, which is what the
// two-int signature is for (the caller counts the payload, the instruction and the never-sent tail,
// because only it can see all three).
func TestNeedsIterativeIsBlindToThePricedRegion(t *testing.T) {
	const nctx = 40960
	var all []*ir.Entry
	for RegionTokens(all, nil) < 2*nctx {
		all = append(all, txt("e"+strconv.Itoa(len(all)), "user", 500))
	}
	if RegionTokens(all, nil) < 2*nctx {
		t.Fatal("the fixture has to look enormous for the ruler's sum to be worth comparing")
	}
	// The same conversation, counted by a server: 24k of a 40k window. One call, on the cache.
	if NeedsIterative(24000, nctx) {
		t.Fatalf("a prompt the server counted inside the window must be sent: the ruler priced %d entries at %d",
			len(all), RegionTokens(all, nil))
	}
	if !NeedsIterative(2*nctx, nctx) {
		t.Fatal("a prompt twice the window is a call the provider rejects: that is the chain's job")
	}
}

// TestChunkPlanReportsWhereAndHowMuch pins the two numbers a part carries for the log line, because
// the investigation could not tell a 20x ruler error from a working chain without them: a part must say
// where it starts in the region (the caller cuts the payload's messages at the same place) and what it
// costs, and the parts must partition the region exactly.
func TestChunkPlanReportsWhereAndHowMuch(t *testing.T) {
	p := chainParams()
	const nctx = 40960
	r := region(400) // worth ~50k tokens against a part of 28672: two parts, and a place to cut between them
	chunks := PlanChunks(r, nil, nctx, p)
	if len(chunks) < 2 {
		t.Fatalf("expected several parts, got %d", len(chunks))
	}
	next, sum := 0, 0
	for i, c := range chunks {
		if c.Start != next {
			t.Fatalf("part %d starts at %d, want %d: the payload would be sliced in the wrong place", i+1, c.Start, next)
		}
		next += len(c.Entries)
		part := 0
		for _, e := range c.Entries {
			part += RegionTokens([]*ir.Entry{e}, nil)
		}
		if c.Tokens != part {
			t.Fatalf("part %d reports %d tokens for entries worth %d: the log would print a number that is not the part",
				i+1, c.Tokens, part)
		}
		sum += c.Tokens
	}
	if next != len(r) {
		t.Fatalf("the parts stop at %d of %d entries", next, len(r))
	}
	if sum != RegionTokens(r, nil) {
		t.Fatalf("the parts add up to %d of the region's %d tokens", sum, RegionTokens(r, nil))
	}
}
