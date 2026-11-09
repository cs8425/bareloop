package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The login gate: one credential pair from the daemon config, a browser session
// cookie issued by POST /api/login, and HTTP Basic as the parallel way in for
// everything a browser popup cannot reach (curl -u, go tool pprof, the profiler
// mounted on this same port). Basic is accepted preemptively and never demanded
// with a challenge — see denied. Both ways are accepted on every protected
// route, and a request already carrying a live cookie is never asked for a
// password again — that precedence is the whole "pprof passes if you logged
// into the UI" rule, one if-order, not a second system.
//
// What the gate is not: an account system. One operator, one password, no
// users table — and when a forward-auth proxy or OAuth frontend arrives, the
// whole gate switches off by being unconfigured, because the proxy authenticates
// the HTTP request (the WS upgrade included) and an app-layer login of its own
// would only be a second gate nobody opens.
//
// The absence of any of this is a supported configuration too, and the common
// one on loopback: no auth stanza in config.json, no gate, routes behave
// exactly as they always did.

// sessionCookie names the cookie a successful login sets. The value is 32
// random bytes, kept in a server-side map and never derived from anything: no
// signing secret exists to leak, and deleting the map entry ends the session
// everywhere at once.
const sessionCookie = "bareloop_session"

const (
	// loginHold is the constant time every /api/login answer waits out, right
	// or wrong. It exists because a fast "wrong password" beside a slow
	// "no such user" (or a compare that returns early) is a timing oracle for
	// free; half a second also prices online guessing at two attempts per
	// second per connection, per the same reasoning as the failure window.
	loginHold = 500 * time.Millisecond
	// loginBodyCap bounds the login body: it holds two short strings, and a
	// pre-auth endpoint should not be a place to stream bytes toward.
	loginBodyCap = 16 << 10
	// failMapCap bounds the failure-window map the way fpMapCap bounds the
	// fingerprint map — a flood that fills it has outgrown per-key limiting,
	// and emptying the map (fail-open) beats refusing connections (fail-closed)
	// for a daemon whose owner must never be locked out by an attacker.
	failMapCap = 8192
)

// Auth is the gate. The zero piece does not exist: NewAuth returns one, and a
// nil *Auth on a Server means no gate at all.
type Auth struct {
	user   string
	pass   string
	idle   time.Duration // a session untouched for this long stops answering
	window time.Duration // the failure window
	limit  int           // failures per key per window before 429
	secure bool          // set the cookie Secure (the server itself is serving TLS)
	// hold is a field so tests need not wait half a second per wrong password.
	hold time.Duration
	logf func(string, ...any)

	mu       sync.Mutex
	sessions map[string]time.Time   // session token → last seen
	fails    map[string]*failWindow // rate-limit key → current failures
	// fps keys failed logins by TLS fingerprint when the request arrived over
	// TLS, falling back to clientIP; nil is legal (plain-HTTP deployments).
	fps *fpRecorder
	// trusted, when non-empty, names the networks whose X-Forwarded-For may be
	// believed. Empty means the TCP peer address is the client, full stop —
	// the safe default, because trusting XFF without a boundary lets any
	// request choose which bucket it charges.
	trusted []*net.IPNet
	// now is a field so tests can move the clock.
	now func() time.Time
}

func NewAuth(user, pass string, idle, window time.Duration, limit int, secure bool, trusted []*net.IPNet, logf func(string, ...any)) *Auth {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Auth{
		user: user, pass: pass, idle: idle, window: window, limit: limit,
		secure: secure, hold: loginHold, logf: logf, trusted: trusted,
		sessions: map[string]time.Time{},
		fails:    map[string]*failWindow{},
		now:      time.Now,
	}
}

// startCleaner reaps idle sessions, expired failure windows, and stale
// fingerprints on one tick. A process with no gate builds none of that state
// and calls nothing. EnableAuth calls it after wiring the fingerprint recorder.
func (a *Auth) startCleaner() {
	go func() {
		for range time.Tick(10 * time.Minute) {
			a.clean()
			if a.fps != nil {
				a.fps.Sweep()
			}
		}
	}()
}

func (a *Auth) clean() {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for tok, seen := range a.sessions {
		if now.Sub(seen) > a.idle {
			delete(a.sessions, tok)
		}
	}
	for k, w := range a.fails {
		if now.Sub(w.start) > a.window {
			delete(a.fails, k)
		}
	}
}

