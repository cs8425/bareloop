package agent

import (
	"context"
	"strings"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
)

// How much of a round the summariser is shown. The narrow rule — ask only about the region the round
// replaces — is what the compactor did between 2026-11-11 and 2026-11-17, and the field verdict on it
// is that long sessions got worse to watch: a summariser that never sees the surviving tail writes down
// what was dropped and misses where the work stands. So the request now reaches to the end of the round,
// and the tail it read is then committed beside the summary verbatim — two accounts of one stretch, on
// purpose, the second in bytes.
//
// What must not move is the boundary: the summary being *shown* the tail does not give the compaction
// leave to *replace* it. That is why this fixture drives summarize with a plan and reads only the
// request — the plan it is handed is the plan that gets committed, unchanged.
func TestTheSummaryIsShownTheWholeRound(t *testing.T) {
	s, cp := chainFixture(t, "scope-path", 200000)
	fillNumbered(t, s, 6, 200)
	cfg := s.Config()
	built, sizes, tools, err := s.build()
	if err != nil {
		t.Fatalf("the fixture's window could not be read: %v", err)
	}
	cur, curIDs := built.All(), asm.EntryIDs(built)
	refs := summaryPayload(nil, nil, cur, curIDs, tools, len(built.Entries))
	// A cut with room behind it: entries 03–06 survive this round verbatim, so their text is the
	// witness that the request either did or did not reach the end of the round.
	plan := &compact.Plan{Cut: 2}
	ask := func(scope string) []asm.Message {
		t.Helper()
		p := compact.Params{SummaryMode: compact.ModePrefixStable, SummaryScope: scope,
			ThresholdPct: 70, SummaryCapPct: 50, Marker: compact.DefaultMarker}
		if _, _, err := s.summarize(context.Background(), cfg, p, plan, built.Entries, sizes, refs, "", 1, 1,
			func(ir.SummaryAttempt) {}); err != nil {
			t.Fatalf("summarize (scope %q): %v", scope, err)
		}
		all := cp.all()
		req := all[len(all)-1]
		if len(req.Messages) == 0 {
			t.Fatalf("scope %q sent an empty request", scope)
		}
		last := req.Messages[len(req.Messages)-1]
		if !strings.Contains(msgText(last), "Please compress") {
			t.Fatalf("scope %q does not end with the instruction the round adds:%s", scope, dumpForTest(req.Messages))
		}
		if isOneOf(cur, last) {
			t.Fatalf("scope %q sent a payload message as its instruction:%s", scope, dumpForTest(req.Messages))
		}
		return req.Messages[:len(req.Messages)-1]
	}

	body := ask("")
	if len(body) != len(cur) {
		t.Fatalf("the default scope carries %d of the round's %d messages: a round is asked about its whole path, not about its cut:%s",
			len(body), len(cur), dumpForTest(body))
	}
	if !strings.Contains(msgText(body[len(body)-1]), "entry 06") {
		t.Fatalf("the request ends before the entry that survives this round, so the summary is written blind to it:%s",
			dumpForTest(body))
	}
	// The bytes ahead of it are the payload's own, message for message: reaching further is still a
	// prefix of what the conversation is sending, which is the whole reason this shape was chosen.
	for i, m := range body {
		if m.Role != cur[i].Role || msgText(m) != msgText(cur[i]) {
			t.Fatalf("message %d is not the message the loop is sending:%s", i, dumpForTest(body))
		}
	}

	// The narrow rule is one config key away, and it is still a prefix — it simply stops at the cut.
	body = ask(compact.ScopeRegion)
	if want := asm.HeadLen(curIDs) + plan.Cut; len(body) != want {
		t.Fatalf("scope region carries %d messages, want the head plus the cut's %d:%s", len(body), plan.Cut, dumpForTest(body))
	}
	for _, m := range body {
		if strings.Contains(msgText(m), "entry 06") {
			t.Fatal("the narrow scope must stop at the cut, or the two scopes are the same rule")
		}
	}
}
