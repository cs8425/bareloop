// The other wall. n_ctx bounds the tokens a model will read; this bounds the bytes a server will
// accept — and the two fail in different places, which is why one number cannot stand in for the
// other. A body over the wall is refused with 413 before a token is counted, so the session that
// trips it cannot be saved by any token arithmetic: the request does not go out, and neither does
// the compaction request that would have shrunk it (it reads the same path). That is how a session
// bricks itself at 32 MiB of screenshot rather than at 262k tokens.
//
// So the rule here is the same shape as nctx.go's, over bytes: config (the operator knows their -bs)
// and a refusal (413 at N proves the ceiling is under N, and N is measured exactly — the transport
// knows the length of what it wrote) combine as a minimum over the values that exist, and neither
// third means unknown, which is reported rather than guessed at, because cutting a summary request "a
// bit" with no number to cut to is how you lose part of a conversation for no reason.
//
// Like the resolved window, the resolved ceiling is runtime state only: it is never written back to
// config.json, so a config file copied to another machine keeps meaning "my limit", not "the limit
// I last happened to measure".

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/llm"
)

// resolveBodyCeiling is the rule above, as a function of two numbers — and it is the same expression as
// resolveNCTX, for the same reason: min over the values that exist. Config first when it is the only
// number, because it describes the server; the measurement when it is smaller, because a body the
// server has already sent back is evidence, and honouring a claim over the evidence means tripping the
// wall again on every later request with the ceiling refusing to move.
func resolveBodyCeiling(cfgBytes, measuredBytes int) int {
	switch {
	case cfgBytes <= 0:
		return measuredBytes // nobody configured it: the refusal is all there is
	case measuredBytes <= 0:
		return cfgBytes // nothing refused yet: take the operator at their word
	case measuredBytes < cfgBytes:
		return measuredBytes // the wall was met below what config claims
	default:
		return cfgBytes
	}
}

// bodyCeilingSource labels the decision for the log line and the status, in the same spirit as
// nctxSource: an operator reading "12 MiB limit (measured)" knows the number came from a refusal and
// may be one request too generous, and "unknown" is the state that must be reported, because with no
// number nothing can be cut in advance.
func bodyCeilingSource(cfgBytes, measuredBytes int) string {
	switch {
	case cfgBytes > 0 && measuredBytes > 0 && measuredBytes < cfgBytes:
		return "measured (a request this size was refused below the configured limit)"
	case cfgBytes > 0:
		return "config (llm.maxRequestBodyBytes)"
	case measuredBytes > 0:
		return "measured (a request this size was refused)"
	default:
		return "unknown (set llm.maxRequestBodyBytes)"
	}
}

