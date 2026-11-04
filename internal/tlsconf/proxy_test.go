package tlsconf

// What the proxy field decides at the transport level: which address a request is pointed at, what an
// unusable address costs, and what is left untouched when nothing was said. What a handshake does once it is
// through a proxy is net/http's behaviour and is not re-tested here; every line below is about a decision
// this package makes.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestProxyValidation(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"nothing said", Options{}, ""},
		{"socks5", Options{Proxy: "socks5://127.0.0.1:1080"}, ""},
		{"socks5h", Options{Proxy: "socks5h://proxy.internal:1080"}, ""},
		{"http with a credential", Options{Proxy: "http://user:pw@proxy.internal:3128"}, ""},
		{"https", Options{Proxy: "https://proxy.internal:443"}, ""},
		{"direct is a word, not a URL", Options{Proxy: "direct"}, ""},
		{"alongside asking not to verify", Options{InsecureTLS: &yes, Proxy: "socks5://p:1080"}, ""},
		{"a host with no scheme is not an address", Options{Proxy: "127.0.0.1:1080"}, "not a proxy address"},
		{"a scheme that is not offered", Options{Proxy: "socks4://proxy:1080"}, "not supported"},
		{"no host", Options{Proxy: "socks5://"}, "no host"},
		{"a path is not part of a proxy", Options{Proxy: "http://proxy:3128/please"}, "host and port"},
		{"a query is not part of a proxy", Options{Proxy: "http://proxy:3128?up=1"}, "query string"},
		{"the endpoint is named in the message", Options{Proxy: "ftp://proxy"}, "llm"},
		// The one contradiction in this package is still only the certificate pair: wanting a proxy and a
		// private CA is a normal thing to want, and refusing it would push both into one line of a config.
		{"a proxy and a caFile are not a contradiction", Options{CAFile: "no-such.pem", Proxy: "socks5://p:1080"}, "no such file"},
	} {
		err := tc.opts.Validate("llm")
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: accepted, want an error containing %q", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q does not contain %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestProxyTransport is about the three answers a proxy field can give the transport, and about what a
// policy that says nothing about one half must not touch in the other.
func TestProxyTransport(t *testing.T) {
	yes := true

	// proxyFor asks the transport the question net/http asks it: for this request, where do I connect? A nil
	// func and a func returning a nil URL both mean "directly", and they are worth telling apart in a
	// failure message because only the first is what "direct" asks for.
	proxyFor := func(t *testing.T, tr http.RoundTripper, to string) string {
		t.Helper()
		rt, ok := tr.(*http.Transport)
		if !ok {
			t.Fatalf("transport is %T, want the *http.Transport this package clones", tr)
		}
		if rt.Proxy == nil {
			return "<no Proxy func>"
		}
		u, err := url.Parse(to)
		if err != nil {
			t.Fatalf("target %q: %v", to, err)
		}
		got, err := rt.Proxy(&http.Request{Method: http.MethodGet, URL: u})
		if err != nil {
			t.Fatalf("Proxy: %v", err)
		}
		if got == nil {
			return "<direct for this request>"
		}
		return got.String()
	}

	// Nothing said: no transport at all. nil is http.DefaultTransport, and that is how the proxy from the
	// environment, the pooling, the timeouts and the HTTP/2 defaults all survive.
	tr, err := (Options{}).Transport("llm")
	if err != nil || tr != nil {
		t.Fatalf("an empty policy gave %v, %v — want nil, nil", tr, err)
	}

	// A named proxy is the answer for every request this endpoint makes, whatever the environment says:
	// http.ProxyURL is a constant function, and NO_PROXY going with it is the consequence worth pinning —
	// localhost included, an endpoint that named a proxy is not also at the machine's mercy.
	tr, err = (Options{Proxy: "socks5://127.0.0.1:1080"}).Transport(`mcp server "vr"`)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"https://api.example.com/v1/chat", "http://localhost:1234/v1"} {
		if got := proxyFor(t, tr, to); got != "socks5://127.0.0.1:1080" {
			t.Errorf("request to %s would go to %q, want the named proxy", to, got)
		}
	}
	// Only what the policy names is replaced. http.DefaultTransport carries a tls.Config of its own — the
	// clone brought it, this policy did not — so what is worth pinning is not "nil" but "nothing this package
	// decided": verification still on, and still against the system's own anchors.
	if cfg := tr.(*http.Transport).TLSClientConfig; cfg != nil && cfg.InsecureSkipVerify {
		t.Fatal("a proxy-only policy turned off certificate verification")
	} else if cfg != nil && cfg.RootCAs != nil {
		t.Fatal("a proxy-only policy replaced the trust anchors")
	}

	// "direct" is the one value that overrides the environment with a no: a nil func, which net/http reads
	// as "no proxy at all". Leaving the clone's ProxyFromEnvironment in place would have answered it with
	// exactly the thing it rules out.
	tr, err = (Options{Proxy: "direct"}).Transport("llm")
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyFor(t, tr, "https://api.example.com/v1"); got != "<no Proxy func>" {
		t.Fatalf(`"direct" gave %q, want no Proxy func`, got)
	}

	// A certificate policy on its own keeps the environment's proxy. This is the assertion that fails the
	// moment Transport stops being a clone of DefaultTransport, which is the mistake it was written for.
	tr, err = (Options{InsecureTLS: &yes}).Transport("llm")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := tr.(*http.Transport)
	if !ok || rt.Proxy == nil {
		t.Fatal("a certificate policy dropped the environment's proxy: that transport is not a clone")
	}
	if rt.TLSClientConfig == nil || !rt.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecureTLS did not reach the tls.Config")
	}

	// Both halves together, which is a private proxy in front of a private CA.
	tr, err = (Options{InsecureTLS: &yes, Proxy: "http://proxy.internal:3128"}).Transport("llm")
	if err != nil {
		t.Fatal(err)
	}
	if got := proxyFor(t, tr, "https://api.example.com/v1"); got != "http://proxy.internal:3128" {
		t.Fatalf("proxy = %q", got)
	}
	if cfg := tr.(*http.Transport).TLSClientConfig; cfg == nil || !cfg.InsecureSkipVerify {
		t.Fatal("the certificate half was dropped once the proxy half was set")
	}
}

// TestSafeProxy is the one place a proxy address is written out where it is not an error message, and an
// address that authenticates to a proxy is a line in a log file: the host is what makes the line useful and
// the password is what makes it a secret.
func TestSafeProxy(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"direct", "direct"},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"socks5h://proxy.internal:1080", "socks5h://proxy.internal:1080"},
		{"socks4://nope:1080", "<proxy address that cannot be read>"},
	} {
		if got := (Options{Proxy: tc.in}).SafeProxy(); got != tc.want {
			t.Errorf("SafeProxy(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got := (Options{Proxy: "http://user:secret@proxy.internal:3128"}).SafeProxy()
	if strings.Contains(got, "secret") {
		t.Fatalf("the password survived into a log line: %q", got)
	}
	if !strings.Contains(got, "proxy.internal:3128") {
		t.Fatalf("the address is no longer recognisable: %q", got)
	}
}
