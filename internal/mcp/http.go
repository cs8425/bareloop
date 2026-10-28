package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPConfig connects to a streamable HTTP MCP endpoint.
type HTTPConfig struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Timeout for the whole request round trip. 0 = no timeout (streams are exempt).
	Timeout time.Duration                    `json:"-"`
	HTTP    *http.Client                     `json:"-"`
	Logf    func(format string, args ...any) `json:"-"`
}

type httpTransport struct {
	cfg      HTTPConfig
	era      string
	hc       *http.Client
	ids      idSource
	in       chan *msg
	nmu      sync.Mutex
	onNotify func(*msg)
	// legacy session id, learned from the initialize response
	smu       sync.Mutex
	sessionID string
	closed    bool
	logf      func(string, ...any)
}

// setNotifyHandler routes notifications that belong to no in-flight request.
func (t *httpTransport) setNotifyHandler(f func(*msg)) {
	t.nmu.Lock()
	t.onNotify = f
	t.nmu.Unlock()
}

func (t *httpTransport) notifyHandler() func(*msg) {
	t.nmu.Lock()
	defer t.nmu.Unlock()
	return t.onNotify
}

// newHTTP builds a transport for one endpoint. The era is set by the client after
// negotiation (the first call may itself be the negotiation probe).
func newHTTP(cfg HTTPConfig, era string) *httpTransport {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if era == "" {
		era = eraModern
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	return &httpTransport{
		cfg:  cfg,
		era:  era,
		hc:   hc,
		in:   make(chan *msg, 32),
		logf: cfg.Logf,
	}
}

func (t *httpTransport) label() string { return "http:" + t.cfg.URL }

func (t *httpTransport) setEra(era string) { t.era = era }

func (t *httpTransport) protocolVersion() string {
	if t.era == eraLegacy {
		return protocolLegacyVersion
	}
	return protocolModernVersion
}

func (t *httpTransport) session() string {
	t.smu.Lock()
	defer t.smu.Unlock()
	return t.sessionID
}

func (t *httpTransport) setSession(id string) {
	if id == "" {
		return
	}
	t.smu.Lock()
	t.sessionID = id
	t.smu.Unlock()
}

// headerOpts describes the modern routing headers.
type headerOpts struct {
	method string
	name   string
	params map[string]string // x-mcp-header mirrored values
}

func (t *httpTransport) writeHeaders(req *http.Request, ho headerOpts) {
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", t.protocolVersion())
	if sid := t.session(); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if t.era == eraModern {
		// Stateless routing headers: required by 2026-07-28, ignored by intermediaries that
		// do not understand them.
		if ho.method != "" {
			req.Header.Set("Mcp-Method", HeaderValue(ho.method))
		}
		if ho.name != "" {
			req.Header.Set("Mcp-Name", HeaderValue(ho.name))
		}
		for k, v := range ho.params {
			req.Header.Set("Mcp-Param-"+k, HeaderValue(v))
		}
	}
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
}

// post sends one message. When streaming is false the configured timeout applies. A non-nil
// *http.Response means the answer arrives as an SSE stream the caller must close.
func (t *httpTransport) post(ctx context.Context, m *msg, ho headerOpts, streaming bool) (*http.Response, *msg, *rpcError, error) {
	body, err := marshalMsg(m)
	if err != nil {
		return nil, nil, nil, err
	}
	reqCtx := ctx
	var cancel context.CancelFunc
	if !streaming && t.cfg.Timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, t.cfg.Timeout)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, t.cfg.URL, bytes.NewReader(body))
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	t.writeHeaders(req, ho)

	resp, err := t.hc.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, nil, err
	}
	if cancel != nil {
		// The stream owns the context from now on.
		resp.Body = &cancelOnClose{resp.Body, cancel}
	}
	t.setSession(resp.Header.Get("Mcp-Session-Id"))

	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &msg{JSONRPC: "2.0"}, nil, nil
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, nil, &rpcError{Code: CodeMethodNotFound, Message: "HTTP 404: unknown, expired or invalid session id"}, nil
	case resp.StatusCode >= 400:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if e := parseRPCError(resp, raw); e != nil {
			return nil, nil, e, nil
		}
		return nil, nil, nil, fmt.Errorf("mcp: %s: %.400s", resp.Status, string(raw))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return resp, nil, nil, nil
	}
	// Single JSON body (object or batched array).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &msg{JSONRPC: "2.0"}, nil, nil
	}
	var single msg
	if err := json.Unmarshal(raw, &single); err == nil && (single.isResponse() || single.isNotification() || single.isRequest()) {
		return nil, &single, nil, nil
	}
	var batch []msg
	if err := json.Unmarshal(raw, &batch); err == nil {
		for i := range batch {
			if batch[i].isResponse() {
				return nil, &batch[i], nil, nil
			}
		}
		if len(batch) > 0 {
			return nil, &batch[0], nil, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("mcp: unrecognised response body: %.200s", string(raw))
}

// parseRPCError extracts a JSON-RPC error from an HTTP error body.
func parseRPCError(resp *http.Response, raw []byte) *rpcError {
	var wrapper struct {
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Error != nil {
		return wrapper.Error
	}
	var e rpcError
	if err := json.Unmarshal(raw, &e); err == nil && e.Message != "" {
		return &e
	}
	if resp != nil && resp.StatusCode == http.StatusBadRequest {
		return &rpcError{Code: CodeInvalidRequest, Message: "HTTP 400"}
	}
	if resp != nil && resp.StatusCode == http.StatusUnauthorized {
		return &rpcError{Code: http.StatusUnauthorized, Message: "HTTP 401 unauthorized"}
	}
	if resp != nil && resp.StatusCode == http.StatusForbidden {
		return &rpcError{Code: http.StatusForbidden, Message: "HTTP 403 forbidden"}
	}
	if resp != nil && resp.StatusCode == http.StatusMethodNotAllowed {
		return &rpcError{Code: CodeMethodNotFound, Message: "HTTP 405 method not allowed"}
	}
	return nil
}

type cancelOnClose struct {
	io.Reader
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := func() error {
		if cl, ok := c.Reader.(io.Closer); ok {
			return cl.Close()
		}
		return nil
	}()
	c.cancel()
	return err
}

// call implements transport. The response may arrive as JSON or inside an SSE stream;
// notifications on that stream are forwarded to onMsg.
func (t *httpTransport) call(ctx context.Context, method string, params any, onMsg func(*msg), opts *callOpts) (json.RawMessage, *rpcError, error) {
	rawParams, err := toRaw(params)
	if err != nil {
		return nil, nil, err
	}
	id := t.ids.next()
	m := &msg{JSONRPC: "2.0", ID: id, Method: method, Params: rawParams}
	streaming := opts != nil && opts.Streaming
	resp, immediate, rpc, err := t.post(ctx, m, t.headerOptsFor(method, rawParams, opts), streaming)
	if err != nil {
		return nil, nil, err
	}
	if rpc != nil {
		// The HTTP layer carried a JSON-RPC error (an unknown method, an expired session id,
		// 400/401/403). It stays typed: the era probe and the retry logic read the code.
		return nil, rpc, nil
	}
	if resp == nil {
		if immediate == nil || len(immediate.ID) == 0 {
			return json.RawMessage("{}"), nil, nil
		}
		return finish(immediate, id)
	}
	defer resp.Body.Close()
	return t.readStream(ctx, resp, id, onMsg)
}

// headerOptsFor builds the modern routing headers, including x-mcp-header mirroring.
func (t *httpTransport) headerOptsFor(method string, params json.RawMessage, opts *callOpts) headerOpts {
	ho := headerOpts{}
	if opts != nil {
		ho.params = opts.Headers
	}
	if t.era != eraModern {
		return ho
	}
	ho.method = method
	if opts != nil && opts.Name != "" {
		ho.name = opts.Name
		return ho
	}
	switch method {
	case mToolsCall, mResourcesRead, mPromptsGet:
		var p struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		}
		_ = json.Unmarshal(params, &p)
		switch {
		case p.Name != "":
			ho.name = p.Name
		case p.URI != "":
			ho.name = p.URI
		}
	}
	return ho
}

// applyMirroredHeaders adds Mcp-Param-* headers for parameters the tool schema declared
// with `x-mcp-header`. Called by Client.CallTool once the schema is known.
func (t *httpTransport) mirrored(schema json.RawMessage, argsJSON json.RawMessage) map[string]string {
	decls := DeclaredHeaders(schema)
	if len(decls) == 0 || len(argsJSON) == 0 {
		return nil
	}
	var args any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return nil
	}
	out := map[string]string{}
	for _, d := range decls {
		v, ok := Ptr(args, d.Ptr)
		if !ok {
			continue
		}
		s, ok := scalarString(v)
		if !ok {
			continue
		}
		out[d.ParamName] = s
	}
	return out
}

