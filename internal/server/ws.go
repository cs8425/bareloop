package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
)

// wsClient is one browser websocket connection.
//
// Transport notes: the read limit is switched off because image attachments arrive as base64
// inside a single JSON message; frames are text (JSON) only; a per-connection write queue is
// drained by exactly one goroutine, because coder/websocket allows only one concurrent writer
// per Conn.
type wsClient struct {
	srv  *Server
	conn *websocket.Conn
	sess *conn
	// raw carries reply frames. Every frame of this connection is written by the single pump
	// goroutine (coder/websocket allows only one writer at a time), commands included.
	raw chan []byte
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	sub := r.URL.Query().Get("session")
	var sessions []string
	if sub != "" {
		sessions = strings.Split(sub, ",")
	}
	sc := newConn(len(sessions) == 0, sessions)
	s.hub.register(sc)
	defer s.hub.unregister(sc)

	// Authentication is expected to terminate at a reverse proxy; origin checking stays
	// permissive so the UI works on any local port.
	opts := &websocket.AcceptOptions{InsecureSkipVerify: true}
	c, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.logf("ws accept: %v", err)
		return
	}
	// Images are base64 inside one message: never limit the read size.
	c.SetReadLimit(-1)

	cli := &wsClient{srv: s, conn: c, sess: sc, raw: make(chan []byte, 64)}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Write pump: everything that leaves this connection goes through this goroutine.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sc.closed:
				return
			case <-ticker.C:
				pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					return
				}
			case b, ok := <-cli.raw:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					return
				}
			case e, ok := <-sc.out:
				if !ok {
					return
				}
				b, err := json.Marshal(e)
				if err != nil {
					continue
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err = c.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					return
				}
			}
		}
	}()

	push(sc, agent.Event{Type: agent.EvtHello, Info: "connected"})
	cli.readLoop(ctx)

	cancel()
	<-pumpDone
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// push is a non-blocking publish used by this file (the hub does the blocking-free fan-out).
func push(sc *conn, e agent.Event) {
	select {
	case sc.out <- e:
	case <-sc.closed:
	default:
	}
}

// reply answers one command. The frame is queued for the write pump, which is the only
// goroutine allowed to write on the connection; if the queue is full the client is too slow to
// be worth answering and the reply is dropped (the hub already deals with that case for
// events).
func (c *wsClient) reply(req string, v any, err error) {
	msg := map[string]any{"type": "reply", "req": req}
	switch {
	case err != nil:
		msg["type"] = "reply_error"
		msg["error"] = err.Error()
	case v != nil:
		msg["data"] = v
	}
	b, jsonErr := json.Marshal(msg)
	if jsonErr != nil {
		b = []byte(`{"type":"reply_error","error":"marshal failed"}`)
	}
	select {
	case c.raw <- b:
	default:
		c.srv.logf("ws: reply queue full, dropping one reply")
	}
}

// command is one websocket message from the browser.
type command struct {
	Type    string          `json:"type"`
	Session string          `json:"session"`
	Req     string          `json:"req,omitempty"`
	Text    string          `json:"text,omitempty"`
	Images  []ir.Part       `json:"images,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	Answer  string          `json:"answer,omitempty"`
	Id      string          `json:"id,omitempty"`
	Content map[string]any  `json:"content,omitempty"`
}

func (c *wsClient) readLoop(ctx context.Context) {
	for {
		mt, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageText && mt != websocket.MessageBinary {
			continue
		}
		var cmd command
		if err := json.Unmarshal(data, &cmd); err != nil {
			c.reply("", nil, fmt.Errorf("bad message: %w", err))
			continue
		}
		c.handle(ctx, cmd)
	}
}

// handle executes one client command. Actions resolve the session on demand, so a browser may
// open the socket before any session exists.
func (c *wsClient) handle(ctx context.Context, cmd command) {
	switch cmd.Type {
	case "ping":
		c.reply(cmd.Req, map[string]any{"pong": time.Now().UTC().Format(time.RFC3339)}, nil)
		return
	case "subscribe":
		var sessions []string
		if cmd.Session != "" {
			sessions = strings.Split(cmd.Session, ",")
		}
		c.sess.setSessions(sessions...)
		c.reply(cmd.Req, map[string]any{"ok": true}, nil)
		return
	case "snapshot":
		for _, e := range c.srv.hub.Snapshot(cmd.Session) {
			push(c.sess, e)
		}
		c.reply(cmd.Req, map[string]any{"ok": true}, nil)
		return
	}

	if cmd.Session == "" {
		c.reply(cmd.Req, nil, fmt.Errorf("message %q needs a session id", cmd.Type))
		return
	}
	sess, err := c.srv.mgr.Get(ctx, cmd.Session)
	if err != nil {
		c.reply(cmd.Req, nil, err)
		return
	}

	switch cmd.Type {
	case "send":
		err = sess.Send(ctx, cmd.message())
	case "interrupt":
		err = sess.Interrupt(cmd.message())
	case "stop":
		err = sess.Stop()
	case "resume":
		err = sess.Resume(ctx)
	case "compact":
		res, cerr := sess.CompactNow(ctx, cmd.textOrBody("instruction"))
		c.reply(cmd.Req, res, cerr)
		return
	case "gate":
		err = sess.AnswerGate(cmd.Id, agent.GateAnswer(cmd.Answer))
	case "elicit":
		err = sess.AnswerElicitation(cmd.Id, cmd.Answer, cmd.Content)
	case "inspect":
		c.reply(cmd.Req, sess.Inspect(), nil)
		return
	case "status":
		c.reply(cmd.Req, sess.Status(), nil)
		return
	case "pending":
		c.reply(cmd.Req, sess.PendingIDs(), nil)
		return
	case "entryPatch":
		var b struct {
			Id      string `json:"id"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(cmd.Body, &b)
		err = sess.PatchEntry(b.Id, b.Content)
	case "entryDelete":
		var b struct {
			Id string `json:"id"`
		}
		_ = json.Unmarshal(cmd.Body, &b)
		err = sess.DeleteEntry(b.Id)
	case "tip":
		var b struct {
			Id string `json:"id"`
		}
		_ = json.Unmarshal(cmd.Body, &b)
		err = sess.SetTip(b.Id)
	case "name":
		var b struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(cmd.Body, &b)
		err = sess.SetName(b.Name)
	default:
		err = fmt.Errorf("unknown command %q", cmd.Type)
	}
	c.reply(cmd.Req, nil, err)
}

func (q command) message() agent.Message {
	m := agent.Message{Text: q.Text}
	for _, img := range q.Images {
		p := img
		if p.Type == "" {
			p.Type = ir.PartImage
		}
		m.Images = append(m.Images, p)
	}
	return m
}

// textOrBody reads one string field out of the optional body object, falling back to Text.
func (q command) textOrBody(field string) string {
	if len(q.Body) > 0 {
		var m map[string]any
		if json.Unmarshal(q.Body, &m) == nil {
			if v, ok := m[field].(string); ok {
				return v
			}
		}
	}
	return q.Text
}
