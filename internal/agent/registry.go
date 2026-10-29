package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// registry is a session's MCP server pool plus the name mapping the model sees.
//
// There are no built-in tools: everything the model may call comes from an MCP server. A tool is
// published under its own name by default — no namespace, because tool names are already written to
// be read — or under "<toolPrefix>_<tool>" when the session asks for one, sanitised to the character
// set OpenAI-style function calling accepts and mapped back to (server, tool). When two tools want
// the same name the later one is published as "<server>_<tool>" and the collision is logged; nothing
// already published is ever renamed. That stability is the point: the `tools` array the chat template
// renders must not change under the KV prefix cache, and old transcripts quote the names they were
// written with (spec §5.3).
//
// Which tools are offered at all is per-session policy (mcp.Config.Disabled, DisabledTools,
// ToolPrefix). The registry caches every server's last tools/list result, so changing the policy
// is a re-filter of that cache: no tools/list round trip, and no reconnect.
type registry struct {
	mu      sync.RWMutex
	servers map[string]*mcp.Client
	order   []string // deterministic server order
	// assigned is the model-visible name each (server, tool) is published under; assignedBase is
	// the un-suffixed name it came from, which is how a prefix change is told apart from a
	// collision suffix.
	assigned     map[string]string
	assignedBase map[string]string
	// retired holds model-visible names a tool was published under before a prefix change renamed
	// it. They keep resolving to the same (server, tool): a transcript quotes them, and a model
	// imitating an old name means the same tool. Tools that are merely switched off deliberately
	// do not land here — a hidden tool must stay uncallable.
	retired map[string]toolRef
	// last is each server's last tools/list result; policy is this session's tool switches.
	// Together they are what defs and byName are rebuilt from.
	policy map[string]toolPolicy
	last   map[string][]mcp.Tool
	byName map[string]toolRef
	defs   []llm.Tool
	status map[string]ServerStatus
	errs   map[string]string
	emit   func(Event)
	logf   func(string, ...any)
	notify bool
	// askElicit forwards a server input request to the user; set by the owning Session.
	askElicit func(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResponse, error)
}

type toolRef struct {
	Server string
	Tool   string
}

// toolPolicy is one server's tool switches as configured by this session.
type toolPolicy struct {
	// prefix is the sanitised override for the name prefix; "" means use the server name.
	prefix string
	// off lists the tools that must not be published: exact raw names and/or globs.
	off []string
}

// on reports whether one tool is published. raw is the tool's own name from tools/list, never
// the model-visible name: the latter is derived (prefix, plus a suffix on a collision), and a
// policy that depends on a derived value would change meaning every time a tool is added.
// Matching is case sensitive, because MCP tool names are.
func (p toolPolicy) on(raw string) bool {
	for _, pattern := range p.off {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == raw {
			return false
		}
		if strings.ContainsAny(pattern, "*?[") {
			if ok, err := path.Match(pattern, raw); err == nil && ok {
				return false
			}
		}
	}
	return true
}

var safeNameRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func sanitize(s string) string {
	s = safeNameRE.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_-")
	if s == "" {
		s = "x"
	}
	return s
}

func newRegistry(emit func(Event), logf func(string, ...any), notify bool) *registry {
	r := &registry{
		servers:      map[string]*mcp.Client{},
		assigned:     map[string]string{},
		assignedBase: map[string]string{},
		retired:      map[string]toolRef{},
		policy:       map[string]toolPolicy{},
		last:         map[string][]mcp.Tool{},
		byName:       map[string]toolRef{},
		status:       map[string]ServerStatus{},
		errs:         map[string]string{},
		emit:         emit,
		logf:         logf,
		notify:       notify,
	}
	if logf == nil {
		r.logf = func(string, ...any) {}
	}
	return r
}

