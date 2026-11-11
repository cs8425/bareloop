package compact

// The two things a summary request is made of: the payload it is cut from, and the size it is allowed
// to answer with. Both are supposed to be measured rather than guessed (§0), and both had been
// quietly built from a number of the model's own invention — the first because a chain that re-renders
// the conversation is not the payload, and the second because a completion the server just counted is
// a better number than bytes÷4.

import (
	"reflect"
	"strings"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

// TestSummarySizeUsesTheReportedNumberFirst: the acceptance check decides whether a summary is kept, so
// it is measured against the ceiling of the window. What the server reported for the completion is a
// measurement; bytes÷4 is a fallback for the case where nothing was reported — and the two are not
// interchangeable, which is why a reported number must win even when it disagrees with the text.
func TestSummarySizeUsesTheReportedNumberFirst(t *testing.T) {
	text := strings.Repeat("word ", 400) // 2000 bytes → 500 tokens by the fallback ruler
	if got := SummarySize(1234, 0, text); got != 1234 {
		t.Fatalf("SummarySize = %d, want the number the server reported (1234): the estimate is for what was never measured", got)
	}
	if got := SummarySize(0, 0, text); got != 500 {
		t.Fatalf("with nothing reported, SummarySize = %d, want bytes÷4 (500)", got)
	}
	if got := SummarySize(-1, 0, text); got != 500 {
		t.Fatalf("a negative report is not a measurement either: %d", got)
	}
	// Never raw bytes: a 2000-byte summary is not a 2000-token summary, and a cap read that way
	// refuses every summary the model can write.
	if got := SummarySize(0, 0, "abcd"); got != 1 {
		t.Fatalf("a 4-byte summary = %d, want 1 token", got)
	}
	if got := SummarySize(0, 0, "abc"); got != 1 {
		t.Fatalf("a 3-byte summary rounds up to %d, want 1", got)
	}
	// A reported number of 0 completion tokens is indistinguishable from "nothing reported", which is
	// the honest reading: an empty answer is refused by the caller anyway.
	if got := SummarySize(0, 0, ""); got != 0 {
		t.Fatalf("an empty summary = %d, want 0", got)
	}
}

// TestPrefixStableRequestIsTheSentPayloadPlusTheInstruction is the shape that makes an ordinary
// (non-chain) compaction cheap, and it is the one shape the four compactions in the live session never
// reached — every one of them was pushed into the chain by a ruler that over-counted the region.
func TestPrefixStableRequestIsTheSentPayloadPlusTheInstruction(t *testing.T) {
	p := chainParams()
	payload := []asm.Message{
		{Role: "system", Content: "You are a test assistant."},
		{Role: "user", Content: "the oldest question still on the path"},
		{Role: "assistant", Content: "the answer to it"},
	}
	tools := testTools()
	call := p.BuildRequest(40960, payload, tools, false, "")
	if len(call.Messages) != len(payload)+1 {
		t.Fatalf("the request must be the payload plus one instruction, got %d messages:%s", len(call.Messages), dump(call.Messages))
	}
	for i := range payload {
		if call.Messages[i].Role != payload[i].Role || !reflect.DeepEqual(call.Messages[i].Content, payload[i].Content) {
			t.Fatalf("message %d is not the payload's own: %s", i, dump(call.Messages))
		}
	}
	last := call.Messages[len(call.Messages)-1]
	if last.Role != "user" || !strings.Contains(textOf(last), "compress") {
		t.Fatalf("the instruction must be appended as a user message, got %s %q", last.Role, textOf(last))
	}
	if !reflect.DeepEqual(call.Tools, tools) {
		t.Fatalf("tools = %s, want the loop's own %s: the template renders them before the first message, so a missing array forks at token 0",
			names(call.Tools), names(tools))
	}
	if call.MaxTokens != p.SummaryCap(40960) {
		t.Fatalf("max_tokens = %d, want the summary cap for the window", call.MaxTokens)
	}

	// A user instruction joins the instruction it was appended to — never the prefix, whose bytes are
	// the cache.
	withInstr := p.BuildRequest(40960, payload, tools, true, "keep the error messages verbatim")
	if len(withInstr.Messages) != len(payload)+1 {
		t.Fatal("an extra instruction must not grow the prefix")
	}
	got := textOf(withInstr.Messages[len(payload)])
	if !strings.Contains(got, "keep the error messages verbatim") {
		t.Fatalf("the user's instruction is missing: %s", clip(got, 120))
	}
	if !strings.Contains(got, p.SummaryInstruction(true)) {
		t.Fatal("the split instruction is what a split boundary has to be asked for")
	}
}

// TestTranscriptModeAsksNothingItCouldNotCache is here so the mode that gives up cache affinity stays a
// deliberate choice and not a surprise: it renders the region flat, and it must not carry tools it did
// not ask about.
func TestTranscriptModeAsksNothingItCouldNotCache(t *testing.T) {
	p := chainParams()
	p.SummaryMode = ModeTranscript
	call := p.BuildRequest(40960, []asm.Message{{Role: "user", Content: "x"}}, testTools(), false, "")
	if len(call.Messages) != 1 || call.Messages[0].Role != "system" {
		t.Fatalf("shape B is one system message of instruction, got:%s", dump(call.Messages))
	}
	if len(call.Tools) != 0 {
		t.Fatalf("shape B claims no cache affinity, so it must not pretend to share a prompt: %s", names(call.Tools))
	}
}

// TestTranscriptNeverSendsBase64 is the reason shape B exists for servers that cannot take shape A, and
// the reason it can never be the chain's replacement: an attachment is named, never carried.
func TestTranscriptNeverSendsBase64(t *testing.T) {
	entries := []*ir.Entry{
		// No text of its own, so the transcript has to reach the attachment itself.
		{ID: "u", Kind: ir.KindUser, Role: "user",
			Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: strings.Repeat("QUJD", 500), Name: "shot.png"}}},
		{ID: "a", Kind: ir.KindAssistant, Role: "assistant", ToolCalls: []ir.ToolCall{{Name: "fs_read", Args: []byte(`{"path":"huge"}`)}}},
	}
	out := Transcript(entries)
	if strings.Contains(out, "QUJDQUJD") {
		t.Fatal("an attachment must never reach a summary call as data")
	}
	if !strings.Contains(out, "shot.png") {
		t.Fatalf("the transcript must at least name the attachment: %s", clip(out, 200))
	}
	if !strings.Contains(out, "fs_read") {
		t.Fatalf("a tool call survives as its name, so the summary can say what was asked: %s", clip(out, 200))
	}
}

// TestEmptyEntriesMakeNoTranscript: an entry with neither text nor calls nor parts contributes nothing,
// so a region of nothing is a transcript of nothing — the empty check the caller relies on.
func TestEmptyEntriesMakeNoTranscript(t *testing.T) {
	if got := Transcript([]*ir.Entry{{ID: "a", Kind: ir.KindUser, Role: "user"}, nil}); got != "" {
		t.Fatalf("nothing to say should print nothing, got %q", clip(got, 80))
	}
}