func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), "."), true
	case nil:
		return "", false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// readStream consumes an SSE response until the matching response message arrives.
// Cancelling ctx closes the stream, which is the 2026-07-28 cancellation signal.
func (t *httpTransport) readStream(ctx context.Context, resp *http.Response, id json.RawMessage, onMsg func(*msg)) (json.RawMessage, *rpcError, error) {
	var out json.RawMessage
	var rpc *rpcError
	var failure error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := readSSE(ctx, resp.Body, func(data []byte) error {
			if len(bytes.TrimSpace(data)) == 0 {
				return nil
			}
			var m msg
			if err := json.Unmarshal(data, &m); err != nil {
				t.logf("sse: bad message: %v", err)
				return nil
			}
			switch {
			case m.isResponse():
				if idKey(m.ID) == idKey(id) {
					out, rpc = m.Result, m.Error
					return errStopStream
				}
				// A response to a request we already cancelled: ignore.
			case m.isRequest():
				if t.era == eraLegacy {
					t.deliver(&m)
				} else {
					t.logf("server sent a request on the %s stream, which %s forbids; answering unsupported", t.cfg.URL, t.protocolVersion())
					t.respondServerUnsupported(context.Background(), m.ID)
				}
			default:
				if onMsg != nil {
					onMsg(&m)
				} else if h := t.notifyHandler(); h != nil {
					h(&m)
				}
			}
			return nil
		})
		failure = err
	}()
	select {
	case <-done:
		if failure != nil && !errors.Is(failure, errStopStream) {
			return nil, nil, failure
		}
		return finishMsg(out, rpc, id)
	case <-ctx.Done():
		// Closing the body terminates the stream and signals cancellation.
		resp.Body.Close()
		<-done
		return nil, nil, ctx.Err()
	}
}