// isBodyTooLarge recognises the family of provider answers that mean "the body was refused", as
// opposed to "the window was too small". The status code is the fact: internal/llm formats every
// non-2xx as "llm: <status> <reason>: <body> [brief]", and llama.cpp answers an oversized body with
// the status and an empty body, which is why the sentence is matched rather than its contents.
//
// 413 is the wall proper; 431 (header fields too large) is folded in because a session that carries a
// huge system prompt through a proxy can be refused that way for the same reason and needs the same
// rescue. The prose patterns are the safety net for a server that formats its own answer — a false
// positive here only costs one compaction, a false positive on the token side costs a wrong window.
func isBodyTooLarge(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	for _, m := range []string{"llm: 413 ", "llm: 431 "} {
		if strings.Contains(t, m) {
			return true
		}
	}
	for _, m := range []string{"request entity too large", "payload too large", "too large for a request body",
		"exceeds the maximum request body", "max request size exceeded"} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// bodyBytesOf asks the transport how big this request's body is, exactly — the same encoder that
// will produce the bytes on the wire, so the answer is not an estimate of one. 0 means "this provider
// cannot say" (it does not implement llm.BodySizer), which is carried as 0 everywhere: unknown, not
// free.
func (s *Session) bodyBytesOf(req llm.Request) int {
	bs, ok := s.provider.(llm.BodySizer)
	if !ok {
		return 0
	}
	n, err := bs.BodyBytes(req)
	if err != nil {
		s.logf("body size of the outgoing request could not be measured: %v", err)
		return 0
	}
	return n
}

// noteBodyRefused records the size of a body the server refused, which is a measurement of the wall:
// the ceiling is somewhere below it. The request is still in hand at the call site (the moment the
// provider answered), so the number is the transport's own, not a reconstruction from prose.
//
// A refusal smaller than what is already known replaces it, because it proves a lower wall; a larger
// one cannot (a later request may have been shrunk by compaction, so its survival proves little).
func (s *Session) noteBodyRefused(bytes int) {
	if bytes <= 0 {
		return // unmeasurable: the ceiling stays whatever it was, including unknown
	}
	s.mu.Lock()
	changed := false
	if s.bodyRefused == 0 || bytes < s.bodyRefused {
		s.bodyRefused = bytes
		changed = true
	}
	cfg, refused := s.bodyCfg, s.bodyRefused
	ceil := resolveBodyCeiling(cfg, refused)
	s.mu.Unlock()
	if changed {
		// Both numbers are read inside the lock and printed outside it: the sentence is for whoever is
		// watching the wall, and it has to agree with what the resolver just decided.
		s.logf("body: %s refused by this endpoint; ceiling now %s - %s", humanBytes(bytes),
			humanBytes(ceil), bodyCeilingSource(cfg, refused))
	}
}

// bodyCeilingLocked is the effective byte wall (0 = nobody knows yet). Callers holding s.mu may use
// it directly; the rest use bodyCeiling().
func (s *Session) bodyCeilingLocked() int {
	return resolveBodyCeiling(s.bodyCfg, s.bodyRefused)
}

func (s *Session) bodyCeiling() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodyCeilingLocked()
}

// lastRefusedBody is the size of the biggest body this endpoint has called too large, 0 if nothing has
// been measured yet (an unset config and a provider that cannot size a request).
func (s *Session) lastRefusedBody() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodyRefused
}

// errBodyCeiling marks a request that was measured before sending and found over the wall with no room
// left to cut. The attempt loop stops on it instead of re-sending: the size is known exactly, so
// asking again is another upload of bytes the server has already refused — which is how one 413 used
// to cost three, and why a session with a 100 MiB path and a 32 MiB limit took a minute to say no.
var errBodyCeiling = errors.New("summary request would not fit this endpoint's body limit")

// noteEnvelopeBytes records the one part of the body the path estimate cannot see: the system prompt,
// the tool schemas and the sampling parameters — which the transport counts exactly and cheaply, being
// the request without its messages. Every send leaves the freshest figure here, so the next turn's
// pre-send check is a byte count of the request as it would actually go out rather than a sum of entry
// sizes alone. A provider that cannot say leaves the previous figure standing: a skeleton measured one
// turn ago is closer than nothing, and this number moves by hundreds of bytes, not megabytes.
func (s *Session) noteEnvelopeBytes(req llm.Request) {
	bs, ok := s.provider.(llm.BodySizer)
	if !ok {
		return
	}
	env, err := bs.EnvelopeBytes(req)
	if err != nil || env <= 0 {
		return
	}
	s.mu.Lock()
	s.bodyEnv = env
	s.mu.Unlock()
}

// bodyGuardMarginPct is how much room the pre-send check leaves under the wall before it acts. Two
// things make a bare "estimate >= ceiling" the wrong test, and the second one is not hypothetical: it
// was watched happening at 117 MiB.
//
//   - The estimate reads low. Calibrated against the transport it is 0.001% on a path of nothing but
//     images (1,141 bytes on 123 MiB) and up to 8% on a tool-heavy one, because quoted text in tool
//     results is where the residual lives.
//   - A refusal sets the ceiling to the size it refused — and that size is by definition one the
//     endpoint has already sent back. So the wall and "the next request of the same shape" are the same
//     number, and any estimate error at all, in the reading-low direction, puts the next request under a
//     ceiling it is going to breach. Without a margin the session uploads 117 MiB, is refused, learns a
//     ceiling of exactly 117 MiB, and then uploads 117 MiB again forever: the guard never fires because it
//     is comparing a number that reads low against a number that is itself the failure.
//
// Acting at 90% of the wall covers the 8% residual with something to spare, and what it costs is one
// compaction slightly early on a path that is about to be too big anyway. guardFit does the same thing
// at 98% for the window, so this is the house rule, not a new invention.
const bodyGuardMarginPct = 90

