package server

import (
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fingerprintOf renders the stable identity of a client's Hello as a short hex
// string: the five lists a TLS fingerprint has always been made of — negotiated
// version, cipher suites, extension IDs, curves, point formats — each kept in the
// order the client sent them, because the order is half of what the fingerprint is.
//
// The lists are the stdlib's parsed view of the ClientHello (crypto/tls exposes the
// extension **IDs**, not their bodies). Bodies are where a naive fingerprint would
// drift on every connection — key_share's fresh key, ALPS content, the ECH payload —
// and an ID-only view never reads them. But IDs drift too, in one way worth naming:
// GREASE. Chrome injects random 0xdAdA values into the cipher-suites, extensions and
// curves lists of every handshake, as a tripwire against middleboxes that rewrite
// Hellos — and a tripwire that fires on the fingerprint too: unfiltered, every Chrome
// connection fingerprints differently, which is exactly what this function is not
// allowed to do. RFC 8701 reserves the entire 0xdAdA space for GREASE, so dropping it
// here cannot drop a registered ID — and Chrome still announces itself, because the
// real lists around the GREASE noise are its own.
//
// The fingerprint is a rate-limit bucket key and a log line, nothing else: a client
// willing to forge one is a client that exists (uTLS is a Go library), and anyone who
// can build ClientHellos is bounded by everything else this server does. Format is
// JA3-shaped and JA3-compatible only by accident; it is an internal key, and nothing
// external is allowed to depend on its bytes. MD5 is the fingerprint's alphabet here,
// not a security primitive.
func fingerprintOf(chi *tls.ClientHelloInfo) string {
	sum := md5.Sum([]byte(helloLists(chi))) //nolint:gosec // fingerprint, not a digest of anything trusted
	return hex.EncodeToString(sum[:])
}

// helloLists renders the five fingerprint inputs as one string — the pre-image of
// the fingerprint, and the one form that lets a human diff two Hellos by eye.
func helloLists(chi *tls.ClientHelloInfo) string {
	// A Hello with no supported_versions extension predates TLS 1.3 negotiation;
	// the stdlib documents SupportedVersions as extrapolated in that case, and a
	// still-empty list means legacy 1.2 semantics. GREASE rides in this list too
	// (0x1a1a and friends made it into live logs as versions "26.26" and "58.58"),
	// and the max would otherwise record the tripwire as the client's version.
	ver := uint16(tls.VersionTLS12)
	for _, v := range chi.SupportedVersions {
		if isGrease(v) {
			continue
		}
		if v > ver {
			ver = v
		}
	}
	var b strings.Builder
	// major.minor as plain integers, so 1.3 reads "772.4" the way fingerprint tools print it.
	b.WriteString(strconv.Itoa(int(ver>>8) & 0xff))
	b.WriteByte('.')
	b.WriteString(strconv.Itoa(int(ver & 0xff)))
	b.WriteByte(',')
	b.WriteString(joinU16(chi.CipherSuites))
	b.WriteByte(',')
	b.WriteString(joinExtIDs(chi.Extensions))
	b.WriteByte(',')
	b.WriteString(joinCurveIDs(chi.SupportedCurves))
	b.WriteByte(',')
	b.WriteString(joinBytes(chi.SupportedPoints))
	return b.String()
}

// joinExtIDs renders the extensions list as a sorted set — the one input treated
// as a set, because it is the one input Chrome shuffles on every connection (the
// anti-fingerprint counterpart of GREASE, and live logs showed every Chrome
// handshake with a different arrangement) and sprinkles with per-connection
// state: session_ticket and early_data come and go with the client's session
// cache. Order and cache contents change every connection; software identity does not. The set of
// extensions still separates clients hard — curl's set and Chrome's set share
// hardly any members — the arrangement adds discrimination the shuffle would
// only make unstable, and instability in a rate-limit key is a bypass: every
// connection a browser makes would open a fresh failure window.
func joinExtIDs(xs []uint16) string {
	kept := make([]uint16, 0, len(xs))
	for _, x := range xs {
		if !isGrease(x) && !extIsState(x) {
			kept = append(kept, x)
		}
	}
	slices.Sort(kept)
	parts := make([]string, len(kept))
	for i, x := range kept {
		parts[i] = strconv.Itoa(int(x))
	}
	return strings.Join(parts, "-")
}

// extIsState reports the extensions whose presence in a first Hello depends on
// which session cache the client happens to be holding, not on what software it
// runs: session_ticket (an empty ID asking for a ticket, sent only while the
// client has none to resume — live proof: Firefox announced 35 on the first
// visit to a host and dropped it on every visit after), pre_shared_key and
// psk_key_exchange_modes (RFC 8446 offers PSK modes only when the client means
// to resume — live proof: the same Firefox sent 45 in a normal window and
// dropped it in a private one, where there is no session to resume), and
// early_data (0-RTT appetite, which likewise changes with the cache). A
// fingerprint keying on these logs one browser as two people, and a rate-limit
// key that does so is a rate limit with a turnstile. No Hello that arrives here
// carries pre_shared_key today (hello-retry Hellos are skipped), and it is
// listed because that is one policy change away from untrue.
func extIsState(x uint16) bool {
	switch x {
	case extensionSessionTicket, extensionPreSharedKey, extensionEarlyData, extensionPSKKeyExchangeModes:
		return true
	}
	return false
}

// The wire IDs (RFC 8446 §4.2, RFC 5077 bis) of the cache-state extensions.
const (
	extensionSessionTicket       uint16 = 35 // 0x23
	extensionPreSharedKey        uint16 = 40 // 0x28
	extensionEarlyData           uint16 = 41 // 0x29
	extensionPSKKeyExchangeModes uint16 = 45 // 0x2d
)

// isGrease reports the RFC 8701 pattern, 0xdAdA — equal bytes whose low nibble is A.
// The whole pattern space is reserved for the tripwire, which is why filtering it out
// of every list costs nothing: no cipher suite, extension, or curve can ever be
// assigned a value the fingerprint drops.
func isGrease(x uint16) bool {
	// x&0xff00 == x<<8 compares the two bytes of x within one type: shifting a
	// uint16 left by 8 parks the high byte where the low one was.
	return x&0xff00 == x<<8 && x&0x0f == 0x0a
}

func joinU16(xs []uint16) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		if !isGrease(x) {
			parts = append(parts, strconv.Itoa(int(x)))
		}
	}
	return strings.Join(parts, "-")
}

