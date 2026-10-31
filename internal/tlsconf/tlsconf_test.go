package tlsconf

import (
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

// newServerCert makes a throwaway CA and a leaf it signed, valid for localhost and 127.0.0.1. A real
// two-level chain is the point: "caFile" has to work the way an operator's self-signed endpoint works,
// and a leaf that is its own CA would let a test pass for the wrong reason.
func newServerCert(t *testing.T) (tls.Certificate, []byte) {
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
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func tlsServer(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"ok":true}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func yesNo(b bool) *bool { return &b }

// TestTransportHandshake is the test this package exists for: it dials a real TLS server whose CA no
// system store knows, once per policy, and the interesting case is the one where the connection must
// not be allowed. A unit test of a tls.Config field would not catch a fallback to the system store —
// only an actual handshake can.
func TestTransportHandshake(t *testing.T) {
	cert, caPEM := newServerCert(t)
	srv := tlsServer(t, cert)

	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	good := write("ca.pem", caPEM)
	// Somebody else's CA: a file that reads, parses and holds a certificate, and still cannot verify
	// this server. This is the case that would pass if RootCAs ever fell back to the system pool.
	_, otherPEM := newServerCert(t)
	other := write("other.pem", otherPEM)

	for _, tc := range []struct {
		name    string
		opts    Options
		wantOK  bool
		wantErr string
	}{
		{"default verification cannot check a private CA", Options{}, false, "certificate"},
		{"insecureTLS false is not the same as absent, but verifies the same", Options{InsecureTLS: yesNo(false)}, false, "certificate"},
		{"insecureTLS gets through", Options{InsecureTLS: yesNo(true)}, true, ""},
		{"caFile that signed the server gets through", Options{CAFile: good}, true, ""},
		{"caFile of another CA fails instead of falling back", Options{CAFile: other}, false, "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := tc.opts.Transport("llm")
			if err != nil {
				t.Fatalf("Transport: %v", err)
			}
			client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
			res, err := client.Get(srv.URL)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				defer res.Body.Close()
				if res.StatusCode != http.StatusOK {
					t.Fatalf("status %d", res.StatusCode)
				}
				return
			}
			if err == nil {
				res.Body.Close()
				t.Fatal("the handshake was allowed — this is the silent fallback the package promises not to do")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not look like %q", err, tc.wantErr)
			}
		})
	}
}

func TestTransportWithoutPolicyIsNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{{"nothing set", Options{}}, {"explicitly verifying", Options{InsecureTLS: yesNo(false)}}} {
		tr, err := tc.opts.Transport("llm")
		if err != nil {
			t.Fatalf("%s: Transport: %v", tc.name, err)
		}
		if tr != nil {
			t.Errorf("%s: got a transport (%T), want nil so the caller keeps http.DefaultTransport", tc.name, tr)
		}
	}
	// A policy of nothing, or of an explicit false, is not a policy; either way the caller keeps
	// http.DefaultTransport.
	if none := (Options{}); none.Active() {
		t.Error("nothing set is not a policy")
	}
	if verify := (Options{InsecureTLS: yesNo(false)}); verify.Active() {
		t.Error("an explicit false is not a policy")
	}
	if skip, pinned := (Options{InsecureTLS: yesNo(true)}), (Options{CAFile: "x"}); !skip.Active() || !pinned.Active() {
		t.Error("insecureTLS true or a caFile is a policy")
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	_, caPEM := newServerCert(t)
	good := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(good, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// PEM that parses and holds no certificate — the shape of an operator pointing at their private
	// key, which has to be refused rather than trusted as "an empty set of anchors".
	keyish := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyish, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a certificate")}), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		what    string
		opts    Options
		wantErr string
	}{
		{"nothing set", "llm", Options{}, ""},
		{"insecure alone", "llm", Options{InsecureTLS: yesNo(true)}, ""},
		{"caFile that reads", "llm", Options{CAFile: good}, ""},
		{"the two together", "llm", Options{InsecureTLS: yesNo(true), CAFile: good}, "pick one"},
		{"missing file", "llm", Options{CAFile: filepath.Join(dir, "nope.pem")}, "no such file"},
		{"file with no certificate", "llm", Options{CAFile: keyish}, "holds no certificate"},
		{"the endpoint is named", `mcp server "vr"`, Options{CAFile: keyish}, `mcp server "vr"`},
	} {
		err := tc.opts.Validate(tc.what)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: Validate: %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%s: accepted, want an error containing %q", tc.name, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%s: error %q does not contain %q", tc.name, err, tc.wantErr)
		}
	}
}
