// Package mcp is a client for the two MCP revisions this project supports:
//
//   - 2026-07-28 — stateless: no initialize handshake, every request carries
//     _meta.io.modelcontextprotocol/*, server/discover is the only discovery probe,
//     resultType marks completion and inputRequests + requestState implement multi
//     round-trip requests instead of server-initiated requests.
//   - 2025-11-25 — initialize / notifications/initialized handshake, Mcp-Session-Id,
//     a GET SSE stream, and server-initiated JSON-RPC requests.
//
// Both revisions run over stdio (newline-delimited JSON-RPC) and streamable HTTP.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol eras this client can speak.
const (
	eraModern = "modern" // 2026-07-28
	eraLegacy = "legacy" // 2025-11-25
)

// maxInputRounds bounds the multi round-trip loop.
const maxInputRounds = 16

// additional methods used by this client.
const (
	mResourcesRead = "resources/read"
	mPromptsGet    = "prompts/get"
	mPing          = "ping"
)

// configurable is what both transports implement.
type configurable interface {
	transport
	setNotifyHandler(func(*msg))
	stream(ctx context.Context, method string, params any, onMsg func(*msg)) (func(), error)
}

// headerMirrorer is implemented by the HTTP transport (x-mcp-header -> Mcp-Param-*).
type headerMirrorer interface {
	mirrored(schema json.RawMessage, args json.RawMessage) map[string]string
}

