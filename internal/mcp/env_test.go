package mcp

import (
	"runtime"
	"strings"
	"testing"
)

// The parent environment the tests resolve names against: what a bareloopd that loaded a
// real config.json would carry — a couple of base-set names, and two provider keys whose
// whole reason to be in the environment is to never reach a file.
func testParent() []string {
	return []string{
		"PATH=/usr/bin",
		"HOME=/home/operator",
		"TERM=xterm-256color",
		"OPENAI_API_KEY=sk-secret-never-inherits",
		"MY_MCP_TOKEN=token-from-daemon-env",
		"Path=W:\\mispelled\\but\\windows\\does\\this",
	}
}

var testBase = []string{"PATH", "HOME", "TERM", "GONE"} // GONE: not in the parent

func lookup(t *testing.T, env []string, key string) (string, bool) {
	t.Helper()
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], key) {
			return kv[i+1:], true
		}
	}
	return "", false
}

func TestBuildEnvBaseSetOnly(t *testing.T) {
	out := buildEnvWith(testParent(), testBase, StdioConfig{})
	if got, ok := lookup(t, out, "PATH"); !ok || got != "/usr/bin" {
		t.Errorf("PATH = %q, %v; want the parent's value", got, ok)
	}
	if _, ok := lookup(t, out, "GONE"); ok {
		t.Error("a base name the parent lacks was passed anyway; there is no value behind it")
	}
	// The point of the whole file: the daemon's secrets are not in here.
	for _, kv := range out {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Fatalf("provider key reached the child environment: %q", out)
		}
	}
	if _, ok := lookup(t, out, "MY_MCP_TOKEN"); ok {
		t.Error("an unmentioned name reached the child; passthrough must be named")
	}
}

func TestBuildEnvPassthrough(t *testing.T) {
	out := buildEnvWith(testParent(), testBase, StdioConfig{
		EnvPassthrough: []string{"MY_MCP_TOKEN", "GONE", "", " SPACEY ", "PATH"},
	})
	if got, ok := lookup(t, out, "MY_MCP_TOKEN"); !ok || got != "token-from-daemon-env" {
		t.Errorf("passthrough MY_MCP_TOKEN = %q, %v; want the daemon's value", got, ok)
	}
	// PATH now appears twice (base + passthrough) with the same value: os/exec's documented
	// last-occurrence rule is what the child sees, and both spellings resolve identically.
	n := 0
	for _, kv := range out {
		if strings.EqualFold(kv, "PATH=/usr/bin") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("PATH occurrences = %d, want 2 (base and passthrough); duplicates are exec's to resolve", n)
	}
}

// A passthrough name finds its line first by exact spelling, then case-insensitively, and
// the parent's own spelling is what the child receives. testParent carries Windows' favourite
// trick — PATH and Path in one environment, disagreeing — so "path" and "Path" can be shown
// to resolve to different lines.
func TestBuildEnvPassthroughSpelling(t *testing.T) {
	out := buildEnvWith(testParent(), nil, StdioConfig{EnvPassthrough: []string{"path", "Path", "my_mcp_token"}})
	want := []string{
		"PATH=/usr/bin", // "path": no exact spelling, fold match takes the first line
		"Path=W:\\mispelled\\but\\windows\\does\\this", // "Path": exact wins over the PATH line
		"MY_MCP_TOKEN=token-from-daemon-env",           // "my_mcp_token": fold match
	}
	if len(out) != len(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, out[i], want[i])
		}
	}
	if _, ok := lookup(t, out, "HOME"); ok {
		t.Error("a name that was not asked for slipped in")
	}
}

func TestBuildEnvEnvOverrides(t *testing.T) {
	// env lines land last, so a same-name literal is what the child ends up with (exec keeps
	// the last of a duplicate key) — and free-form names that no layer mentioned ride along.
	out := buildEnvWith(testParent(), testBase, StdioConfig{
		Env: []string{"HOME=/fake/home", "SERVER_MODE=staging", "NOEQUALS"},
	})
	if out[len(out)-3] != "HOME=/fake/home" || out[len(out)-2] != "SERVER_MODE=staging" || out[len(out)-1] != "NOEQUALS" {
		t.Errorf("env lines are not appended last as-is: %v", out)
	}
	// The earlier HOME is still in the slice (that is exec's business, not ours); what is
	// ours is that both are present with the literal last.
	if out[0] == "HOME=/fake/home" {
		t.Errorf("literal HOME arrived first: %v", out)
	}
}

