// Command mcpstub is a test MCP server used to exercise internal/mcp without a third-party
// server. It speaks both protocol eras and both transports:
//
//	mcpstub -transport stdio                     // newline-delimited JSON-RPC on stdin/stdout
//	mcpstub -transport http -addr 127.0.0.1:8911 // POST /mcp
//	mcpstub -version 2025-11-25                  // legacy: initialize handshake + server-initiated requests
//
// In the 2026-07-28 era there is no initialize: the client probes server/discover and every
// request carries its identity in params._meta. In the 2025-11-25 era the handshake is required
// and the server may ask the client questions with real JSON-RPC requests (stdio only; the HTTP
// stub answers tools/call with a plain JSON body, so multi round-trip is demonstrated instead by
// the MRTR path: resultType=input_required + inputRequests + requestState).
//
// Tools: echo, add, now, image, big, slow, fail, headers, need_input.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	modern  = "2026-07-28"
	legacy  = "2025-11-25"
	metaKey = "_meta"
)

var serverInfo = map[string]string{"name": "mcpstub", "version": "0.1.0", "title": "MCP test stub"}

// tinyPNG is a 1x1 transparent PNG, returned by the image tool.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Bytes() []byte {
	b, _ := json.Marshal(e)
	return b
}

// responder writes one JSON-RPC message.
type responder func(v any) error

// stub is the server state shared by both transports.
type stub struct {
	version string // the era this stub serves
	ask     func(method string, params any) (json.RawMessage, error)
	logf    func(string, ...any)
}

func main() {
	transport := flag.String("transport", "stdio", "stdio|http")
	addr := flag.String("addr", "127.0.0.1:8911", "HTTP listen address (http transport)")
	version := flag.String("version", modern, "protocol era: "+modern+"|"+legacy)
	echoEnv := flag.Bool("env", false, "log the environment of the request (headers / env)")
	flag.Parse()

	logf := log.New(os.Stderr, "mcpstub ", log.LstdFlags|log.Lmsgprefix).Printf
	s := &stub{version: *version, logf: logf}
	logf("serving %s over %s", *version, *transport)

	switch *transport {
	case "http":
		mux := http.NewServeMux()
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			s.handleHTTP(w, r, *echoEnv)
		})
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
		srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		log.Fatal(srv.ListenAndServe())
	case "stdio":
		if err := s.serveStdio(); err != nil {
			logf("stdio: %v", err)
			os.Exit(1)
		}
	default:
		log.Fatalf("unknown transport %q", *transport)
	}
}

// ---- dispatch ----

// handleOne dispatches a single message, returning the response (nil for notifications).
// ask may be nil; it is used only by the legacy era for server-initiated requests.
func (s *stub) handleOne(m *rpcMessage, ask func(string, any) (json.RawMessage, error), headers http.Header) (json.RawMessage, *rpcError) {
	params := map[string]any{}
	if len(m.Params) > 0 {
		_ = json.Unmarshal(m.Params, &params)
	}

	switch m.Method {
	case "initialize":
		// The modern era has no handshake; a client that sends initialize is a legacy client.
		want, _ := params["protocolVersion"].(string)
		if s.version != legacy {
			return nil, &rpcError{Code: -32022, Message: "UnsupportedProtocolVersion", Data: map[string]any{"supported": []string{s.version}}}
		}
		if want != "" && want != legacy {
			return nil, &rpcError{Code: -32022, Message: "UnsupportedProtocolVersion", Data: map[string]any{"supported": []string{legacy}}}
		}
		return must(map[string]any{
			"protocolVersion": legacy,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      serverInfo,
			"instructions":    "mcpstub (legacy 2025-11-25). Try the tools echo, add, image, need_input.",
		}), nil

	case "server/discover":
		if s.version != modern {
			return nil, &rpcError{Code: -32601, Message: "Method not found"}
		}
		meta, _ := params[metaKey].(map[string]any)
		if s.version == modern && meta == nil {
			// The modern spec requires the client identity on every request.
			return nil, &rpcError{Code: -32021, Message: "MissingRequiredClientCapability"}
		}
		return must(map[string]any{
			"supportedVersions": []string{modern, legacy},
			"latestVersion":     modern,
			"versions": []any{
				map[string]any{"version": modern, "available": true},
				map[string]any{"version": legacy, "available": true, "deprecatedAt": "2027-01-01"},
			},
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
			"_meta":        map[string]json.RawMessage{"io.modelcontextprotocol/serverInfo": must(serverInfo)},
			"ttlMs":        3600000,
		}), nil

	case "notifications/initialized", "notifications/cancelled":
		return nil, nil

	case "ping":
		return must(map[string]any{}), nil

	case "tools/list":
		return must(map[string]any{"tools": tools()}), nil

	case "tools/call":
		if s.version == modern {
			if _, ok := params[metaKey]; !ok {
				// The 2026-07-28 spec requires the client identity on every request.
				return nil, &rpcError{Code: -32021, Message: "MissingRequiredClientCapability: params._meta is required"}
			}
		}
		return s.callTool(params, ask, headers)

	default:
		if strings.HasPrefix(m.Method, "notifications/") {
			return nil, nil
		}
		return nil, &rpcError{Code: -32601, Message: "Method not found: " + m.Method}
	}
}

