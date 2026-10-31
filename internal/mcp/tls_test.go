package mcp

// What these TLS fields are worth is decided in two places, and both are pinned here: Identity(), which
// decides whether a saved change reconnects, and the http.Client the url transport dials with. What a
// policy does to a handshake — including that a caFile is never traded for the system trust store — is
// proven once with a real chain in internal/tlsconf, so this file only checks that the client and the
// reconnect logic are built from the fields at all.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func yesNo(b bool) *bool { return &b }

// TestIdentitySeparatesTheCertificatePolicy: Identity decides whether a session's MCP connection is torn
// down and rebuilt. A policy is the one config field that can change what the server accepts without
// changing a single byte of the url — so it has to be in here, and the one case that asks for nothing
// (an explicit false, which a UI sends without being asked) must stay out.
func TestIdentitySeparatesTheCertificatePolicy(t *testing.T) {
	base := Config{Type: "http", URL: "https://x.invalid/mcp"}

	if base.Identity() != (Config{Type: "http", URL: "https://x.invalid/mcp", InsecureTLS: yesNo(false)}).Identity() {
		t.Fatal("an explicit false asks for the default, so reconnecting for it reconnects nothing")
	}
	if base.Identity() == (Config{Type: "http", URL: "https://x.invalid/mcp", InsecureTLS: yesNo(true)}).Identity() {
		t.Fatal("a connection that was refused an unverifiable certificate has to be retried once the operator says otherwise")
	}
	ca := Config{Type: "http", URL: "https://x.invalid/mcp", CAFile: "/x/ca.pem"}
	if base.Identity() == ca.Identity() {
		t.Fatal("a different trust anchor is a different connection")
	}
	if ca.Identity() == (Config{Type: "http", URL: "https://x.invalid/mcp", CAFile: "/y/ca.pem"}).Identity() {
		t.Fatal("changing the file has to reconnect too")
	}
}

// TestIsHTTPIsTheOneList: agent/validateTLS asks this same question to decide whether a policy can mean
// anything, so the accepted spellings live in exactly one place.
func TestIsHTTPIsTheOneList(t *testing.T) {
	for _, ty := range []string{"http", "HTTP", " streamable-http ", "streamable_http", "sse"} {
		if !(Config{Type: ty}).IsHTTP() {
			t.Errorf("IsHTTP(%q) = false, so a TLS policy on such an entry would be refused as stdio", ty)
		}
	}
	for _, ty := range []string{"", "stdio", "websocket"} {
		if (Config{Type: ty}).IsHTTP() {
			t.Errorf("IsHTTP(%q) = true", ty)
		}
	}
	// The default branch of Connect has to stay an error: a typo in "type" must not start a process
	// nobody asked for, and must not be mistaken for a url either.
	if _, err := Connect(context.Background(), Config{Name: "typo", Type: "carrier-pigeon"}); err == nil ||
		!strings.Contains(err.Error(), "unknown transport type") {
		t.Fatalf("Connect with an unknown type: %v", err)
	}
}