// A name containing '=' is a value trying to be a name; skipping it beats passing a line
// whose key is "MY=THING".
func TestBuildEnvIgnoresNonNames(t *testing.T) {
	out := buildEnvWith(testParent(), nil, StdioConfig{EnvPassthrough: []string{"MY_MCP_TOKEN=x", "="}})
	if len(out) != 0 {
		t.Errorf("non-names passed: %v", out)
	}
}

func TestDefaultInheritedEnvVarsIsSafe(t *testing.T) {
	base := defaultInheritedEnvVars() // bare names, not K=V lines
	baseHas := func(name string) bool {
		for _, n := range base {
			if n == name {
				return true
			}
		}
		return false
	}
	if len(base) == 0 {
		t.Fatal("empty base set")
	}
	if !baseHas("PATH") {
		t.Error("the base set lacks PATH; nothing starts without it")
	}
	for _, name := range base {
		upper := strings.ToUpper(name)
		for _, bad := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "AUTH", "CREDENTIAL", "PROXY"} {
			if strings.Contains(upper, bad) {
				t.Errorf("%q looks secret-shaped; the base set must be launch-critical only", name)
			}
		}
	}
	if runtime.GOOS != "windows" {
		if !baseHas("HOME") {
			t.Error("unix base set lacks HOME")
		}
	}
}

// EnvBase:false is the strict spelling: not even the base set — an environment of exactly
// the names the entry carries, which is what an operator who counts HOME as information
// asked for. Unset and explicit true must behave the same (absence is "yes", like every
// other *bool policy field here).
func TestEnvBaseOff(t *testing.T) {
	onTrue := StdioConfig{EnvBase: ptr(true)}
	off := StdioConfig{EnvBase: ptr(false)}
	out := buildEnv(testParent(), off)
	if len(out) != 0 {
		t.Errorf("strict mode passed %v; want an empty environment", out)
	}
	if got := buildEnv(testParent(), StdioConfig{}); len(got) == 0 {
		t.Error("unset EnvBase dropped the base set; unset means yes")
	}
	if got := buildEnv(testParent(), onTrue); len(got) != len(buildEnv(testParent(), StdioConfig{})) {
		t.Error("explicit true behaves differently from unset")
	}
	// strict + named is still exactly the named:
	out = buildEnv(testParent(), StdioConfig{EnvBase: ptr(false), EnvPassthrough: []string{"MY_MCP_TOKEN"}, Env: []string{"A=1"}})
	if len(out) != 2 || out[0] != "MY_MCP_TOKEN=token-from-daemon-env" || out[1] != "A=1" {
		t.Errorf("strict+named = %v", out)
	}
}

func TestIdentityIncludesEnvBase(t *testing.T) {
	unset := Config{Name: "s", Command: "c"}
	on := Config{Name: "s", Command: "c", EnvBase: ptr(true)}
	off := Config{Name: "s", Command: "c", EnvBase: ptr(false)}
	if on.Identity() == off.Identity() {
		t.Error("identity blind to envBase")
	}
	// unset and explicit false behave identically in the child, so they must carry the same
	// identity: an editor that drops the key must not cause a pointless reconnect.
	if unset.Identity() != off.Identity() {
		t.Error("unset envBase has a different identity than explicit false; the child cannot tell")
	}
}

func ptr(b bool) *bool { return &b }

// EnvPassthrough decides the process's environment, so it decides its Identity: changing a
// passthrough name must reconnect, exactly like changing an env value.
func TestIdentityIncludesPassthrough(t *testing.T) {
	a := Config{Name: "s", Command: "c", EnvPassthrough: []string{"ONE"}}
	b := Config{Name: "s", Command: "c", EnvPassthrough: []string{"TWO"}}
	if a.Identity() == b.Identity() {
		t.Error("identity blind to envPassthrough")
	}
	c := Config{Name: "s", Command: "c"}
	if a.Identity() == c.Identity() {
		t.Error("identity blind to a new passthrough")
	}
}
