package agent

// n_ctx resolution (the rule the operator set, and the reason a stale config was silent for days):
//
//	config not set            -> use what the server reports
//	config <  server         -> use config (the operator is deliberately spending less of the window)
//	config >  server         -> ignore config, use the server: sending more than the server has is a
//	                           guaranteed refusal, so a too-large config cannot be honoured
//	server reports nothing    -> use config
//	neither                   -> 0, and the UI asks the user to fill it in
//
// Which is one expression: min over the values that exist. The resolved number is runtime state
// only: it is never written back to config.json, so swapping in a server with a bigger or smaller
// window needs no config edit, and a config file copied to another machine keeps meaning "my
// ceiling", not "the window I last happened to measure".

import (
	"context"
	"strings"
	"time"
)

// probeTimeout caps one probe. The provider walks a few metadata endpoints ("/props", "/model",
// "/models") with its own short timeouts; this is the outer bound so a frozen server cannot add
// seconds to the first message of a session.
const probeTimeout = 6 * time.Second

// resolveNCTX is the rule above, as a function of two numbers.
func resolveNCTX(cfgNCTX, serverNCTX int) int {
	switch {
	case serverNCTX <= 0:
		return cfgNCTX // the server has no opinion (or has not been asked yet)
	case cfgNCTX <= 0:
		return serverNCTX // config is unset: the server decides
	case cfgNCTX < serverNCTX:
		return cfgNCTX // deliberate under-spend
	default:
		return serverNCTX // config cannot be honoured
	}
}

// nctxSource labels the decision for the log line. "unset" is the state that must be reported to
// the user: with no window there is no trigger level, no budget and no fit check.
func nctxSource(cfgNCTX, serverNCTX int) string {
	switch {
	case serverNCTX <= 0 && cfgNCTX <= 0:
		return "unset"
	case serverNCTX <= 0:
		return "config"
	case cfgNCTX <= 0:
		return "server"
	case cfgNCTX == serverNCTX:
		return "config (the server reports the same window)"
	case cfgNCTX < serverNCTX:
		return "config (below the server window)"
	default:
		return "server (config asked for more than the server has)"
	}
}

// isContextOverflow recognises the family of provider answers that mean "that request is larger
// than the window". llama.cpp is explicit (code exceed_context_size_error, and it even reports the
// real n_ctx); OpenAI-compatible servers use context_length_exceeded. A wrong guess here only
// costs the extra probe, so the patterns are deliberately generous.
func isContextOverflow(text string) bool {
	if text == "" {
		return false
	}
	t := strings.ToLower(text)
	for _, m := range []string{
		"exceed_context_size_error",
		"exceeds the available context size",
		"context_length_exceeded",
		"context length exceeded",
		"maximum context length",
		"exceeds the model",
		"context size exceeded",
		"please reduce your prompt",
	} {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// probeNCTX asks the server what its window is and re-derives the effective value.
//
// It is called at exactly two moments: once before a session sends anything, and once when the
// provider refused a request for being too large (which may mean the window got smaller while the
// session was idle). It never writes config, and it never holds the session lock while asking.
func (s *Session) probeNCTX(why string) {
	s.mu.Lock()
	if s.nctxProbing {
		s.mu.Unlock()
		return
	}
	s.nctxProbing = true
	cfgN := s.nctxCfg
	before := s.nctx
	prov := s.provider
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	server := prov.NCTX(ctx)
	cancel()

	s.mu.Lock()
	s.nctxProbing = false
	s.nctxServer = server
	s.nctxProbed = true
	s.nctx = resolveNCTX(cfgN, server)
	after := s.nctx
	s.mu.Unlock()

	switch {
	case after == 0:
		s.logf("n_ctx: UNKNOWN (config unset, server %d, %s) - compaction and the fit check stay off; set llm.nCtx in Settings", server, why)
	case after == before:
		s.logf("n_ctx: %d from %s (config %d, server %d, %s)", after, nctxSource(cfgN, server), cfgN, server, why)
	default:
		s.logf("n_ctx: %d -> %d from %s (config %d, server %d, %s)", before, after, nctxSource(cfgN, server), cfgN, server, why)
	}
	s.emitState()
}

// needsProbe reports whether this process has never asked this session's server for its window.
// Probing once per session (plus the overflow retry) is the whole policy: a session that never
// sends pays no request, and a running session does not poll its own server.
func (s *Session) needsProbe() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.nctxProbed
}