// callTool implements the tools.
func (s *stub) callTool(params map[string]any, ask func(string, any) (json.RawMessage, error), headers http.Header) (json.RawMessage, *rpcError) {
	name, _ := params["name"].(string)
	args := map[string]any{}
	if raw, ok := params["arguments"].(map[string]any); ok {
		args = raw
	}
	str := func(k, def string) string {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	num := func(k string, def float64) float64 {
		if v, ok := args[k].(float64); ok {
			return v
		}
		return def
	}
	text := func(t string) map[string]any {
		out := map[string]any{"content": []any{map[string]any{"type": "text", "text": t}}}
		if rt := resultType(s.version); rt != "" {
			out["resultType"] = rt // absent on 2025-11-25 servers
		}
		return out
	}

	switch name {
	case "echo":
		return must(text("echo: " + str("text", "(nothing)"))), nil
	case "add":
		return must(text(fmt.Sprintf("%.4g", num("a", 0)+num("b", 0)))), nil
	case "now":
		return must(text(time.Now().Format(time.RFC3339Nano))), nil
	case "big":
		n := int(num("n", 200))
		var sb strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "line %d: the quick brown fox jumps over the lazy dog\n", i)
		}
		return must(text(sb.String())), nil
	case "image":
		out := map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "a 1x1 PNG follows"},
				map[string]any{"type": "image", "data": tinyPNG, "mimeType": "image/png"},
			},
		}
		if rt := resultType(s.version); rt != "" {
			out["resultType"] = rt
		}
		return must(out), nil
	case "slow":
		d := time.Duration(num("seconds", 5*1000)) * time.Millisecond
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		time.Sleep(d)
		return must(text(fmt.Sprintf("slept %s", d))), nil
	case "structured":
		// The four shapes one of these answers can take, so the four structuredContent modes can be
		// tried against a real MCP server instead of only in a unit test — what each one costs is the
		// question the modes exist to answer.
		payload := map[string]any{"answer": str("mode", "unique"), "n": 42}
		flat, _ := json.Marshal(payload) // map keys are sorted, so these bytes are reproducible
		pretty, _ := json.MarshalIndent(payload, "", "  ")
		out := map[string]any{"structuredContent": json.RawMessage(flat)}
		switch str("mode", "unique") {
		case "dup": // the text block holds exactly those bytes: "auto" must not repeat them
			out["content"] = []any{map[string]any{"type": "text", "text": string(flat)}}
		case "pretty": // same data, different bytes: no substring test can tell they are equal
			out["content"] = []any{map[string]any{"type": "text", "text": string(pretty)}}
		case "only": // no text block at all: "fallback" exists for this shape
		default: // "unique": the text says something the JSON does not
			out["content"] = []any{map[string]any{"type": "text", "text": "structured answer attached"}}
		}
		if rt := resultType(s.version); rt != "" {
			out["resultType"] = rt
		}
		return must(out), nil
	case "fail":
		out := map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "this tool always fails"}},
			"isError": true,
		}
		if rt := resultType(s.version); rt != "" {
			out["resultType"] = rt
		}
		return must(out), nil
	case "headers":
		var sb strings.Builder
		for k, v := range headers {
			if strings.HasPrefix(strings.ToLower(k), "mcp-") || strings.HasPrefix(strings.ToLower(k), "x-mcp") {
				fmt.Fprintf(&sb, "%s: %s\n", k, strings.Join(v, ","))
			}
		}
		if sb.Len() == 0 {
			sb.WriteString("(no MCP headers: stdio transport or legacy era)")
		}
		sb.WriteString("\nargument mirrored via x-mcp-header: " + str("text", "(none)"))
		return must(text(sb.String())), nil

	case "need_input":
		return s.needInput(params, args)

	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + name}
	}
}