// Root is one root the client offers (legacy roots/list).
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// Config describes one MCP server.
type Config struct {
	Name string `json:"name"`
	// Type is "stdio" (default) or "http".
	Type string `json:"type,omitempty"`

	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     []string          `json:"env,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Timeout bounds a single round trip (streams are exempt). Default 60s.
	Timeout time.Duration `json:"-"`
	HTTP    *http.Client  `json:"-"`
	// Startup bounds the connect/negotiate phase. Default 30s.
	Startup time.Duration                    `json:"-"`
	Logf    func(format string, args ...any) `json:"-"`
	// Roots are offered to legacy servers that ask for them.
	Roots []Root `json:"roots,omitempty"`
}

// Event is a notification received from the server.
type Event struct {
	Server string          `json:"server"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ElicitRequest is a server asking the user for structured input, either as a legacy
// server-initiated request or as a 2026-07-28 input request inside a tool result.
type ElicitRequest struct {
	Server   string          `json:"server"`
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	Message  string          `json:"message"`
	Mode     string          `json:"mode,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	Pending  []InputRequest  `json:"pending,omitempty"`
	Round    int             `json:"round"`
	ToolName string          `json:"tool,omitempty"`
}

// ElicitResponse is the user's answer.
type ElicitResponse struct {
	// Behavior is accept, decline or cancel.
	Behavior string         `json:"behavior"`
	Content  map[string]any `json:"content,omitempty"`
}

// Client is one connected MCP server.
type Client struct {
	cfg  Config
	logf func(string, ...any)
	t    transport

	mu           sync.RWMutex
	era          string
	version      string
	info         Implementation
	caps         ServerCapabilities
	instructions string
	tools        []Tool
	toolSchema   map[string]json.RawMessage
	toolsAt      time.Time
	toolsTTL     time.Duration

	// OnEvent receives server notifications (list_changed, log, progress, ...).
	OnEvent func(Event)
	// Elicit answers server input requests. nil means "decline".
	Elicit func(context.Context, ElicitRequest) (ElicitResponse, error)

	stopOnce  sync.Once
	stopReq   context.CancelFunc
	subCancel func()
	closed    atomic.Bool
}

// Connect starts (or connects to) the server and negotiates the protocol revision.
//
// Negotiation order follows the 2026-07-28 probing rules: send server/discover first; a
// modern JSON-RPC error identifies a modern server speaking another version; anything else
// (method-not-found, transport error, garbage) falls back to the legacy handshake.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Startup == 0 {
		cfg.Startup = 30 * time.Second
	}
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = cfg.Command
		if cfg.Name == "" {
			cfg.Name = cfg.URL
		}
	}
	c := &Client{cfg: cfg, logf: cfg.Logf, era: eraModern, toolSchema: map[string]json.RawMessage{}}

	var t transport
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "", "stdio":
		st, err := newStdio(ctx, StdioConfig{
			Command: cfg.Command, Args: cfg.Args, Env: cfg.Env, Dir: cfg.Dir, Logf: cfg.Logf,
		})
		if err != nil {
			return nil, err
		}
		t = st
	case "http", "streamable-http", "streamable_http", "sse":
		t = newHTTP(HTTPConfig{URL: cfg.URL, Headers: cfg.Headers, Timeout: cfg.Timeout, HTTP: cfg.HTTP, Logf: cfg.Logf}, eraModern)
	default:
		return nil, fmt.Errorf("mcp: unknown transport type %q", cfg.Type)
	}
	c.t = t
	if cc, ok := t.(configurable); ok {
		cc.setNotifyHandler(c.onNotification)
	}

	nctx, cancel := context.WithTimeout(ctx, cfg.Startup)
	defer cancel()
	if err := c.negotiate(nctx); err != nil {
		_ = t.close()
		return nil, err
	}

	sctx, scancel := context.WithCancel(context.Background())
	c.stopReq = scancel
	go c.serveServerRequests(sctx)
	return c, nil
}

// modernMeta builds the _meta object every 2026-07-28 request must carry.
func modernMeta(version string) map[string]any {
	return map[string]any{
		metaProtocolVersion: version,
		metaClientCaps: map[string]any{
			"roots":       map[string]any{"listChanged": false},
			"elicitation": map[string]any{},
		},
		metaClientInfo: impl,
	}
}

// params builds a params object for one request, injecting _meta on the modern path.
func (c *Client) params(base map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	c.mu.RLock()
	era, version := c.era, c.version
	c.mu.RUnlock()
	if era != eraModern {
		return base
	}
	if _, ok := base[metaKey]; !ok {
		base[metaKey] = modernMeta(version)
	}
	return base
}

// versionFromError reads the supported versions out of an UnsupportedProtocolVersion error.
func versionFromError(e *rpcError) (string, []string) {
	if len(e.Data) == 0 {
		return "", nil
	}
	var d struct {
		Supported         []string `json:"supported"`
		SupportedVersions []string `json:"supportedVersions"`
	}
	_ = json.Unmarshal(e.Data, &d)
	list := d.Supported
	if len(list) == 0 {
		list = d.SupportedVersions
	}
	if len(list) == 0 {
		return "", nil
	}
	for _, v := range list {
		if v == protocolModernVersion {
			return v, list
		}
	}
	return list[0], list
}

// pickVersion chooses the newest version we support.
func pickVersion(d *DiscoverResult) string {
	if d != nil {
		if d.LatestVersion != "" {
			return d.LatestVersion
		}
		for _, v := range d.SupportedVersions {
			if v == protocolModernVersion {
				return v
			}
		}
		if len(d.SupportedVersions) > 0 {
			return d.SupportedVersions[0]
		}
		for _, v := range d.Versions {
			if v.Available == nil || *v.Available {
				return v.Version
			}
		}
	}
	return protocolModernVersion
}

func (c *Client) negotiate(ctx context.Context) error {
	// ---- modern probe ----
	raw, rpcErr, err := c.t.call(ctx, mDiscover, map[string]any{metaKey: modernMeta(protocolModernVersion)}, nil, nil)
	switch {
	case err == nil && rpcErr == nil:
		var d DiscoverResult
		if e := json.Unmarshal(raw, &d); e == nil {
			c.applyDiscover(&d, pickVersion(&d))
			c.logf("%s: %s (stateless %s)", c.cfg.Name, c.info.Name+"/"+c.info.Version, c.version)
			return nil
		}
		err = fmt.Errorf("bad server/discover result: %.200s", string(raw))
	case rpcErr != nil && rpcErr.Modern():
		v, list := versionFromError(rpcErr)
		if v == "" {
			v = protocolModernVersion
		}
		_ = list
		c.mu.Lock()
		c.era, c.version = eraModern, v
		c.mu.Unlock()
		if ht, ok := c.t.(*httpTransport); ok {
			ht.setEra(eraModern)
		}
		c.logf("%s: modern server, negotiating %s (server supports %v)", c.cfg.Name, v, list)
		return nil
	}
	if rpcErr != nil {
		c.logf("%s: no server/discover (%v) — falling back to %s handshake", c.cfg.Name, rpcErr, protocolLegacyVersion)
	} else if err != nil {
		c.logf("%s: server/discover failed (%v) — falling back to %s handshake", c.cfg.Name, err, protocolLegacyVersion)
	}

	// ---- legacy handshake ----
	params := map[string]any{
		"protocolVersion": protocolLegacyVersion,
		"capabilities": map[string]any{
			"roots":       map[string]any{"listChanged": false},
			"elicitation": map[string]any{},
		},
		"clientInfo": impl,
	}
	raw, rpcErr, err = c.t.call(ctx, mInitialize, params, nil, nil)
	if err == nil && rpcErr == nil {
		var res InitializeResult
		if e := json.Unmarshal(raw, &res); e == nil {
			c.mu.Lock()
			c.era, c.version = eraLegacy, res.ProtocolVersion
			if c.version == "" {
				c.version = protocolLegacyVersion
			}
			c.info = res.ServerInfo
			c.instructions = res.Instructions
			_ = json.Unmarshal(mustMarshal(res.Capabilities), &c.caps)
			c.mu.Unlock()
			if ht, ok := c.t.(*httpTransport); ok {
				ht.setEra(eraLegacy)
			}
			if err := c.t.notify(ctx, mInitialized, nil); err != nil {
				c.logf("%s: notifications/initialized: %v", c.cfg.Name, err)
			}
			c.startLegacyStream(ctx)
			c.logf("%s: %s (%s handshake, %s)", c.cfg.Name, c.info.Name+"/"+c.info.Version, c.version, "initialize")
			return nil
		}
		err = fmt.Errorf("bad initialize result: %.200s", string(raw))
	}
	if rpcErr != nil {
		return fmt.Errorf("mcp %s: initialize: %w", c.cfg.Name, rpcErr)
	}
	if err != nil {
		return fmt.Errorf("mcp %s: initialize: %w", c.cfg.Name, err)
	}
	return fmt.Errorf("mcp %s: could not negotiate a protocol version", c.cfg.Name)
}

func (c *Client) applyDiscover(d *DiscoverResult, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.era, c.version = eraModern, version
	c.info = d.ServerInfo()
	_ = json.Unmarshal(mustMarshal(d.Capabilities), &c.caps)
	if d.TTLMs != nil && *d.TTLMs > 0 {
		c.toolsTTL = time.Duration(*d.TTLMs) * time.Millisecond
	}
}

// startLegacyStream opens the optional GET SSE stream (server-initiated messages).
func (c *Client) startLegacyStream(ctx context.Context) {
	ht, ok := c.t.(*httpTransport)
	if !ok {
		return
	}
	stop, err := ht.getStream(ctx)
	if err != nil {
		c.logf("%s: no GET stream (%v) — server-initiated messages may be unavailable", c.cfg.Name, err)
		return
	}
	c.subCancel = stop
}

func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// onNotification handles notifications unrelated to an in-flight request.
func (c *Client) onNotification(m *msg) {
	if m.Method == "" {
		return
	}
	c.mu.Lock()
	changed := false
	switch m.Method {
	case mToolsListChanged:
		c.tools = nil
		c.toolsAt = time.Time{}
		changed = true
	}
	c.mu.Unlock()
	if changed {
		c.logf("%s: tools list changed, will refresh", c.cfg.Name)
	}
	if c.OnEvent != nil {
		c.OnEvent(Event{Server: c.cfg.Name, Method: m.Method, Params: m.Params})
	}
}

// serveServerRequests answers server-initiated requests (legacy revision only).
func (c *Client) serveServerRequests(ctx context.Context) {
	in := c.t.incoming()
	if in == nil {
		return
	}
	for m := range in {
		go c.handleServerRequest(ctx, m)
	}
}

func (c *Client) handleServerRequest(ctx context.Context, m *msg) {
	if len(m.ID) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	switch m.Method {
	case mPing:
		_ = c.t.respond(ctx, m.ID, map[string]any{}, nil)
	case mElicitation:
		req := ElicitRequest{Server: c.cfg.Name, ID: idKey(m.ID), Method: m.Method, Params: m.Params}
		var p struct {
			Message         string          `json:"message"`
			Mode            string          `json:"mode"`
			RequestedSchema json.RawMessage `json:"requestedSchema"`
			InputSchema     json.RawMessage `json:"inputSchema"`
		}
		_ = json.Unmarshal(m.Params, &p)
		req.Message, req.Mode = p.Message, p.Mode
		req.Schema = p.RequestedSchema
		if len(req.Schema) == 0 {
			req.Schema = p.InputSchema
		}
		resp := c.askUser(cctx, req)
		_ = c.t.respond(ctx, m.ID, elicitationResult(resp), nil)
	case mRootsList:
		c.mu.RLock()
		roots := c.cfg.Roots
		c.mu.RUnlock()
		_ = c.t.respond(ctx, m.ID, map[string]any{"roots": roots}, nil)
	case "logging/setLevel":
		_ = c.t.respond(ctx, m.ID, map[string]any{}, nil)
	default:
		_ = c.t.respond(ctx, m.ID, nil, &rpcError{
			Code: CodeMethodNotFound, Message: "unsupported server request: " + m.Method,
		})
	}
}

// elicitationResult renders the legacy-shaped elicitation result.
func elicitationResult(r ElicitResponse) map[string]any {
	b := r.Behavior
	if b == "" {
		b = "decline"
	}
	out := map[string]any{"action": b, "behavior": b}
	if len(r.Content) > 0 {
		out["content"] = r.Content
	}
	return out
}

func (c *Client) askUser(ctx context.Context, req ElicitRequest) ElicitResponse {
	if c.Elicit == nil {
		return ElicitResponse{Behavior: "decline"}
	}
	resp, err := c.Elicit(ctx, req)
	if err != nil {
		c.logf("%s: elicitation failed: %v", c.cfg.Name, err)
		return ElicitResponse{Behavior: "cancel"}
	}
	if resp.Behavior == "" {
		resp.Behavior = "decline"
	}
	return resp
}

// Era reports the negotiated era ("modern" or "legacy").
func (c *Client) Era() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.era }

// Version reports the negotiated protocol version string.
func (c *Client) Version() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.version }

// Name reports the configured server name.
func (c *Client) Name() string { return c.cfg.Name }

// Info reports the server implementation.
func (c *Client) Info() Implementation { c.mu.RLock(); defer c.mu.RUnlock(); return c.info }

// Instructions reports the legacy server instructions string.
func (c *Client) Instructions() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.instructions }

// Capabilities reports the parsed server capability map.
func (c *Client) Capabilities() ServerCapabilities { c.mu.RLock(); defer c.mu.RUnlock(); return c.caps }

// Modern reports whether the modern (2026-07-28) revision is in use.
func (c *Client) Modern() bool { return c.Era() == eraModern }

// Tools returns the cached tool list, refreshing when empty or stale.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	c.mu.RLock()
	tools, at, ttl := c.tools, c.toolsAt, c.toolsTTL
	c.mu.RUnlock()
	if len(tools) > 0 && (ttl <= 0 || time.Since(at) < ttl) {
		return tools, nil
	}
	return c.refreshTools(ctx)
}

// refreshTools walks tools/list pagination.
func (c *Client) refreshTools(ctx context.Context) ([]Tool, error) {
	var out []Tool
	cursor := ""
	for page := 0; page < 100; page++ {
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		raw, rpc, err := c.t.call(ctx, mToolsList, c.params(p), nil, nil)
		if err != nil {
			return nil, err
		}
		if rpc != nil {
			return nil, rpc
		}
		var res ToolsListResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp %s: bad tools/list: %.200s", c.cfg.Name, string(raw))
		}
		out = append(out, res.Tools...)
		cursor = res.NextCursor
		if cursor == "" {
			break
		}
	}
	schema := map[string]json.RawMessage{}
	for _, t := range out {
		schema[t.Name] = t.InputSchema
	}
	c.mu.Lock()
	c.tools, c.toolSchema, c.toolsAt = out, schema, time.Now()
	c.mu.Unlock()
	return out, nil
}

// Tool looks up one cached tool.
func (c *Client) Tool(name string) (Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, t := range c.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// CallResult is a completed tool call.
type CallResult struct {
	Content    []ContentBlock  `json:"content,omitempty"`
	Structured json.RawMessage `json:"structuredContent,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	Rounds     int             `json:"rounds"`
	Raw        json.RawMessage `json:"-"`
}

// Text flattens the text content blocks.
func (r *CallResult) Text() string {
	var sb strings.Builder
	for _, c := range r.Content {
		t := c.Text2()
		if t == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(t)
	}
	if sb.Len() == 0 && len(r.Structured) > 0 {
		return string(r.Structured)
	}
	return sb.String()
}

// CallTool calls tools/call, running the multi round-trip loop when the server answers
// with resultType=input_required (2026-07-28). Each retry is a new JSON-RPC request
// carrying requestState back unchanged.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage, onMsg func(Event)) (*CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	c.mu.RLock()
	schema := c.toolSchema[name]
	c.mu.RUnlock()

	var opts *callOpts
	if hm, ok := c.t.(headerMirrorer); ok {
		if h := hm.mirrored(schema, args); len(h) > 0 {
			opts = &callOpts{Name: name, Headers: h}
		}
	} else if c.Modern() {
		opts = &callOpts{Name: name}
	}

	var state json.RawMessage
	var responses map[string]any
	round := 0
	for {
		round++
		if round > maxInputRounds {
			return nil, fmt.Errorf("mcp %s: tool %s asked for input %d times", c.cfg.Name, name, round)
		}
		p := map[string]any{"name": name, "arguments": json.RawMessage(args)}
		if len(responses) > 0 {
			p["inputResponses"] = responses
		}
		if len(state) > 0 && string(state) != "null" {
			p["requestState"] = json.RawMessage(state)
		}
		raw, rpc, err := c.t.call(ctx, mToolsCall, c.params(p), func(m *msg) {
			if onMsg != nil {
				onMsg(Event{Server: c.cfg.Name, Method: m.Method, Params: m.Params})
			}
		}, opts)
		if err != nil {
			return nil, err
		}
		if rpc != nil {
			return nil, rpc
		}
		var out ToolOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("mcp %s: bad tools/call result: %.200s", c.cfg.Name, string(raw))
		}
		if out.Complete() {
			return &CallResult{
				Content: out.Content, Structured: out.StructuredContent,
				IsError: out.IsError, Rounds: round, Raw: raw,
			}, nil
		}
		// resultType=input_required: answer every input request, then retry.
		if len(out.InputRequests) == 0 {
			return nil, fmt.Errorf("mcp %s: input_required without inputRequests", c.cfg.Name)
		}
		responses = map[string]any{}
		for _, req := range out.InputRequests {
			responses[req.ID] = c.answerInput(ctx, req, name, round, out.InputRequests)
		}
		state = out.RequestState
		c.logf("%s: tool %s round %d: %d input request(s)", c.cfg.Name, name, round, len(out.InputRequests))
	}
}

