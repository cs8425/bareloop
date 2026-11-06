package compact

import (
	"strings"
	"testing"
)

// TestSummarySizeSharesAReasoningCompletionOutByBytes is the thinking-model case: the completion the
// server counted is mostly chain of thought, and the chain of thought is not kept — commitCompaction
// writes the summary text and nothing else. So the number that prices the summary is the text's share of
// that completion, shared out by bytes: 200 tokens over 40 bytes of reasoning and 60 of text is 120.
//
// The live shape this was written against: a compaction committed as summary=4506 while the whole context
// afterwards measured 1505 — the request's 4458 characters of reasoning were being billed to an entry
// that does not contain them, and every later Select would price that entry about three times too big.
func TestSummarySizeSharesAReasoningCompletionOutByBytes(t *testing.T) {
	text := strings.Repeat("a", 60)
	if got := SummarySize(200, 40, text); got != 120 {
		t.Fatalf("200 tokens over 40+60 bytes = %d, want 120 (the text's share, not the whole completion)", got)
	}
	if got := SummarySize(200, 540, text); got != 20 {
		t.Fatalf("a mostly-reasoning completion = %d, want 20: reasoning nobody kept must not occupy the path", got)
	}
	if got := SummarySize(1, 1000, "abc"); got != 1 {
		t.Fatalf("a share that rounds to %d, want 1: a summary that exists is never zero tokens", got)
	}
	// The two old behaviours must survive untouched, or this is a second ruler rather than a fix.
	if got := SummarySize(200, 0, text); got != 200 {
		t.Fatalf("with no reasoning the reported number must stand as-is, got %d", got)
	}
	if got := SummarySize(0, 5000, text); got != 15 {
		t.Fatalf("with nothing reported the estimate is of the text alone: %d, want 60÷4=15 (reasoning is not in the path)", got)
	}
}
