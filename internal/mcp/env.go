package mcp

// The environment a stdio server starts with is a security decision, not a convenience one:
// bareloopd holds every provider key the operator expanded out of config.json (that is the
// point of ${ENV} — keys stay off disk, so the process environment is the only place they
// live). A child process that inherited os.Environ() wholesale could read all of them at
// startup, and every stdio MCP server — however legitimate — is third-party code with a
// stdout, a stderr, and the daemon's permission to open connections of its own. Before this
// file existed, `env: [...]` only *added* to that wholesale inheritance, and leaving it
// empty inherited everything by exec's own default.
//
// The policy matches what the MCP reference clients converged on (both the TypeScript and the
// Python SDK moved to exactly this in their v2 lines, the variable list "inspired by the
// default env inheritance of sudo", as the TypeScript one puts it): a small platform-specific
// whitelist of what a process needs to start at all, plus whatever the operator names for
// this one server. There is deliberately no "inherit everything" switch: naming a variable is
// the whole protocol.
//
// Codex spells the same policy `env` (values to forward) + `env_vars` (a whitelist to source
// from its own environment); here that is `env` (unchanged: appended, last value wins) and
// `envPassthrough` (names whose values are read from bareloopd's own environment at spawn).
// An entry can drop the base set too — `envBase: false` — for an operator who considers even
// HOME a secret; the child then starts from an environment of exactly the variables it was
// named, which is the whole policy in both directions.

import (
	"runtime"
	"strings"
)

// defaultInheritedEnvVars is the base set every stdio server gets: the variables a process
// needs to launch and find its home, and nothing that can carry a secret. The two lists are
// the ones the MCP TypeScript/Python SDKs (v2) inherit, kept identical so that "works under
// the reference clients" implies "works here". The list is alive upstream — expect Windows
// entries to be added over time; a variable missing here is a line of envPassthrough away.
func defaultInheritedEnvVars() []string {
	if runtime.GOOS == "windows" {
		return []string{
			"APPDATA", "COMSPEC", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATH",
			"PATHEXT", "PROCESSOR_ARCHITECTURE", "PROGRAMDATA", "PROGRAMFILES",
			"PROGRAMFILES(X86)", "PROGRAMW6432", "SYSTEMDRIVE", "SYSTEMROOT", "TEMP",
			"USERNAME", "USERPROFILE", "WINDIR",
		}
	}
	return []string{"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER"}
}

// buildEnv is the whole environment of a stdio child, in three layers, each visible in
// data/<id>/config.json:
//
//   - the platform base set, sourced from parent by name — unless the entry says otherwise
//     (EnvBase below), for an operator who wants the child to see nothing but what they named;
//   - cfg.EnvPassthrough — names the operator chose to forward from bareloopd's own
//     environment. Only the name is configured: a passthrough value never enters config.json,
//     never comes back from GET /config, and so never has to be a ${ENV} expansion whose
//     resolved value a settings page would echo (that is what distinguishes it from writing
//     the same variable into env with its value);
//   - cfg.Env — literal KEY=value lines, last layer, unchanged semantics: it overrides a base
//     or passed-through name because exec keeps the last of a duplicate key.
//
// A name in the first two layers that the daemon itself does not have is skipped — passing a
// name through requires there to be a value behind it, and an empty one would be a different
// answer than the server got from the environment it expected. A name is looked up first by
// exact spelling — envPassthrough: ["Path"] on a parent that spells it `Path` gets that line,
// which matters on Windows, where PATH and Path can sit in one environment disagreeing — and
// only then case-insensitively, so "path" still finds the parent's PATH. The parent's own
// spelling reaches the child either way.
//
// Duplicate keys across layers are left for os/exec to resolve ("only the value of the last
// occurrence is used" — case-insensitively on Windows), which is exactly the semantics the
// layers above promise.
func buildEnv(parent []string, cfg StdioConfig) []string {
	base := defaultInheritedEnvVars()
	if cfg.EnvBase != nil && !*cfg.EnvBase {
		base = nil // strict: nothing reaches the child except what this entry names
	}
	return buildEnvWith(parent, base, cfg)
}

// buildEnvWith takes the base set as an argument so both platforms' lists are testable
// whichever platform runs the test.
func buildEnvWith(parent []string, base []string, cfg StdioConfig) []string {
	out := make([]string, 0, len(base)+len(cfg.EnvPassthrough)+len(cfg.Env))
	for _, name := range append(append([]string{}, base...), cfg.EnvPassthrough...) {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsRune(name, '=') {
			continue // not a name
		}
		if kv, ok := lookupEnv(parent, name); ok {
			out = append(out, kv)
		}
	}
	out = append(out, cfg.Env...)
	return out
}

// lookupEnv finds one variable's "Key=value" line in the parent's environment: the exact
// spelling first, because that is a line the operator asked for by name and both spellings
// can legitimately exist at once on Windows; the case-insensitive sweep after, so a lowercase
// name still finds the uppercase reality. The first match wins within each pass.
func lookupEnv(parent []string, name string) (string, bool) {
	for _, kv := range parent {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[:i] == name {
			return kv, true
		}
	}
	for _, kv := range parent {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], name) {
			return kv, true
		}
	}
	return "", false
}