// needInput shows both ways of asking the client for input.
//
// 2026-07-28: multi round-trip requests. The first answer is resultType=input_required with an
// inputRequests list and opaque requestState; the client then retries tools/call with
// inputResponses (under a NEW jsonrpc id) and the requestState echoed back unchanged.
//
// 2025-11-25: a real server-initiated JSON-RPC request, which only the stdio transport can
// carry (an HTTP stub would need the GET SSE stream, so it reports that instead).
func (s *stub) needInput(params, args map[string]any) (json.RawMessage, *rpcError) {
	prompt := "What should the tool use?"
	if v, ok := args["text"].(string); ok && v != "" {
		prompt = v
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string", "description": "Your answer"},
		},
		"required": []string{"answer"},
	}
	type inputResponse struct {
		Behavior string         `json:"behavior"`
		Content  map[string]any `json:"content"`
	}
	answerFrom := func(set map[string]inputResponse) (string, bool) {
		for _, r := range set {
			if r.Behavior != "accept" {
				return "", false
			}
			if v, ok := r.Content["answer"].(string); ok {
				return v, true
			}
		}
		return "", false
	}

	if s.version == modern {
		if raw, ok := params["inputResponses"]; ok {
			var set map[string]inputResponse
			_ = json.Unmarshal(must(raw), &set)
			answer, ok := answerFrom(set)
			if !ok {
				return must(map[string]any{
					"content":    []any{map[string]any{"type": "text", "text": "the user declined to answer"}},
					"isError":    true,
					"resultType": "complete",
				}), nil
			}
			return must(map[string]any{
				"content":    []any{map[string]any{"type": "text", "text": "thanks, the answer is: " + answer}},
				"resultType": "complete",
			}), nil
		}
		return must(map[string]any{
			"resultType": "input_required",
			"inputRequests": []any{map[string]any{
				"id":     "q1",
				"method": "elicitation/create",
				"params": map[string]any{"message": "mcpstub asks: " + prompt, "mode": "form", "requestedSchema": schema},
			}},
			"requestState": map[string]any{"nonce": nonce(), "asked": prompt},
		}), nil
	}

	// Legacy era: ask with a real server-initiated request when the transport allows it.
	if s.ask == nil {
		return must(map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": "legacy elicitation needs the stdio transport (the HTTP stub does not open a GET stream)",
			}},
			"isError": true,
		}), nil
	}
	raw, err := s.ask("elicitation/create", map[string]any{"message": "mcpstub asks: " + prompt, "requestedSchema": schema})
	if err != nil {
		return must(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "elicitation failed: " + err.Error()}},
			"isError": true,
		}), nil
	}
	var resp inputResponse
	_ = json.Unmarshal(raw, &resp)
	if resp.Behavior != "accept" {
		return must(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "the user declined to answer"}},
			"isError": true,
		}), nil
	}
	answer, _ := resp.Content["answer"].(string)
	return must(map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "thanks, the answer is: " + answer}},
	}), nil
}

// nonce returns 8 random hex bytes, used as opaque requestState.
func nonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func resultType(version string) string {
	if version == modern {
		return "complete"
	}
	return ""
}

