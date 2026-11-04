package agent

// Which of the two proxy fields wins, and everywhere that answer has to reach: the dialer, the identity
// comparison that decides whether a connection is remade, the config checks, the PATCH merge and the log
// line. What an address means once it is chosen is internal/tlsconf's business and is tested there; these are
// the rules this package adds on top — and the reason the resolution happens on a copy is the same pricing
// bug that keeps turning up in this project (r33 §2.13): an answer the daemon supplied must not come back
// through GET /config looking like something the user wrote.

import (
	"context"
	"strings"
	"testing"

	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// TestProxyPrecedence is the whole rule in a table: the endpoint's answer, else the session's, else nothing
// said. Both levels exist so that a deployment with one proxy can state it once and the one odd endpoint can
// still say otherwise, and the second case is the one a future chaining proposal would have to keep working.
func TestProxyPrecedence(t *testing.T) {
	for _, tc := range []struct{ name, session, endpoint, want string }{
		{"neither says", "", "", ""},
		{"session only", "socks5://global:1080", "", "socks5://global:1080"},
		{"endpoint only", "", "socks5://own:1080", "socks5://own:1080"},
		{"both: the nearer answer wins", "socks5://global:1080", "http://own:3128", "http://own:3128"},
		{"the endpoint opts out of the session's", "socks5://global:1080", "direct", "direct"},
		{"whitespace is nothing said", "  ", "  ", ""},
	} {
		cfg := Config{
			Proxy: tc.session,
			LLM:   llm.Config{BaseURL: "https://x.invalid/v1", Proxy: tc.endpoint},
			MCP:   []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp", Proxy: tc.endpoint}},
		}
		if got := cfg.llmDial().Proxy; got != tc.want {
			t.Errorf("%s: the llm would dial through %q, want %q", tc.name, got, tc.want)
		}
		if got := cfg.mcpDial()[0].Proxy; got != tc.want {
			t.Errorf("%s: the mcp entry would dial through %q, want %q", tc.name, got, tc.want)
		}
		// The copy has to stay a copy. This is the assertion that would catch the config.json and the API
		// starting to report a proxy nobody wrote into that field.
		if cfg.LLM.Proxy != tc.endpoint || cfg.MCP[0].Proxy != tc.endpoint || cfg.Proxy != tc.session {
			t.Errorf("%s: resolving wrote into the stored config: proxy=%q llm.proxy=%q mcp[0].proxy=%q",
				tc.name, cfg.Proxy, cfg.LLM.Proxy, cfg.MCP[0].Proxy)
		}
	}
}

// TestSessionProxyIsNotTheStdioEntryAnswer: a stdio entry is started as a child process, so the session's
// proxy is not an answer to anything it was asked. Letting the fallback reach it would both refuse a config
// over a proxy its owner wrote elsewhere and restart a working child process every time that other field is
// PATCHed.
func TestSessionProxyIsNotTheStdioEntryAnswer(t *testing.T) {
	cfg := Config{
		Proxy: "socks5://global:1080",
		MCP:   []mcp.Config{{Name: "files", Command: "true"}},
	}
	if got := cfg.mcpDial()[0].Proxy; got != "" {
		t.Fatalf("a stdio entry was handed the session's proxy %q", got)
	}
	if cfg.mcpDial()[0].Identity() != (Config{MCP: []mcp.Config{{Name: "files", Command: "true"}}}).mcpDial()[0].Identity() {
		t.Fatal("a session-level proxy changed a stdio entry's identity: PATCHing it would restart the child process")
	}
	// While a proxy written on that entry itself is somebody's mistake, and the one they can fix.
	if err := validatePolicy(Config{MCP: []mcp.Config{{Name: "files", Command: "true", Proxy: "socks5://p:1080"}}}); err == nil {
		t.Fatal("a stdio entry accepted a proxy: there is no connection for it to route")
	} else if !strings.Contains(err.Error(), "no connection to route") {
		t.Fatalf("error %q does not say what was refused", err)
	}
}

// TestMergeDefaultsInheritsTheSessionAnswer: the daemon can state the proxy once, like the price tag and the
// timezone, and a session that says anything at all replaces it.
func TestMergeDefaultsInheritsTheSessionAnswer(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{Proxy: "socks5://daemon:1080"}, nil, nil)
	if got := m.MergeDefaults(Config{ID: "s"}).Proxy; got != "socks5://daemon:1080" {
		t.Fatalf("a session that says nothing about proxies must inherit the daemon's, got %q", got)
	}
	if got := m.MergeDefaults(Config{ID: "s", Proxy: "direct"}).Proxy; got != "direct" {
		t.Fatalf("the session's own answer must replace the daemon's, got %q", got)
	}
	// The endpoints are not filled in by inheritance: the fallback is applied at dial time, so patching the
	// session's proxy later still reaches every endpoint that never named one.
	got := m.MergeDefaults(Config{ID: "s", MCP: []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp"}}})
	if got.MCP[0].Proxy != "" {
		t.Fatalf("inheritance wrote the daemon's proxy into an endpoint that said nothing, got %q", got.MCP[0].Proxy)
	}
}

// TestValidationReadsTheResolvedPolicy: what is checked is what would be dialled, so a session-level proxy
// cannot smuggle an unusable address past a config save because no endpoint repeated it.
func TestValidationReadsTheResolvedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"the llm gets the session's typo", Config{Proxy: "127.0.0.1:1080", LLM: llm.Config{BaseURL: "https://x.invalid/v1"}}, "not a proxy address"},
		// A session-level answer that is wrong is wrong for every endpoint at once, and the walker names the
		// first one — the llm — which is actionable enough: the field to fix is the one they did not write on
		// an endpoint. An endpoint's own typo is therefore the case that has to name the entry.
		{"the session's typo is named at the first endpoint", Config{Proxy: "socks4://p:1080", MCP: []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp"}}}, "llm"},
		{"an entry's own typo is named at the entry", Config{MCP: []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp", Proxy: "socks4://p:1080"}}}, `mcp server "vr"`},
		{"a stdio entry beside a session proxy is fine", Config{Proxy: "socks5://p:1080", MCP: []mcp.Config{{Name: "files", Command: "true"}}}, ""},
		{"an http entry may name one", Config{MCP: []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp", Proxy: "socks5://p:1080"}}}, ""},
		{"and so may the llm", Config{LLM: llm.Config{BaseURL: "https://x.invalid/v1", Proxy: "direct"}}, ""},
	} {
		err := validatePolicy(tc.cfg)
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

// TestPatchCarriesTheProxyLikeAStoredString: what a PATCH does not say is not a change, at either level, and
// the two levels stay separate fields in the stored config — an endpoint that named a proxy keeps it when the
// session's is replaced, and an endpoint that named none keeps naming none.
func TestPatchCarriesTheProxyLikeAStoredString(t *testing.T) {
	ctx := context.Background()
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(ctx, Config{
		ID:    "sproxy",
		Proxy: "socks5://global:1080",
		LLM:   llm.Config{BaseURL: "https://x.invalid/v1", Proxy: "socks5://own:1080"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateConfig(ctx, Config{MaxTurns: 3}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config(); got.MaxTurns != 3 || got.Proxy != "socks5://global:1080" || got.LLM.Proxy != "socks5://own:1080" {
		t.Fatalf("an unrelated patch lost a proxy: proxy=%q llm.proxy=%q", got.Proxy, got.LLM.Proxy)
	}

	// The session-level field is the one that speaks for every endpoint without its own answer, so this is
	// how a deployment is moved onto or off a proxy in one line.
	if err := s.UpdateConfig(ctx, Config{Proxy: "http://new:3128"}); err != nil {
		t.Fatal(err)
	}
	got := s.Config()
	if got.Proxy != "http://new:3128" {
		t.Fatalf("session proxy = %q", got.Proxy)
	}
	if got.LLM.Proxy != "socks5://own:1080" {
		t.Fatalf("an endpoint that named a proxy lost it to the session's: %q", got.LLM.Proxy)
	}
	if d := got.llmDial(); d.Proxy != "socks5://own:1080" {
		t.Fatalf("the endpoint would dial through %q, want its own answer", d.Proxy)
	}

	// And an endpoint's own proxy follows the same rule as its caFile: a body that leaves it out changed
	// nothing about it.
	if err := s.UpdateConfig(ctx, Config{LLM: llm.Config{Model: "other"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().LLM; got.Proxy != "socks5://own:1080" || got.Model != "other" {
		t.Fatalf("after a patch naming only a model: proxy=%q model=%q", got.Proxy, got.Model)
	}
}

// TestProxyIsAnnouncedOnce: a session whose traffic leaves by a host nobody would guess from the endpoint URL
// is a deliberate deviation, and the log is where someone else finds it (D5). The credential is not.
func TestProxyIsAnnouncedOnce(t *testing.T) {
	logf, count := captureLog()
	m := NewManager(t.TempDir(), false, Config{}, nil, logf)
	ctx := context.Background()

	s, err := m.Create(ctx, Config{
		ID:  "sproxied",
		LLM: llm.Config{BaseURL: "https://x.invalid/v1", Proxy: "socks5://user:secret@proxy.internal:1080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := count("is reached through"); got != 1 {
		t.Fatalf("a session with a proxy said so %d times, want 1", got)
	}
	if got := count("secret"); got != 0 {
		t.Fatalf("a proxy credential reached the log %d times", got)
	}
	// What the log hides, the stored config keeps: the line is for reading and config.json is for dialling.
	if got := s.Config().LLM.Proxy; got != "socks5://user:secret@proxy.internal:1080" {
		t.Fatalf("the stored config no longer says what was configured: %q", got)
	}

	// The other answer is worth a line too: "no proxy, not even HTTP_PROXY" is as much a deviation as a
	// proxy is, and it is the one an operator on a machine that sets HTTP_PROXY everywhere would otherwise
	// never see.
	if _, err := m.Create(ctx, Config{ID: "sdirect", LLM: llm.Config{BaseURL: "https://y.invalid/v1", Proxy: "direct"}}); err != nil {
		t.Fatal(err)
	}
	if got := count("connected directly"); got != 1 {
		t.Fatalf(`"direct" was reported %d times, want 1`, got)
	}

	// A session whose endpoint names no proxy is the one the session-level field speaks for, and patching it
	// is therefore an endpoint change: the log restates the route, which is the only place an operator can see
	// that the endpoint they are looking at now goes somewhere else. (An endpoint that named a proxy of its own
	// is not restated, and rightly: patching that field did not move it.)
	sglobal, err := m.Create(ctx, Config{ID: "sglobal", LLM: llm.Config{BaseURL: "https://z.invalid/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := count("is reached through"); got != 1 {
		t.Fatalf("a session with no proxy in it was counted as proxied (%d lines)", got)
	}
	if err := sglobal.UpdateConfig(ctx, Config{Proxy: "socks5://global:1080"}); err != nil {
		t.Fatal(err)
	}
	if got := count("is reached through"); got != 2 {
		t.Fatalf("after patching the session's proxy it was said %d times, want 2", got)
	}

	// And a session with no proxy in it says nothing about proxies.
	quiet, quietCount := captureLog()
	mq := NewManager(t.TempDir(), false, Config{}, nil, quiet)
	if _, err := mq.Create(ctx, Config{ID: "squiet", LLM: llm.Config{BaseURL: "https://x.invalid/v1"}}); err != nil {
		t.Fatal(err)
	}
	if got := quietCount("proxy") + quietCount("directly"); got != 0 {
		t.Fatalf("a default session produced %d lines about proxies", got)
	}
}

// TestProxyChangeRemakesTheConnection: an MCP server's proxy is part of which connection it has, and so is
// the session's answer for a server that named none — otherwise PATCHing the proxy would report success,
// log the new route, and keep talking through the old one.
func TestProxyChangeRemakesTheConnection(t *testing.T) {
	same := []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp"}}
	if !sameMCP(Config{MCP: same}.mcpDial(), Config{MCP: same}.mcpDial()) {
		t.Fatal("the same two entries are not the same connection")
	}
	if sameMCP(Config{Proxy: "socks5://a:1080", MCP: same}.mcpDial(), Config{MCP: same}.mcpDial()) {
		t.Fatal("giving the session a proxy left its endpoints' identity alone: no reconnect, and the old route stays")
	}
	moved := []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp", Proxy: "socks5://b:1080"}}
	if sameMCP(Config{MCP: moved}.mcpDial(), Config{MCP: same}.mcpDial()) {
		t.Fatal("an endpoint's own proxy left its identity alone")
	}
	// Tool policy is not part of an identity and a proxy is: one is a filter over the same connection.
	if !sameMCP(Config{MCP: []mcp.Config{{Name: "vr", Type: "http", URL: "https://x.invalid/mcp", DisabledTools: []string{"x"}}}}.mcpDial(),
		Config{MCP: same}.mcpDial()) {
		t.Fatal("switching a tool off must not take the connection down with it")
	}
}
