package agent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// There are no built-in tools: everything the model may call comes from an MCP server. Tool
// names are prefixed with the server name and sanitised to the character set OpenAI-style
// function calling accepts, and a mapping back to (server, tool) is kept. Assigned names are
// never re-used, so adding a tool never renames the others and the `tools` array the chat
// template renders stays stable (spec §5.3).
type registry struct {
	mu      sync.RWMutex
	servers map[string]*mcp.Client
	order   []string // deterministic server order
	assigned map[string]string // "server\x00tool" -> model-visible name
	byName   map[string]toolRef
	defs     []llm.Tool
	status   map[string]ServerStatus
	errs     map[string]string
	emit     func(Event)
	logf     func(string, ...any)
	notify   bool
	// askElicit forwards a server input request to the user; set by the owning Session.
	askElicit func(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResponse, error)
}

type toolRef struct {
	Server string
	Tool   string
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
		servers:  map[string]*mcp.Client{},
		assigned: map[string]string{},
		byName:   map[string]toolRef{},
		status:   map[string]ServerStatus{},
		errs:     map[string]string{},
		emit:     emit,
		logf:     logf,
		notify:   notify,
	}
	if logf == nil {
		r.logf = func(string, ...any) {}
	}
	return r
}

// connect starts/connects every configured server. Failures are recorded per server: one
// broken MCP server must not stop the session (fail open, tools from it are just missing).
func (r *registry) connect(ctx context.Context, cfgs []mcp.Config) {
	r.mu.Lock()
	r.order = r.order[:0]
	for _, c := range cfgs {
		r.order = append(r.order, c.Name)
	}
	r.mu.Unlock()

	for _, c := range cfgs {
		cfg := c
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

// onServerEvent forwards MCP notifications to the UI and refreshes the tool list when the
// server says it changed.
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

// refreshTools re-lists every server's tools and rebuilds the model-visible tool array.
// The array is sorted (server, tool) so the rendered prefix is deterministic.
func (r *registry) refreshTools(ctx context.Context) ([]llm.Tool, error) {
	r.mu.RLock()
	servers := make([]string, 0, len(r.servers))
	for name := range r.servers {
		servers = append(servers, name)
	}
	cli := func(name string) *mcp.Client { return r.servers[name] }
	r.mu.RUnlock()
	sort.Strings(servers)

	type entry struct {
		ref  toolRef
		tool mcp.Tool
	}
	var all []entry
	var firstErr error
	for _, name := range servers {
		tools, err := cli(name).Tools(ctx)
		r.mu.Lock()
		st := r.status[name]
		st.Tools = len(tools)
		r.status[name] = st
		r.mu.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", name, err)
			}
			r.logf("mcp %s: tools/list: %v", name, err)
			continue
		}
		for _, t := range tools {
			all = append(all, entry{ref: toolRef{Server: name, Tool: t.Name}, tool: t})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ref.Server != all[j].ref.Server {
			return all[i].ref.Server < all[j].ref.Server
		}
		return all[i].ref.Tool < all[j].ref.Tool
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	byName := map[string]toolRef{}
	defs := make([]llm.Tool, 0, len(all))
	for _, e := range all {
		key := e.ref.Server + "\x00" + e.ref.Tool
		name, ok := r.assigned[key]
		if !ok {
			name = r.uniqueName(e.ref)
			r.assigned[key] = name
		}
		byName[name] = e.ref
		desc := e.tool.Description
		if e.tool.Title != "" && desc == "" {
			desc = e.tool.Title
		}
		if extra := e.tool.Annotations; extra != nil && extra.Title != "" {
			if desc == "" {
				desc = extra.Title
			}
		}
		defs = append(defs, llm.Tool{
			Name: name, Description: clip(desc, 4000), Parameters: normalizeSchema(e.tool.InputSchema),
		})
	}
	r.byName, r.defs = byName, defs
	return defs, firstErr
}

// uniqueName builds "<server>__<tool>", sanitised and length-bounded, avoiding collisions.
func (r *registry) uniqueName(ref toolRef) string {
	base := sanitize(ref.Server) + "__" + sanitize(ref.Tool)
	if len(base) > 60 {
		h := sha1.Sum([]byte(ref.Server + "\x00" + ref.Tool))
		base = base[:40] + "_" + hex.EncodeToString(h[:])[:12]
	}
	name := base
	for i := 2; ; i++ {
		taken := false
		for _, existing := range r.assigned {
			if existing == name {
				taken = true
				break
			}
		}
		if !taken {
			return name
		}
		name = fmt.Sprintf("%s_%d", base, i)
	}
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

// Tools returns the current model-visible tool definitions.
func (r *registry) Tools() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]llm.Tool, len(r.defs))
	copy(out, r.defs)
	return out
}

// resolve maps a model-visible name back to (server, tool).
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

// closeAll shuts every MCP server down (stdio children are terminated by the transport).
func (r *registry) closeAll() {
	r.mu.Lock()
	servers := r.servers
	r.servers = map[string]*mcp.Client{}
	r.byName = map[string]toolRef{}
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
