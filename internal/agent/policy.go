package agent

import (
	"errors"
	"fmt"
	"strings"

	"bareloop/internal/mcp"
	"bareloop/internal/tlsconf"
)

// Every endpoint in a session config that can carry a transport policy — how its certificate is checked and
// where its traffic is sent — with the name a message should use when it is wrong. One list, walked in the
// same order by validatePolicy and logPolicy, so the check and the log line cannot disagree about which
// endpoints are covered.
//
// Both walkers read the resolved policy (llmDial, mcpDial), which is what makes a session-level proxy a thing
// this file can refuse, report, and hand to the right endpoint: an operator who sets one and sees nothing for
// it has somewhere to look, and a stdio entry — which resolves to nothing, having no connection to route —
// never inherits a promise it cannot keep.
type endpointPolicy struct {
	// what names the endpoint in a message: "llm", or `mcp server "vr"`.
	what string
	// url is the endpoint the policy applies to, empty when there is none yet.
	url string
	// opts is the policy itself, in the shape internal/tlsconf takes.
	opts tlsconf.Options
	// unreachable is set for an entry with no connection at all — a stdio MCP server started as a child
	// process — where a certificate policy is not merely inert but a line in a config that does nothing, and
	// a proxy is worse: a promise about a route no traffic will ever take.
	unreachable bool
}

func endpointPolicies(cfg Config) []endpointPolicy {
	out := []endpointPolicy{{what: "llm", url: cfg.LLM.BaseURL, opts: cfg.llmDial().Policy()}}
	for _, mc := range cfg.mcpDial() {
		out = append(out, endpointPolicy{
			what:        fmt.Sprintf("mcp server %q", mc.Name),
			url:         mc.URL,
			opts:        mc.Policy(),
			unreachable: !mc.IsHTTP(),
		})
	}
	return out
}

// validatePolicy refuses a transport policy that cannot mean anything, before any of it reaches
// data/<id>/config.json.
//
// Two things are refused: a policy that contradicts itself or cannot be parsed (insecureTLS together with
// caFile, an unreadable caFile, a proxy address that is not one of the four schemes), and a policy on a
// stdio MCP entry, which has no connection to verify or tunnel either way — accepting it would leave someone
// reading a config that claims a connection is protected for a reason it is not.
//
// It is also what puts a caFile path on disk at save time: tlsconf reads the file, so a wrong path is
// reported to the person who typed it rather than surfacing as a failed request an hour later. The proxy is
// parsed and not dialled: a proxy that is unreachable is an outage to report when it is needed, while a proxy
// whose address was typed wrong is a bug in a file, and only the second one is worth refusing to save.
func validatePolicy(cfg Config) error {
	for _, p := range endpointPolicies(cfg) {
		if p.unreachable {
			// The two halves are reported apart because they are two different mistakes: a proxy on a stdio
			// entry usually means somebody read the session-level field as "where this daemon's traffic goes"
			// and put it on the entry, which is a question about the other endpoint in the file.
			if p.opts.Proxy != "" {
				return fmt.Errorf(`%s: proxy is about how a connection is reached, and this entry is a stdio server started as a child process — there is no connection to route. Set proxy at the session level for the endpoints that do connect, or "direct" to stop the environment deciding`, p.what)
			}
			if p.opts.Certs() {
				return fmt.Errorf("%s: insecureTLS and caFile are about a certificate, and this entry is a stdio server with no connection to verify", p.what)
			}
			continue
		}
		if err := p.opts.Validate(p.what); err != nil {
			return err
		}
	}
	return nil
}

// logPolicy says out loud the transport policy that is not the default, once when a session opens and once
// when its endpoints change. It exists because the effect of these fields is invisible from the session: a
// request being impersonated, or leaving by a proxy nobody meant it to, looks exactly like a request that is
// not.
//
// The same reasoning put the compaction and structuredContent lines in the log (D5) — a daemon that only
// reports what went wrong cannot report a deliberate deviation, which is the thing an operator needs to find
// in someone else's log file.
func (s *Session) logPolicy(cfg Config) {
	for _, p := range endpointPolicies(cfg) {
		if p.unreachable || p.url == "" {
			// An entry with no url is not connected yet, and says so somewhere other than here; a stdio
			// entry was already refused a policy by validatePolicy.
			continue
		}
		// The proxy first, and from SafeProxy rather than the field: an address that authenticates to a proxy
		// is still worth naming in a log, while the password in it is not.
		if safe := p.opts.SafeProxy(); safe != "" {
			if p.opts.Direct() {
				s.logf("%s: %s is connected directly — HTTP_PROXY and NO_PROXY are not consulted for it", p.what, p.url)
			} else {
				s.logf("%s: %s is reached through %s", p.what, p.url, safe)
			}
		}
		if !p.opts.Certs() {
			continue
		}
		secure := strings.HasPrefix(p.url, "https://")
		switch {
		case p.opts.Insecure() && secure:
			s.logf("%s: insecureTLS is on for %s — any host between here and there can impersonate it and nothing here will say so", p.what, p.url)
		case p.opts.Insecure():
			s.logf("%s: insecureTLS is set for %s, which is not https — there is no certificate to skip", p.what, p.url)
		case secure:
			s.logf("%s: %s is verified against %s only; the system trust store is not consulted", p.what, p.url, p.opts.CAFile)
		default:
			s.logf("%s: caFile is set for %s, which is not https — it is not used", p.what, p.url)
		}
	}
}

// ErrMCPLocked is the daemon saying that the MCP servers are its decision and not a session's
// (the config.json field mcpLock). Every route that could write a session's server list —
// PATCH, create, import — can answer it, and none of the everyday things trip it: toggling a
// pre-registered server, its tool switches, its prefix. The message is written to be read by
// whoever got the 403, telling them both what is still theirs to do and where the door that
// opens this one is.
var ErrMCPLocked = errors.New("agent: the MCP servers are locked by the daemon config (mcpLock): enabling, disabling and tool policy stay yours, the server list is not — to change the servers themselves, stop the daemon and edit its config.json")

// sameMCPLocked compares two MCP lists the way the lock cares about them: itemwise equality
// of everything that decides the server, ignoring only the enabled bit — the one field the
// lock exists to leave free. Position matters as much as content, because the list is what
// the tool registry walks; a reordering is a rewrite of it, and a rewrite is what is locked.
// The tool-policy fields (disabledTools, toolPrefix, structuredContent) never entered
// Identity in the first place, so a body re-saving them unchanged — or changed, which the
// lock does not forbid — reads as the same list here and passes.
func sameMCPLocked(old, next []mcp.Config) bool {
	if len(old) != len(next) {
		return false
	}
	for i := range old {
		if mcpLockedKey(old[i]) != mcpLockedKey(next[i]) {
			return false
		}
	}
	return true
}

// mcpLockedKey is a server's Identity with the enabled bit pressed flat: Identity is already
// "everything about this entry that would mean reconnecting", which is exactly the surface
// the lock freezes, and borrowing it keeps the two from ever disagreeing about what the entry
// is. The value receiver makes clearing Disabled free.
func mcpLockedKey(c mcp.Config) string {
	c.Disabled = false
	return c.Identity()
}