// connect starts/connects every configured server. Failures are recorded per server: one broken
// MCP server must not stop the session (fail open, tools from it are just missing). A server the
// session switched off is not started at all, and says so without pretending to be an error.
func (r *registry) connect(ctx context.Context, cfgs []mcp.Config) {
	r.mu.Lock()
	r.order = r.order[:0]
	for _, c := range cfgs {
		r.order = append(r.order, c.Name)
	}
	r.mu.Unlock()

	for _, c := range cfgs {
		cfg := c
		if cfg.Disabled {
			r.mu.Lock()
			delete(r.errs, cfg.Name)
			delete(r.last, cfg.Name)
			r.status[cfg.Name] = ServerStatus{Name: cfg.Name, Transport: transportOf(cfg), Disabled: true}
			r.mu.Unlock()
			r.logf("mcp %s: switched off for this session, not connecting", cfg.Name)
			continue
		}
		if cfg.Logf == nil {
			cfg.Logf = func(format string, args ...any) {
				r.logf("[%s] %s", cfg.Name, fmt.Sprintf(format, args...))
				r.emitLog(cfg.Name, fmt.Sprintf(format, args...))
			}
		}
		cli, err := mcp.Connect(ctx, cfg)
		r.mu.Lock()
		if err != nil {
			r.errs[cfg.Name] = err.Error()
			r.status[cfg.Name] = ServerStatus{Name: cfg.Name, Transport: transportOf(cfg), Error: err.Error()}
			r.mu.Unlock()
			r.logf("mcp %s: connect failed: %v", cfg.Name, err)
			continue
		}
		r.servers[cfg.Name] = cli
		r.status[cfg.Name] = ServerStatus{
			Name: cfg.Name, Transport: transportOf(cfg), Connected: true,
			Era: cli.Era(), Version: cli.Version(), Impl: cli.Info().Name + "/" + cli.Info().Version,
			Info: clip(cli.Instructions(), 400),
		}
		r.mu.Unlock()
		cli.OnEvent = func(e mcp.Event) { r.onServerEvent(e) }
		cli.Elicit = func(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResponse, error) {
			r.mu.RLock()
			ask := r.askElicit
			r.mu.RUnlock()
			if ask == nil {
				return mcp.ElicitResponse{Behavior: "decline"}, errNoUI
			}
			return ask(ctx, req)
		}
		if r.notify {
			if ok, err := cli.Subscribe(ctx, map[string]bool{"toolsListChanged": true}, func(e mcp.Event) {
				r.onServerEvent(mcp.Event{Server: cfg.Name, Method: e.Method, Params: e.Params})
			}); err != nil {
				r.logf("mcp %s: subscribe: %v", cfg.Name, err)
			} else if ok {
				r.logf("mcp %s: subscribed to list changes", cfg.Name)
			}
		}
	}
	if _, err := r.refreshTools(ctx); err != nil {
		r.logf("tools/list: %v", err)
	}
}

func transportOf(c mcp.Config) string {
	if strings.TrimSpace(c.Type) == "" {
		return "stdio"
	}
	return c.Type
}

func (r *registry) emitLog(server, msg string) {
	if r.emit == nil {
		return
	}
	r.emit(Event{Type: EvtLog, Level: "info", Info: server + ": " + msg})
}

// onServerEvent forwards MCP notifications to the UI and refreshes the tool list when the server
// says it changed. A tool that appears this way is subject to the policy in force already, so a
// server cannot hand back a tool the session switched off by re-listing it.
func (r *registry) onServerEvent(e mcp.Event) {
	if r.emit != nil {
		r.emit(Event{Type: EvtLog, Level: "info", Info: e.Server + " " + e.Method + " " + clip(string(e.Params), 200)})
	}
	if e.Method == "notifications/tools/list_changed" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := r.refreshTools(ctx); err != nil {
			r.logf("mcp %s: refresh after list_changed: %v", e.Server, err)
		}
	}
}