func (t *httpTransport) deliver(m *msg) {
	select {
	case t.in <- m:
	case <-time.After(2 * time.Second):
		t.logf("no handler for server request %s", m.Method)
	}
}

func (t *httpTransport) respondServerUnsupported(ctx context.Context, id json.RawMessage) {
	e := &rpcError{Code: CodeMethodNotFound, Message: "server-initiated requests are not supported in " + t.protocolVersion()}
	_ = t.respond(ctx, id, nil, e)
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	rawParams, err := toRaw(params)
	if err != nil {
		return err
	}
	_, _, _, err = t.post(ctx, &msg{JSONRPC: "2.0", Method: method, Params: rawParams},
		headerOpts{method: method}, false)
	return err
}

func (t *httpTransport) respond(ctx context.Context, id json.RawMessage, result any, e *rpcError) error {
	m := &msg{JSONRPC: "2.0", ID: id}
	if e != nil {
		m.Error = e
	} else {
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		m.Result = raw
	}
	_, _, _, err := t.post(ctx, m, headerOpts{method: ""}, false)
	return err
}

func (t *httpTransport) incoming() <-chan *msg { return t.in }

func (t *httpTransport) close() error {
	t.closed = true
	t.hc.CloseIdleConnections()
	return nil
}

// stream starts a long-lived request (2026-07-28 subscriptions/listen). The returned func
// stops the stream.
func (t *httpTransport) stream(ctx context.Context, method string, params any, onMsg func(*msg)) (func(), error) {
	rawParams, err := toRaw(params)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	id := t.ids.next()
	m := &msg{JSONRPC: "2.0", ID: id, Method: method, Params: rawParams}
	resp, immediate, rpc, err := t.post(sctx, m, headerOpts{method: method}, true)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp == nil {
		// Non-stream answer (server does not support streams for this method).
		go func() {
			if onMsg != nil && immediate != nil {
				onMsg(immediate)
			}
			if rpc != nil {
				t.logf("stream %s: %v", method, rpc)
			}
		}()
		return cancel, nil
	}
	go func() {
		defer resp.Body.Close()
		defer cancel()
		readSSE(sctx, resp.Body, func(data []byte) error {
			var m msg
			if json.Unmarshal(data, &m) != nil {
				return nil
			}
			if onMsg != nil {
				onMsg(&m)
			}
			return nil
		})
	}()
	return cancel, nil
}