// bodyActLine is where the pre-send guard acts: the wall with the margin applied to it. Its own method
// because two questions are asked of the same number — is this payload at the line, and how much may the
// messages of a request weigh once the skeleton has been paid for — and the two must not drift.
// Zero is "no wall known", which every caller reads as "bytes have no vote".
func (s *Session) bodyActLine() int {
	ceil := s.bodyCeiling()
	if ceil <= 0 {
		return 0
	}
	return ceil * bodyGuardMarginPct / 100
}

// keptWireBudget is the wall translated into the planner's currency: what the messages of the next
// request may weigh. The envelope comes off because compact weighs messages and the ceiling weighs
// requests, and the margin comes off because that is the line the guard acts at — planning to the wall
// itself and acting at 90% of it would mean the plan is satisfied while the guard keeps compacting.
// A skeleton that alone eats the act line answers 0, which hands the decision back to the token ruler:
// the payload is going over no matter how deep the cut goes, so the honest move is to send what the
// floors demand and let the refusal be the one that reports it.
func (s *Session) keptWireBudget() int {
	line := s.bodyActLine()
	s.mu.Lock()
	env := s.bodyEnv
	s.mu.Unlock()
	if line <= env {
		return 0
	}
	return line - env
}

// bodyOverWall is the pre-send question by itself: does the payload about to be assembled (the path's
// wire estimate, plus the skeleton measured on the last send) reach the endpoint's wall? Split out
// because the answer is worth testing without making a compaction happen to prove it — and because the
// three numbers together are what a log line has to agree with. The returned ceiling is the wall itself,
// not the line the guard acts at; the margin is in the decision, where it belongs.
func (s *Session) bodyOverWall(sizes *asm.Sizes) (bool, int, int) {
	ceil := s.bodyCeiling()
	if ceil <= 0 {
		return false, 0, 0 // no wall known: the reactive path is the only instrument, and that is by design
	}
	s.mu.Lock()
	env := s.bodyEnv
	s.mu.Unlock()
	est := sizes.WireTotal() + env
	return est >= s.bodyActLine(), est, ceil
}

// guardBody is the byte twin of guardFit, and it deliberately stops short of guardFit's verdict: when
// the payload this turn is about to upload is already over the endpoint's wall, it spends one forced
// compaction now instead of the upload later. It never refuses the send, because the ruler here is the
// estimate, and the estimate is trusted with "where to cut", not with "whether it fits" — measured
// against the transport's own count the residual runs 1.5% for one image and 8% for a tool-heavy path.
// Sending anyway costs one round trip and turns the wall into a measurement, which is exactly what the
// reactive path in generate already knows how to record; refusing on an 8% error would open a whole new
// way for a session to fail at a size it could have shipped.
//
// It reports whether the path changed, so the caller re-assembles the payload the way it does for
// guardFit: a request built before the boundary describes a conversation that no longer exists.
//
// Compaction is asked for with force, as in guardFit — this is a fit problem, not a policy question, so
// compact.auto gets no vote. A session with auto compaction off is precisely the one that will otherwise
// upload 119 MiB at a 119 MiB wall every turn of its life and learn the wall over again each time.
func (s *Session) guardBody(ctx context.Context, sizes *asm.Sizes) bool {
	over, est, ceil := s.bodyOverWall(sizes)
	if !over {
		return false
	}
	s.logf("body: the next request is about %s, at or past %d%% of this endpoint's %s wall - compacting before sending",
		humanBytes(est), bodyGuardMarginPct, humanBytes(ceil))
	res := s.compactHere(ctx, compact.TriggerManual, true)
	if res.Committed {
		s.emit(Event{Type: EvtCompact, Data: res})
		return true
	}
	if res.Skipped != "" {
		s.logf("body: compaction left it at %s (%s)", humanBytes(est), res.Skipped)
	}
	return false
}

