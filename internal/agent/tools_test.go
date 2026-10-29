package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"bareloop/internal/mcp"
)

// toolRegistry builds a registry that has already listed two servers, without needing an MCP
// server: the policy machinery works on the cached tools/list results, so that cache is all a
// test has to fake.
//
// Both servers offer fs_read, which is deliberate: it is the situation the naming rules exist for,
// so most tests here are also quietly asserting what a collision publishes.
func toolRegistry() *registry { return toolRegistryLogged(nil) }

func toolRegistryLogged(logf func(string, ...any)) *registry {
	r := newRegistry(nil, logf, false)
	r.last["beta"] = []mcp.Tool{
		{Name: "fs_read", Description: "read a file"},
		{Name: "fs_write", Description: "write a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
	}
	r.last["alpha"] = []mcp.Tool{
		{Name: "fs_read", Description: "read the other file"},
		{Name: "search", Description: "grep"},
	}
	r.status["beta"] = ServerStatus{Name: "beta", Connected: true}
	r.status["alpha"] = ServerStatus{Name: "alpha", Connected: true}
	r.SetPolicy([]mcp.Config{{Name: "beta"}, {Name: "alpha"}})
	return r
}

func visibleNames(r *registry) []string {
	tools := r.Tools()
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// defaultNames is what the fixture publishes with no policy at all: tools of the alphabetically
// first server keep their own names, and the tool that collides with one is published under
// <server>_<tool>.
var defaultNames = []string{"fs_read", "search", "beta_fs_read", "fs_write"}

// TestPublishedNamesHaveNoPrefix is the default a user gets without writing anything: no namespace,
// the tool's own name, because fs_read already says what it is.
func TestPublishedNamesHaveNoPrefix(t *testing.T) {
	r := newRegistry(nil, nil, false)
	r.last["files"] = []mcp.Tool{{Name: "fs_read"}, {Name: "fs_write"}}
	r.status["files"] = ServerStatus{Name: "files", Connected: true}
	r.SetPolicy([]mcp.Config{{Name: "files"}})
	if got := visibleNames(r); !equalNames(got, []string{"fs_read", "fs_write"}) {
		t.Fatalf("no prefix configured: got %v, want [fs_read fs_write]", got)
	}
}

// TestToolPolicyFiltersPublished: a switched-off tool is absent from the published array AND
// unresolvable by name, which is what makes "hidden" mean "the model cannot call it" without a
// second permission layer.
func TestToolPolicyFiltersPublished(t *testing.T) {
	r := toolRegistry()
	if before := visibleNames(r); !equalNames(before, defaultNames) {
		t.Fatalf("default policy: got %v, want %v", before, defaultNames)
	}

	r.SetPolicy([]mcp.Config{{Name: "beta", DisabledTools: []string{"fs_write"}}, {Name: "alpha"}})
	got := visibleNames(r)
	if !equalNames(got, []string{"fs_read", "search", "beta_fs_read"}) {
		t.Fatalf("after switching fs_write off: got %v", got)
	}
	if _, ok := r.resolve("fs_write"); ok {
		t.Fatal("a switched-off tool must not resolve")
	}
	if _, ok := r.resolve("beta_fs_read"); !ok {
		t.Fatal("a tool that is still on must resolve")
	}
}

// TestToolPolicyGlob: patterns are matched against the tool's own name, so every tool a server
// can add later is already covered by "fs_*".
func TestToolPolicyGlob(t *testing.T) {
	r := newRegistry(nil, nil, false)
	r.last["s"] = []mcp.Tool{{Name: "fs_read"}, {Name: "fs_write"}, {Name: "patch"}}
	r.status["s"] = ServerStatus{Name: "s", Connected: true}
	r.SetPolicy([]mcp.Config{{Name: "s", DisabledTools: []string{"fs_*"}}})
	if got := visibleNames(r); !equalNames(got, []string{"patch"}) {
		t.Fatalf("glob: got %v, want [patch]", got)
	}
}

// TestToolNamesAreStableAcrossToggles is the whole point of never reusing a name: switching a
// tool off and on again must leave every other tool's name (and therefore the rendered prompt
// prefix) byte-identical, and must put the tool back under the name old transcripts quote.
func TestToolNamesAreStableAcrossToggles(t *testing.T) {
	r := toolRegistry()
	r.SetPolicy([]mcp.Config{{Name: "beta", DisabledTools: []string{"fs_read"}}, {Name: "alpha"}})
	if off := visibleNames(r); !equalNames(off, []string{"fs_read", "search", "fs_write"}) {
		t.Fatalf("with beta/fs_read off: got %v", off)
	}
	r.SetPolicy([]mcp.Config{{Name: "beta"}, {Name: "alpha"}})
	if got := visibleNames(r); !equalNames(got, defaultNames) {
		t.Fatalf("after switching it back on: got %v (names must not drift)", got)
	}
}

// TestCollidingToolIsPublishedByServerAndLogged: with no prefix two servers offering fs_read want
// the same name. The first one keeps it — renaming it every time a server is added would move the
// KV prefix of every session that has history — and the second is published as <server>_<tool>
// rather than fs_read_2, because a name has to be readable to be usable. The user is told.
func TestCollidingToolIsPublishedByServerAndLogged(t *testing.T) {
	var logged []string
	r := toolRegistryLogged(func(format string, args ...any) {
		logged = append(logged, strings.TrimSpace(fmt.Sprintf(format, args...)))
	})
	if got := visibleNames(r); !equalNames(got, defaultNames) {
		t.Fatalf("collision: got %v, want %v", got, defaultNames)
	}
	if _, ok := r.resolve("fs_read_2"); ok {
		t.Fatal("a numbered fallback name must not be used when <server>_<tool> is free")
	}
	ref, ok := r.resolve("beta_fs_read")
	if !ok || ref.Server != "beta" || ref.Tool != "fs_read" {
		t.Fatalf("the colliding tool must resolve to itself, got %v %v", ref, ok)
	}
	var found bool
	for _, l := range logged {
		if strings.Contains(l, "collision") && strings.Contains(l, "beta_fs_read") && strings.Contains(l, "fs_read") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a collision that renames a tool must be logged, got %v", logged)
	}
}

// TestToolPrefixRenamesAndKeepsAliases: the prefix is how two servers offering tools of the same
// name are told apart on purpose, and changing it must not orphan a transcript written under the
// old names.
func TestToolPrefixRenamesAndKeepsAliases(t *testing.T) {
	r := toolRegistry()
	r.SetPolicy([]mcp.Config{{Name: "beta", ToolPrefix: "vb"}, {Name: "alpha"}})
	got := visibleNames(r)
	if !equalNames(got, []string{"fs_read", "search", "vb_fs_read", "vb_fs_write"}) {
		t.Fatalf("custom prefix: got %v (one underscore, no double underscore)", got)
	}
	// The name it was published under before the rename still resolves: it belongs to this same
	// tool, and a transcript quotes it.
	ref, ok := r.resolve("beta_fs_read")
	if !ok || ref.Tool != "fs_read" || ref.Server != "beta" {
		t.Fatalf("renamed-away name must still resolve, got %v %v", ref, ok)
	}
	// Changing the prefix back must restore the original names, not mint "beta_fs_read_2".
	r.SetPolicy([]mcp.Config{{Name: "beta"}, {Name: "alpha"}})
	if !equalNames(visibleNames(r), defaultNames) {
		t.Fatalf("prefix reverted: got %v, want %v", visibleNames(r), defaultNames)
	}
}

// TestToolPrefixCollision: two servers may be given the same prefix (that is a user's choice), and
// two tools may still want the same name — the server name settles it, and a name is never shared.
func TestToolPrefixCollision(t *testing.T) {
	r := newRegistry(nil, nil, false)
	r.last["one"] = []mcp.Tool{{Name: "read"}}
	r.last["two"] = []mcp.Tool{{Name: "read"}}
	r.status["one"] = ServerStatus{Name: "one", Connected: true}
	r.status["two"] = ServerStatus{Name: "two", Connected: true}
	r.SetPolicy([]mcp.Config{{Name: "one", ToolPrefix: "x"}, {Name: "two", ToolPrefix: "x"}})
	got := visibleNames(r)
	if !equalNames(got, []string{"x_read", "two_read"}) {
		t.Fatalf("collision: got %v, want [x_read two_read]", got)
	}
	if _, ok := r.resolve("x_read_2"); ok {
		t.Fatal("numbered fallback must not be used when the server name is free")
	}
}

// TestPolicyPendingIsDerived: "did my change land?" has to be answered by comparing the config
// with what is published, because a stored flag can be set after the run that was waiting for it
// has already finished.
func TestPolicyPendingIsDerived(t *testing.T) {
	r := toolRegistry()
	cfg := []mcp.Config{{Name: "beta"}, {Name: "alpha"}}
	if r.PolicyPending(cfg) {
		t.Fatal("just applied: nothing may be pending")
	}
	if !r.PolicyPending([]mcp.Config{{Name: "beta", DisabledTools: []string{"fs_read"}}, {Name: "alpha"}}) {
		t.Fatal("a tool switch that is not published yet must read as pending")
	}
	// Whitespace is not a change; a prefix that happens to equal the server name IS one, since it
	// renames alpha's tools from fs_read to alpha_fs_read.
	if r.PolicyPending([]mcp.Config{{Name: "beta"}, {Name: "alpha", ToolPrefix: "  "}}) {
		t.Fatal("whitespace around an empty prefix must not read as pending")
	}
	if !r.PolicyPending([]mcp.Config{{Name: "beta"}, {Name: "alpha", ToolPrefix: "alpha"}}) {
		t.Fatal("a prefix equal to the server name renames tools, so it is a change")
	}
}

// TestCountsSeparatesPublishedFromOffered: the sidebar shows "7/11 tools", where 11 is what the
// servers offer and 7 is what this session leaves switched on.
func TestCountsSeparatesPublishedFromOffered(t *testing.T) {
	r := toolRegistry()
	published, enabled, total := r.counts()
	if published != 4 || enabled != 4 || total != 4 {
		t.Fatalf("no policy: got published=%d enabled=%d total=%d", published, enabled, total)
	}
	r.SetPolicy([]mcp.Config{{Name: "beta", DisabledTools: []string{"fs_write"}}, {Name: "alpha"}})
	published, enabled, total = r.counts()
	if published != 3 || enabled != 3 || total != 4 {
		t.Fatalf("one off: got published=%d enabled=%d total=%d", published, enabled, total)
	}
}

// TestViewShowsDisabledToolsToo: the drawer has to list the tools that are off, or there is no way
// to switch them back on — and it has to carry what each tool is (description, parameters), since
// that is what reading a name is not.
func TestViewShowsDisabledToolsToo(t *testing.T) {
	r := toolRegistry()
	cfg := []mcp.Config{{Name: "beta", DisabledTools: []string{"fs_write"}, ToolPrefix: "vb"}, {Name: "alpha"}}
	r.SetPolicy(cfg)
	v := r.view(cfg)
	if v.Total != 4 || v.Visible != 3 {
		t.Fatalf("view counts: got visible=%d total=%d", v.Visible, v.Total)
	}
	if len(v.Servers) != 2 || v.Servers[0].Name != "alpha" {
		t.Fatalf("servers: %+v", v.Servers)
	}
	if v.Servers[1].Prefix != "vb" || v.Servers[0].Prefix != "" {
		t.Fatalf("prefixes: alpha=%q beta=%q, want \"\" and \"vb\"", v.Servers[0].Prefix, v.Servers[1].Prefix)
	}
	var found bool
	for _, row := range v.Servers[1].Tools {
		if row.RawName == "fs_write" {
			found = true
			if row.Enabled {
				t.Fatal("fs_write must be listed and off")
			}
			if row.Name != "vb_fs_write" {
				t.Fatalf("a disabled tool still needs its model-visible name, got %q", row.Name)
			}
			if !strings.Contains(string(row.Schema), `"path"`) {
				t.Fatalf("the drawer needs the parameters to be worth switching off, got %s", row.Schema)
			}
			if row.EstTokens <= 0 {
				t.Fatal("est tokens must be reported (the drawer warns about the cache cost)")
			}
		}
	}
	if !found {
		t.Fatalf("the disabled tool is missing from the drawer: %+v", v.Servers[1].Tools)
	}
}

// TestViewExplainsARenamedTool: a tool published under a name its server did not choose has to say
// so in the drawer, or the switch labelled beta_fs_read is a mystery.
func TestViewExplainsARenamedTool(t *testing.T) {
	r := toolRegistry()
	v := r.view([]mcp.Config{{Name: "beta"}, {Name: "alpha"}})
	rows := map[string]ToolRow{}
	for _, srv := range v.Servers {
		for _, row := range srv.Tools {
			rows[srv.Name+"/"+row.RawName] = row
		}
	}
	if got := rows["alpha/fs_read"]; got.Name != "fs_read" || got.RenamedFrom != "" {
		t.Fatalf("the first claimer needs no explanation, got %+v", got)
	}
	got := rows["beta/fs_read"]
	if got.Name != "beta_fs_read" || got.RenamedFrom != "fs_read" {
		t.Fatalf("the colliding tool must say what it would have been, got %+v", got)
	}
}

// TestDisabledServerIsNotConnected: "disabled" used to be a key config.json carried that nothing
// read. It is a real switch now, and says "off" rather than "broken".
func TestDisabledServerIsNotConnected(t *testing.T) {
	r := newRegistry(nil, nil, false)
	// No transport can be reached in a test, so a disabled server is also the one case that proves
	// connect() does not even try: nothing is connected, and the status is disabled rather than an
	// error.
	r.connect(t.Context(), []mcp.Config{{Name: "off-server", Type: "http", URL: "http://127.0.0.1:1/mcp", Disabled: true}})
	st := r.statusSnapshot()
	if len(st) != 1 {
		t.Fatalf("status: %+v", st)
	}
	if !st[0].Disabled || st[0].Connected || st[0].Error != "" {
		t.Fatalf("want disabled, not connected, no error: %+v", st[0])
	}
	if got := r.Tools(); len(got) != 0 {
		t.Fatalf("a disabled server offers no tools, got %v", got)
	}
}

// TestPolicyIsNotAConnectionChange is the promise the whole design rests on: the MCP servers are
// left alone when only tool policy moves, and are restarted when the server itself is switched
// off. sameMCP is what UpdateConfig uses to decide.
func TestPolicyIsNotAConnectionChange(t *testing.T) {
	a := []mcp.Config{{Name: "s", Type: "http", URL: "http://x/mcp"}}
	b := []mcp.Config{{Name: "s", Type: "http", URL: "http://x/mcp", DisabledTools: []string{"fs_write"}, ToolPrefix: "p"}}
	if !sameMCP(a, b) {
		t.Fatal("a tool policy change must not reconnect a server")
	}
	off := []mcp.Config{{Name: "s", Type: "http", URL: "http://x/mcp", Disabled: true}}
	if sameMCP(a, off) {
		t.Fatal("switching a whole server off is a connection change")
	}
}