func joinCurveIDs(xs []tls.CurveID) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		if !isGrease(uint16(x)) {
			parts = append(parts, strconv.Itoa(int(x)))
		}
	}
	return strings.Join(parts, "-")
}

func joinBytes(xs []uint8) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(int(x))
	}
	return strings.Join(parts, "-")
}

// fpRecorder remembers the fingerprint last seen arriving from each host, filled at
// the TLS handshake and read while deciding which rate-limit window a failed login
// charges. It exists because crypto/tls hands the Hello exactly once — in
// GetConfigForClient, before any request runs — and tls.ConnectionState is what is
// left afterwards: the server's own selections, identical for every client of one
// configuration, and no fingerprint in it.
//
// The key is the remote host, not the connection, because a fingerprint identifies
// software: every connection from one client tool agrees on it, and a login attempt
// on a keep-alive connection cannot be matched back to the Hello that opened it
// (requests do not carry their net.Conn). Behind a proxy many hosts collapse into
// one address and the fingerprint there is the proxy's anyway — the caller's own
// IP remains the fallback key either way.
//
// Entries expire: get touches, so a host that keeps connecting is never forgotten
// mid-burst, and the sweep drops hosts quieter than any honest client's idle. The
// cap is fail-open — a handshake flood that fills the map has already outgrown every
// defense in this file, and the cheaper wrong answer is to forget fingerprints.
type fpRecorder struct {
	mu sync.Mutex
	m  map[string]fpEntry
	// now is a field so tests can freeze it.
	now func() time.Time
	// logf receives one line whenever an address shows a fingerprint the
	// server had not just seen: steady-state reconnects from the same software
	// stay silent, while a new client (or a client that changed) announces
	// itself — which is also how an operator confirms the fingerprints of
	// curl, python, and a browser really do differ.
	logf func(format string, args ...any)
}

type fpEntry struct {
	fp       string
	lastSeen time.Time
}

// fpMapCap bounds remembered hosts; fpQuiet is how long a host may go untouched
// before the sweep forgets its fingerprint.
const (
	fpMapCap = 8192
	fpQuiet  = time.Hour
)

func newFPRecorder(logf func(string, ...any)) *fpRecorder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &fpRecorder{m: make(map[string]fpEntry), now: time.Now, logf: logf}
}

// hook is a tls.Config.GetConfigForClient. Returning a nil Config means "use the
// config you already had", so wiring a fingerprint observer into a server changes
// nothing else about the handshake.
func (r *fpRecorder) hook(chi *tls.ClientHelloInfo) (*tls.Config, error) {
	if chi == nil || chi.Conn == nil {
		return nil, nil
	}
	if chi.HelloRetryRequest {
		// TLS 1.3 hello retry: this second Hello is the continuation of a
		// handshake whose first Hello was already recorded, and it is
		// deliberately shorter than the first (fewer extensions, one key_share
		// group). Recording it would log one client as two and let a retry
		// overwrite the fingerprint a failed login was about to be charged to.
		return nil, nil
	}
	host, _, err := net.SplitHostPort(chi.Conn.RemoteAddr().String())
	if err != nil {
		host = chi.Conn.RemoteAddr().String()
	}
	fp := fingerprintOf(chi)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) >= fpMapCap && r.m[host].fp == "" {
		r.m = make(map[string]fpEntry) // flooded: forgetting everything beats refusing connections
	}
	changed := r.m[host].fp != fp
	r.m[host] = fpEntry{fp: fp, lastSeen: now}
	if changed {
		// The 8-char prefix is a log anchor, not a key: the full value only ever
		// lives in the map, and collisions in a prefix have no consequence here.
		r.logf("tls client %s from %s", fp[:8], host)
	}
	return nil, nil
}

// key returns the rate-limit key for a request: the fingerprint last recorded for
// the address it came from, when it arrived over TLS; "" tells the caller to use
// its own client-IP answer. clientIP is not computed here so this file knows
// nothing about proxies — that belongs to the trusted-proxy boundary.
func (r *fpRecorder) key(req *http.Request) string {
	if r == nil || req.TLS == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[host]
	if !ok {
		return ""
	}
	e.lastSeen = r.now()
	r.m[host] = e
	return e.fp
}

// Sweep forgets hosts untouched for fpQuiet. The auth cleaner calls it on the same
// tick that cleans login-failure windows; a server without auth never builds one.
func (r *fpRecorder) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	stale := r.now().Add(-fpQuiet)
	for h, e := range r.m {
		if e.lastSeen.Before(stale) {
			delete(r.m, h)
		}
	}
}