// refreshTools re-lists every server's tools, caches the results, and republishes the
// model-visible tool array. The array is sorted (server, tool) so the rendered prefix is
// deterministic.
func (r *registry) refreshTools(ctx context.Context) ([]llm.Tool, error) {
	r.mu.RLock()
	servers := make([]string, 0, len(r.servers))
	for name := range r.servers {
		servers = append(servers, name)
	}
	cli := func(name string) *mcp.Client { return r.servers[name] }
	r.mu.RUnlock()
	sort.Strings(servers)

	var firstErr error
	for _, name := range servers {
		tools, err := cli(name).Tools(ctx)
		r.mu.Lock()
		if err != nil {
			// No tools/list, no tools: the previous list is dropped rather than kept, so a server
			// that stops answering cannot keep publishing tools it no longer has.
			delete(r.last, name)
			r.mu.Unlock()
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
			r.logf("mcp %s: tools/list: %v", name, err)
			continue
		}
		r.last[name] = tools
		r.mu.Unlock()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	defs := r.rebuildLocked()
	return defs, firstErr
}

// collisionNote is what a name collision costs the user: a tool published under a name its server
// did not choose. It goes to the log at warn level, because a name that changed behind your back has
// to be findable there — and because "why is this switch called beta_fs_read" needs an answer that
// does not involve reading the source.
func (r *registry) collisionNote(server, raw, got, want string) {
	r.logf("tool name collision: %s/%s would be %q, which another tool already owns; publishing it as %q instead (mcp[].toolPrefix lets you choose the prefix, or rename the server)", server, raw, want, got)
	if r.emit != nil {
		r.emit(Event{Type: EvtLog, Level: "warn", Info: server + ": tool name collision — " + raw + " is published as " + got + " instead of " + want + "; set mcp[].toolPrefix if you want a different name"})
	}
}

// rebuildLocked republishes the model-visible tool array from the cached tools/list results and
// the policy in force. Filtering happens here and nowhere else: a tool that is not in defs is
// invisible to the model and unresolvable by name, so no separate permission check is needed.
// The caller holds r.mu.
func (r *registry) rebuildLocked() []llm.Tool {
	servers := make([]string, 0, len(r.last))
	for name := range r.last {
		servers = append(servers, name)
	}
	sort.Strings(servers)

	byName := map[string]toolRef{}
	defs := make([]llm.Tool, 0, 64)
	for _, name := range servers {
		tools := append([]mcp.Tool(nil), r.last[name]...)
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
		pol := r.policyOfLocked(name)
		enabled := 0
		for _, t := range tools {
			if !pol.on(t.Name) {
				continue
			}
			enabled++
			ref := toolRef{Server: name, Tool: t.Name}
			vis := r.assignLocked(ref, pol.prefix)
			byName[vis] = ref
			defs = append(defs, llm.Tool{
				Name: vis, Description: clip(toolDesc(t), 4000), Parameters: normalizeSchema(t.InputSchema),
			})
		}
		if st, ok := r.status[name]; ok {
			st.Tools, st.Enabled, st.Hidden = len(tools), enabled, len(tools)-enabled
			r.status[name] = st
		}
	}
	// Renamed-away names keep resolving (they belong to a tool that is still published), so an
	// old transcript's tool_calls and a model imitating an old name both still mean one thing.
	for name, ref := range r.retired {
		if _, live := byName[name]; !live {
			byName[name] = ref
		}
	}
	r.byName, r.defs = byName, defs
	out := make([]llm.Tool, len(defs))
	copy(out, defs)
	return out
}

// publishedName is the model-visible name one tool asks for. With no prefix — the default — that is
// the tool's own name, because most servers already name their tools so that one reader knows what
// they mean, and nobody wants to type a namespace into every prompt. A prefix is joined with a
// single underscore. A name that would be too long is hashed rather than truncated, so two long
// names that share a beginning still get different names.
func publishedName(prefix, raw string) string {
	name := sanitize(raw)
	if p := strings.TrimSpace(prefix); p != "" {
		name = sanitize(p) + "_" + sanitize(raw)
	}
	if len(name) > 60 {
		h := sha1.Sum([]byte(name))
		name = name[:40] + "_" + hex.EncodeToString(h[:])[:12]
	}
	return name
}

// assignLocked returns the model-visible name of one tool, kept for as long as the prefix does not
// change: switching a tool off and on again, or adding another tool to the server, must not rename
// anything, because the rendered tool block has to stay byte-identical for the KV prefix cache and
// because old transcripts quote the names they were written with.
//
// A name that is already somebody else's is settled by freeNameLocked; nothing that is already
// published is ever renamed to make room. The note about that is logged here, at the moment the name
// is decided — not on every re-filter of the cache, which would say it again on every run.
func (r *registry) assignLocked(ref toolRef, prefix string) string {
	key := ref.Server + "\x00" + ref.Tool
	want := publishedName(prefix, ref.Tool)
	if old, ok := r.assigned[key]; ok {
		if r.assignedBase[key] == want {
			return old
		}
		// The prefix changed: the tool is renamed, and the name it used to have keeps pointing at it.
		r.retired[old] = ref
	}
	name := r.freeNameLocked(want, ref)
	if name != want {
		r.collisionNote(ref.Server, ref.Tool, name, want)
	}
	r.assigned[key], r.assignedBase[key] = name, want
	return name
}

// freeNameLocked chooses what to publish when the wanted name is already owned by another tool.
//
// A collision is not solved with a numbered suffix: "fs_read_2" tells the reader nothing about which
// server it came from. The colliding tool is published under "<server>_<tool>" instead, which says
// exactly that, and only if even that name is taken does a number appear. The first tool to claim a
// name keeps it — renaming tools every time a server is added would invalidate the KV prefix of every
// session that already has history.
func (r *registry) freeNameLocked(want string, ref toolRef) string {
	if r.claimableLocked(want, ref) {
		return want
	}
	if fallback := publishedName(ref.Server, ref.Tool); fallback != want {
		if r.claimableLocked(fallback, ref) {
			return fallback
		}
		for i := 2; i < 100; i++ {
			cand := fmt.Sprintf("%s_%d", fallback, i)
			if r.claimableLocked(cand, ref) {
				return cand
			}
		}
	}
	return want
}

// claimableLocked reports whether a name is free for this tool: unused, or one this very tool owned
// before a rename, in which case the alias is consumed by taking it back — so a prefix that was
// changed and then changed back puts the tool under the name a transcript already uses.
func (r *registry) claimableLocked(name string, ref toolRef) bool {
	if owner, retired := r.retired[name]; retired {
		if owner == ref {
			delete(r.retired, name)
			return true
		}
		return false
	}
	for _, existing := range r.assigned {
		if existing == name {
			return false
		}
	}
	return true
}

// nameTakenLocked reports whether a name belongs to somebody other than this tool. It mutates
// nothing, so the drawer (which only holds a read lock) can ask what a switched-off tool would be
// called: a tool that is off is never passed to assignLocked, so its assignment is frozen at the
// name it last had, which says nothing about the prefix in force now.
func (r *registry) nameTakenLocked(name string, ref toolRef) bool {
	if owner, retired := r.retired[name]; retired {
		return owner != ref
	}
	key := ref.Server + "\x00" + ref.Tool
	for assigned, existing := range r.assigned {
		if existing == name && assigned != key {
			return true
		}
	}
	return false
}

// policyOfLocked returns the policy for one server: the configured one, or the default (every tool
// on, no prefix) for a server nobody configured.
func (r *registry) policyOfLocked(server string) toolPolicy {
	if p, ok := r.policy[server]; ok {
		return p
	}
	return toolPolicy{}
}

// SetPolicy installs this session's tool policy and republishes the tool array from the cached
// tools/list results. It never re-lists and never reconnects: switching one tool off must not
// take a working MCP server down with it, and must not restart a stdio child process. Call it
// before connect too, so a server's tools are filtered from the moment they are first listed.
// It reports whether the published array changed, which is what a caller needs to know to decide
// whether the prompt prefix just moved.
func (r *registry) SetPolicy(cfgs []mcp.Config) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := make([]llm.Tool, len(r.defs))
	copy(before, r.defs)
	r.policy = policiesOf(cfgs)
	r.rebuildLocked()
	return !sameToolNames(before, r.defs)
}