// Middleware wraps the whole handler set. The exemptions below are the complete
// list of what answers without credentials, and each one is a fact rather than
// a preference.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/login" && r.Method == http.MethodPost:
			a.handleLogin(w, r)
			return
		case r.URL.Path == "/api/logout" && r.Method == http.MethodPost:
			// Logout is idempotent and the cookie being cleared is the proof;
			// making it authenticated would strand a UI that cannot tell an
			// expired session from an absent one.
			a.handleLogout(w, r)
			return
		case r.URL.Path == "/api/health":
			// The container healthcheck carries no credentials. The reply is
			// {"ok":true,"time":...}, which reveals less than a TCP connect.
			next.ServeHTTP(w, r)
			return
		case !protected(r):
			// The UI itself: index.html and the assets that render the login
			// dialog live here, so they cannot hide behind the gate they
			// announce. Static files carry no session data; neither does
			// /testdata/, which asked to be development-only in the first place.
			next.ServeHTTP(w, r)
			return
		}

		// Cookie first: a browser that logged in once carries it on every
		// channel (fetch, EventSource, WebSocket alike) without any wiring,
		// and pprof behind that same browser answers without a popup.
		if tok, err := r.Cookie(sessionCookie); err == nil && a.liveSession(tok.Value) {
			if r.URL.Path == "/api/whoami" && r.Method == http.MethodGet {
				a.handleWhoami(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// Basic second: the way in for curl -u and go tool pprof. A wrong
		// pair is an explicit guess and charges the failure window; the
		// absence of an Authorization header is not a guess, it is an
		// unauthenticated request, and answering it with 401 must not cost
		// the caller anything (a stale script must not lock its own owner out).
		if user, pass, ok := r.BasicAuth(); ok {
			if a.match(user, pass) {
				if r.URL.Path == "/api/whoami" && r.Method == http.MethodGet {
					a.handleWhoami(w, r)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			n := a.charge(a.keyFor(r))
			a.logf("basic auth failure from %s (%d in %s)", a.clientIP(r), n, a.window)
			a.denied(w, r)
			return
		}
		// Neither: the bare 401. No WWW-Authenticate header, ever — see denied.
		a.denied(w, r)
	})
}

// protected names what the gate speaks to: API routes, the websocket, and the
// profiler when it shares the port. Everything else answers anonymous on
// purpose (see Middleware).
func protected(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/api/") || p == "/ws" || strings.HasPrefix(p, "/debug/")
}

// denied answers without a door: 401 JSON and no WWW-Authenticate header. The
// absence of that header is the whole design, learned live twice over. A
// challenge is an invitation every browser must obey — it pops Chrome's native
// dialog over the API's own JSON body, and worse it enrols the origin into the
// browser's HTTP auth cache, where one bad pair later rides every request and
// turns even fetches into ERR_INVALID_AUTH_CREDENTIALS, killing the very 401
// the UI's login dialog exists to catch. A machine client does not need the
// invitation either: curl -u sends its Authorization preemptively on request
// one, and go tool pprof -header does the same. So Basic stays fully accepted
// (the compare is below, in the credential order) — it is only never asked for.
func (a *Auth) denied(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusUnauthorized, errAuth)
}

var errAuth = authError{}

type authError struct{}

func (authError) Error() string { return "authentication required" }

// handleLogin answers at a constant time on purpose, held out by a deadline
// measured in real time at entry: a wrong user, a wrong password, and a right
// answer should not be separable by their latency, and a 500ms floor prices
// scripted guessing beyond what the failure window already forbids. The hold
// deliberately does not read a.now: business time (sessions, windows) may be a
// test's clock or a jumped one, and an answer must never sleep for the size of
// a clock jump.
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		if d := a.hold - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}()

	key := a.keyFor(r)
	if wait := a.blocked(key); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, errLimited)
		return
	}

	var body struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyCap)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, errBadLoginBody)
		return
	}
	if !a.match(body.User, body.Pass) {
		n := a.charge(key)
		a.logf("login failed from %s (%d in %s)", a.clientIP(r), n, a.window)
		// One message for every wrong pair, so the endpoint cannot enumerate
		// which user this daemon knows.
		writeErr(w, http.StatusUnauthorized, errAuth)
		return
	}
	a.release(key)
	tok := rand.Text() // 32 random bytes, base64url — no secret behind it, the map entry is the state
	a.mu.Lock()
	a.sessions[tok] = a.now()
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.secure,
		// No Max-Age on purpose: closing the browser drops the cookie, which
		// is a logout that needs no button. The server side expires idle
		// sessions regardless — an old cookie cannot age back to life.
	})
	writeJSON(w, map[string]any{"ok": true})
}

var (
	errLimited      = staticError{"too many failed logins, try again later"}
	errBadLoginBody = staticError{"login body must be {\"user\":...,\"pass\":...}"}
)

