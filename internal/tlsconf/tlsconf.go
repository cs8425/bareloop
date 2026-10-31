// Package tlsconf turns the two TLS fields an endpoint config carries — "insecureTLS" and "caFile" —
// into an http transport, and refuses the combinations that cannot mean anything.
//
// It is its own package because the two things that reach the network (internal/llm and internal/mcp)
// both need it and neither may import the other.
//
// One rule runs through the whole package: asking for less verification is allowed, quietly getting
// less verification is not. A caFile that cannot be read is an error rather than "use the system trust
// store then", because the endpoint that needs a caFile is precisely one the system store cannot verify
// — falling back would turn a wrong path into a session that appears to work over a connection nobody
// checks.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// Options is the pair of config fields. A nil *bool is not the same input as false — it means nobody
// configured this endpoint — but it is the same behaviour, which is why Insecure reads the pointer and
// a transport is only built when one of the two is actually asked for.
type Options struct {
	InsecureTLS *bool
	CAFile      string
}

// Insecure reports whether this endpoint asks for certificates not to be checked. Absent and false are
// the same answer here, so a PATCH that only ever sends false never looks like a policy change.
func (o Options) Insecure() bool { return o.InsecureTLS != nil && *o.InsecureTLS }

// Active reports whether there is a policy at all. When it is false the caller keeps the default
// transport: "verify normally, against the system store" is what happens when no transport is built,
// so building one here would only be another chance to get it wrong.
func (o Options) Active() bool { return o.Insecure() || o.CAFile != "" }

// Validate refuses what cannot work. what names the endpoint in the message ("llm", `mcp server "vr"`)
// because the same two fields appear under several endpoints in one config file, and "caFile is not
// readable" on its own does not say which endpoint to go and fix.
func (o Options) Validate(what string) error {
	if o.Insecure() && o.CAFile != "" {
		return fmt.Errorf(`%s: insecureTLS and caFile are both set (%q): one says "do not check the certificate", the other says which one to check — pick one`, what, o.CAFile)
	}
	if o.CAFile != "" {
		if _, err := certPool(o.CAFile); err != nil {
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
// Transport does not. Only TLSClientConfig is replaced, and what is still verified after that differs by
// field, which is worth knowing precisely: a caFile changes the trust anchors and nothing else, so the
// hostname is still checked, while InsecureSkipVerify skips the name along with the chain — in
// crypto/tls those are one decision, not two, which is why the session log says the endpoint can be
// impersonated rather than that its certificate was ignored.
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
	tr := http.DefaultTransport.(*http.Transport).Clone() // deliberately not comma-ok: see the note above
	tr.TLSClientConfig = cfg
	return tr, nil
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