// PolicyPending reports whether cfgs say something different from the policy the registry is
// publishing right now — a tool change that is written to config.json but has not taken effect,
// because a run is carrying the previous array. Deriving it from the two policies instead of
// from a flag is what makes "did the change land?" a question with one answer.
func (r *registry) PolicyPending(cfgs []mcp.Config) bool {
	next := policiesOf(cfgs)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !samePolicies(r.policy, next)
}

// policiesOf turns MCP configs into the registry's own policy shape. An empty toolPrefix means the
// tool's own name (no namespace at all), so unlike an omitted transport field it is never inferred
// from the server name — "no prefix" and "a prefix that happens to equal the server name" are two
// different published names and must not compare equal here.
func policiesOf(cfgs []mcp.Config) map[string]toolPolicy {
	next := make(map[string]toolPolicy, len(cfgs))
	for _, c := range cfgs {
		p := toolPolicy{prefix: strings.TrimSpace(c.ToolPrefix)}
		if len(c.DisabledTools) > 0 {
			p.off = append([]string(nil), c.DisabledTools...)
		}
		next[c.Name] = p
	}
	return next
}

func samePolicies(a, b map[string]toolPolicy) bool {
	if len(a) != len(b) {
		return false
	}
	for name, p := range a {
		q, ok := b[name]
		if !ok || p.prefix != q.prefix || len(p.off) != len(q.off) {
			return false
		}
		for i := range p.off {
			if p.off[i] != q.off[i] {
				return false
			}
		}
	}
	return true
}

