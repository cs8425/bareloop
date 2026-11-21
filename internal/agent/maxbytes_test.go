package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"bareloop/internal/compact"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// The wall that 413 is: bytes, not tokens. These tests are about which sentence names it, which of the
// two numbers (config, measurement) decides the ceiling, and — the part that bricks a session — that a
// summary request is cut to fit before it goes out, because a request that reads the whole path is as
// big as the conversation nobody can send.

func TestTheByteWallIsNotTheWindow(t *testing.T) {
	for _, c := range []struct {
		text string
		want bool
	}{
		// The real sentence, in the shape internal/llm builds for every non-2xx: llama.cpp answers an
		// oversized body with the status and an empty body, so nothing but the code is there to read.
		{"llm: 413 Request Entity Too Large: (empty body) [142 msgs, 118.4 MB sent, 12 tools]", true},
		// The same failure after it has been stored as an assistant entry and clipped.
		{"[generation failed: llm: 413 Request Entity Too Large: (empty body) [142 msgs, 118.4 MB sent]]", true},
		// A proxy that formats its own answer.
		{"llm: 413 : <html><body>Request Entity Too Large</body></html>", true},
		{"llm: 431 Request Header Fields Too Large: nginx", true},
		// The token wall, which needs a different rescue (and a different number in its sentence).
		{"llm: 400 Bad Request: {\"error\":{\"code\":\"context_length_exceeded\"}} [4 msgs, 2.1 KB sent]", false},
		{"llm: 500 Internal Server Error: the request exceeds the available context size", false},
		{"llm: 503 Service Unavailable: upstream busy [4 msgs, 2.1 KB sent]", false},
		{"", false},
	} {
		if got := isBodyTooLarge(c.text); got != c.want {
			t.Errorf("isBodyTooLarge(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	// The two classifiers answer different questions, and one request can only be saved by the right one.
	if isBodyTooLarge("llm: 400 Bad Request: context_length_exceeded") && !isContextOverflow("llm: 400 Bad Request: context_length_exceeded") {
		t.Error("both walls claim the same sentence, so the session will be rescued with the wrong number")
	}
}

func TestTheCeilingIsTheSmallerOfConfigAndProof(t *testing.T) {
	const mb = 1 << 20
	if got := resolveBodyCeiling(32*mb, 0); got != 32*mb {
		t.Errorf("config alone = %d, want 32 MB", got)
	}
	if got := resolveBodyCeiling(0, 8*mb); got != 8*mb {
		t.Errorf("measurement alone = %d, want 8 MB", got)
	}
	if got := resolveBodyCeiling(0, 0); got != 0 {
		t.Errorf("neither = %d, want 0 (unknown is a number, and it is not free)", got)
	}
	// The interesting pair: config says 32 MB, a 12 MB body was refused. Sending 30 MB because the
	// config allows it would be refused by the evidence, every time, with the ceiling never moving.
	if got := resolveBodyCeiling(32*mb, 12*mb); got != 12*mb {
		t.Errorf("config above the measurement = %d, want 12 MB (min over the values that exist)", got)
	}
	if got := resolveBodyCeiling(4*mb, 12*mb); got != 4*mb {
		t.Errorf("config below the measurement = %d, want 4 MB (the operator's own tighter limit holds)", got)
	}
	for _, c := range []struct {
		cfg, measured int
		want          string
	}{
		{32 * mb, 0, "config"},
		{0, 12 * mb, "measured"},
		{32 * mb, 12 * mb, "measured"},
		{0, 0, "unknown"},
	} {
		if got := bodyCeilingSource(c.cfg, c.measured); !strings.Contains(got, c.want) {
			t.Errorf("source(%d,%d) = %q, want it to say %q", c.cfg, c.measured, got, c.want)
		}
	}
	// An unset config is the state that must be reported to a person: nothing can be cut in advance.
	if !strings.Contains(bodyCeilingSource(0, 0), "llm.maxRequestBodyBytes") {
		t.Error("the unknown answer does not name the setting that would fix it")
	}
}

func TestARefusalIsRememberedAndNeverRaisesTheWall(t *testing.T) {
	s, cp := chainFixture(t, "note-refused", 8192)
	s.provider = &sizedProvider{chainProvider: cp, skeleton: 1000}
	if got := s.bodyCeiling(); got != 0 {
		t.Fatalf("a session that has never been refused claims a ceiling of %d", got)
	}
	s.noteBodyRefused(9000)
	if got := s.bodyCeiling(); got != 9000 {
		t.Fatalf("after a refusal at 9000 the ceiling is %d", got)
	}
	if got := s.lastRefusedBody(); got != 9000 {
		t.Fatalf("the refused size is %d, want the number the transport measured", got)
	}
	// A later, larger refusal proves only that the wall is below that larger number, which is weaker
	// than what is already known: the ceiling must not creep up.
	s.noteBodyRefused(12000)
	if got := s.bodyCeiling(); got != 9000 {
		t.Fatalf("a bigger refusal raised the wall to %d; it is an upper bound, not a measurement of the limit", got)
	}
	s.noteBodyRefused(7000)
	if got := s.bodyCeiling(); got != 7000 {
		t.Fatalf("a smaller refusal did not lower the wall (%d)", got)
	}
	// A provider that cannot size a request answers 0, and 0 must not be read as "the wall is at zero".
	s.noteBodyRefused(0)
	if got := s.bodyCeiling(); got != 7000 {
		t.Fatalf("an unmeasurable refusal reset the wall to %d", got)
	}
	// Config is the operator's answer and outranks nothing that has been refused below it, but it does
	// put a tighter cap on a session that has only ever met a wide-open server.
	s.bodyCfg = 5000
	if got := s.bodyCeiling(); got != 5000 {
		t.Fatalf("config 5000 with a wall measured at 7000 = %d, want 5000", got)
	}
}

// TestTheWallTakesEffectOnAPatchAndNotOnlyOnACreate is the live-fire lesson: the field reached the
// session at create time and never arrived on a PATCH, because UpdateConfig only re-reads the llm
// block when `llm.Config.Same` says it changed — and Same is a hand-rolled comparison of transport
// fields, not a DeepEqual. A limit nobody can change without restarting is a limit that silently
// ignores the Settings panel, which is exactly how a stale n_ctx once sat quiet for days.
func TestTheWallTakesEffectOnAPatchAndNotOnlyOnACreate(t *testing.T) {
	s, _ := chainFixture(t, "patch-wall", 8192)
	if got := s.bodyCeiling(); got != 0 {
		t.Fatalf("a fresh session claims a ceiling of %d", got)
	}
	cfg := s.Config()
	cfg.LLM.MaxBodyBytes = 4096
	if err := s.UpdateConfig(context.Background(), cfg); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got := s.bodyCeiling(); got != 4096 {
		t.Fatalf("a PATCH of llm.maxRequestBodyBytes did not reach the running session (ceiling %d): "+
			"llm.Config.Same has to know this field, or the change waits for a restart", got)
	}
	if got := s.Status().BodyCeiling; got != 4096 {
		t.Fatalf("Status.bodyCeiling = %d after the patch, want 4096", got)
	}
	// The same patch re-runs cleanly: an unchanged ceiling is not a change to the endpoint, so nothing
	// about it has to be relearned.
	if err := s.UpdateConfig(context.Background(), cfg); err != nil {
		t.Fatalf("patch again: %v", err)
	}
	if got := s.bodyCeiling(); got != 4096 {
		t.Fatalf("re-patching the same ceiling lost it (%d)", got)
	}
}

func TestTheConfiguredWallReachesTheSessionAndTheStatusLine(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(context.Background(), Config{
		ID:  "wired-ceiling",
		LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1", Model: "m", NCTX: 4096, MaxBodyBytes: 33554432},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer s.Close()
	if got := s.bodyCeiling(); got != 32<<20 {
		t.Fatalf("llm.maxRequestBodyBytes did not reach the session: ceiling %d", got)
	}
	st := s.Status()
	if st.BodyCeiling != 32<<20 {
		t.Errorf("Status.bodyCeiling = %d, want the configured 32 MB", st.BodyCeiling)
	}
	if !strings.Contains(st.BodyCeilingSource, "config") {
		t.Errorf("Status.bodyCeilingSource = %q, want it to name where the number came from", st.BodyCeilingSource)
	}
	// The window and the wall are separate answers: a big window with a small body limit is the normal
	// shape of a llama.cpp behind a proxy, and one number in the status line would hide it.
	if st.NCTX != 4096 {
		t.Errorf("Status.nCtx = %d, want 4096 beside the byte wall, not instead of it", st.NCTX)
	}
}

// sizedProvider is a summariser that charges for what it is given: the body is the skeleton plus the
// messages, so the only way to keep a request under a limit is to ask about less conversation. The
// arithmetic is deliberately not the real encoder's — internal/llm's golden test pins that — because
// what is under test here is what a session is willing to send.
type sizedProvider struct {
	*chainProvider
	skeleton int
}

func (p *sizedProvider) BodyBytes(req llm.Request) (int, error) {
	n := p.skeleton
	for _, m := range req.Messages {
		n += len(msgText(m)) + 40
	}
	return n, nil
}

func (p *sizedProvider) EnvelopeBytes(llm.Request) (int, error) { return p.skeleton, nil }

// pathOf writes n alternating user/assistant entries of size bytes each.
func pathOf(t *testing.T, s *Session, n, size int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := &ir.Entry{Kind: ir.KindUser, Role: "user", Ts: time.Now().UTC(),
			Content: strings.Repeat("x", size)}
		if i%2 == 1 {
			e = &ir.Entry{Kind: ir.KindAssistant, Role: "assistant", Ts: time.Now().UTC(),
				Content: strings.Repeat("y", size), Model: "a-reasoning-model", Finish: "stop",
				Usage: &ir.Usage{PromptTokens: size / 4, CompletionTokens: size / 4, HasPrompt: true}}
		}
		if _, err := s.appendEntry(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// pathTailHeavy writes twelve short entries and then four long ones. The shape is the test: the byte
// cut drops the NEWEST entries, so the only bytes it can reach are the kept tail's. With a uniform path
// the tail is a fifth of the request and the cuttable band is a few kilobytes wide — wide enough for a
// hardcoded ceiling to land on either side of the floor by luck (it did). Making the tail fat turns the
// band into the whole point, and leaves the assertion about the cut rather than about where the
// arithmetic happened to fall.
func pathTailHeavy(t *testing.T, s *Session) {
	t.Helper()
	for i := 0; i < 16; i++ {
		size := 300
		if i >= 12 {
			size = 3000
		}
		e := &ir.Entry{Kind: ir.KindUser, Role: "user", Ts: time.Now().UTC(),
			Content: strings.Repeat("x", size)}
		if i%2 == 1 {
			e = &ir.Entry{Kind: ir.KindAssistant, Role: "assistant", Ts: time.Now().UTC(),
				Content: strings.Repeat("y", size), Model: "a-reasoning-model", Finish: "stop",
				Usage: &ir.Usage{PromptTokens: size / 4, CompletionTokens: size / 4, HasPrompt: true}}
		}
		if _, err := s.appendEntry(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// TestTheSummaryRequestIsCutToFitTheBody is the whole point of the byte wall: before this, the request
// that was supposed to rescue an oversized session was itself oversized, so nothing could be sent and
// nothing could shrink. The proof is an A/B over two identical paths: the one with no wall asks about
// everything, the one with a wall asks about less and still commits.
func TestTheSummaryRequestIsCutToFitTheBody(t *testing.T) {
	build := func(t *testing.T, id string, wall int) (*sizedProvider, llm.Request, CompactResult) {
		t.Helper()
		s, cp := chainFixture(t, id, 8000)
		sp := &sizedProvider{chainProvider: cp, skeleton: 1000}
		s.provider = sp
		pathTailHeavy(t, s)
		s.bodyCfg = wall
		res, err := s.CompactNow(context.Background(), "")
		if err != nil {
			t.Fatalf("compact %s: %v", id, err)
		}
		asks := sp.all()
		if len(asks) == 0 {
			t.Fatalf("%s: the summariser was never asked; the round did not reach the request: %+v", id, res)
		}
		return sp, asks[0], res
	}

	plainSP, plainReq, plainRes := build(t, "no-wall", 0)
	if !plainRes.Committed {
		t.Fatalf("the control round did not commit (%s): %+v", plainRes.Skipped, plainRes)
	}
	plainBytes, err := plainSP.BodyBytes(plainReq)
	if err != nil {
		t.Fatal(err)
	}
	// The wall is placed relative to the control's own measured size rather than hardcoded, and the
	// distance is chosen so the band cannot be missed: the floor at plan.Cut is always at least one kept
	// entry below the whole path, and in this fixture the last entry alone is worth 3000 bytes. So a wall
	// 1000 under the whole path is guaranteed to be above the floor (something must be cut) and below it
	// (the round still has to shrink) — no fraction of the total, no luck, no drift when keepBudget moves.
	limit := plainBytes - 1000

	cutSP, cutReq, cutRes := build(t, "wall", limit)
	if !cutRes.Committed {
		t.Fatalf("a compaction whose request fits the wall did not commit (%s): %+v", cutRes.Skipped, cutRes)
	}
	cutBytes, err := cutSP.BodyBytes(cutReq)
	if err != nil {
		t.Fatal(err)
	}
	if cutBytes > limit {
		t.Fatalf("a %s request went to an endpoint whose limit is %s: the wall was not cut",
			humanBytes(cutBytes), humanBytes(limit))
	}
	if cutBytes >= plainBytes {
		t.Fatalf("the walled round asked about %s and the unwalled one about %s: nothing was cut",
			humanBytes(cutBytes), humanBytes(plainBytes))
	}
	if len(cutReq.Messages) >= len(plainReq.Messages) {
		t.Fatalf("the cut request carries %d messages against the whole path's %d: the scope never moved",
			len(cutReq.Messages), len(plainReq.Messages))
	}
}

// TestAWallTooLowToSendAnythingIsSaidOnce covers the other half of the promise: when the smallest
// question this round can ask is still over the wall, the session must not upload it three times.
func TestAWallTooLowToSendAnythingIsSaidOnce(t *testing.T) {
	s, cp := chainFixture(t, "wall-too-low", 8000)
	sp := &sizedProvider{chainProvider: cp, skeleton: 1000}
	s.provider = sp
	pathOf(t, s, 16, 1500)
	s.bodyCfg = 1200 // below even the skeleton, so nothing can ever fit

	res, err := s.CompactNow(context.Background(), "")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if got := sp.count(); got != 0 {
		t.Fatalf("a body known to be over the wall was sent %d time(s); a measured refusal needs no rehearsal", got)
	}
	if res.Committed {
		t.Fatal("something committed although every request this round could make is over the wall")
	}
	for _, want := range []string{"narrowest summary", "raise llm.maxRequestBodyBytes"} {
		if !strings.Contains(res.Skipped, want) {
			t.Fatalf("the skip reason does not say %q: %q", want, res.Skipped)
		}
	}
}

// TestAWallNothingCanFitStillReportsTheNumbers is the debug surface for the case above: the sentence a
// person reads has to carry both sizes, because the decision they are making is how much to raise it.
func TestAWallNothingCanFitStillReportsTheNumbers(t *testing.T) {
	if got := humanBytes(999); got != "999 B" {
		t.Errorf("humanBytes(999) = %q", got)
	}
	if got := humanBytes(20000); !strings.HasSuffix(got, "KB") {
		t.Errorf("humanBytes(20000) = %q, want KB", got)
	}
	if got := humanBytes(32 << 20); got != "32.0 MB" {
		t.Errorf("humanBytes(32 MB) = %q", got)
	}
}

// TestTheByteWallCompactsTheTurnBeforeTheUpload is guardBody's reason to exist: the token window is
// nowhere near its limit, so nothing in the existing triggers would have moved — but the request about
// to be uploaded is already over the endpoint's wall, and one compaction now buys back the upload. It
// tries three walls, because the band that decides the outcome runs between the round's floor and the
// payload: a wall inside it is a win the guard has to take, a wall below it is a round that cannot be
// rescued and a send the guard must not block, and no wall at all is the control that leaves the path
// alone.
func TestTheByteWallCompactsTheTurnBeforeTheUpload(t *testing.T) {
	s, cp := chainFixture(t, "guard-wall", 8000)
	s.provider = &sizedProvider{chainProvider: cp, skeleton: 1000}
	// The shape is the one TestTheSummaryRequestIsCutToFitTheBody proves commits at this window: a fat
	// tail and a cuttable head. A uniform path under a 262k window is a request over its byte wall and a
	// compactor that sees nothing old enough to replace, so the guard cannot win — which is a fact about
	// the fixture rather than about the guard, and the fixture borrows the proven shape.
	pathTailHeavy(t, s)

	_, sizes, err := s.sizesNow()
	if err != nil {
		t.Fatalf("sizes: %v", err)
	}
	if sizes.Total() >= 8000*98/100 {
		t.Fatalf("the window would have fired too (%d tokens); this test is about bytes", sizes.Total())
	}
	// The band a wall has to sit in is narrower than "smaller than the payload": it runs from the round's
	// floor (the narrowest question the summariser may ask, which is the region it is replacing — here
	// measured at 11.4 KB) up to the payload itself (15.6 KB). Placement is relative for the reason the
	// comment above TestTheSummaryRequestIsCutToFitTheBody gives, with one side of that band pinned by its
	// own case below rather than trusted to arithmetic.
	s.bodyCfg = sizes.WireTotal() - 2000
	before := sizes.WireTotal()
	if over, est, ceil := s.bodyOverWall(sizes); !over {
		t.Fatalf("the pre-send check did not see the wall: %d B against %d B", est, ceil)
	}
	if !s.guardBody(context.Background(), sizes) {
		// The comparison fired and still nothing came back. Ask the compactor the same question once
		// more, in the open, so the failure names the half that let the test down.
		res := s.compactHere(context.Background(), compact.TriggerManual, true)
		t.Fatalf("guardBody fired at %d B against a %d B wall but the path did not shrink (asked again: committed=%v skipped=%q)",
			before, s.bodyCfg, res.Committed, res.Skipped)
	}
	_, afterSizes, err := s.sizesNow()
	if err != nil {
		t.Fatalf("sizes after: %v", err)
	}
	if afterSizes.WireTotal() >= before {
		t.Fatalf("guardBody reported a compaction that did not shrink the payload: %d B -> %d B", before, afterSizes.WireTotal())
	}

	// The other side of the band: a wall so low that no question this round can ask fits under it, so
	// there is nothing to hand back. What matters is what the guard does then — it says no and lets the
	// turn send, because an estimate has no business refusing an upload, and because refusing here would
	// invent a second failure path for a problem the reactive leg already reports with numbers. The cost
	// of the shrug is one plan and no generation: the skip happens before anything is sent.
	low, lowCP := chainFixture(t, "guard-floor", 8000)
	low.provider = &sizedProvider{chainProvider: lowCP, skeleton: 1000}
	pathTailHeavy(t, low)
	low.bodyCfg = 1
	_, lowSizes, err := low.sizesNow()
	if err != nil {
		t.Fatalf("floor sizes: %v", err)
	}
	if over, est, ceil := low.bodyOverWall(lowSizes); !over {
		t.Fatalf("a %d B payload was not seen as over a %d B wall", est, ceil)
	}
	if low.guardBody(context.Background(), lowSizes) {
		t.Fatalf("the guard reported a shrink at a wall nothing this round can get under")
	}

	plain, plainCP := chainFixture(t, "guard-nowall", 8000)
	plain.provider = &sizedProvider{chainProvider: plainCP, skeleton: 1000}
	pathTailHeavy(t, plain)
	_, plainSizes, err := plain.sizesNow()
	if err != nil {
		t.Fatalf("control sizes: %v", err)
	}
	if plain.guardBody(context.Background(), plainSizes) {
		t.Fatalf("a session with no wall known compacted anyway; the ceiling is unknown, not zero")
	}
}

// TestTheByteGuardCountsTheSkeletonToo pins the half of the number the path estimate cannot see: a turn
// that fits by 1.6 KB does not fit once the system prompt and the tool schemas are counted, and that is
// what the pre-send comparison exists to catch. Asserted on the comparison rather than on a compaction:
// whether a small path is worth summarising is a separate question with its own floor, and a test that
// conflates the two goes red for the wrong reason.
func TestTheByteGuardCountsTheSkeletonToo(t *testing.T) {
	s, cp := chainFixture(t, "guard-env", 262144)
	sp := &sizedProvider{chainProvider: cp, skeleton: 4000}
	s.provider = sp
	pathOf(t, s, 10, 300)
	s.noteEnvelopeBytes(llm.Request{})
	if s.bodyEnv != 4000 {
		t.Fatalf("noteEnvelopeBytes recorded %d, want the skeleton the provider reported (4000)", s.bodyEnv)
	}
	_, sizes, err := s.sizesNow()
	if err != nil {
		t.Fatalf("sizes: %v", err)
	}
	// The two guard rails that make the case below a case rather than a coincidence.
	if sizes.WireTotal() >= 5000 {
		t.Fatalf("the path alone is already at %d B against a 5 KB wall; shrink pathOf", sizes.WireTotal())
	}
	if sizes.WireTotal()+s.bodyEnv < 5000 {
		t.Fatalf("even the envelope does not cross the wall (%d + %d < 5000); thicken pathOf", sizes.WireTotal(), s.bodyEnv)
	}
	s.bodyCfg = 5000
	over, est, ceil := s.bodyOverWall(sizes)
	if ceil != 5000 {
		t.Fatalf("the wall in the answer is %d, not the configured 5000", ceil)
	}
	if est != sizes.WireTotal()+4000 {
		t.Fatalf("the figure is %d, which is not the path's %d plus the skeleton's 4000", est, sizes.WireTotal())
	}
	if !over {
		t.Fatalf("the pre-send check called %d B against a %d B wall a fit", est, ceil)
	}
	// And the same payload without the envelope figure is nobody's problem: this is the envelope moving
	// the verdict, not a wall that happened to be small. chainFixture's provider is a plain
	// chainProvider, which is not a BodySizer, so nothing ever measures this session's skeleton.
	noEnv, _ := chainFixture(t, "guard-noenv", 262144)
	pathOf(t, noEnv, 10, 300)
	noEnv.bodyCfg = 5000
	_, noEnvSizes, err := noEnv.sizesNow()
	if err != nil {
		t.Fatalf("control sizes: %v", err)
	}
	if noEnv.bodyEnv != 0 {
		t.Fatalf("the control session measured an envelope (%d) without a BodySizer provider", noEnv.bodyEnv)
	}
	if noEnvSizes.WireTotal()+noEnv.bodyEnv >= 5000 {
		t.Fatalf("the control path is not under the wall on its own (%d B); shrink pathOf", noEnvSizes.WireTotal())
	}
	if over2, est2, _ := noEnv.bodyOverWall(noEnvSizes); over2 {
		t.Fatalf("the same path was over the wall with no skeleton counted (%d B)", est2)
	}
}

// TestTheLimitIsQuotedAsTheEvidenceStatesIt pins the sentence a person reads when a body comes back
// refused. The refused size is an upper bound on the wall, not the wall, and the two numbers being equal
// is the common case rather than the corner: the refusal is where the ceiling came from.
func TestTheLimitIsQuotedAsTheEvidenceStatesIt(t *testing.T) {
	if got := bodyLimitPhrase(0, 0); got != "unknown" {
		t.Errorf("no ceiling and nothing measured: %q, want \"unknown\"", got)
	}
	if got := bodyLimitPhrase(119<<20, 119<<20); got != "under the 119.0 MB it refused" {
		t.Errorf("a wall taken from this very refusal is quoted as %q, which asks the reader to fit inside the size that was just refused", got)
	}
	// A configured wall sitting ABOVE the true one must not be quoted as if it were the limit: the refusal
	// is the better evidence, and quoting 286 MB here would tell the reader their 196 MB body was fine.
	if got := bodyLimitPhrase(286<<20, 196<<20); got != "under the 196.0 MB it refused" {
		t.Errorf("a config wall above this refusal is quoted as %q, want the refused size quoted instead", got)
	}
	if got := bodyLimitPhrase(32<<20, 40<<20); got != "32.0 MB" {
		t.Errorf("a configured wall (32 MB) against a larger body (40 MB): %q, want no hedge", got)
	}
	if got := bodyLimitPhrase(8<<20, 16<<20); strings.Contains(got, "refused") {
		t.Errorf("a wall lower than the body just refused: %q — it is not this request's own size, so it reads as a limit", got)
	}
}

// TestTheGuardActsBelowTheWallItWasGiven pins the margin, which exists because of something watched
// happening at 117 MB: a refusal records the ceiling as the size it refused, and that size is one the
// endpoint has already sent back. A same-shaped request then estimates a few bytes under that ceiling
// (the estimate reads low) and goes straight out to be refused again — forever, the guard never firing.
// So the check acts at 90% of the wall, and a wall 20% above the payload is the far side of that.
func TestTheGuardActsBelowTheWallItWasGiven(t *testing.T) {
	s, cp := chainFixture(t, "guard-margin", 8000)
	sp := &sizedProvider{chainProvider: cp, skeleton: 1000}
	s.provider = sp
	pathOf(t, s, 6, 500)
	s.noteEnvelopeBytes(llm.Request{})
	_, sizes, err := s.sizesNow()
	if err != nil {
		t.Fatalf("sizes: %v", err)
	}
	est := sizes.WireTotal() + s.bodyEnv

	s.bodyCfg = est * 6 / 5 // 20% of headroom: outside the margin, nobody's problem yet
	if over, gotEst, gotCeil := s.bodyOverWall(sizes); over || gotEst != est || gotCeil != s.bodyCfg {
		t.Fatalf("a wall 20%% above the payload fired (over=%v est=%d ceil=%d, payload %d); the margin is acting too early",
			over, gotEst, gotCeil, est)
	}

	s.bodyCfg = est * 10 / 9 // the far edge of the margin: 90% of this wall is the payload itself
	if over, _, _ := s.bodyOverWall(sizes); !over {
		t.Fatalf("a wall whose 90%% line the payload has reached did not fire (payload %d, wall %d)", est, s.bodyCfg)
	}
}

// TestTheByteWallCutsDeepEnoughToReachItsSideOfTheRoom is the live scene that the margin alone could
// not fix: the pre-send guard fired, the summary request was cut to fit, the round committed — and the
// turn was still refused, because the plan had only replaced two entries and the endpoint counts bytes
// while the keep budget counts tokens. On a path of screenshots those rulers disagree by two orders of
// magnitude (an image is ~500 tokens and several megabytes), so "well inside the window" and "far over
// the wall" are both true of the same tail.
//
// The proof is an A/B over two identical paths and the same forced round: the one with no wall gets the
// token answer, the one with a wall gets a deeper cut — and the depth is compared, not inferred, because
// "the boundary moved" is the claim. The payload landing under the line afterwards is what says the move
// was worth an upload.
func TestTheByteWallCutsDeepEnoughToReachItsSideOfTheRoom(t *testing.T) {
	ctx := context.Background()

	// The shape is built so the two rulers cannot agree, and the constant that decides it is
	// minSummarizeTokens — a flat 2,000 tokens (compact's default), not a share of the window. A path of
	// small entries therefore cannot be cut shallowly at all: 300-byte entries are 75 tokens, so the floor
	// demands 14 of them before a summary is "worth asking about", and that is how the control landed on
	// the same boundary as the byte bound and proved nothing. Uniform entries of 4,400 bytes are 1,100
	// tokens apiece: the floor is satisfied by two, so the token answer sits at 2 while the same 16
	// entries weigh 70,400 bytes and no wall this test can set reaches them without cutting past that.
	//
	// The window is 100,000 so the other token ruler stays out too: keepBudget is 20% of it, a 20,000-token
	// cap against a 17,600-token path, which binds nowhere. Everything about the control's boundary is then
	// the floor that makes a summary worth having, and nothing else — which is the point of comparing.
	plain, pp := chainFixture(t, "deep-plain", 100000)
	plain.provider = &sizedProvider{chainProvider: pp, skeleton: 1000}
	pathOf(t, plain, 16, 4400)
	resPlain := plain.compactHere(ctx, compact.TriggerManual, true)
	if !resPlain.Committed {
		t.Fatalf("the control round did not even commit (skipped %q); the fixture is broken, not the bound", resPlain.Skipped)
	}
	if resPlain.Replaced > 4 {
		t.Fatalf("the control is not the shallow token answer (replaced %d): the token rulers are already cutting deep, so there is no room for bytes to move anything", resPlain.Replaced)
	}

	s, cp := chainFixture(t, "deep-wall", 100000)
	s.provider = &sizedProvider{chainProvider: cp, skeleton: 1000}
	pathOf(t, s, 16, 4400)
	_, sizes, err := s.sizesNow()
	if err != nil {
		t.Fatalf("sizes: %v", err)
	}
	before := sizes.WireTotal()
	if sizes.Total() >= 100000*98/100 {
		t.Fatalf("the window would have fired too (%d tokens); this test is about bytes", sizes.Total())
	}
	// Two thirds of the payload puts the path at 1.5x the wall, inside the band a round can work in: the
	// wall caps the tail the round keeps and, through the question the round has to ask, the region it
	// replaces — and those two added together are the path. A wall under about half the payload therefore
	// asks for a cut so deep that a summary of what it replaced no longer fits the wall itself, and the
	// round skips ("the narrowest summary this round can ask is 11.4 KB and this endpoint takes 7.6 KB"):
	// correct behaviour, wrong end of the band for this test. The fixed cost of asking — the instruction,
	// the envelope, and whatever the estimate leaves out — is proportionally far larger on a 70 KB path
	// than on the live 146 MB one, which is why the fixture sits at 1.5x while live was safe at 1.25x.
	s.bodyCfg = before * 2 / 3
	if over, est, ceil := s.bodyOverWall(sizes); !over {
		t.Fatalf("the pre-send check did not see the wall: %d B against %d B", est, ceil)
	}
	if resPlain.Replaced < 1 {
		t.Fatalf("control replaced %d entries; the A/B has no token answer to compare against", resPlain.Replaced)
	}
	// compactHere rather than guardBody so the depth is readable: a bool cannot report who moved the
	// boundary. guardBody's own wiring is pinned by TestTheByteWallCompactsTheTurnBeforeTheUpload, which
	// calls it and reads the shrink back.
	res := s.compactHere(ctx, compact.TriggerManual, true)
	if !res.Committed {
		t.Fatalf("the byte-bound round did not commit (skipped %q); the wall is probably outside the band a round can work in", res.Skipped)
	}
	if res.Replaced <= resPlain.Replaced {
		t.Fatalf("the wall did not deepen the cut: replaced %d, where the token answer alone replaces %d",
			res.Replaced, resPlain.Replaced)
	}

	_, afterSizes, err := s.sizesNow()
	if err != nil {
		t.Fatalf("sizes after: %v", err)
	}
	if afterSizes.WireTotal() >= before {
		t.Fatalf("the byte-bound round did not shrink the payload: %d B -> %d B", before, afterSizes.WireTotal())
	}
	// The claim the live run owed: one compaction, and the next request is off the wall — not a
	// compaction every turn that changes nothing.
	if over, est, ceil := s.bodyOverWall(afterSizes); over {
		t.Fatalf("after a byte-bound compaction the payload is still at the line: %d B against %d B (before %d B)",
			est, ceil, before)
	}
}

// ─── the reactive resend: what the loop does after the endpoint refuses a body ──────────────

// wallProvider is a run's model that answers by size. It refuses the first `refuse` requests the way
// llama.cpp refuses an oversized body — the status and an empty body, no prose to match against — and
// answers the rest with an ordinary one-word reply; a negative `refuse` refuses everything. It borrows
// both the summariser and the byte arithmetic from sizedProvider, so the number it refuses is a number the
// session itself measured, and it records the size of every request it saw: that is the only way to tell
// "it resent the same thing" apart from "it resent less", which is the whole difference between a rescue
// and a loop.
type wallProvider struct {
	*sizedProvider
	mu     sync.Mutex
	tried  []int
	refuse int
}

func (p *wallProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	n, err := p.sizedProvider.BodyBytes(req)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.tried = append(p.tried, n)
	outstanding := p.refuse
	if outstanding > 0 {
		p.refuse--
	}
	p.mu.Unlock()
	if outstanding != 0 {
		return nil, errors.New("llm: 413 Payload Too Large: (empty body)")
	}
	return says("done")(ctx, req, nil)
}

func (p *wallProvider) sizes() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.tried...)
}

// TestARefusedBodyResendsItselfAfterTheCompaction is the fix for a loop that stopped after saving itself.
// The byte branch of the error path compacts the path and then used to end the run with "the context was
// compacted, please send again" — which spends a summary generation, leaves the message unanswered, and
// does it in the one moment the session knows the wall best. One send now has to produce one answer, and
// the endpoint has to have seen it twice: refused at the old size, answered at a smaller one.
func TestARefusedBodyResendsItselfAfterTheCompaction(t *testing.T) {
	s, cp := chainFixture(t, "resend-once", 100000)
	wp := &wallProvider{sizedProvider: &sizedProvider{chainProvider: cp, skeleton: 1000}, refuse: 1}
	s.provider = wp
	pathOf(t, s, 30, 1100)

	send(t, s, "answer this")
	waitForIdle(t, s)

	sizes := wp.sizes()
	if len(sizes) != 2 {
		t.Fatalf("the endpoint saw %d request(s) (%v), want two — the one it refused and the one it answered: %s",
			len(sizes), sizes, describePath(s.path()))
	}
	if sizes[1] >= sizes[0] {
		t.Fatalf("the resend was %d bytes after a refused %d: the compaction was paid for and not used", sizes[1], sizes[0])
	}
	path := s.path()
	tip := path[len(path)-1]
	if tip.Kind != ir.KindAssistant || !strings.Contains(tip.Content, "done") {
		t.Fatalf("the run ended on %s %q instead of an answer: the message is still unanswered", tip.Kind, clip(tip.Content, 120))
	}
	// The refusal stays in the story. The loop worked around it, and whoever reads this transcript later
	// should be able to see that it did rather than being shown an answer that looks first-time right.
	var saw413 bool
	for _, e := range path {
		if strings.Contains(e.Content, "413") {
			saw413 = true
		}
	}
	if !saw413 {
		t.Fatal("the refused request left no trace on the path: a retry nobody can see is a retry nobody trusts")
	}
}

// TestTheResendIsBoundedAndEachAttemptIsSmaller is the other half of the promise. On an endpoint that
// refuses everything, the loop must stop with a count in the sentence rather than upload forever — and
// every attempt has to be smaller than the last, because a refusal measures an upper bound: the session
// learns "it is under this", so the next request it builds is a tighter question, not the same one again.
func TestTheResendIsBoundedAndEachAttemptIsSmaller(t *testing.T) {
	s, cp := chainFixture(t, "resend-bound", 200000)
	wp := &wallProvider{sizedProvider: &sizedProvider{chainProvider: cp, skeleton: 1000}, refuse: -1}
	s.provider = wp
	// What this test can honestly promise, and what it cannot. It promises that a run against an endpoint
	// that refuses everything uploads a BOUNDED number of times, that each upload is smaller than the last,
	// and that the run ends on the refusal rather than on a fake answer. It deliberately does not promise
	// that maxBodyResends is what stops it here: inside one turn a forced compaction usually gets one shot,
	// because the cut lands on a floor and the next 10% has nothing outside it to summarize. Measured, not
	// theorised — 401 entries at 457 KB came back refused and the summary left 147 entries at 165 KB, which
	// is keepBudget (20% of a 200k window = 40k tokens) and not the byte line (the wall allowed ~365);
	// the second refusal then found the path already sitting on that floor. So the bound is asserted as a
	// ceiling. Its job is the shape where the path DOES stay cuttable — a run that grows again between
	// refusals, i.e. tool rounds, where without a counter the loop would pay a full upload per refusal.
	pathOf(t, s, 400, 1100)

	send(t, s, "answer this")
	waitForIdle(t, s)

	sizes := wp.sizes()
	if len(sizes) < 2 {
		t.Fatalf("the endpoint saw only %d request(s) (%v): a refused body has to go out again after the compaction: %s",
			len(sizes), sizes, describePath(s.path()))
	}
	if limit := maxBodyResends + 1; len(sizes) > limit {
		t.Fatalf("the endpoint saw %d request(s) (%v), past what maxBodyResends allows (%d): the reactive branch uploads forever: %s",
			len(sizes), sizes, limit, describePath(s.path()))
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] >= sizes[i-1] {
			t.Fatalf("attempt %d was %d bytes after attempt %d's %d: a refusal has to tighten what goes out next, not repeat it",
				i+1, sizes[i], i, sizes[i-1])
		}
	}
	path := s.path()
	tip := path[len(path)-1]
	if tip.Kind == ir.KindAssistant && strings.Contains(tip.Content, "done") {
		t.Fatal("an endpoint that refuses everything ended with an answer: the run did not stop on the refusal")
	}
	if !strings.Contains(tip.Content, "413") {
		t.Fatalf("the run ended on %q, not on the refusal it gave up fighting", clip(tip.Content, 160))
	}
}
