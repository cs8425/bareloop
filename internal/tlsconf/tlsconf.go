// Package tlsconf turns the fields an endpoint config carries about how it is reached — "insecureTLS",
// "caFile" and "proxy" — into an http transport, and refuses the combinations that cannot mean anything.
//
// It is its own package because the two things that reach the network (internal/llm and internal/mcp)
// both need it and neither may import the other.
//
// One rule runs through the whole package: asking for less verification is allowed, quietly getting
// less verification is not. A caFile that cannot be read is an error rather than "use the system trust
// store then", because the endpoint that needs a caFile is precisely one the system store cannot verify
// — falling back would turn a wrong path into a session that appears to work over a connection nobody
// checks.
//
// The same rule shapes the proxy field, and for the same reason: a proxy string that cannot be parsed is
// a refused config rather than a connection made directly. Traffic that was meant to leave through a host
// somebody chose is not allowed to leave some other way just because the address was typed wrong — which
// is also how net/http reasons about its own environment handling, in transport.go's overrideProxy:
// ignoring a value it could not parse "would risk accidentally connecting directly without using the
// proxy".
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Options is the set of config fields one endpoint carries: the pair about its certificate, and how it is
// reached. A nil *bool is not the same input as false — it means nobody configured this endpoint — but it
// is the same behaviour, which is why Insecure reads the pointer and a transport is only built when one of
// the fields is actually asked for.
type Options struct {
	InsecureTLS *bool
	CAFile      string
	// Proxy is where traffic to this endpoint is sent: socks5://, socks5h://, http:// or https://, with
	// user:password when the proxy asks for one. The literal "direct" is the other answer — connect to the
	// endpoint itself, ignoring HTTP_PROXY and friends — and is spelled rather than left empty because an
	// empty string is already spent on "this endpoint says nothing", the same wall caFile ran into: a field
	// that cannot say "no" cannot be taken back to no. Empty here means the environment is still in charge,
	// which is what every deployment that never set this field expects.
	Proxy string
}

// Insecure reports whether this endpoint asks for certificates not to be checked. Absent and false are
// the same answer here, so a PATCH that only ever sends false never looks like a policy change.
func (o Options) Insecure() bool { return o.InsecureTLS != nil && *o.InsecureTLS }

// Certs reports whether anything was asked about how this endpoint's certificate is checked. It is the
// certificate half of Active, and the half that means nothing at all for an endpoint reached over a plain
// http:// url or started as a child process.
func (o Options) Certs() bool { return o.Insecure() || o.CAFile != "" }

// Active reports whether there is a policy at all. When it is false the caller keeps the default
// transport: "verify normally, against the system store, through whatever the environment says" is what
// happens when no transport is built, so building one here would only be another chance to get it wrong.
func (o Options) Active() bool { return o.Certs() || o.Proxy != "" }

// Direct reports whether this policy asks for the endpoint to be reached itself, with no proxy and no
// environment variables consulted. It is a method because the spelling of that answer is this package's
// business: a caller that prints or branches on it should not have to know what the word is.
func (o Options) Direct() bool { return strings.TrimSpace(o.Proxy) == proxyDirect }

// SafeProxy is this policy's proxy as it may be written into a log line: "" when nothing was said, the word
// "direct" when that is the answer, and otherwise the address with any password taken out. Naming the proxy
// an endpoint is being reached through is what makes the line worth having; a secret somebody put in a config
// file is not, and taking it out is what keeps the address readable enough to be recognised.
func (o Options) SafeProxy() string {
	p := strings.TrimSpace(o.Proxy)
	if p == "" || p == proxyDirect {
		return p
	}
	u, err := proxyURL(p)
	if err != nil {
		// Validate refuses this address, so arriving here means a caller that skipped validating; saying the
		// address could not be read is more use in a log than saying nothing.
		return "<proxy address that cannot be read>"
	}
	return u.Redacted()
}

