package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// The lock answers one question — "may a session decide which MCP servers this machine
// runs?" — with a no that costs the everyday nothing: toggling a pre-registered server and
// its tool policy keep working, and every config already on disk when the lock went up is
// left exactly as it was. The door that still opens is config.json with the daemon stopped.

func vrServer() mcp.Config {
	return mcp.Config{Name: "vr", Type: "http", URL: "https://x.invalid/mcp"}
}

func lockDefaults() Config {
	return Config{
		LLM: llm.Config{BaseURL: "https://x.invalid/v1"},
		MCP: []mcp.Config{vrServer()},
	}
}

func lockedManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), false, lockDefaults(), nil, nil)
	m.SetMCPLock(true)
	return m
}

func TestMCPLockPatch(t *testing.T) {
	ctx := context.Background()
	m := lockedManager(t)
	s, err := m.CreateFromConfig(ctx, Config{ID: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	// The everyday answers first. A list a body never mentioned is not a change — the
	// settings page re-saves the whole config it read, and that has to keep landing.
	if err := s.UpdateConfig(ctx, Config{MaxTurns: 3}); err != nil {
		t.Fatalf("a patch that says nothing about MCP: %v", err)
	}
	if err := s.UpdateConfig(ctx, Config{Name: "renamed", MCP: []mcp.Config{vrServer()}}); err != nil {
		t.Fatalf("re-saving the stored list verbatim: %v", err)
	}
	// The enabled bit is the one thing in the list the lock exists to leave free, and tool
	// policy was never part of the comparison to begin with.
	off := vrServer()
	off.Disabled = true
	if err := s.UpdateConfig(ctx, Config{MCP: []mcp.Config{off}}); err != nil {
		t.Fatalf("disabling a pre-registered server: %v", err)
	}
	toggled := vrServer()
	toggled.DisabledTools = []string{"x"}
	if err := s.UpdateConfig(ctx, Config{MCP: []mcp.Config{toggled}}); err != nil {
		t.Fatalf("tool policy: %v", err)
	}
	if got := s.Config(); got.Name != "renamed" || got.MaxTurns != 3 || len(got.MCP) != 1 || got.MCP[0].Disabled {
		t.Fatalf("the everyday patches did not land as asked: %+v", got.MCP)
	}
	// The refusals: every way a body could put a different server set on the session.
	evil := mcp.Config{Name: "evil", Command: "sh", Args: []string{"-c", "curl evil"}}
	moved := vrServer()
	moved.URL = "https://evil.invalid/mcp"
	env := vrServer()
	env.Env = []string{"VROOT=1"}
	for _, tc := range []struct {
		what string
		m    []mcp.Config
	}{
		{"a second server", []mcp.Config{vrServer(), evil}},
		{"an entry swapped out", []mcp.Config{evil}},
		{"a moved url", []mcp.Config{moved}},
		{"an env line", []mcp.Config{env}},
		{"every server gone", []mcp.Config{}},
	} {
		if err := s.UpdateConfig(ctx, Config{MCP: tc.m}); !errors.Is(err, ErrMCPLocked) {
			t.Errorf("%s: err = %v, want ErrMCPLocked", tc.what, err)
		}
	}
	// And every refusal changed nothing: the session still carries the one server, enabled.
	got := s.Config()
	if len(got.MCP) != 1 || got.MCP[0].Name != "vr" || got.MCP[0].Disabled || got.MCP[0].URL != "https://x.invalid/mcp" {
		t.Fatalf("a refused patch moved the MCP list anyway: %+v", got.MCP)
	}
}

// The list is what the tool registry walks, so a reordering is a rewrite of it — and a
// rewrite is what is locked. Two entries are needed to say that, and toggling one of them
// has to stay free all the same.
func TestMCPLockPatchReorderingIsARewrite(t *testing.T) {
	ctx := context.Background()
	m := NewManager(t.TempDir(), false, lockDefaults(), nil, nil)
	a := vrServer()
	b := vrServer()
	b.Name, b.URL = "vr2", "https://y.invalid/mcp"
	s, err := m.Create(ctx, Config{ID: "pair", MCP: []mcp.Config{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	m.SetMCPLock(true)
	if err := s.UpdateConfig(ctx, Config{MCP: []mcp.Config{b, a}}); !errors.Is(err, ErrMCPLocked) {
		t.Fatalf("reordering the list: %v, want ErrMCPLocked", err)
	}
	flip := []mcp.Config{a, b}
	flip[1].Disabled = true
	if err := s.UpdateConfig(ctx, Config{MCP: flip}); err != nil {
		t.Fatalf("toggling one of two locked servers: %v", err)
	}
}

func TestMCPLockCreate(t *testing.T) {
	ctx := context.Background()
	m := lockedManager(t)
	// A body that says nothing gets the defaults, exactly as an unlocked daemon does.
	s, err := m.Create(ctx, Config{ID: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Config().MCP; len(got) != 1 || got[0].Name != "vr" {
		t.Fatalf("a silent create did not get the defaults: %+v", got)
	}
	// The defaults with the enabled bit moved are still the defaults.
	toggled := vrServer()
	toggled.Disabled = true
	if _, err := m.Create(ctx, Config{ID: "toggled", MCP: []mcp.Config{toggled}}); err != nil {
		t.Fatalf("creating with the defaults, one disabled: %v", err)
	}
	evil := mcp.Config{Name: "evil", Command: "sh", Args: []string{"-c", "id"}}
	if _, err := m.Create(ctx, Config{ID: "evil", MCP: []mcp.Config{evil}}); !errors.Is(err, ErrMCPLocked) {
		t.Fatalf("create with a foreign server: %v, want ErrMCPLocked", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "evil")); !os.IsNotExist(err) {
		t.Fatal("the refused create left a directory behind")
	}
	// "no servers for this one" is a list too, and under the lock the answer is the daemon's.
	if _, err := m.Create(ctx, Config{ID: "none", MCP: []mcp.Config{}}); !errors.Is(err, ErrMCPLocked) {
		t.Fatalf("create with an empty list: %v, want ErrMCPLocked", err)
	}
	// config.json is the trust root the lock itself lives in: its startup lines never answer
	// to it, whatever list they carry.
	legacy := []mcp.Config{{Name: "old", Type: "http", URL: "https://old.invalid/mcp"}}
	if _, err := m.CreateFromConfig(ctx, Config{ID: "legacy", MCP: legacy}); err != nil {
		t.Fatalf("CreateFromConfig with a foreign list: %v", err)
	}
	// An unlocked manager behaves exactly as it always did.
	m2 := NewManager(t.TempDir(), false, lockDefaults(), nil, nil)
	if _, err := m2.Create(ctx, Config{ID: "free", MCP: legacy}); err != nil {
		t.Fatalf("unlocked create with a foreign list: %v", err)
	}
}

// A fork carries the source's servers because the copy introduces no server the daemon did
// not already host — the one internal Create the lock deliberately lets through, which is
// also what keeps a session predating the lock forkable.
func TestMCPLockForkCarriesTheSource(t *testing.T) {
	ctx := context.Background()
	m := NewManager(t.TempDir(), false, lockDefaults(), nil, nil)
	forkSrc(t, m, "src", shoppingTree()[:2], "s2")
	legacy := []mcp.Config{{Name: "old", Type: "http", URL: "https://old.invalid/mcp"}}
	s, err := m.Get(ctx, "src")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateConfig(ctx, Config{MCP: legacy}); err != nil { // legal before the lock
		t.Fatal(err)
	}
	m.SetMCPLock(true)
	if _, _, err := m.Fork(ctx, "src", ForkOptions{ID: "dst"}); err != nil {
		t.Fatalf("fork of a legacy server set: %v", err)
	}
	dst, err := m.Get(ctx, "dst")
	if err != nil {
		t.Fatal(err)
	}
	if got := dst.Config().MCP; len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("the copy lost the source's servers: %+v", got)
	}
	// The exemption is for the daemon's own copy: the same list handed in from outside is
	// still a new set of servers as far as the lock is concerned.
	if _, err := m.Create(ctx, Config{ID: "copycat", MCP: legacy}); !errors.Is(err, ErrMCPLocked) {
		t.Fatalf("create with the fork's list: %v, want ErrMCPLocked", err)
	}
}

// The lock says itself on the startup log exactly once — an operator who later wonders why
// a PATCH came back 403 has one line to find — and turning it off says nothing.
func TestMCPLockAnnounced(t *testing.T) {
	logf, count := captureLog()
	m := NewManager(t.TempDir(), false, lockDefaults(), nil, logf)
	m.SetMCPLock(true)
	if got := count("mcp lock on"); got != 1 {
		t.Fatalf("the lock announced itself %d times, want 1", got)
	}
	m.SetMCPLock(false)
	if got := count("mcp lock on"); got != 1 {
		t.Fatalf("turning the lock off spoke: %d lines", got)
	}
	quiet, quietCount := captureLog()
	mq := NewManager(t.TempDir(), false, lockDefaults(), nil, quiet)
	if got := quietCount("mcp lock"); got != 0 {
		t.Fatalf("an unlocked daemon said something about the lock: %d lines", got)
	}
	_ = mq
}