func tools() []map[string]any {
	schema := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	str := map[string]any{"type": "string"}
	num := map[string]any{"type": "number"}
	return []map[string]any{
		{"name": "echo", "description": "Return the given text.", "inputSchema": schema(map[string]any{"text": str}, "text")},
		{"name": "add", "description": "Add two numbers.", "inputSchema": schema(map[string]any{"a": num, "b": num}, "a", "b")},
		{"name": "now", "description": "Current server time (RFC3339).", "inputSchema": schema(nil)},
		{"name": "big", "description": "Return n lines of filler text (to test large tool results).", "inputSchema": schema(map[string]any{"n": num})},
		{"name": "image", "description": "Return an image content block (1x1 PNG).", "inputSchema": schema(nil)},
		{"name": "slow", "description": "Sleep for a number of seconds before answering.", "inputSchema": schema(map[string]any{"seconds": num})},
		{"name": "fail", "description": "Always fail (isError result).", "inputSchema": schema(nil)},
		{"name": "headers", "description": "Echo the MCP routing headers it received (HTTP transport).",
			"inputSchema": schema(map[string]any{"text": map[string]any{"type": "string", "x-mcp-header": "text"}})},
		{"name": "need_input", "description": "Ask the user a question before answering (MRTR / elicitation).",
			"inputSchema": schema(map[string]any{"text": str})},
		{"name": "structured", "description": "Answer with structuredContent in shape mode: dup (text holds exactly those bytes), unique (text says something else), pretty (text is the same data pretty-printed), only (no text block).",
			"inputSchema":  schema(map[string]any{"mode": str}, "mode"),
			"outputSchema": schema(map[string]any{"answer": str, "n": num}, "answer", "n")},
	}
}

// ---- stdio ----

func (s *stub) serveStdio() error {
	var mu sync.Mutex
	enc := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := os.Stdout.Write(append(b, '\n')); err != nil {
			return err
		}
		return nil
	}
	// Legacy server-initiated requests: id -> response channel.
	var pmu sync.Mutex
	pending := map[string]chan *rpcMessage{}
	var idmu sync.Mutex
	nextID := 1

	ask := func(method string, params any) (json.RawMessage, error) {
		idmu.Lock()
		id := fmt.Sprintf("stub-%d", nextID)
		nextID++
		idmu.Unlock()
		ch := make(chan *rpcMessage, 1)
		pmu.Lock()
		pending[id] = ch
		pmu.Unlock()
		defer func() {
			pmu.Lock()
			delete(pending, id)
			pmu.Unlock()
		}()
		idRaw, _ := json.Marshal(id)
		if err := enc(map[string]any{"jsonrpc": "2.0", "id": idRaw, "method": method, "params": params}); err != nil {
			return nil, err
		}
		select {
		case m := <-ch:
			if m.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
			}
			return m.Result, nil
		case <-time.After(5 * time.Minute):
			return nil, fmt.Errorf("%s: timed out", method)
		}
	}
	s.ask = ask

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<22), 1<<22)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			s.logf("bad line: %v", err)
			continue
		}
		// A message with an id but no method is a response to one of our requests.
		if m.Method == "" && len(m.ID) > 0 {
			var id string
			if json.Unmarshal(m.ID, &id) != nil {
				id = string(m.ID)
			}
			pmu.Lock()
			ch, ok := pending[id]
			pmu.Unlock()
			if ok {
				select {
				case ch <- &m:
				default:
				}
			}
			continue
		}
		if m.Method == "" {
			continue
		}
		go func(m *rpcMessage) {
			result, rerr := s.handleOne(m, ask, nil)
			if m.ID == nil {
				return // notification
			}
			resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if rerr != nil {
				resp["error"] = rerr
			} else {
				resp["result"] = json.RawMessage(result)
			}
			if err := enc(resp); err != nil {
				s.logf("write: %v", err)
			}
		}(&m)
	}
	return scanner.Err()
}

// ---- http ----

func (s *stub) handleHTTP(w http.ResponseWriter, r *http.Request, echoEnv bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var m rpcMessage
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if echoEnv {
		s.logf("%s proto=%q method=%q name=%q session=%q", r.URL.Path,
			r.Header.Get("MCP-Protocol-Version"), r.Header.Get("Mcp-Method"),
			r.Header.Get("Mcp-Name"), r.Header.Get("Mcp-Session-Id"))
	}
	result, rerr := s.handleOne(&m, nil, r.Header)
	w.Header().Set("Content-Type", "application/json")
	if m.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = json.RawMessage(result)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// helpers

func must(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