// getStream opens the legacy GET SSE stream (server-initiated messages, list_changed).
// It returns nil when the server does not support it (405 etc).
func (t *httpTransport) getStream(ctx context.Context) (func(), error) {
	if t.era != eraLegacy {
		return nil, errors.New("mcp: the GET stream does not exist in " + protocolModernVersion)
	}
	sctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, t.cfg.URL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	t.writeHeaders(req, headerOpts{})
	resp, err := t.hc.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode >= 400 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("mcp: GET stream unsupported (%d)", resp.StatusCode)
	}
	go func() {
		defer resp.Body.Close()
		defer cancel()
		readSSE(sctx, resp.Body, func(data []byte) error {
			var m msg
			if json.Unmarshal(data, &m) != nil {
				return nil
			}
			if m.isRequest() {
				t.deliver(&m)
			} else if h := t.notifyHandler(); h != nil {
				h(&m)
			}
			return nil
		})
	}()
	return cancel, nil
}

// ---- SSE reading ----

var errStopStream = errors.New("mcp: stream read stopped")

// readSSE parses text/event-stream data payloads. Returning errStopStream from onData
// stops reading.
func readSSE(ctx context.Context, r io.Reader, onData func(data []byte) error) error {
	br := bufio.NewReaderSize(r, 1<<16)
	var data []byte
	haveData := false
	emit := func() error {
		if !haveData {
			return nil
		}
		payload := data
		data, haveData = nil, false
		return onData(bytes.TrimSpace(payload))
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := br.ReadString('\n')
		if line != "" {
			l := strings.TrimRight(line, "\r\n")
			switch {
			case l == "":
				if e := emit(); e != nil {
					return e
				}
			case strings.HasPrefix(l, ":"):
				// comment / keep-alive
			case strings.HasPrefix(l, "data:"):
				v := strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " ")
				if haveData {
					data = append(data, '\n')
				}
				data = append(data, v...)
				haveData = true
			default:
				// event:/id:/retry: — not needed by this client
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return emit()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errStopStream) {
				return err
			}
			return err
		}
	}
}

// finish converts an immediate (non-stream) message into a call result.
func finish(m *msg, id json.RawMessage) (json.RawMessage, *rpcError, error) {
	return finishMsg(m.Result, m.Error, id)
}

func finishMsg(result json.RawMessage, e *rpcError, id json.RawMessage) (json.RawMessage, *rpcError, error) {
	if e != nil {
		return nil, e, nil
	}
	if len(result) == 0 {
		return json.RawMessage("{}"), nil, nil
	}
	return result, nil, nil
}