type staticError struct{ s string }

func (e staticError) Error() string { return e.s }

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, tok.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure})
	writeJSON(w, map[string]any{"ok": true})
}

// handleWhoami lets the UI show the state it is in (logged in as whom, and a
// way to reach logout) without inventing a second source of truth. The gate
// answers it after a credential passes — before the wrapped handler runs —
// because the wrapped handler is the API, and what user this daemon answers
// for is the gate's fact, not the sessions handler's.
func (a *Auth) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"user": a.user})
}

func (a *Auth) match(user, pass string) bool {
	// Both compares constant-time, and both always run: which half was wrong
	// is not allowed to change the time.
	u := subtle.ConstantTimeCompare([]byte(user), []byte(a.user))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(a.pass))
	return u == 1 && p == 1
}

// liveSession reports whether the token names a session still inside its idle
// window, and slides that window on the way. Sliding is the deliberate
// trade-off documented with the config field: a stolen cookie in constant use
// never expires on its own, and an honest session never notices the deadline.
func (a *Auth) liveSession(tok string) bool {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	seen, ok := a.sessions[tok]
	if !ok || now.Sub(seen) > a.idle {
		return false
	}
	a.sessions[tok] = now
	return true
}

// ---------------------------------------------------------------- rate limit

// failWindow counts explicit credential failures for one key inside one fixed
// window: charging past the limit answers 429 until the window rolls, which is
// a lockout that always ends by itself — the property that keeps a single
// operator from ever needing a way in that does not involve waiting.
type failWindow struct {
	n     int
	start time.Time
}

// charge records one failed credential and returns the window's new count.
func (a *Auth) charge(key string) int {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.fails[key]
	if w == nil || now.Sub(w.start) > a.window {
		if len(a.fails) >= failMapCap {
			a.fails = map[string]*failWindow{} // flooded: forget the counting, keep serving
		}
		w = &failWindow{start: now}
		a.fails[key] = w
	}
	w.n++
	return w.n
}

// blocked returns how long the key still sits out its window, 0 when free.
func (a *Auth) blocked(key string) time.Duration {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.fails[key]
	if w == nil || now.Sub(w.start) > a.window || w.n < a.limit {
		return 0
	}
	return a.window - now.Sub(w.start)
}

// release forgets a key after a success: the honest operator who fat-fingered
// twice starts clean the moment they get in.
func (a *Auth) release(key string) {
	a.mu.Lock()
	delete(a.fails, key)
	a.mu.Unlock()
}

// keyFor is which bucket a credential failure charges: the TLS fingerprint of
// the connection when there is one (software identity: survives the guesser
// rotating source addresses), the network address otherwise.
func (a *Auth) keyFor(r *http.Request) string {
	if fp := a.fps.key(r); fp != "" {
		return "tls:" + fp
	}
	return "ip:" + a.clientIP(r)
}

// clientIP answers where the request came from, honoring X-Forwarded-For only
// when the TCP peer is inside a network the operator declared trustworthy. The
// walk runs right to left through the chain and stops at the first address not
// in those networks: proxies append, so the rightmost untrusted hop is the
// closest thing to the real client that an attacker lower in the chain cannot
// have written. With no trusted networks configured, XFF is not consulted at
// all — an unowned "trusted" default would be the bypass this whole function
// exists to prevent.
func (a *Auth) clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	peer := net.ParseIP(host)
	if len(a.trusted) == 0 || peer == nil || !ipIn(peer, a.trusted) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	chain := strings.Split(xff, ",")
	for i := len(chain) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(chain[i]))
		if ip == nil {
			continue
		}
		if !ipIn(ip, a.trusted) {
			return ip.String()
		}
	}
	// Every hop trusted (somebody's home LAN): the leftmost is the furthest
	// from us, which is as close to "the client" as this chain gets.
	if ip := net.ParseIP(strings.TrimSpace(chain[0])); ip != nil {
		return ip.String()
	}
	return host
}

func ipIn(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseNets turns config CIDR strings into the networks clientIP trusts.
// Bare IPs are accepted and read as /32 or /128. Errors are returned with the
// offending entry named, because a typo in this list silently widens or
// narrows who the server believes.
func ParseNets(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			out = append(out, n)
			continue
		}
		ip := net.ParseIP(e)
		if ip == nil {
			return nil, &net.ParseError{Type: "IP address or CIDR", Text: e}
		}
		bits := 8 * net.IPv4len
		if ip.To4() == nil {
			bits = 8 * net.IPv6len
		}
		out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return out, nil
}