// sameToolNames compares two published arrays by name: a policy change can add, remove or rename
// entries, it can never change a description or a schema.
func sameToolNames(a, b []llm.Tool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
	}
	return true
}

// toolDesc is the description the model is shown: the server's, else the title, else the
// annotation title.
func toolDesc(t mcp.Tool) string {
	desc := t.Description
	if desc == "" {
		desc = t.Title
	}
	if desc == "" && t.Annotations != nil {
		desc = t.Annotations.Title
	}
	return desc
}

// normalizeSchema makes sure the parameters field is a JSON object schema, since some
// providers reject a missing or malformed one.
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	if _, ok := v["type"]; !ok {
		v["type"] = "object"
	}
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return out
}

// estToolTokens is the bytes/4 estimate the inspector already uses for payload sizes. It is a
// way to rank tools against each other, not a measurement: what one tool definition actually
// costs in a rendered prompt was measured at ~118 tokens (Gemma 12B) to ~165 (Qwen 27B), which
// is the order of magnitude this is a proxy for. Anything displaying it says est.
func estToolTokens(name, desc string, schema json.RawMessage) int {
	return (len(name) + len(desc) + len(schema)) / 4
}

// Tools returns the current model-visible tool definitions.
func (r *registry) Tools() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, len(r.defs))
	copy(out, r.defs)
	return out
}

// resolve maps a model-visible name back to (server, tool). A name that is not published — one
// the session switched off, or one nobody ever assigned — does not resolve, which is what makes
// a switched-off tool uncallable rather than merely hidden.
func (r *registry) resolve(name string) (toolRef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.byName[name]
	return ref, ok
}

// call executes one tool call, returning the result and the reference it was resolved to.
func (r *registry) call(ctx context.Context, name string, args json.RawMessage, on func(mcp.Event)) (*mcp.CallResult, toolRef, error) {
	ref, ok := r.resolve(name)
	if !ok {
		return nil, toolRef{}, fmt.Errorf("unknown tool %q", name)
	}
	r.mu.RLock()
	cli := r.servers[ref.Server]
	r.mu.RUnlock()
	if cli == nil {
		return nil, ref, fmt.Errorf("mcp server %q is not connected", ref.Server)
	}
	res, err := cli.CallTool(ctx, ref.Tool, args, on)
	return res, ref, err
}

// counts reports (published now, enabled by policy, offered in total) for the status line. The
// first two differ while a run holds a snapshot of the array and a policy change is waiting.
func (r *registry) counts() (published, enabled, total int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	published = len(r.defs)
	for name, tools := range r.last {
		total += len(tools)
		pol := r.policyOfLocked(name)
		for _, t := range tools {
			if pol.on(t.Name) {
				enabled++
			}
		}
	}
	return published, enabled, total
}