// maxBodyResends bounds what the reactive branch in turns() may do after the endpoint refused a request:
// it compacts, and then it may build that request again and send it — up to this many times per run.
// The bound is not about termination. A resend only happens on a committed compaction, and compaction
// stops committing when the region runs out, so an unbounded loop would still end; what it would not end
// cheaply is the upload. Counted in practice, not assumed: in a single turn expect ONE resend, because the
// first forced cut lands on a floor and the second refusal finds nothing outside it — measured on 400 x
// 1.1 KiB entries in a 200k window, where the summary stopped at 147 entries / 165 KiB (= keepBudget, while
// the byte line allowed ~365 entries) and the next refusal had no region left to cut. The counter earns
// its keep on the shape where the path grows back between refusals, which is a run with tool rounds. The pathological shape is real — the newest entry alone weighs more than the
// wall, so every round commits a summary of older things, the total never drops under the act line, and
// each attempt pays for another hundred megabytes. Two resends buy the normal case (one refusal to learn
// the wall, one cut to get under it) and then hand the problem back to the reader with a count in the
// sentence, which is what the byte branch owed and did not have: it used to stop and say "please send
// again" after spending a summary generation on a path it then never shipped.
const maxBodyResends = 2

// fitScope is the largest i in [lo, hi] whose estimated body still fits budget bytes. The prefix sums
// are monotone, so it is a search rather than a scan: a path of 100k entries is asked about with
// seventeen lookups, and this runs inside a compaction that is already making a network call.
//
// The estimate is asm's Wire (see sizes.go): the bytes an entry would cost on the wire, missing the
// per-message JSON scaffolding, so it runs a few percent short of the truth. That is acceptable here
// because the caller measures the built request and cuts again when the estimate was too kind — the
// estimate only says where to look, the measurement decides.
func fitScope(sizes *asm.Sizes, lo, hi, budget int) int {
	if sizes == nil || hi <= lo || budget <= 0 {
		return lo
	}
	if sizes.WirePrefix(hi) <= budget {
		return hi
	}
	i, j := lo, hi
	for i < j {
		m := (i + j + 1) / 2
		if sizes.WirePrefix(m) <= budget {
			i = m
		} else {
			j = m - 1
		}
	}
	if sizes.WirePrefix(i) <= budget {
		return i
	}
	return lo
}

// bodyLimitPhrase quotes the ceiling in the sentence that tells a person their body came back too large.
// A refusal proves the wall is at or below the size it refused, never at that size itself — and right
// here the two numbers are very often the same one, because the refusal is where the ceiling came from.
// Quoting the ceiling for that reads as a self-report: "request body (147.5 MiB) is over this endpoint's
// limit (147.5 MiB or less)", which sounds like a limit no request can satisfy and invites the
// reasonable-sounding fix of raising it by one byte. So when the wall is this refusal's own size the
// phrase quotes the EVIDENCE instead — "under the 147.5 MiB it refused" — a bound, not a number to squeeze
// inside. That also covers a configured wall sitting above the true one: a 286 MiB config that came back
// refused at 196 MiB says "under the 196.0 MiB it refused" rather than quoting the operator's too-generous
// number. When the ceiling is LOWER than what was just refused it came from somewhere else (config, or an
// earlier refusal) and is quoted as itself.
func bodyLimitPhrase(ceil, sent int) string {
	if ceil <= 0 {
		return "unknown"
	}
	if sent > 0 && ceil >= sent {
		return "under the " + humanBytes(sent) + " it refused"
	}
	return humanBytes(ceil)
}

// humanBytes writes a byte count with a binary divisor (1024) and the label that matches it, so the two
// can be compared by eye while someone is deciding what to cut — and compared against the raw byte counts
// the status API reports without a fourth-of-a-percent vanishing between them. A request reported as
// 124151703 bytes is not a number anyone weighs against a limit.
func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := int64(n) / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