// answerInput handles one input request of a multi round-trip tool result.
func (c *Client) answerInput(ctx context.Context, req InputRequest, tool string, round int, pending []InputRequest) any {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	switch req.Method {
	case mElicitation:
		var p struct {
			Message         string          `json:"message"`
			Mode            string          `json:"mode"`
			RequestedSchema json.RawMessage `json:"requestedSchema"`
			InputSchema     json.RawMessage `json:"inputSchema"`
		}
		_ = json.Unmarshal(req.Params, &p)
		sch := p.RequestedSchema
		if len(sch) == 0 {
			sch = p.InputSchema
		}
		resp := c.askUser(cctx, ElicitRequest{
			Server: c.cfg.Name, ID: req.ID, Method: req.Method, Message: p.Message, Mode: p.Mode,
			Schema: sch, Params: req.Params, Pending: pending, Round: round, ToolName: tool,
		})
		return elicitationResult(resp)
	case mRootsList:
		c.mu.RLock()
		roots := c.cfg.Roots
		c.mu.RUnlock()
		return map[string]any{"roots": roots}
	case mSamplingCreate:
		c.logf("%s: sampling/createMessage is not supported; declining", c.cfg.Name)
		return map[string]any{"behavior": "decline", "action": "decline"}
	default:
		c.logf("%s: unsupported input request %s; declining", c.cfg.Name, req.Method)
		return map[string]any{"behavior": "decline", "action": "decline"}
	}
}