// TestHTTPClientFollowsThePolicy covers the three states the transport can be built in, including the one
// a test relies on: an injected client is the connection, policy fields and all.
func TestHTTPClientFollowsThePolicy(t *testing.T) {
	// No policy, no client: nil is how the transport is told to keep http.DefaultTransport.
	hc, err := Config{Name: "a", Type: "http", URL: "https://x.invalid/mcp"}.httpClient()
	if err != nil || hc != nil {
		t.Fatalf("no policy: (%v, %v), want (nil, nil)", hc, err)
	}
	hc, err = (Config{Name: "a", Type: "http", URL: "https://x.invalid/mcp", InsecureTLS: yesNo(false)}).httpClient()
	if err != nil || hc != nil {
		t.Fatalf("an explicit false is not a policy: (%v, %v), want (nil, nil)", hc, err)
	}

	// Injected: it wins, and the policy fields are not applied on top of it — what an injected client
	// does about certificates is that caller's decision, which is why this branch is checked first.
	mine := &http.Client{Timeout: 42 * time.Second}
	hc, err = Config{Name: "a", Type: "http", URL: "https://x.invalid/mcp", InsecureTLS: yesNo(true), HTTP: mine}.httpClient()
	if err != nil {
		t.Fatal(err)
	}
	if hc != mine {
		t.Fatalf("got client %p, want the injected %p", hc, mine)
	}

	// A policy that needs a transport gets one that says what it was asked for.
	hc, err = Config{Name: "a", Type: "http", URL: "https://x.invalid/mcp", InsecureTLS: yesNo(true)}.httpClient()
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want the cloned http.DefaultTransport", hc.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify was not set")
	}
	if tr.Proxy == nil {
		// Clone() of DefaultTransport is what keeps the proxy from the environment; a hand-written
		// Transport would have silently dropped it for every endpoint with a caFile.
		t.Fatal("the policy transport is no longer a clone of http.DefaultTransport")
	}

	// A caFile that cannot be read is a failed connection, not a connection with a missing trust anchor.
	missing := filepath.Join(t.TempDir(), "nope.pem")
	hc, err = Config{Name: "vr", Type: "http", URL: "https://x.invalid/mcp", CAFile: missing}.httpClient()
	if err == nil {
		t.Fatalf("accepted an unreadable caFile (client %v)", hc)
	}
	if hc != nil {
		t.Error("a refused policy must not hand back a client")
	}
	if !strings.Contains(err.Error(), `mcp server "vr"`) || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error %q does not name the server or the reason", err)
	}
}

// TestConnectUsesThePolicy is the end-to-end claim: the client built from the policy is the one that
// reaches the wire. The server is an https one whose certificate nothing in a trust store knows, and it
// answers junk, so the two outcomes are distinguishable by error alone — a refused certificate before
// the request, a protocol complaint after it.
func TestConnectUsesThePolicy(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "not jsonrpc", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name    string
		cfg     Config
		wantTLS bool
	}{
		{"no policy refuses the certificate", Config{}, true},
		{"insecureTLS gets past it", Config{InsecureTLS: yesNo(true)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			cfg := tc.cfg
			cfg.Name, cfg.Type, cfg.URL = "s", "http", srv.URL
			cfg.Startup = 5 * time.Second
			cfg.Logf = func(string, ...any) {}
			_, err := Connect(context.Background(), cfg)
			if err == nil {
				t.Fatal("the junk server negotiated a protocol, which makes the assertion below meaningless")
			}
			gotTLS := strings.Contains(err.Error(), "certificate")
			if gotTLS != tc.wantTLS {
				t.Fatalf("error %q is a certificate error = %t, want %t", err, gotTLS, tc.wantTLS)
			}
			if !tc.wantTLS && hits.Load() == 0 {
				t.Error("nothing reached the server, so the connection was not made by the policy either")
			}
		})
	}

	// A caFile nobody can read fails before any request is attempted.
	missing := filepath.Join(t.TempDir(), "nope.pem")
	if _, err := Connect(context.Background(), Config{
		Name: "s", Type: "http", URL: srv.URL, CAFile: missing, Startup: 5 * time.Second,
	}); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("an unreadable caFile must fail the connect: %v", err)
	}
}

// TestPolicyClientHasNoTimeout is why the client is built without one: an http.Client timeout bounds every
// request it makes, including the GET that carries a server-sent-events stream — a connection meant to stay
// open until the server stops talking. Config.Timeout bounds a single round trip inside the transport.
func TestPolicyClientHasNoTimeout(t *testing.T) {
	hc, err := Config{Name: "a", Type: "http", URL: "https://x.invalid/mcp", InsecureTLS: yesNo(true)}.httpClient()
	if err != nil {
		t.Fatal(err)
	}
	if hc.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %v, which would cut an SSE stream short", hc.Timeout)
	}
	if _, ok := hc.Transport.(*http.Transport); !ok {
		t.Fatal("the policy transport is not a clone of http.DefaultTransport, so proxy and keep-alive are gone")
	}
}
