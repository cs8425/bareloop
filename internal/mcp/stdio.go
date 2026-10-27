package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// StdioConfig starts an MCP server as a subprocess (stdio is newline-delimited JSON-RPC).
type StdioConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	// PrefixFraming sends legacy `M <<json>>` framing. Receive-side prefix framing is
	// always tolerated. The 2026-07-28 revision dropped prefix framing entirely.
	PrefixFraming bool `json:"prefixFraming,omitempty"`
	// MaxLine caps one inbound message (default 64 MiB).
	MaxLine int `json:"maxLine,omitempty"`
	// Logf receives transport diagnostics.
	Logf func(format string, args ...any) `json:"-"`
}

type stdio struct {
	cfg      StdioConfig
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdoutR  *bufio.Reader
	stderrR  *bufio.Reader
	wmu      sync.Mutex
	d        *demux
	in       chan *msg
	ids      idSource
	closed   bool
	cmu      sync.Mutex
	logf     func(string, ...any)
	readerDone chan struct{}
}

// newStdio starts the subprocess and begins reading.
func newStdio(ctx context.Context, cfg StdioConfig) (*stdio, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("mcp: stdio server needs a command")
	}
	if cfg.MaxLine <= 0 {
		cfg.MaxLine = 64 << 20
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	t := &stdio{
		cfg:        cfg,
		in:         make(chan *msg, 16),
		readerDone: make(chan struct{}),
		logf:       cfg.Logf,
	}
	t.d = newDemux(t.in, func(m *msg) { /* notifications before a call: drop */ })

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %s: %w", cfg.Command, err)
	}
	t.cmd, t.stdin = cmd, stdin
	t.stdoutR = bufio.NewReaderSize(stdout, 1<<20)
	t.stderrR = bufio.NewReaderSize(stderr, 1<<16)
	go t.readLoop()
	go t.stderrLoop()
	return t, nil
}

func (t *stdio) label() string { return "stdio:" + t.cfg.Command }