// Subscribe starts subscriptions/listen (2026-07-28). It returns false when the server is
// on the legacy revision (which pushes over the GET SSE stream instead).
func (c *Client) Subscribe(ctx context.Context, notifications map[string]bool, on EventFunc) (bool, error) {
	cc, ok := c.t.(configurable)
	if !ok {
		return false, nil
	}
	if !c.Modern() {
		return false, nil
	}
	params := map[string]any{"notifications": notifications}
	stop, err := cc.stream(ctx, mSubscribe, c.params(params), func(m *msg) {
		if m.Method != "" && on != nil {
			on(Event{Server: c.cfg.Name, Method: m.Method, Params: m.Params})
		}
	})
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	c.subCancel = stop
	c.mu.Unlock()
	return true, nil
}

// EventFunc receives server events.
type EventFunc func(Event)

// Ping checks the connection (legacy `ping`; modern `server/discover`).
func (c *Client) Ping(ctx context.Context) error {
	method := mDiscover
	var p any = map[string]any{metaKey: modernMeta(c.Version())}
	if !c.Modern() {
		method, p = mPing, map[string]any{}
	}
	_, rpc, err := c.t.call(ctx, method, p, nil, nil)
	if err != nil {
		return err
	}
	if rpc != nil && !rpc.Modern() {
		return rpc
	}
	return nil
}

// Close shuts the connection down.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.stopOnce.Do(func() {
		if c.stopReq != nil {
			c.stopReq()
		}
		c.mu.Lock()
		if c.subCancel != nil {
			c.subCancel()
		}
		c.mu.Unlock()
	})
	return c.t.close()
}

// ErrDeclined is returned when the user declines a server input request.
var ErrDeclined = errors.New("mcp: input request declined")
