package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAuth() *Auth {
	a := NewAuth("op", "s3cret", 7*24*time.Hour, 15*time.Minute, 5, false, nil, nil)
	a.hold = time.Millisecond // tests are not made to wait half a second per wrong password
	return a
}

func gated(a *Auth) http.Handler {
	return a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "who": r.URL.Path})
	}))
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func do(h http.Handler, method, path, body string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func loginAs(h http.Handler, user, pass string) (string, *httptest.ResponseRecorder) {
	rec := do(h, "POST", "/api/login", `{"user":"`+user+`","pass":"`+pass+`"}`, nil)
	var tok string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			tok = c.Value
		}
	}
	return tok, rec
}

func sess(v string) *http.Cookie { return &http.Cookie{Name: sessionCookie, Value: v} }

// The gate's shape in one table: what answers anonymously, what answers 401 with
// a Basic challenge, and — once a credential matches by either way in — what
// finally reaches the wrapped handler.
func TestGateTable(t *testing.T) {
	a := testAuth()
	h := gated(a)

	if rec := do(h, "GET", "/api/health", "", nil); rec.Code != 200 {
		t.Fatalf("health is exempt for the container healthcheck: %d", rec.Code)
	}
	if rec := do(h, "GET", "/", "", nil); rec.Code != 200 {
		t.Fatalf("the UI must load to be able to ask for a login: %d", rec.Code)
	}
	for _, p := range []string{"/api/sessions", "/ws", "/debug/pprof/"} {
		rec := do(h, "GET", p, "", nil)
		if rec.Code != 401 {
			t.Fatalf("%s without credentials: %d", p, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "authentication required" {
			t.Fatalf("%s: 401 body is not the usual JSON error shape: %s", p, rec.Body)
		}
	}
}

// No 401 carries a Basic challenge, to any client. The challenge enrols the
// origin into the browser's HTTP auth cache — live, that turned the UI's own
// fetches into ERR_INVALID_AUTH_CREDENTIALS and hung the boot behind a login
// dialog that could no longer see its 401 — and a machine client never needed
// it: curl -u and go tool pprof -header send Authorization preemptively.
func TestNoChallengeAnywhere(t *testing.T) {
	a := testAuth()
	h := gated(a)
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"curl", nil},
		{"address bar", map[string]string{"Sec-Fetch-Mode": "navigate"}},
		{"fetch", map[string]string{"Sec-Fetch-Mode": "cors"}},
		{"websocket upgrade", map[string]string{"Sec-WebSocket-Key": "abc="}},
	}
	for _, c := range cases {
		rec := do(h, "GET", "/api/sessions", "", c.headers)
		if rec.Code != 401 {
			t.Fatalf("%s: %d", c.name, rec.Code)
		}
		if wa := rec.Header().Get("WWW-Authenticate"); wa != "" {
			t.Fatalf("%s: a challenge would pop the browser or poison its auth cache: %q", c.name, wa)
		}
	}
	// And a wrong Basic gets the same bare answer — the challenge must not come
	// back on the retry path either, which is exactly where Chrome would eat it.
	if rec := do(h, "GET", "/api/sessions", "", map[string]string{"Authorization": basic("op", "wrong")}); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("wrong basic: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestCookieAndBasic(t *testing.T) {
	a := testAuth()
	h := gated(a)

	tok, rec := loginAs(h, "op", "s3cret")
	if rec.Code != 200 || tok == "" {
		t.Fatalf("login: %d %s (token %q)", rec.Code, rec.Body, tok)
	}
	c := rec.Result().Cookies()[0]
	if c.MaxAge != 0 || !c.HttpOnly || c.Path != "/" || c.Secure {
		t.Fatalf("cookie attributes: %+v (want HttpOnly, Path /, Lax, no Max-Age, insecure)", c)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite: %v (Lax is the cross-site wall; the Origin check was dropped for it)", c.SameSite)
	}
	if rec := do(h, "GET", "/api/sessions", "", nil, sess(tok)); rec.Code != 200 {
		t.Fatalf("live cookie: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/whoami", "", nil, sess(tok)); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"user":"op"`) {
		t.Fatalf("whoami: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/api/sessions", "", nil, sess("forged")); rec.Code != 401 {
		t.Fatalf("forged token must name no session: %d", rec.Code)
	}
	// Basic: the parallel way in for curl -u and go tool pprof.
	if rec := do(h, "GET", "/debug/pprof/", "", map[string]string{"Authorization": basic("op", "s3cret")}); rec.Code != 200 {
		t.Fatalf("basic: %d", rec.Code)
	}
	// Cookie outranks a wrong Basic: a logged-in browser is never locked out by
	// a stale Authorization header some tool left on a shared origin.
	if rec := do(h, "GET", "/api/sessions", "", map[string]string{"Authorization": basic("op", "stale-wrong")}, sess(tok)); rec.Code != 200 {
		t.Fatalf("live cookie beside stale wrong basic: %d", rec.Code)
	}
	// Logout ends the session everywhere the token reached; the token never
	// comes back, expired-cleanly or otherwise.
	if rec := do(h, "POST", "/api/logout", "", nil, sess(tok)); rec.Code != 200 {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/sessions", "", nil, sess(tok)); rec.Code != 401 {
		t.Fatalf("after logout: %d", rec.Code)
	}
}

func TestLoginWrongIsUniformAndHeld(t *testing.T) {
	a := testAuth()
	a.hold = 80 * time.Millisecond
	h := gated(a)
	start := time.Now()
	_, rec := loginAs(h, "op", "wrong")
	d := time.Since(start)
	if rec.Code != 401 {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	// One message for every wrong pair, so the endpoint cannot enumerate which
	// user this daemon knows — and it arrives no faster than a right answer.
	if !strings.Contains(rec.Body.String(), "authentication required") {
		t.Fatalf("wrong-pair message should not distinguish user from password: %s", rec.Body)
	}
	if d < a.hold {
		t.Fatalf("answer came back in %v, under the %v hold — the timing oracle the hold exists to close", d, a.hold)
	}
}

func TestFailureWindow(t *testing.T) {
	a := testAuth()
	a.limit = 3
	h := gated(a)
	// The bucket: failed logins and failed Basics, one counter (one attack surface).
	for i := 0; i < 2; i++ {
		if _, rec := loginAs(h, "op", "wrong"); rec.Code != 401 {
			t.Fatalf("login failure %d: %d", i, rec.Code)
		}
	}
	if rec := do(h, "GET", "/api/sessions", "", map[string]string{"Authorization": basic("op", "wrong")}); rec.Code != 401 {
		t.Fatalf("basic failure: %d", rec.Code)
	}
	// An unauthenticated request is not a guess: still 401, never 429, and it
	// charges nothing — a stale script must not lock its own owner out.
	if rec := do(h, "GET", "/api/sessions", "", nil); rec.Code != 401 {
		t.Fatalf("anonymous request inside a filling window: %d", rec.Code)
	}
	// The next explicit guess is refused with a clock, and the lockout ends by
	// itself — nothing to un-wedge, no restart ritual.
	_, rec := loginAs(h, "op", "guess")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth failure: %d %s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || strings.HasPrefix(ra, "-") {
		t.Fatalf("429 Retry-After: %q", ra)
	}
	// Blocked attempts never reach the counter, so hammering a locked door does
	// not push the unlock further out.
	before := a.blocked("ip:192.0.2.1")
	for i := 0; i < 5; i++ {
		loginAs(h, "op", "guess")
	}
	if after := a.blocked("ip:192.0.2.1"); after > before {
		t.Fatalf("the lockout grew while blocked: %v -> %v", before, after)
	}
	// The window rolls; the key starts clean, and success releases it outright.
	a.now = frozen(+time.Hour)
	if _, rec := loginAs(h, "op", "s3cret"); rec.Code != 200 {
		t.Fatalf("login after the window rolled: %d %s", rec.Code, rec.Body)
	}
	if w := a.blocked("ip:192.0.2.1"); w != 0 {
		t.Fatalf("a success should release the key, still locked for %v", w)
	}
	// Two fresh failures now count from zero against the new window: still 401s,
	// not a carried-over 429.
	for i := 0; i < 2; i++ {
		if _, rec := loginAs(h, "op", "wrong"); rec.Code != 401 {
			t.Fatalf("failure after release %d: %d", i, rec.Code)
		}
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	a := testAuth()
	a.idle = time.Minute
	h := gated(a)
	a.now = frozen(0)
	tok, _ := loginAs(h, "op", "s3cret")
	if tok == "" {
		t.Fatal("login produced no token")
	}
	// Reading slides the idle window: staying just inside it forever still works.
	a.now = frozen(50 * time.Second)
	if rec := do(h, "GET", "/api/sessions", "", nil, sess(tok)); rec.Code != 200 {
		t.Fatalf("inside the idle window: %d", rec.Code)
	}
	a.now = frozen(90 * time.Second) // 40s since the last touch, under the minute
	if rec := do(h, "GET", "/api/sessions", "", nil, sess(tok)); rec.Code != 200 {
		t.Fatalf("sliding: %d", rec.Code)
	}
	// One silence past the window ends it; an old cookie cannot age back to life.
	a.now = frozen(4 * time.Minute)
	if rec := do(h, "GET", "/api/sessions", "", nil, sess(tok)); rec.Code != 401 {
		t.Fatalf("expired session: %d", rec.Code)
	}
}

func TestClientIPTrustBoundary(t *testing.T) {
	nets, err := ParseNets([]string{"10.0.0.0/8", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuth("u", "p", time.Hour, time.Hour, 5, false, nets, nil)

	newReq := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/api/sessions", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct{ remote, xff, want string }{
		// Untrusted peer: XFF is not consulted at all. Believing it without a
		// boundary is the bypass this function exists to prevent — every
		// rate-limit bucket would be self-chosen by the client.
		{"192.168.1.5:1", "1.2.3.4", "192.168.1.5"},
		// Trusted peer: right to left, the first address outside the trusted
		// networks is as close to the real client as the chain can prove.
		{"10.0.0.2:1", "1.2.3.4, 10.0.0.9", "1.2.3.4"},
		{"10.0.0.2:1", "1.2.3.4, 9.9.9.9, 10.0.0.7", "9.9.9.9"},
		// Trusted peer, no header: the peer address itself.
		{"10.0.0.2:1", "", "10.0.0.2"},
		// All-trusted chain (a home LAN behind a local proxy): the leftmost.
		{"10.0.0.2:1", "10.0.0.5, 10.0.0.6", "10.0.0.5"},
		// A bare IP configured is trusted exactly as a single address.
		{"127.0.0.1:1", "8.8.8.8", "8.8.8.8"},
	}
	for _, c := range cases {
		if got := a.clientIP(newReq(c.remote, c.xff)); got != c.want {
			t.Errorf("peer %s xff %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	// The default (no trustedProxies): XFF never wins, not even from loopback.
	plain := testAuth()
	if got := plain.clientIP(newReq("127.0.0.1:1", "1.2.3.4")); got != "127.0.0.1" {
		t.Errorf("without trustedProxies XFF must be ignored, got %s", got)
	}
}

func TestParseNets(t *testing.T) {
	if _, err := ParseNets([]string{"not-an-ip"}); err == nil {
		t.Fatal("garbage parsed")
	}
	if ns, err := ParseNets([]string{"::1"}); err != nil || len(ns) != 1 || ns[0].String() != "::1/128" {
		t.Fatalf("bare IPv6: %v %v", ns, err)
	}
	if ns, err := ParseNets([]string{"1.2.3.0/24", ""}); err != nil || len(ns) != 1 {
		t.Fatalf("empty entries: %v %v", ns, err)
	}
}

func TestLoginBodyShape(t *testing.T) {
	a := testAuth()
	h := gated(a)
	if rec := do(h, "POST", "/api/login", "not json", nil); rec.Code != 400 {
		t.Fatalf("malformed body: %d %s", rec.Code, rec.Body)
	}
	if _, rec := loginAs(h, "", ""); rec.Code != 401 {
		t.Fatalf("an empty pair is a wrong pair, charged like any other: %d", rec.Code)
	}
}

// frozen returns a clock offset from a shared base, so a whole test can move
// business time without touching the real wall clock the constant-time hold
// runs on.
func frozen(off time.Duration) func() time.Time {
	base := time.Now().Add(off)
	return func() time.Time { return base }
}
