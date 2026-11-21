package asm

import (
	"encoding/json"
	"strings"
	"testing"

	"bareloop/internal/ir"
)

// The Wire column exists for one question: how big is the request body this is going to make. A
// server with a body ceiling answers 413 before it reads a token, so no token count predicts it, and
// the log-weight column (Size.Bytes) is actively misleading about it — an image is 69 bytes of
// reference in the log and megabytes of base64 on the wire.

// sizesRefs is the blob table as the size table sees it: sha in, raw bytes out.
type sizesRefs map[string]int

func (f sizesRefs) Size(sha string) (int, bool) {
	n, ok := f[sha]
	return n, ok
}

const pngPrefix = len("data:image/png;base64,")

// b64 is the exact arithmetic base64 does to n raw bytes.
func b64(n int) int { return 4 * ((n + 2) / 3) }

func TestWirePricesAnImageByItsBytesNotByItsReference(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	refs := sizesRefs{sha: 3000}
	e := &ir.Entry{ID: "img", Kind: ir.KindUser, Role: "user",
		Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: ir.RefPrefix + sha}}}
	sizes := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0, refs)

	got, ok := sizes.Get("img")
	if !ok {
		t.Fatal("the row is gone")
	}
	if want := pngPrefix + b64(3000); got.Wire != want {
		t.Fatalf("Wire = %d, want the data URL (%d) of 4000 base64 bytes = %d", got.Wire, pngPrefix, want)
	}
	// The point of the column, stated as a ratio: the older column counts the reference the log
	// holds, this one counts the bytes the wire carries. An entry whose two columns agree is an entry
	// with no attachment — and this entry has one, so the two must part company.
	if got.Wire < 10*got.Bytes {
		t.Fatalf("Bytes = %d and Wire = %d: a 3000-byte image behind a 69-byte reference must move these two numbers apart",
			got.Bytes, got.Wire)
	}
}

func TestWireCountsEveryImageAndRepeatsTheOnesSentTwice(t *testing.T) {
	sha := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	miss := strings.Repeat("c", 64)
	e := &ir.Entry{ID: "many", Kind: ir.KindUser, Role: "user", Content: "two pictures and a caption",
		Parts: []ir.Part{
			{Type: ir.PartText, Text: "see below"},
			{Type: ir.PartImage, MimeType: "image/png", Data: ir.RefPrefix + sha},
			{Type: ir.PartImage, MimeType: "image/png", Data: ir.RefPrefix + sha}, // the same bytes, sent twice
			{Type: ir.PartImage, MimeType: "image/jpeg", Data: ir.RefPrefix + other},
			{Type: ir.PartImage, MimeType: "image/png", Data: ir.RefPrefix + miss}, // never stored
		}}
	refs := sizesRefs{sha: 100, other: 3}
	sizes := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0, refs)

	got, _ := sizes.Get("many")
	want := len("two pictures and a caption") + len("see below") +
		2*(pngPrefix+b64(100)) + // the same picture twice is two pictures on the wire
		len("data:image/jpeg;base64,") + b64(3) +
		pngPrefix // an unknown blob contributes its wrapper and no payload: the send cannot happen anyway
	if got.Wire != want {
		t.Fatalf("Wire = %d, want %d", got.Wire, want)
	}
	// Exactly the one never-stored sha is unpriced, and the table says so rather than letting the sum
	// read as a complete answer.
	if got := sizes.MissingRefs(); got != 1 {
		t.Fatalf("MissingRefs = %d, want the one reference no resolver answered", got)
	}
}

func TestWireOfInlineBytesIsItsOwnLength(t *testing.T) {
	// A part whose data is already base64 (not a reference) is that many bytes in the log and on the
	// wire: no resolver is asked, and none is needed.
	e := &ir.Entry{ID: "inline", Kind: ir.KindUser, Role: "user",
		Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: "aGVsbG8="}}}
	got, _ := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0).Get("inline")
	if want := pngPrefix + len("aGVsbG8="); got.Wire != want {
		t.Fatalf("Wire = %d, want %d", got.Wire, want)
	}
}

