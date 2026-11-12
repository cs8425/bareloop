package agent

// The replay, end to end: a path that grew several times over the line is caught up one tipping
// point at a time, each round asking about the bytes it actually sent and committing its own entry
// with its own bill. The acceptance question the user pinned: the cost of a catch-up must be
// readable twice over — the sum of every summary request across all rounds, and each single
// request as its own line. These tests are that ledger against the wire.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
)

// fillNumbered is fillChainPath with identifiable content: replay tests need to point at the
// entry that survived into the next round, and identical "abcd" blobs cannot be pointed at.
func fillNumbered(t *testing.T, s *Session, n, chars int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := &ir.Entry{Kind: ir.KindUser, Role: "user",
			Content: fmt.Sprintf("entry %02d: %s", i+1, strings.Repeat("abcd ", chars/5)), Ts: time.Now().UTC()}
		if i%2 == 1 {
			e.Kind, e.Role = ir.KindAssistant, "assistant"
		}
		if _, err := s.appendEntry(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// compactionEntries is the ledger as the disk holds it: every compaction entry on the path, in
// order. It deliberately does not read the effective window — a replay round folds the previous
// round's entry into its region by design (that is what "the next turn would have done it too"
// means), so EffectiveMeta masks every but the last. The log keeps them all, and the per-round
// bill lives on each of them.
func compactionEntries(t *testing.T, s *Session) []*ir.Entry {
	t.Helper()
	ids := s.PathIDs("")
	found, err := s.log.Contents(ids, false)
	if err != nil {
		t.Fatalf("the log cannot be read: %v", err)
	}
	var out []*ir.Entry
	for _, id := range ids {
		if e := found[id]; e != nil && e.Kind == ir.KindCompaction {
			out = append(out, e)
		}
	}
	return out
}

// TestReplayCompactOverAnOverwindowPath is the ledger test. Twelve 3000-char entries are roughly
// three times the trigger line, so the path holds several tipping points and one round cannot
// catch it up: every round must summarise only up to the entry that tips it, and after the run
// the whole path estimate must be back inside the line. The bill is then read twice over: the
// provider's calls are summed and compared against the sum of the committed entries (the total
// is conserved — never a triangle of re-counted rounds), and every entry must carry exactly its
// own round's call as its own line.
func TestReplayCompactOverAnOverwindowPath(t *testing.T) {
	s, cp := chainFixture(t, "replay-ledger", 4096)
	fillNumbered(t, s, 12, 3000)

	res, err := s.CompactNow(context.Background(), "")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Committed {
		t.Fatalf("nothing committed: %s", res.Skipped)
	}
	if res.Rounds < 2 {
		t.Fatalf("a path three times over the line committed in round %d — the replay is not running", res.Rounds)
	}
	asks := cp.all()
	if len(asks) != res.Rounds {
		t.Fatalf("%d summariser calls for %d rounds: one round is one question (attempts and retries would show as failures)",
			len(asks), res.Rounds)
	}
	if len(res.SummaryUsage) != len(asks) {
		t.Fatalf("the run billed %d lines for %d calls", len(res.SummaryUsage), len(asks))
	}
	if level := 4096 * 70 / 100; res.After > level {
		t.Fatalf("the replay stopped with the context at ~%d against the %d-token line", res.After, level)
	}
	// No request may ask a window it does not have: that was the chain's reason to exist and the
	// replay earns it differently — by never planning past the tip.
	for i, req := range asks {
		if p := promptFor(req); p > 4096 {
			t.Fatalf("round %d asked with %d tokens against a 4096-token window", i+1, p)
		}
	}

	// The ledger, read from disk. Acceptance ①: the entries' total is the calls' total.
	// Acceptance ②: each entry names exactly its own round's call, and nothing else.
	comps := compactionEntries(t, s)
	if len(comps) != res.Rounds {
		t.Fatalf("%d compaction entries for %d rounds", len(comps), res.Rounds)
	}
	var fromEntries, fromCalls int
	for i, e := range comps {
		if e.Compaction == nil {
			t.Fatalf("compaction entry %d has no meta", i+1)
		}
		if n := len(e.Compaction.Summary); n != 1 {
			t.Fatalf("entry %d carries %d summary calls, want exactly its own round's one: the per-entry bill must not re-count an earlier round",
				i+1, n)
		}
		att := e.Compaction.Summary[0]
		want := promptFor(asks[i])
		if att.Usage == nil || att.Usage.PromptTokens != want {
			t.Fatalf("entry %d bills prompt %v, want round %d's own request (%d tokens)", i+1, att.Usage, i+1, want)
		}
		if e.Usage == nil || e.Usage.PromptTokens != want {
			t.Fatalf("entry %d totals prompt %v, want its own round's %d", i+1, e.Usage, want)
		}
		fromEntries += e.Usage.PromptTokens
	}
	for _, req := range asks {
		fromCalls += promptFor(req)
	}
	if fromEntries != fromCalls {
		t.Fatalf("the entries sum to %d prompt tokens, the provider was asked %d: a catch-up's cost is its own calls, once",
			fromEntries, fromCalls)
	}
}

// TestReplayRoundOneIsABytePrefixOfTheSentPayload keeps the chain's dead spec alive where it was
// right: round 1 is not a request about the conversation, it is the request the loop already made
// with the instruction added — message-for-message the same bytes, ending where the round chose to
// cut — and it is a cache hit only while that is true. And round 2 replays what the next turn
// would have done: it reopens with the entry round 1 committed.
func TestReplayRoundOneIsABytePrefixOfTheSentPayload(t *testing.T) {
	s, cp := chainFixture(t, "replay-prefix", 4096)
	fillNumbered(t, s, 12, 3000)
	built, _, tools, err := s.build()
	if err != nil {
		t.Fatalf("the fixture's window could not be read: %v", err)
	}
	sent, sentIDs := built.All(), asm.EntryIDs(built)
	s.rememberSent(sent, sentIDs, tools)

	res, err := s.CompactNow(context.Background(), "")
	if err != nil || !res.Committed {
		t.Fatalf("compact: %v %+v", err, res)
	}
	asks := cp.all()
	if res.Rounds < 2 || len(asks) < 2 {
		t.Fatalf("this test needs a replay of at least two rounds, got %d rounds / %d calls", res.Rounds, len(asks))
	}

	first := asks[0]
	body := first.Messages[:len(first.Messages)-1]
	if len(body) >= len(sent) {
		t.Fatalf("round 1 carries %d of the payload's %d messages: a round asks about its cut, not the whole path",
			len(body), len(sent))
	}
	for i, m := range body {
		if !reflect.DeepEqual(m, sent[i]) {
			t.Fatalf("message %d of round 1 is not the message the loop sent (%q vs %q): a byte inserted ahead of the prefix is a prompt the provider never cached:%s",
				i, msgText(m), msgText(sent[i]), dumpForTest(first.Messages))
		}
	}
	if isOneOf(sent, first.Messages[len(first.Messages)-1]) {
		t.Fatalf("the instruction must be the message the round adds, not one of the payload's own:%s", dumpForTest(first.Messages))
	}

	// Round 2 plans against [cmp1, kept...]: it carries the entry round 1 committed, as the next
	// turn would have carried it into its own prompt.
	second := asks[1]
	if len(second.Messages) < 3 || second.Messages[1].Role != "user" || !strings.Contains(msgText(second.Messages[1]), "SUMMARY 1") {
		t.Fatalf("round 2 does not reopen with the summary round 1 committed:%s", dumpForTest(second.Messages))
	}
}

// TestASingleTipStillCommitsExactlyOneEntry keeps the ordinary compaction ordinary: a path that
// tipped once runs one round, commits one entry, and the replay loop is invisible.
func TestASingleTipStillCommitsExactlyOneEntry(t *testing.T) {
	s, cp := chainFixture(t, "replay-single", 4096)
	fillChainPath(t, s, 6, 3000)

	res, err := s.CompactNow(context.Background(), "")
	if err != nil || !res.Committed {
		t.Fatalf("compact: %v %+v", err, res)
	}
	if res.Rounds != 1 {
		t.Fatalf("a path that tipped once replayed %d rounds", res.Rounds)
	}
	if cp.count() != 1 {
		t.Fatalf("%d summariser calls for one round", cp.count())
	}
	if comps := compactionEntries(t, s); len(comps) != 1 {
		t.Fatalf("%d compaction entries for one round", len(comps))
	}
}

// TestSummarizeFallsBackToAFlatTranscript drives the two roads of summarize's honest failure
// directly, at sizes the replay loop itself cannot stage: a payload whose ids do not describe its
// messages has nothing that may be sliced at any boundary, and a single pasted document larger
// than the window tips the planning path over the window itself. Both go out as one flattened
// request — lossy, but a summary of nothing is not lossy, it is wrong — and the oversized one is
// read only at the parts that fit.
func TestSummarizeFallsBackToAFlatTranscript(t *testing.T) {
	s, cp := chainFixture(t, "summarize-flat", 4096)
	fillChainPath(t, s, 6, 3000)
	cfg := s.Config()
	p := compact.Params{SummaryMode: compact.ModePrefixStable, ThresholdPct: 70, SummaryCapPct: 50, Marker: compact.DefaultMarker}
	built, sizes, tools, err := s.build()
	if err != nil {
		t.Fatalf("the fixture's window could not be read: %v", err)
	}

	// More entries than the payload has messages: no boundary through it may be trusted.
	refs := summaryPayload(nil, nil, built.All(), asm.EntryIDs(built), tools, len(built.Entries)+3)
	if refs.msgs != nil {
		t.Fatal("the fixture was supposed to produce a payload that cannot be sliced")
	}
	plan := &compact.Plan{Cut: len(built.Entries), Empty: true, Split: true}
	text, size, err := s.summarize(context.Background(), cfg, p, plan, built.Entries, sizes, refs, "", 1, 1, func(ir.SummaryAttempt) {})
	if err != nil {
		t.Fatalf("an unsliceable payload is a reason to fall back, not a reason to fail: %v", err)
	}
	if strings.TrimSpace(text) == "" || size <= 0 {
		t.Fatalf("the fallback answered %q (%d tokens)", text, size)
	}
	req := cp.all()[0]
	last := req.Messages[len(req.Messages)-1]
	if !strings.Contains(msgText(last), "Conversation to compress") {
		t.Fatalf("the flat shape ends with the transcript, got:%s", dumpForTest(req.Messages))
	}
	if !strings.Contains(msgText(last), "abcd") {
		t.Fatal("the transcript carries the region's own text, or the model writes a fiction")
	}
	// A payload that could be sliced never earns the scaffold: only this fallback introduces it.
	if strings.Contains(msgText(req.Messages[0]), "Conversation to compress") {
		t.Fatal("the instruction is the first message, the transcript the last")
	}
}

// TestSummarizeClipsWhatNoWindowCanRead is the second road: one entry larger than the window,
// asked about with bytes that cannot carry it. It goes out clipped to its beginning and end with
// the omission marked — a clipped middle the model is told about, not one it invents.
func TestSummarizeClipsWhatNoWindowCanRead(t *testing.T) {
	s, cp := chainFixture(t, "summarize-clip", 4096)
	if _, err := s.appendEntry(&ir.Entry{Kind: ir.KindUser, Role: "user",
		Content: strings.Repeat("abcd ", 30000)}); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	p := compact.Params{SummaryMode: compact.ModePrefixStable, ThresholdPct: 70, SummaryCapPct: 50, Marker: compact.DefaultMarker}
	built, sizes, tools, err := s.build()
	if err != nil {
		t.Fatalf("the fixture's window could not be read: %v", err)
	}
	refs := summaryPayload(nil, nil, built.All(), asm.EntryIDs(built), tools, len(built.Entries))
	plan := &compact.Plan{Cut: 1, Empty: true, Split: true}
	if _, _, err := s.summarize(context.Background(), cfg, p, plan, built.Entries, sizes, refs, "", 1, 1, func(ir.SummaryAttempt) {}); err != nil {
		t.Fatalf("an oversized region goes flattened, it does not fail: %v", err)
	}
	req := cp.all()[0]
	transcript := msgText(req.Messages[len(req.Messages)-1])
	if !strings.Contains(transcript, "Conversation to compress") {
		t.Fatal("a region that overfits the window is asked about as a transcript")
	}
	if !strings.Contains(transcript, "omitted") {
		t.Fatal("the clipped middle must be marked, or the model invents it")
	}
	if len(transcript) >= len(built.Entries[0].Content) {
		t.Fatal("the clip did nothing: the request is not smaller than the entry")
	}
}