// Validate refuses what cannot work. what names the endpoint in the message ("llm", `mcp server "vr"`)
// because the same fields appear under several endpoints in one config file, and "caFile is not readable"
// on its own does not say which endpoint to go and fix.
func (o Options) Validate(what string) error {
	if o.Insecure() && o.CAFile != "" {
		return fmt.Errorf(`%s: insecureTLS and caFile are both set (%q): one says "do not check the certificate", the other says which one to check — pick one`, what, o.CAFile)
	}
	if o.CAFile != "" {
		if _, err := certPool(o.CAFile); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	// The proxy is checked here rather than where it is used so that a wrong scheme or a missing host is
	// reported to whoever typed it, next to the endpoint they typed it under, instead of arriving as a
	// failed model request later. It is not checked for reachability: a proxy that is down is an outage
	// this program cannot fix and should name as one, while a proxy that is misconfigured is a typo in a
	// file, and only the second one is worth refusing to save.
	if p := strings.TrimSpace(o.Proxy); p != "" && p != proxyDirect {
		if _, err := proxyURL(p); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return nil
}

// Transport returns the transport this policy asks for, or nil when there is no policy. nil is what a
// caller puts in http.Client.Transport, where nil already means http.DefaultTransport.
//
// Cloning DefaultTransport rather than writing a Transport by hand is the point: a hand-written
// Transport resets every other default in it, and the proxy from the environment is the one a test
// checks — internal/mcp asserts the policy transport still has a non-nil Proxy, which a literal
// Transport does not. Only what the policy names is replaced, TLSClientConfig for the certificate pair and
// Proxy for a proxy, so a policy about one of them cannot quietly change the other: an endpoint that names
// a caFile still dials through the environment's proxy, and one that names a proxy still verifies the
// certificate the way it did before. What is still verified after a certificate policy differs by field,
// which is worth knowing precisely: a caFile changes the trust anchors and nothing else, so the hostname is
// still checked, while InsecureSkipVerify skips the name along with the chain — in crypto/tls those are one
// decision, not two, which is why the session log says the endpoint can be impersonated rather than that
// its certificate was ignored.
//
// The assertion on DefaultTransport is deliberately left unchecked: a process that had replaced it with
// something else has already changed what "dial this the normal way" means, and carrying on by dropping
// the policy would be the quieter of the two failures.
func (o Options) Transport(what string) (http.RoundTripper, error) {
	if err := o.Validate(what); err != nil {
		return nil, err
	}
	if !o.Active() {
		return nil, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone() // deliberately not comma-ok: see the note above
	if o.Certs() {
		cfg := &tls.Config{InsecureSkipVerify: o.Insecure()}
		if o.CAFile != "" {
			pool, err := certPool(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
			// Setting RootCAs is what makes the system trust store not consulted at all. An operator who
			// named a file made a trust decision, and adding their CA to the default set would be a
			// different decision — one they did not make — so a server the file cannot verify fails.
			cfg.RootCAs = pool
		}
		tr.TLSClientConfig = cfg
	}
	if p := strings.TrimSpace(o.Proxy); p != "" {
		if p == proxyDirect {
			// A nil Proxy means "no proxy" to net/http, and on a clone it is the only way to say it: the
			// clone brought ProxyFromEnvironment with it, so leaving that in place would answer "direct"
			// with the environment's answer. NO_PROXY goes with it, which is the point — the endpoint said
			// where its traffic goes, and that is the question NO_PROXY exists to answer.
			tr.Proxy = nil
		} else {
			u, err := proxyURL(p)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
			// http.ProxyURL is the constant function — every request to this endpoint goes to this proxy —
			// replacing ProxyFromEnvironment rather than layering on top of it. An endpoint that names a
			// proxy is not also at the mercy of the machine it happens to run on; an operator who wants the
			// environment's answer leaves the field empty.
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return tr, nil
}

// proxyDirect is the one proxy value that is not a URL: it says connect to the endpoint itself, with no
// proxy and no environment variables consulted. It is a word rather than a URL scheme because there is no
// host to name, and it is spelled rather than empty because empty already means "nothing was said".
const proxyDirect = "direct"

// proxyURL parses the proxy address this endpoint was given. Only the four schemes the config promises are
// accepted, and a value outside them is an error: net/http would take anything it can parse, and some of it
// means something else — a socks4:// address would be dialled as if it spoke HTTP CONNECT, and a typo that
// lost the scheme would turn into a hostname — so refusing is the only answer that cannot become a
// connection nobody meant to make.
//
// Both socks5 spellings are passed through as written. Where a hostname is resolved for a SOCKS proxy is the
// standard library's decision rather than this package's, and rewriting one spelling into the other would be
// this program changing an address somebody typed.
func proxyURL(s string) (*url.URL, error) {
	if !strings.Contains(s, "://") {
		return nil, fmt.Errorf("proxy %q: not a proxy address — write one of socks5://, socks5h://, http://, https:// followed by host:port, or %q to connect directly", s, proxyDirect)
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", s, err)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return nil, fmt.Errorf("proxy %q: scheme %q is not supported, use socks5://, socks5h://, http:// or https://", s, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q: no host to connect to", s)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("proxy %q: a proxy address is a host and port, and %q is neither", s, u.Path)
	}
	if u.RawQuery != "" {
		return nil, fmt.Errorf("proxy %q: a proxy address takes no query string", s)
	}
	return u, nil
}

// certPool reads one PEM bundle as a set of trust anchors. It fails when the file is missing, holds no
// certificate, or holds something that merely parses as PEM: a private key reads fine as text and would
// otherwise produce an anchor set of zero entries, which looks like a server that rejects every client.
func certPool(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("caFile: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("caFile %q holds no certificate: it has to be the PEM block(s) starting -----BEGIN CERTIFICATE-----", path)
	}
	return pool, nil
}
