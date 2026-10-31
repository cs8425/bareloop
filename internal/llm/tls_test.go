package llm

// What the two TLS fields are worth depends entirely on the thing that dials the endpoint being built
// from them, which is what this file pins: New refuses a caFile it cannot read instead of dialling
// without it, the transport a policy asks for really carries the request, and Same() notices a policy
// change — otherwise saving one would leave the old provider in place.
//
// What a policy does to a handshake, including that a caFile never falls back to the system trust store,
// is tested once with a real handshake in internal/tlsconf. Nothing here repeats that: these are the
// three ways the wiring could fail while the policy itself is correct.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tlsEndpoint serves a chat completion over HTTPS with a certificate no system trust store knows. It is a
// throwaway CA plus the leaf it signed, because that is how a self-signed endpoint actually works: the
// file an operator is handed is the CA, not the server certificate. internal/tlsconf's tests build one the
// same way and the code is repeated rather than exported — a test helper in an ordinary package is a
// helper every binary links.
func tlsEndpoint(t *testing.T) (url string, caPEM []byte) {
	t.Helper()
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bareloop test ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// writeFile is a caFile in the sense the config means it: a path that exists, or one that does not.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// TestNewRefusesPoliciesItCannotHonour is the config half of "never quietly verify less". Each of these
// would otherwise produce a provider that dials with a policy nobody asked for, and the person reading the
// failed request an hour later would have no way to find out the path was wrong.
func TestNewRefusesPoliciesItCannotHonour(t *testing.T) {
	yes := true
	dir := t.TempDir()
	// PEM that parses and holds no usable certificate: the shape of an operator pointing at their key
	// file, which has to be refused rather than trusted as "an empty set of anchors".
	notACert := writeFile(t, dir, "junk.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}))

	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ca file that cannot be read", Config{BaseURL: "https://x.invalid/v1", CAFile: filepath.Join(dir, "nope.pem")}, "no such file"},
		{"ca file with no certificate in it", Config{BaseURL: "https://x.invalid/v1", CAFile: notACert}, "holds no certificate"},
		{"both answers at once", Config{BaseURL: "https://x.invalid/v1", InsecureTLS: &yes, CAFile: notACert}, "pick one"},
		{"the endpoint is named in the message", Config{BaseURL: "https://x.invalid/v1", CAFile: filepath.Join(dir, "nope.pem")}, "llm"},
	} {
		p, err := New(tc.cfg)
		switch {
		case err == nil:
			t.Errorf("%s: New accepted it (provider %T)", tc.name, p)
		case !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q does not contain %q", tc.name, err, tc.wantErr)
		}
	}
	// Asking not to verify is a request, not a file: nothing about it can fail before a handshake.
	if _, err := New(Config{BaseURL: "https://x.invalid/v1", InsecureTLS: &yes}); err != nil {
		t.Errorf("insecureTLS alone must be accepted: %v", err)
	}
}

// TestProviderDialsAccordingToItsTLSPolicy is the wiring: a provider built with a policy must reach a
// server whose certificate the system store cannot check, and one built without it must not. If
// openai.client() ever goes back to a bare http.Client{}, the two "gets through" cases fail here instead
// of showing up as someone's unexplained connection error.
func TestProviderDialsAccordingToItsTLSPolicy(t *testing.T) {
	url, caPEM := tlsEndpoint(t)
	caFile := writeFile(t, t.TempDir(), "ca.pem", caPEM)
	yes, no := true, false

	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"no policy cannot check a private CA", Config{}, "certificate"},
		{"insecureTLS false verifies like the default", Config{InsecureTLS: &no}, "certificate"},
		{"insecureTLS gets through", Config{InsecureTLS: &yes}, ""},
		{"caFile gets through", Config{CAFile: caFile}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.BaseURL = url + "/v1"
			prov, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := prov.Complete(ctx, Request{Model: "m"})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("the request did not get through: %v", err)
				}
				if res == nil {
					t.Fatal("no result")
				}
				return
			}
			if err == nil {
				t.Fatal("the request went over a connection nothing verified — this is the fallback the config promises not to do")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not look like a refused certificate", err)
			}
		})
	}
}

// TestSameSeesTheCertificatePolicy is what makes a PATCHed policy take effect: Same() decides whether the
// provider is rebuilt, and a policy that changes nothing there is a policy that changes nothing at all.
// Absent and false are deliberately the same answer (neither asks anything of the transport), and neither
// the timeout nor an injected client is part of an endpoint.
func TestSameSeesTheCertificatePolicy(t *testing.T) {
	yes, no := true, false
	a := Config{BaseURL: "https://x/v1", Model: "m"}

	b := a
	b.InsecureTLS = &no
	if !a.Same(b) {
		t.Fatal("absent and false are the same answer, and treating them apart rebuilds a provider nothing changed")
	}
	c := a
	c.InsecureTLS = &yes
	if a.Same(c) {
		t.Fatal("asking not to verify is a different endpoint than asking to verify")
	}
	d := a
	d.CAFile = "/x/ca.pem"
	if a.Same(d) {
		t.Fatal("a different trust anchor is a different endpoint")
	}
	e := Config{BaseURL: "https://x/v1", Model: "m", InsecureTLS: &no, CAFile: "/x/ca.pem"}
	if !d.Same(e) {
		t.Fatal("spelling out false beside a caFile changes nothing")
	}
	f := a
	f.Timeout, f.HTTP = time.Minute, &http.Client{}
	if !a.Same(f) {
		t.Fatal("the timeout and an injected client are not part of an endpoint identity")
	}
}