func TestWithoutAResolverAReferencePricesAtItsWrapper(t *testing.T) {
	// A table derived with no access to the blob table (a test fixture, a preview built elsewhere)
	// says what it can see: the wrapper. It never claims the image is free — it never claims anything
	// about the image, and WireTotal of such a table is a floor rather than an estimate.
	e := &ir.Entry{ID: "img", Kind: ir.KindUser, Role: "user",
		Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: ir.RefPrefix + strings.Repeat("d", 64)}}}
	withRefs, _ := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0, sizesRefs{strings.Repeat("d", 64): 1 << 20}).Get("img")
	noRefs, _ := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0).Get("img")
	if noRefs.Wire != pngPrefix {
		t.Fatalf("Wire without a resolver = %d, want the wrapper's %d", noRefs.Wire, pngPrefix)
	}
	if withRefs.Wire <= noRefs.Wire {
		t.Fatalf("the resolver changed nothing: %d vs %d", withRefs.Wire, noRefs.Wire)
	}
	// And the table admits it: a reference nothing answered is counted, whether the resolver said "not
	// in here" or there was no resolver to ask. The sum is a floor, and this is the field that says so.
	if got := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0, sizesRefs{}).MissingRefs(); got != 1 {
		t.Fatalf("MissingRefs = %d, want the reference a resolver refused to price", got)
	}
	if got := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0).MissingRefs(); got != 1 {
		t.Fatalf("MissingRefs without any resolver = %d, want 1: nothing was priced, so nothing is known", got)
	}
}

func TestWireCutsTheStructuredContentAtItsWireCeiling(t *testing.T) {
	// structuredContent is stored whole and sent capped, so the log column grows by more than the
	// wire ever carries. This is the one case where Wire is the smaller number, and it is the same
	// arithmetic the assembler uses to truncate (structuredWireBytes), not a second opinion.
	plain := &ir.Entry{ID: "plain", Kind: ir.KindTool, Role: "tool", Content: "result"}
	capped := 4096
	doc := &ir.Entry{ID: "doc", Kind: ir.KindTool, Role: "tool", Content: "result",
		Structured: []byte(strings.Repeat("x", capped*5))}
	sizes := DeriveSizes(&Built{Entries: []*ir.Entry{plain, doc}, StructuredCap: capped}, 0)
	a, _ := sizes.Get("plain")
	b, _ := sizes.Get("doc")
	// The document is five times the cap; the wire carries the cap and the note that says what is
	// missing. Anything near 5x means this column stopped using the assembler's own arithmetic.
	if over := b.Wire - a.Wire; over < capped || over > 2*capped {
		t.Fatalf("a document 5x the cap added %d wire bytes, want the %d-byte cap plus its note", over, capped)
	}
	if b.Bytes <= b.Wire {
		t.Fatalf("Bytes = %d and Wire = %d: the log column should carry the whole document the wire truncates",
			b.Bytes, b.Wire)
	}
}

// A thinking model's reasoning channel is on the next request, so it is on the body's bill. This is
// not a rounding detail: on a live session it was three quarters of the bytes the log column did not
// see, which is the same blind spot as the image reference and forty times as common.
func TestWirePricesTheReasoningChannelWhenItIsSent(t *testing.T) {
	hmm := strings.Repeat("let me think about this ", 40)
	e := &ir.Entry{ID: "think", Kind: ir.KindAssistant, Role: "assistant", Content: "yes", Reasoning: hmm}

	on, _ := DeriveSizes(&Built{Entries: []*ir.Entry{e}, SendReasoning: true}, 0).Get("think")
	if on.Wire != len("yes")+len(hmm) {
		t.Fatalf("Wire with reasoning = %d, want its %d bytes counted beside the answer's 3",
			on.Wire, len("yes")+len(hmm))
	}
	off, _ := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0).Get("think")
	if off.Wire != 3 {
		t.Fatalf("Wire with the switch off = %d, want the answer alone: a channel the payload does not "+
			"send must not be charged to it", off.Wire)
	}
	// The log column never saw either version of the channel, which is exactly the disagreement this
	// table exists to make visible.
	if off.Bytes > on.Wire {
		t.Fatalf("Bytes = %d and the reasoning-free Wire = %d: the log column has swallowed a channel the "+
			"wire may not even carry", off.Bytes, on.Wire)
	}
}