// readLoop parses one JSON-RPC message per line, tolerating legacy prefix framing.
func (t *stdio) readLoop() {
	defer close(t.readerDone)
	defer t.d.closeAll(errors.New("mcp: stdio stream ended"))
	for {
		line, err := t.readLine()
		if err != nil {
			if err != io.EOF {
				t.logf("stdio read: %v", err)
			}
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw := []byte(line)
		if !strings.HasPrefix(line, "{") {
			// Legacy prefix framing: "M <<json>>".
			if i := strings.IndexByte(line, ' '); i > 0 && json.Valid([]byte(line[i+1:])) {
				raw = []byte(line[i+1:])
			} else {
				continue // not JSON-RPC: ignore (servers sometimes print banners)
			}
		}
		var m msg
		if err := json.Unmarshal(raw, &m); err != nil {
			t.logf("stdio: bad line: %v (%.120s)", err, line)
			continue
		}
		t.d.route(&m)
	}
}

func (t *stdio) readLine() (string, error) {
	var parts []string
	for {
		chunk, err := t.stdoutR.ReadBytes('\n')
		if len(chunk) > 0 {
			parts = append(parts, string(chunk))
			total := 0
			for _, p := range parts {
				total += len(p)
			}
			if total > t.cfg.MaxLine {
				return "", fmt.Errorf("mcp: stdio line exceeds %d bytes", t.cfg.MaxLine)
			}
		}
		if err != nil {
			if len(parts) > 0 && err == io.EOF && strings.HasSuffix(parts[len(parts)-1], "\n") {
				return strings.Join(parts, ""), nil
			}
			return strings.Join(parts, ""), err
		}
		if strings.HasSuffix(chunk, "\n") {
			return strings.Join(parts, ""), nil
		}
	}
}

func (t *stdio) stderrLoop() {
	buf := make([]byte, 8<<10)
	for {
		n, err := t.stderrR.Read(buf)
		if n > 0 {
			s := strings.TrimRight(string(buf[:n]), "\r\n")
			for _, line := range strings.Split(s, "\n") {
				if strings.TrimSpace(line) != "" {
					t.logf("stderr %s", line)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *stdio) write(b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.isClosed() {
		return errors.New("mcp: stdio transport closed")
	}
	if t.cfg.PrefixFraming {
		b = append(append([]byte("M "), b...), '\n')
	} else {
		b = append(b, '\n')
	}
	_, err := t.stdin.Write(b)
	return err
}

func (t *stdio) call(ctx context.Context, method string, params any, onMsg func(*msg), _ *callOpts) (json.RawMessage, *rpcError, error) {
	rawParams, err := toRaw(params)
	if err != nil {
		return nil, nil, err
	}
	id := t.ids.next()
	m := &msg{JSONRPC: "2.0", ID: id, Method: method, Params: rawParams}
	// A progress token lets progress/log notifications find their way back to this call.
	if onMsg != nil {
		m.Params = addProgressToken(rawParams, idKey(id), t.d)
	}
	b, err := marshalMsg(m)
	if err != nil {
		return nil, nil, err
	}
	ch := t.d.register(id, onMsg)
	if err := t.write(b); err != nil {
		t.d.unregister(idKey(id))
		return nil, nil, err
	}
	select {
	case <-ctx.Done():
		t.d.unregister(idKey(id))
		_ = t.notify(context.Background(), mCancelled, map[string]any{
			"requestId": json.RawMessage(id), "reason": "client cancelled",
		})
		return nil, nil, ctx.Err()
	case res, ok := <-ch:
		if !ok {
			return nil, nil, errors.New("mcp: transport closed while waiting for " + method)
		}
		return res.result, res.rpcErr, res.err
	}
}

func (t *stdio) notify(ctx context.Context, method string, params any) error {
	rawParams, err := toRaw(params)
	if err != nil {
		return err
	}
	b, err := marshalMsg(&msg{JSONRPC: "2.0", Method: method, Params: rawParams})
	if err != nil {
		return err
	}
	return t.write(b)
}

func (t *stdio) respond(ctx context.Context, id json.RawMessage, result any, e *rpcError) error {
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
	b, err := marshalMsg(m)
	if err != nil {
		return err
	}
	return t.write(b)
}

func (t *stdio) incoming() <-chan *msg { return t.in }

// setNotifyHandler routes notifications that belong to no in-flight request.
func (t *stdio) setNotifyHandler(f func(*msg)) { t.d.setNotifyHandler(f) }

// stream implements the configurable interface. stdio delivers notifications on the shared
// connection, so a subscription request is an ordinary request.
func (t *stdio) stream(ctx context.Context, method string, params any, onMsg func(*msg)) (func(), error) {
	_, _, err := t.call(ctx, method, params, onMsg, nil)
	return func() {}, err
}

func (t *stdio) isClosed() bool {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	return t.closed
}

func (t *stdio) close() error {
	t.cmu.Lock()
	if t.closed {
		t.cmu.Unlock()
		return nil
	}
	t.closed = true
	t.cmu.Unlock()

	_ = t.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = t.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = t.cmd.Process.Kill()
		<-done
	}
	t.d.closeAll(errors.New("mcp: transport closed"))
	return nil
}

// ---- demultiplexing ----

type callResult struct {
	result json.RawMessage
	rpcErr *rpcError
	err    error
}

type pendingCall struct {
	id      json.RawMessage
	ch      chan callResult
	onMsg   func(*msg)
	tokens  map[string]bool
}

type demux struct {
	mu       sync.Mutex
	pending  map[string]*pendingCall
	tokenOf  map[string]string
	incoming chan *msg
	onNotify func(*msg)
	closed   bool
}

func newDemux(incoming chan *msg, onNotify func(*msg)) *demux {
	return &demux{
		pending:  map[string]*pendingCall{},
		tokenOf:  map[string]string{},
		incoming: incoming,
		onNotify: onNotify,
	}
}

func (d *demux) register(id json.RawMessage, onMsg func(*msg)) <-chan callResult {
	p := &pendingCall{id: id, ch: make(chan callResult, 1), onMsg: onMsg, tokens: map[string]bool{}}
	d.mu.Lock()
	if !d.closed {
		d.pending[idKey(id)] = p
	}
	d.mu.Unlock()
	return p.ch
}

func (d *demux) unregister(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.pending[id]; ok {
		for tok := range p.tokens {
			delete(d.tokenOf, tok)
		}
		d.clearTokensFor(id)
		delete(d.pending, id)
	}
}

// clearTokensFor drops token -> id entries (the token defaults to the request id, so the
// map usually holds one entry per in-flight call).
func (d *demux) clearTokensFor(id string) {
	for tok, v := range d.tokenOf {
		if v == id {
			delete(d.tokenOf, tok)
		}
	}
}

func (d *demux) setNotifyHandler(f func(*msg)) {
	d.mu.Lock()
	d.onNotify = f
	d.mu.Unlock()
}

func (d *demux) addToken(token, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tokenOf[token] = id
	if p, ok := d.pending[id]; ok {
		p.tokens[token] = true
	}
}

// route sends one inbound message to its owner.
func (d *demux) route(m *msg) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	switch {
	case m.isResponse():
		p, ok := d.pending[idKey(m.ID)]
		if ok {
			for tok := range p.tokens {
				delete(d.tokenOf, tok)
			}
			d.clearTokensFor(idKey(m.ID))
			delete(d.pending, idKey(m.ID))
		}
		d.mu.Unlock()
		if !ok {
			return // late reply to a cancelled request
		}
		res := callResult{}
		switch {
		case m.Error != nil:
			res.rpcErr = m.Error
		default:
			res.result = m.Result
		}
		p.ch <- res
		close(p.ch)
	case m.isRequest():
		d.mu.Unlock()
		select {
		case d.incoming <- m:
		default:
			// Nobody is reading server requests: answer method-not-found so the server
			// does not hang.
		}
	default: // notification
		target := ""
		if len(m.Params) > 0 {
			var p struct {
				RelatedRequestID json.RawMessage `json:"relatedRequestId"`
				ProgressToken    json.RawMessage `json:"progressToken"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				if len(p.RelatedRequestID) > 0 {
					target = idKey(p.RelatedRequestID)
				}
				if target == "" && len(p.ProgressToken) > 0 {
					target = d.tokenOf[strings.Trim(string(p.ProgressToken), `"`)]
				}
			}
		}
		p, ok := d.pending[target]
		handler := d.onNotify
		d.mu.Unlock()
		switch {
		case ok && p.onMsg != nil:
			p.onMsg(m)
		case handler != nil:
			handler(m)
		}
	}
}

func (d *demux) closeAll(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	d.tokenOf = map[string]string{}
	for id, p := range d.pending {
		select {
		case p.ch <- callResult{err: err}:
		default:
		}
		close(p.ch)
		delete(d.pending, id)
	}
}

// addProgressToken injects _meta.progressToken into a params object.
func addProgressToken(params json.RawMessage, token string, d *demux) json.RawMessage {
	var m map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &m); err != nil {
			return params
		}
	} else {
		m = map[string]json.RawMessage{}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := m[metaKey]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	tok, err := json.Marshal(token)
	if err != nil {
		return params
	}
	meta[metaProgressToken] = tok
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return params
	}
	m[metaKey] = metaRaw
	out, err := json.Marshal(m)
	if err != nil {
		return params
	}
	d.addToken(token, token)
	return out
}
