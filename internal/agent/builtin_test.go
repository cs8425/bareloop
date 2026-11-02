package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"bareloop/internal/mcp"
)

// The tests here are about publishing and routing: which of the daemon's own tools a session sees,
// and what happens when an MCP tool already owns one of those names. What each tool actually
// answers is internal/builtin's own business.

func TestBuiltinsAreOffUntilNamed(t *testing.T) {
	r := toolRegistry()
	// The fixture's two servers and no builtinTools key at all — which is what every session written
	// before these tools existed looks like.
	if got := visibleNames(r); !equalNames(got, defaultNames) {
		t.Fatalf("a session that asked for nothing gained a tool: got %v, want %v", got, defaultNames)
	}
	for _, name := range []string{"wait", "get_datetime"} {
		if _, ok := r.resolve(name); ok {
			t.Fatalf("%s resolves without being switched on", name)
		}
	}
	// Off is not the same as nonexistent: the drawer still lists them, or there is no way to find
	// the switch.
	built := r.view(Config{MCP: []mcp.Config{{Name: "beta"}, {Name: "alpha"}}}).Servers[2]
	if built.Enabled != 0 || built.Known != 2 {
		t.Fatalf("the builtin group is not showing both tools switched off: %+v", built)
	}
}

func TestBuiltinPublishesAndRoutesToTheDaemon(t *testing.T) {
	r := newRegistry(nil, nil, false)
	r.SetPolicy(Config{BuiltinTools: []string{"get_datetime"}})
	if got := visibleNames(r); len(got) != 1 || got[0] != "get_datetime" {
		t.Fatalf("published: got %v, want [get_datetime]", got)
	}
	ref, ok := r.resolve("get_datetime")
	if !ok || !ref.Builtin || ref.Server != BuiltinServer {
		t.Fatalf("a builtin must route to the daemon: %+v %v", ref, ok)
	}
	res, ref, err := r.call(context.Background(), "get_datetime", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !ref.Builtin {
		t.Fatal("the result must say it came from the daemon")
	}
	if res.IsError {
		t.Fatalf("a builtin answered with an error: %+v", res.Content)
	}
	// No timezone configured means UTC, and the answer has to say so out loud.
	text := res.Content[0].Text
	if !strings.Contains(text, " (UTC) ") || !strings.Contains(text, "+00:00") {
		t.Fatalf("get_datetime answered %q: an unlabelled timestamp is a claim about a zone", text)
	}
	// A builtin answers with no MCP server connected at all, which is the point of the route.
	if len(r.servers) != 0 {
		t.Fatalf("a builtin asked a server something: %v", r.servers)
	}
}

// TestBuiltinTimezoneSettingIsPerSession is the config knob arriving where the tool can see it.
func TestBuiltinTimezoneSettingIsPerSession(t *testing.T) {
	r := newRegistry(nil, nil, false)
	r.SetPolicy(Config{BuiltinTools: []string{"get_datetime"}, Timezone: "Asia/Taipei"})
	res, _, err := r.call(context.Background(), "get_datetime", nil, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if text := res.Content[0].Text; !strings.Contains(text, "+08:00") || !strings.Contains(text, "Asia/Taipei") {
		t.Fatalf("session timezone ignored: %q", text)
	}
	// An argument still beats it, because the interesting question is what time it is somewhere else.
	res, _, err = r.call(context.Background(), "get_datetime", json.RawMessage(`{"timezone":"UTC"}`), nil)
	if err != nil {
		t.Fatalf("call with an argument: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "(UTC)") {
		t.Fatalf("the argument did not win: %q", res.Content[0].Text)
	}
}

// TestMCPToolKeepsANameABuiltinWants is the collision rule: an MCP tool already using the name is
// not moved for a builtin, because that would rename a tool behind the transcript's back to make
// room for a tool the session could have had under a prefix instead.
func TestMCPToolKeepsANameABuiltinWants(t *testing.T) {
	var logged []string
	r := newRegistry(nil, func(format string, args ...any) {
		logged = append(logged, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}, false)
	r.last["files"] = []mcp.Tool{{Name: "wait", Description: "hold on"}}
	r.status["files"] = ServerStatus{Name: "files", Connected: true}
	r.SetPolicy(Config{
		MCP:          []mcp.Config{{Name: "files"}},
		BuiltinTools: []string{"wait"},
	})
	ref, ok := r.resolve("wait")
	if !ok {
		t.Fatal("the MCP tool lost a name it already had")
	}
	if ref.Builtin || ref.Server != "files" {
		t.Fatalf("wait resolved to the wrong owner: %+v", ref)
	}
	// What the drawer is shown: the switch stays where the config put it, because the checkbox *is*
	// the config — the note is what says the tool was never offered to the model.
	built := r.view(Config{MCP: []mcp.Config{{Name: "files"}}}).Servers[1]
	if !built.Builtin || built.Enabled != 1 || built.Known != 2 {
		t.Fatalf("the builtin group does not read back what was configured: %+v", built)
	}
	if !strings.Contains(built.Note, "wait is switched on but not published") ||
		!strings.Contains(built.Note, "toolPrefix") {
		t.Fatalf("the drawer was given no way to explain the missing tool: %q", built.Note)
	}
	var warned bool
	for _, l := range logged {
		if strings.Contains(l, "builtin tool wait is not published") && strings.Contains(l, "files/wait") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a builtin that lost its name said nothing: %v", logged)
	}
	// Re-applying the same policy must not say it again: a warning that repeats while nothing has
	// changed stops being a warning.
	r.SetPolicy(Config{MCP: []mcp.Config{{Name: "files"}}, BuiltinTools: []string{"wait"}})
	// A prefix added mid-life renames the tool but does not hand the name over: the name it used to
	// have keeps pointing at it for the rest of this process, which is what `retired` is for. A
	// transcript full of tool_call{name:"wait"} must not start parking the loop just because the MCP
	// tool moved, so the builtin waits for a registry that never saw the name — which is what a prefix
	// in the config, before the server is ever listed, does.
	r.SetPolicy(Config{MCP: []mcp.Config{{Name: "files", ToolPrefix: "fs"}}, BuiltinTools: []string{"wait"}})
	if got := visibleNames(r); len(got) != 1 || got[0] != "fs_wait" {
		t.Fatalf("a prefix that moved the tool also gave the name away: got %v, want only [fs_wait]", got)
	}
	if ref, ok := r.resolve("wait"); !ok || ref.Builtin {
		t.Fatalf("the name the transcript quotes stopped meaning the tool it meant: %+v ok=%v", ref, ok)
	}
	// And the drawer still says so, because the tool is still switched on and still not published.
	if after := r.view(Config{MCP: []mcp.Config{{Name: "files", ToolPrefix: "fs"}}}).Servers[1]; after.Note == "" {
		t.Fatal("the blocked builtin went quiet just because the other tool was renamed")
	}
	said := 0
	for _, l := range logged {
		if strings.Contains(l, "builtin tool wait is not published") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the same blocked builtin warned %d times", said)
	}
}

// TestAPrefixInTheConfigFirstFreesTheName is the other half of the rule, and the one the drawer's note
// advises: a server that was never listed without a prefix never holds the bare name, so the builtin
// gets it on the first policy. A toolPrefix does make room — as long as it is there before the name is
// ever used.
func TestAPrefixInTheConfigFirstFreesTheName(t *testing.T) {
	var logged []string
	r := newRegistry(nil, func(format string, args ...any) {
		logged = append(logged, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}, false)
	r.last["files"] = []mcp.Tool{{Name: "wait", Description: "hold on"}}
	r.status["files"] = ServerStatus{Name: "files", Connected: true}
	r.SetPolicy(Config{
		MCP:          []mcp.Config{{Name: "files", ToolPrefix: "fs"}},
		BuiltinTools: []string{"wait"},
	})
	if got := visibleNames(r); len(got) != 2 || got[0] != "fs_wait" || got[1] != "wait" {
		t.Fatalf("the name was not free for the builtin: got %v, want [fs_wait wait]", got)
	}
	if ref, ok := r.resolve("wait"); !ok || !ref.Builtin {
		t.Fatalf("wait answers to %s rather than to the daemon: %+v ok=%v", ref.Server, ref, ok)
	}
	for _, l := range logged {
		if strings.Contains(l, "not published") {
			t.Fatalf("a builtin that got its name complained about it: %v", logged)
		}
	}
}

// TestBuiltinNameIsReleasedWhenSwitchedOff: off means off, and the name becomes free again —
// otherwise switching wait off by hand would leave a placeholder nothing can fill.
func TestBuiltinNameIsReleasedWhenSwitchedOff(t *testing.T) {
	r := newRegistry(nil, nil, false)
	r.SetPolicy(Config{BuiltinTools: []string{"wait"}})
	if _, ok := r.resolve("wait"); !ok {
		t.Fatal("the builtin is not published while it is on")
	}
	r.last["files"] = []mcp.Tool{{Name: "wait", Description: "hold on"}}
	r.status["files"] = ServerStatus{Name: "files", Connected: true}
	r.SetPolicy(Config{
		MCP:          []mcp.Config{{Name: "files"}},
		BuiltinTools: nil, // every one of them off again
	})
	ref, ok := r.resolve("wait")
	if !ok || ref.Builtin || ref.Server != "files" {
		t.Fatalf("a switched-off builtin kept the name: %+v %v", ref, ok)
	}
}

func TestBuiltinPolicyIsRejectedAtTheAPI(t *testing.T) {
	for name, cfg := range map[string]Config{
		"a name that is not a builtin": {BuiltinTools: []string{"datetime"}},
		"a server called builtin":      {MCP: []mcp.Config{{Name: BuiltinServer}}},
		"a zone that does not exist":   {Timezone: "Asia/Typo"},
	} {
		if err := validateBuiltin(cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := validateBuiltin(Config{
		MCP:          []mcp.Config{{Name: "files"}},
		BuiltinTools: []string{"wait", "get_datetime"},
		Timezone:     "UTC+8",
	}); err != nil {
		t.Errorf("a correct config was refused: %v", err)
	}
}

// TestBuiltinOffListIsTranslatedFromTheDrawer: the drawer writes one language for every group
// ("these are the ones switched off in it"), so the on-list the config stores has to come out of that.
func TestBuiltinOffListIsTranslatedFromTheDrawer(t *testing.T) {
	if got := builtinEnabledFromOff([]string{"wait"}); len(got) != 1 || got[0] != "get_datetime" {
		t.Fatalf("off wait: got %v, want [get_datetime]", got)
	}
	// Nothing off means every builtin is on — the same language the MCP group uses. That is not
	// ambiguous with "all off", because the drawer always sends the whole off-list, so all-off arrives
	// as every name rather than as an empty one.
	if got := builtinEnabledFromOff(nil); len(got) != 2 {
		t.Fatalf("nothing off came back as %v, want both builtins on", got)
	}
	// A stale name in a request body cannot switch on something that does not exist, because the
	// answer is built from the tools that exist rather than from what was asked for.
	if got := builtinEnabledFromOff([]string{"nope"}); len(got) != 2 {
		t.Fatalf("unknown name: got %v, want both builtins on", got)
	}
	if got := builtinEnabledFromOff([]string{"wait", "get_datetime"}); len(got) != 0 {
		t.Fatalf("all off: got %v, want none on", got)
	}
}
