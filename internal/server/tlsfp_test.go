package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func hello() *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		CipherSuites:      []uint16{4865, 4866, 4867},
		Extensions:        []uint16{0, 10, 11, 35, 16, 23, 13},
		SupportedCurves:   []tls.CurveID{29, 23, 24},
		SupportedPoints:   []uint8{0},
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	}
}

func TestFingerprintStableAndSensitive(t *testing.T) {
	base := fingerprintOf(hello())
	if again := fingerprintOf(hello()); again != base {
		t.Fatalf("same Hello produced two fingerprints: %s vs %s", base, again)
	}
	if len(base) != 32 {
		t.Fatalf("want 32 hex chars, got %d", len(base))
	}
	// Every one of the five lists moves the fingerprint on its own.
	mutate := map[string]func(h *tls.ClientHelloInfo){
		"cipher":   func(h *tls.ClientHelloInfo) { h.CipherSuites = append(h.CipherSuites, ciphersuiteTLS10) },
		"version":  func(h *tls.ClientHelloInfo) { h.SupportedVersions = []uint16{tls.VersionTLS12} },
		"curve":    func(h *tls.ClientHelloInfo) { h.SupportedCurves = h.SupportedCurves[:2] },
		"point":    func(h *tls.ClientHelloInfo) { h.SupportedPoints = nil },
		"extAdd":   func(h *tls.ClientHelloInfo) { h.Extensions = append(h.Extensions, extensionRenegotiation) },
		"extOrder": func(h *tls.ClientHelloInfo) { h.Extensions[0], h.Extensions[1] = h.Extensions[1], h.Extensions[0] },
	}
	for name, f := range mutate {
		h := hello()
		f(h)
		if fingerprintOf(h) == base {
			t.Errorf("mutating %s did not change the fingerprint", name)
		}
	}
}

// wire-order constants the mutation cases reuse; spelled here so the test reads as
// "a cipher, an extension" rather than unexplained numbers.
const (
	ciphersuiteTLS10       uint16 = 0x002f // TLS_RSA_WITH_AES_128_CBC_SHA
	extensionRenegotiation uint16 = 0xff01 // renegotiation_info, absent from the fake Hello
)

func TestFingerprintLegacyDefaultsTo12(t *testing.T) {
	h := hello()
	h.SupportedVersions = nil
	legacy := fingerprintOf(h)
	h2 := hello()
	h2.SupportedVersions = []uint16{tls.VersionTLS12}
	if got := fingerprintOf(h2); got != legacy {
		t.Errorf("an empty version list is legacy 1.2 semantics: %s != %s", got, legacy)
	}
}

// stubConn answers only what the hook reads: its remote address.
type stubConn struct {
	net.Conn
	ra string
}

func (s stubConn) RemoteAddr() net.Addr {
	a, err := net.ResolveTCPAddr("tcp", s.ra)
	if err != nil {
		panic(err)
	}
	return a
}

func tlsRequest(remote string) *http.Request {
	req, err := http.NewRequest("POST", "http://x/api/login", nil)
	if err != nil {
		panic(err)
	}
	req.RemoteAddr = remote
	req.TLS = &tls.ConnectionState{} // any non-nil marks the request as arrived over TLS
	return req
}

func TestFPRecorderHookThenKey(t *testing.T) {
	r := newFPRecorder(nil)
	if _, err := r.hook(&tls.ClientHelloInfo{Conn: stubConn{ra: "9.9.9.9:44444"}, CipherSuites: []uint16{1}}); err != nil {
		t.Fatal(err)
	}
	want := fingerprintOf(&tls.ClientHelloInfo{Conn: stubConn{ra: "-"}, CipherSuites: []uint16{1}})
	if got := r.key(tlsRequest("9.9.9.9:1")); got != want {
		t.Fatalf("key from recorded host = %q, want %q", got, want)
	}
	// A second tool from the same host overwrites: the recorder tracks the fingerprint
	// last seen at an address, which is all a rate-limit key needs.
	if _, err := r.hook(&tls.ClientHelloInfo{Conn: stubConn{ra: "9.9.9.9:44445"}, CipherSuites: []uint16{2}}); err != nil {
		t.Fatal(err)
	}
	if got := r.key(tlsRequest("9.9.9.9:1")); got == want {
		t.Fatal("the newer Hello from the same host should win")
	}
	// Plain HTTP asks the recorder nothing; the caller's IP fallback takes over.
	if got := r.key(&http.Request{RemoteAddr: "9.9.9.9:1"}); got != "" {
		t.Fatalf("plaintext request should have no fingerprint, got %q", got)
	}
}

func TestFPRecorderSweepAndTouch(t *testing.T) {
	r := newFPRecorder(nil)
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return clock }
	if _, err := r.hook(&tls.ClientHelloInfo{Conn: stubConn{ra: "8.8.8.8:1"}, CipherSuites: []uint16{7}}); err != nil {
		t.Fatal(err)
	}
	// Still fresh inside the quiet window.
	r.Sweep()
	if r.key(tlsRequest("8.8.8.8:2")) == "" {
		t.Fatal("a recent host must survive a sweep")
	}
	// key() touches: keep asking across the window and the entry must not age out.
	for i := 0; i < 3; i++ {
		clock = clock.Add(40 * time.Minute)
		if r.key(tlsRequest("8.8.8.8:2")) == "" {
			t.Fatal("a touched entry aged out while it was being read")
		}
	}
	// Silence past the quiet window forgets it.
	clock = clock.Add(2 * time.Hour)
	r.Sweep()
	if got := r.key(tlsRequest("8.8.8.8:2")); got != "" {
		t.Fatalf("a quiet host should be forgotten, still got %q", got)
	}
}

func TestFPRecorderFloodIsFailOpen(t *testing.T) {
	r := newFPRecorder(nil)
	r.now = func() time.Time { return time.Now() }
	for i := 0; i <= fpMapCap; i++ {
		chi := &tls.ClientHelloInfo{Conn: stubConn{ra: fmt.Sprintf("10.0.%d.%d:1", i/256, i%256)}, CipherSuites: []uint16{uint16(i)}}
		if _, err := r.hook(chi); err != nil {
			t.Fatal(err)
		}
	}
	r.mu.Lock()
	size := len(r.m)
	r.mu.Unlock()
	if size > fpMapCap {
		t.Fatalf("the map grew past its cap: %d", size)
	}
	// Emptied or not, it still answers: a flooded recorder degrades to IP fallback,
	// it never refuses a handshake.
	if got := r.key(tlsRequest("10.0.0.9:9")); got != "" && len(got) != 32 {
		t.Fatalf("unexpected key %q", got)
	}
}