// view reports every tool this session's servers have ever listed, published or not — the tools
// drawer's data. It reads the cache, so it answers while a run is in flight and never asks a
// server anything (a tool of an offline server is still worth showing: that is when deciding
// whether to keep it is free).
func (r *registry) view(cfgs []mcp.Config) *ToolsView {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(cfgs))
	byServer := map[string]mcp.Config{}
	for _, c := range cfgs {
		if _, dup := byServer[c.Name]; !dup {
			names = append(names, c.Name)
		}
		byServer[c.Name] = c
	}
	for name := range r.last {
		if _, ok := byServer[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := &ToolsView{AppliesTo: "next-run"}
	for _, name := range names {
		cfg := byServer[name]
		st := r.status[name]
		pol := r.policyOfLocked(name)
		prefix := pol.prefix // empty means the tool's own name, which is the default
		if prefix != "" {
			// What is in force, not what was typed: publishedName sanitises too, and a hand-written
			// config could say "my tools" while every tool says my_tools. ConfiguredPrefix shows the raw.
			prefix = sanitize(prefix)
		}
		tools := append([]mcp.Tool(nil), r.last[name]...)
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

		ts := ToolsServer{
			Name:             name,
			Transport:        transportOf(cfg),
			Connected:        st.Connected,
			Disabled:         st.Disabled || cfg.Disabled,
			Error:            st.Error,
			Impl:             st.Impl,
			Prefix:           prefix,
			ConfiguredPrefix: cfg.ToolPrefix,
			Known:            len(tools),
		}
		for _, t := range tools {
			enabled := pol.on(t.Name)
			ref := toolRef{Server: name, Tool: t.Name}
			want := publishedName(pol.prefix, t.Name)
			published := r.assigned[name+"\x00"+t.Name]
			if !enabled || published == "" {
				// Nothing to trust here: a switched-off tool keeps whatever name it last had, and an
				// unconnected server never got one. Show the name it would be published under, which is
				// the name the switch is about.
				published = want
				if r.nameTakenLocked(want, ref) {
					published = publishedName(name, t.Name) // <server>_<tool>, as freeNameLocked would
				}
			}
			row := ToolRow{
				Name:        published,
				RawName:     t.Name,
				Title:       t.Title,
				Description: clip(toolDesc(t), 240),
				Schema:      normalizeSchema(t.InputSchema),
				Enabled:     enabled,
				EstTokens:   estToolTokens(published, toolDesc(t), t.InputSchema),
			}
			if published != want {
				row.RenamedFrom = want // a collision published it under <server>_<tool>
			}
			if a := t.Annotations; a != nil && a.ReadOnlyHint != nil {
				row.ReadOnly = *a.ReadOnlyHint
			}
			ts.Tools = append(ts.Tools, row)
			if enabled {
				ts.Enabled++
			}
		}
		if len(tools) == 0 {
			// Configured but nothing known: never connected, or tools/list failed. An empty box
			// without a reason reads like a bug, so say which of the two it is.
			ts.Note = "no tools/list result yet"
			if ts.Error != "" {
				ts.Note = "tools/list failed: " + ts.Error
			}
		}
		out.Servers = append(out.Servers, ts)
		out.Total += len(tools)
		out.Visible += ts.Enabled
	}
	return out
}

// statusSnapshot reports the servers for the UI.
func (r *registry) statusSnapshot() []ServerStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.status))
	for n := range r.status {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ServerStatus, 0, len(names))
	for _, n := range names {
		out = append(out, r.status[n])
	}
	return out
}

// closeAll shuts every MCP server down (stdio children are terminated by the transport). Name
// assignments survive: a name is never handed out twice, so reconnecting the same tools puts them
// back under the names a transcript already uses.
func (r *registry) closeAll() {
	r.mu.Lock()
	servers := r.servers
	r.servers = map[string]*mcp.Client{}
	r.byName = map[string]toolRef{}
	r.last = map[string][]mcp.Tool{}
	r.defs = nil
	r.mu.Unlock()
	for _, c := range servers {
		_ = c.Close()
	}
}

// askElicit is wired by Session; errNoUI is returned when no client can answer.
var errNoUI = fmt.Errorf("no client connected to answer the input request")

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
