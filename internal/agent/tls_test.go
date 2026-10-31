package agent

// Where a certificate policy is refused and where it is announced: the same two places the config is
// written — Create, PATCH, and a session opening. The rules themselves are in tls.go and internal/tlsconf;
// what is pinned here is that they run at those points, that a refused config leaves nothing behind, and
// that a PATCH can neither lose a stored policy it never mentioned nor keep a contradiction that only the
// merged result forms.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

func yesNo(b bool) *bool { return &b }

// caFileOnDisk writes a self-signed CA — which is what a caFile holds — because these tests are about
// *when* a policy is checked. What a policy does to a handshake is proven with a real one in
// internal/tlsconf.
func caFileOnDisk(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bareloop test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// captureLog is the manager's logf plus a count of the lines carrying a substring, which is how the D5
// claims (one line, at the moment the policy starts applying) are checked.
func captureLog() (func(string, ...any), func(string) int) {
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return logf, func(want string) int {
		mu.Lock()
		defer mu.Unlock()
		var n int
		for _, l := range lines {
			if strings.Contains(l, want) {
				n++
			}
		}
		return n
	}
}

// TestValidateRefusesWhatCannotMeanAnything is the rule table, including that a stdio server is refused a
// policy rather than storing one that can never apply.
func TestValidateRefusesWhatCannotMeanAnything(t *testing.T) {
	ca := caFileOnDisk(t)
	missing := filepath.Join(t.TempDir(), "nope.pem")

	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"nothing set", Config{}, ""},
		{"insecure llm", Config{LLM: llm.Config{BaseURL: "https://x/v1", InsecureTLS: yesNo(true)}}, ""},
		{"caFile llm", Config{LLM: llm.Config{BaseURL: "https://x/v1", CAFile: ca}}, ""},
		{"both", Config{LLM: llm.Config{BaseURL: "https://x/v1", InsecureTLS: yesNo(true), CAFile: ca}}, "pick one"},
		{"unreadable caFile", Config{LLM: llm.Config{BaseURL: "https://x/v1", CAFile: missing}}, "no such file"},
		{
			"stdio with a caFile",
			Config{MCP: []mcp.Config{{Name: "vr", Command: "true", CAFile: ca}}},
			`stdio server`,
		},
		{
			"stdio asked not to verify",
			Config{MCP: []mcp.Config{{Name: "vr", Command: "true", InsecureTLS: yesNo(true)}}},
			`mcp server "vr"`,
		},
		{
			"http entry is allowed a policy",
			Config{MCP: []mcp.Config{{Name: "vr", Type: "http", URL: "https://x/mcp", CAFile: ca}}},
			"",
		},
	} {
		err := validateTLS(tc.cfg)
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

// TestRefusedConfigLeavesNothingBehind: the check runs before the directory exists. A session whose config
// was rejected half-way would otherwise look like a session with no TLS policy at all — the failure
// invisible exactly where the operator is looking.
func TestRefusedConfigLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	ca := caFileOnDisk(t)
	m := NewManager(root, false, Config{}, nil, nil)
	ctx := context.Background()

	for _, id := range []string{"sboth", "sstdio"} {
		cfg := Config{ID: id, LLM: llm.Config{BaseURL: "https://x.invalid/v1"}}
		if id == "sboth" {
			cfg.LLM.InsecureTLS, cfg.LLM.CAFile = yesNo(true), ca
		} else {
			cfg.MCP = []mcp.Config{{Name: "vr", Command: "true", InsecureTLS: yesNo(true)}}
		}
		if _, err := m.Create(ctx, cfg); err == nil {
			t.Fatalf("%s: Create accepted it", id)
		}
		if _, err := os.Stat(filepath.Join(root, id)); !os.IsNotExist(err) {
			t.Errorf("%s: a refused config left data/%s behind (%v)", id, id, err)
		}
	}

	// A policy that is fine is stored as written, and a session opened with it is the point of the test:
	// nothing about it may be dropped on the way to disk.
	s, err := m.Create(ctx, Config{ID: "sok", LLM: llm.Config{BaseURL: "https://x.invalid/v1", CAFile: ca}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config().LLM.CAFile; got != ca {
		t.Fatalf("stored caFile = %q, want %q", got, ca)
	}
}

// TestDaemonPolicyIsInheritedAsAPair: trust belongs to the endpoint, so a daemon whose default endpoint is a
// private https server says so once. It is inherited as a pair because a create body cannot say "no caFile"
// (empty means inherit, as everywhere else) — so a session that asks for anything replaces the answer
// instead of colliding with it.
func TestDaemonPolicyIsInheritedAsAPair(t *testing.T) {
	ca := caFileOnDisk(t)
	m := NewManager(t.TempDir(), false, Config{
		LLM: llm.Config{BaseURL: "https://private.invalid/v1", CAFile: ca},
	}, nil, nil)
	ctx := context.Background()

	if got := m.MergeDefaults(Config{ID: "s"}); got.LLM.CAFile != ca {
		t.Fatalf("a session that says nothing about certificates must inherit the daemon's, got %q", got.LLM.CAFile)
	}
	got := m.MergeDefaults(Config{ID: "s", LLM: llm.Config{InsecureTLS: yesNo(true)}})
	if got.LLM.CAFile != "" || got.LLM.InsecureTLS == nil || !*got.LLM.InsecureTLS {
		t.Fatalf("the session's own answer must replace the daemon's pair, got caFile=%q insecure=%v", got.LLM.CAFile, got.LLM.InsecureTLS)
	}
	// And that session must be creatable: inheriting the daemon's file beside an explicit insecureTLS
	// would be a contradiction its caller has no way to express its way out of.
	if _, err := m.Create(ctx, Config{ID: "sinherit", LLM: llm.Config{InsecureTLS: yesNo(true)}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// TestPatchMergesThePairNotTheFields is the three-state promise over the API, in the order an operator would
// actually use it: a policy survives a patch that never mentions it, asking not to verify is the whole
// answer (a string cannot spell "no file"), and a contradiction that only the merge forms is refused
// without touching the stored config.
func TestPatchMergesThePairNotTheFields(t *testing.T) {
	ca := caFileOnDisk(t)
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	ctx := context.Background()
	s, err := m.Create(ctx, Config{ID: "spatch", LLM: llm.Config{BaseURL: "https://x.invalid/v1", CAFile: ca}})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(what Config) error { return s.UpdateConfig(ctx, what) }

	// 1. An unrelated patch keeps the policy — and MaxTurns is checked too, so "kept" cannot mean "the
	// patch was ignored altogether".
	if err := patch(Config{MaxTurns: 3}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config(); got.MaxTurns != 3 || got.LLM.CAFile != ca {
		t.Fatalf("after an unrelated patch: maxTurns=%d caFile=%q", got.MaxTurns, got.LLM.CAFile)
	}

	// 2. Asking not to verify replaces the stored caFile.
	if err := patch(Config{LLM: llm.Config{InsecureTLS: yesNo(true)}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().LLM; got.CAFile != "" || !got.TLS().Insecure() {
		t.Fatalf("after insecureTLS:true: caFile=%q insecure=%v", got.CAFile, got.InsecureTLS)
	}

	// 3. An explicit false, with nothing else in the body, says "verify" and keeps nothing it was not told
	// to change: the pair is now settled, so there is nothing left to assert about the file.
	if err := patch(Config{LLM: llm.Config{InsecureTLS: yesNo(false)}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().LLM; got.InsecureTLS == nil || *got.InsecureTLS {
		t.Fatalf("an explicit false must be stored as an explicit false, got %v", got.InsecureTLS)
	}

	// 4. Re-pin a file, then let an explicit false sit beside it: both say verify, so this is not a
	// contradiction and the file must survive.
	if err := patch(Config{LLM: llm.Config{CAFile: ca}}); err != nil {
		t.Fatal(err)
	}
	if err := patch(Config{LLM: llm.Config{InsecureTLS: yesNo(false)}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().LLM; got.CAFile != ca {
		t.Fatalf("a stored caFile must outlive a patch that only spells out false, got %q", got.CAFile)
	}

	// 5. Now the case that can only be caught after the merge: the body carries a file, the session
	// already carries insecureTLS, and neither half is a conflict on its own.
	if err := patch(Config{LLM: llm.Config{InsecureTLS: yesNo(true), CAFile: ""}}); err != nil {
		t.Fatal(err)
	}
	// The stored insecureTLS is what the next body inherits, so this body — a file, and no word about
	// verification — merges into both at once.
	err = patch(Config{LLM: llm.Config{CAFile: ca}})
	if err == nil {
		t.Fatal("a body naming a caFile must not combine with a stored insecureTLS into both at once")
	}
	if !strings.Contains(err.Error(), "pick one") {
		t.Fatalf("error %q is not the contradiction message", err)
	}
	if got := s.Config(); got.LLM.CAFile != "" || !got.LLM.TLS().Insecure() || got.MaxTurns != 3 {
		t.Fatalf("the refused patch changed the config: %+v", got.LLM)
	}
}

// TestPolicyIsSaidOutLoud is D5 again: what these fields change is invisible from the session — a request
// being impersonated looks exactly like one that is not — so the daemon states the deviation, once, at the
// point it starts applying, and says so when a field reaches nothing at all.
func TestPolicyIsSaidOutLoud(t *testing.T) {
	logf, count := captureLog()
	m := NewManager(t.TempDir(), false, Config{}, nil, logf)
	ctx := context.Background()

	s, err := m.Create(ctx, Config{ID: "sinsecure", LLM: llm.Config{BaseURL: "https://self.invalid/v1", InsecureTLS: yesNo(true)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := count("insecureTLS is on"); got != 1 {
		t.Fatalf("an open session with insecureTLS said it %d times, want 1", got)
	}

	// A policy on an endpoint that is not https is inert, and says so.
	if _, err := m.Create(ctx, Config{ID: "splains", LLM: llm.Config{BaseURL: "http://plain.invalid/v1", CAFile: caFileOnDisk(t)}}); err != nil {
		t.Fatal(err)
	}
	if got := count("not https"); got != 1 {
		t.Fatalf("a caFile on a plain http endpoint was reported %d times, want 1", got)
	}

	// A patch that changes the endpoint restates the policy: the operator needs to see it is still in
	// force after the endpoint it applies to moved.
	if err := s.UpdateConfig(ctx, Config{LLM: llm.Config{BaseURL: "https://other.invalid/v1", InsecureTLS: yesNo(true)}}); err != nil {
		t.Fatal(err)
	}
	if got := count("insecureTLS is on"); got != 2 {
		t.Fatalf("after changing the endpoint it was said %d times, want 2", got)
	}

	// And a session with no policy says nothing at all about certificates.
	quiet, quietCount := captureLog()
	mq := NewManager(t.TempDir(), false, Config{}, nil, quiet)
	if _, err := mq.Create(ctx, Config{ID: "squiet", LLM: llm.Config{BaseURL: "https://x.invalid/v1"}}); err != nil {
		t.Fatal(err)
	}
	if got := quietCount("insecureTLS") + quietCount("caFile"); got != 0 {
		t.Fatalf("a default session produced %d lines about certificates", got)
	}
}
