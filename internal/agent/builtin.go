package agent

import (
	"fmt"
	"strings"

	"bareloop/internal/builtin"
)

// BuiltinServer is where the daemon-answered tools live in everything the loop records: the
// tool-result entry's server, and the tools drawer's group. It is not an MCP server, and no MCP
// server may take the name (validateBuiltin refuses one at the API), because a transcript that says
// server "builtin" has to mean one way of routing a call.
const BuiltinServer = "builtin"

// builtinEnabled is the session's on-list in the shape the registry wants it: a set, trimmed, with
// the empty strings a hand-edited config.json produces dropped on the floor.
func builtinEnabled(on []string) map[string]bool {
	set := make(map[string]bool, len(on))
	for _, name := range on {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	return set
}

// builtinEnabledFromOff translates the drawer's write path into config. The drawer speaks one
// language for every group — "this is what is switched off in it" — while these are stored as what
// is switched on, because off-by-default is the policy. So every builtin that exists and was not
// named as off is on, which also makes the result canonical: a stale name in a request body cannot
// switch on something that does not exist.
func builtinEnabledFromOff(off []string) []string {
	isOff := make(map[string]bool, len(off))
	for _, name := range off {
		isOff[strings.TrimSpace(name)] = true
	}
	var on []string
	for _, name := range builtin.Names() {
		if !isOff[name] {
			on = append(on, name)
		}
	}
	return on
}

// unknownBuiltinNames is what a config asks for that does not exist. It is refused at the API
// rather than ignored, because a name that silently does nothing is the worst kind of typo: the
// session looks configured and the tool is not there.
func unknownBuiltinNames(on []string) []string {
	var bad []string
	for _, name := range on {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := builtin.Get(name); !ok {
			bad = append(bad, name)
		}
	}
	return bad
}

// sameBuiltinSet answers "did the answer to which builtins are on change?" — which a map cannot be
// compared with directly.
func sameBuiltinSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for name := range a {
		if !b[name] {
			return false
		}
	}
	return true
}

// validateBuiltin is the config-time check for what the daemon's own tools need in order to be
// right: that builtinTools names tools that exist, that no MCP server took the group name, and that
// the default timezone is a zone this machine can resolve.
//
// It runs where every other config rule runs (Manager.Create and UpdateConfig, the merged result
// included) rather than at call time: a session that says {"timezone":"Asia/Typo"} would otherwise
// spend a turn to answer one tool call with an error, which is a slow way of reporting a typo in a
// file the settings dialog rewrites on every save.
func validateBuiltin(cfg Config) error {
	for _, mc := range cfg.MCP {
		if mc.Name == BuiltinServer {
			return fmt.Errorf("agent: an mcp server cannot be named %q: that is the group the daemon's own tools live in", BuiltinServer)
		}
	}
	if bad := unknownBuiltinNames(cfg.BuiltinTools); len(bad) > 0 {
		return fmt.Errorf("agent: unknown builtin tool %s (the ones there are: %s)", strings.Join(bad, ", "), strings.Join(builtin.Names(), ", "))
	}
	// The value is resolved rather than pattern-matched, because the question is whether this
	// machine can tell what time it is in that zone, and only the time package knows.
	if _, err := builtin.LoadTimezone(cfg.Timezone); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	return nil
}