// A tool call's arguments reach the server quoted: the JSON document inside the JSON string pays one
// extra byte per quote, and on an argument-heavy path that is most of the difference.
func TestWirePricesTheQuotingOfToolArguments(t *testing.T) {
	args := `{"path":"/tmp/a","deep":true}`
	quotes := 0
	for i := 0; i < len(args); i++ {
		if args[i] == '"' {
			quotes++
		}
	}
	e := &ir.Entry{ID: "call", Kind: ir.KindAssistant, Role: "assistant",
		ToolCalls: []ir.ToolCall{{Name: "fs_read", Args: json.RawMessage(args)}}}
	got, _ := DeriveSizes(&Built{Entries: []*ir.Entry{e}}, 0).Get("call")
	if want := len("fs_read") + len(args) + quotes; got.Wire != want {
		t.Fatalf("Wire = %d, want %d (the arguments' own %d bytes plus the %d escapes their quotes cost)",
			got.Wire, want, len(args), quotes)
	}
	// Escaping has two tiers, and both are on the wire: the characters with a short form cost one
	// more byte (\n is written \n), and the ones without it cost five more (0x01 is written \u0001).
	short := &ir.Entry{ID: "nl", Kind: ir.KindAssistant, Role: "assistant",
		ToolCalls: []ir.ToolCall{{Name: "x", Args: json.RawMessage("a\nb")}}}
	nl, _ := DeriveSizes(&Built{Entries: []*ir.Entry{short}}, 0).Get("nl")
	if want := len("x") + 3 + 1; nl.Wire != want {
		t.Fatalf("Wire = %d, want %d: a newline is two bytes on the wire and one in the log", nl.Wire, want)
	}
	long := &ir.Entry{ID: "ctl", Kind: ir.KindAssistant, Role: "assistant",
		ToolCalls: []ir.ToolCall{{Name: "x", Args: json.RawMessage("a\u0001b")}}}
	ctl, _ := DeriveSizes(&Built{Entries: []*ir.Entry{long}}, 0).Get("ctl")
	if want := len("x") + 3 + 5; ctl.Wire != want {
		t.Fatalf("Wire = %d, want %d: a bare control character is \\u0001 by the time it lands", ctl.Wire, want)
	}
}

func TestWirePrefixesWalkThePathInSendOrder(t *testing.T) {
	path := []*ir.Entry{
		{ID: "a", Kind: ir.KindUser, Role: "user", Content: strings.Repeat("a", 100)},
		{ID: "b", Kind: ir.KindAssistant, Role: "assistant", Content: strings.Repeat("b", 200)},
		{ID: "c", Kind: ir.KindUser, Role: "user", Content: strings.Repeat("c", 300)},
	}
	sizes := DeriveSizes(&Built{Entries: path}, 0)
	pre := sizes.WirePrefixes()
	if len(pre) != len(path)+1 || pre[0] != 0 {
		t.Fatalf("prefixes = %v, want one more row than the path and a zero at the front", pre)
	}
	for i := range path {
		if got := sizes.WirePrefix(i); got != pre[i] {
			t.Fatalf("WirePrefix(%d) = %d, WirePrefixes()[%d] = %d", i, got, i, pre[i])
		}
		if pre[i+1] <= pre[i] {
			t.Fatalf("prefixes are not monotone: %v", pre)
		}
		if sizes.WirePrefix(99) != pre[len(path)] {
			t.Fatal("walking off the end of the path should stop at its end, not panic or overshoot")
		}
	}
	if sizes.WireTotal() != pre[len(path)] {
		t.Fatalf("WireTotal = %d, the last prefix = %d", sizes.WireTotal(), pre[len(path)])
	}
	// And the cut is a byte number a caller can decide on: stopping before entry c costs exactly c's
	// weight less than sending the whole path.
	if whole, cut := sizes.WirePrefix(3), sizes.WirePrefix(2); whole-cut != 300 {
		t.Fatalf("the difference of cutting before entry c is %d, want its 300 bytes", whole-cut)
	}
}
