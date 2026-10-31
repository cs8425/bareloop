package agent

import (
	"fmt"
	"strings"

	"bareloop/internal/tlsconf"
)

// Every endpoint in a session config that can carry a certificate policy, with the name a message
// should use when it is wrong. One list, walked in the same order by validateTLS and logTLS, so the
// check and the log line cannot disagree about which endpoints are covered.
type tlsPair struct {
	// what names the endpoint in a message: "llm", or `mcp server "vr"`.
	what string
	// url is the endpoint the policy applies to, empty when there is none yet.
	url string
	// opts is the pair itself, in the shape internal/tlsconf takes.
	opts tlsconf.Options
	// unreachable is set for an entry with no connection to verify — a stdio MCP server — where a TLS
	// policy is not merely inert but a line in a config that does nothing.
	unreachable bool
}

func tlsPairs(cfg Config) []tlsPair {
	out := []tlsPair{{what: "llm", url: cfg.LLM.BaseURL, opts: cfg.LLM.TLS()}}
	for _, mc := range cfg.MCP {
		out = append(out, tlsPair{
			what:        fmt.Sprintf("mcp server %q", mc.Name),
			url:         mc.URL,
			opts:        tlsconf.Options{InsecureTLS: mc.InsecureTLS, CAFile: mc.CAFile},
			unreachable: !mc.IsHTTP(),
		})
	}
	return out
}

// validateTLS refuses a certificate policy that cannot mean anything, before any of it reaches
// data/<id>/config.json.
//
// Two things are refused: a pair that contradicts itself (insecureTLS together with caFile), and a
// policy on a stdio MCP entry, which has no certificate to check either way — accepting it would leave
// someone reading a config that claims a connection is trusted for a reason it is not.
//
// It is also what puts a caFile path on disk at save time: tlsconf reads the file, so a wrong path is
// reported to the person who typed it rather than surfacing as a failed request an hour later.
func validateTLS(cfg Config) error {
	for _, p := range tlsPairs(cfg) {
		if p.opts.Active() && p.unreachable {
			return fmt.Errorf("%s: insecureTLS and caFile are about a certificate, and this entry is a stdio server with no connection to verify", p.what)
		}
		if err := p.opts.Validate(p.what); err != nil {
			return err
		}
	}
	return nil
}

// logTLS says out loud the certificate policy that is not the default, once when a session opens and
// once when its endpoints change. It exists because the effect of these two fields is invisible from
// the session: a request being impersonated looks exactly like a request that is not.
//
// The same reasoning put the compaction and structuredContent lines in the log (D5) — a daemon that
// only reports what went wrong cannot report a deliberate deviation, which is the thing an operator
// needs to find in someone else's log file.
func (s *Session) logTLS(cfg Config) {
	for _, p := range tlsPairs(cfg) {
		if !p.opts.Active() || p.unreachable || p.url == "" {
			// An entry with no url is not connected yet, and says so somewhere other than here; a
			// stdio entry was already refused a policy by validateTLS.
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
