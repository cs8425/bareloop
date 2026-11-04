package agent

import (
	"strings"

	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// A proxy can be configured in two places — once for the session, once for an endpoint — and this file is
// the only place that reads the two together. One function rather than a rule each at the two call sites,
// because "which answer wins" asked twice can drift, and drift here means traffic silently going a way
// somebody configured against.
//
// proxyOf is that rule: the endpoint's own answer, else the session's, else empty.
//
// Empty is not "connect directly" — that is spelled "direct". Empty means nobody said anything, which is
// what leaves HTTP_PROXY and friends in charge, exactly as it was before either field existed. The three
// cases a config can be in:
//
//	session says, endpoint says nothing   → the session's proxy
//	session says nothing, endpoint says  → the endpoint's proxy
//	both say                             → the endpoint's proxy
//
// The third case is the one an operator is most likely to test, and the least likely to want argued with:
// the endpoint is the nearer answer, the one written next to the URL it applies to. What is deliberately not
// done is running one proxy through the other — there are two levels here and no order between them has been
// decided, so chaining is out rather than half-in. A future version that wants it has one function to
// change.
//
// This resolves on a copy at the point of dialling (llmDial, mcpDial) instead of filling the value in when
// the config is loaded, and that choice is the same one the pricing block forced (r33 §2.13) and the API
// key forced (r34 §2.14): the resolved config is also what GET /config would show, and once an answer the
// daemon supplied appears in a field, a settings page that PATCHes the whole config it cached writes it
// back as though the user had typed it. Stored config keeps meaning "what was configured"; only the socket
// knows what was inherited.
func proxyOf(session, endpoint string) string {
	if s := strings.TrimSpace(endpoint); s != "" {
		return s
	}
	return strings.TrimSpace(session)
}

// llmDial is this session's llm endpoint as the dialer should see it: certificate policy untouched, proxy
// resolved against the session's own answer. A copy, so the stored config keeps its own words.
func (c Config) llmDial() llm.Config {
	out := c.LLM
	out.Proxy = proxyOf(c.Proxy, c.LLM.Proxy)
	return out
}

// mcpDial is this session's MCP entries as the dialer should see them, proxy resolved the same way.
//
// Only an entry reached over a url is resolved. A stdio entry is started as a child process, so the session's
// proxy is not its answer to anything, and writing it in would do two things at once: the entry's Identity
// would change every time the session's proxy is patched, taking a working child process down for a change
// that cannot reach it, and validatePolicy would refuse a config over a proxy its owner wrote somewhere else.
// A proxy written on the stdio entry itself is a different matter — that one is kept, and refused.
//
// The slice is built rather than walked in place because cfg.MCP is a shared backing array: assigning into
// c.MCP[i].Proxy would write the inherited value into the stored config, which is the one thing this file
// exists to prevent.
func (c Config) mcpDial() []mcp.Config {
	if len(c.MCP) == 0 {
		return nil
	}
	out := make([]mcp.Config, len(c.MCP))
	for i, mc := range c.MCP {
		if mc.IsHTTP() {
			mc.Proxy = proxyOf(c.Proxy, mc.Proxy)
		}
		out[i] = mc
	}
	return out
}
