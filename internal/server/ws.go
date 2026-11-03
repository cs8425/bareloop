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

// command is one websocket message from the browser: the frame that asks for one of the three transport
// verbs. Nothing a session does is asked for here, so nothing a session does can arrive with a field this
// file forgot about.
type command struct {
	Type    string `json:"type"`
	Session string `json:"session"`
	Req     string `json:"req,omitempty"`
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
		c.handle(cmd)
	}
}

// handle executes one client command. There are three of them, and none touches a session: this socket
// carries events out, and an action that arrives here is pointed at REST rather than quietly served by a
// second implementation of it (which is how a websocket send used to lose the message's label and files -
// one job, two decoders, only one of them used).
func (c *wsClient) handle(cmd command) {
	switch cmd.Type {
	case "ping":
		c.reply(cmd.Req, map[string]any{"pong": time.Now().UTC().Format(time.RFC3339)}, nil)
	case "subscribe":
		var sessions []string
		if cmd.Session != "" {
			sessions = strings.Split(cmd.Session, ",")
		}
		c.sess.setSessions(sessions...)
		c.reply(cmd.Req, map[string]any{"ok": true}, nil)
	case "snapshot":
		for _, e := range c.srv.hub.Snapshot(cmd.Session) {
			push(c.sess, e)
		}
		c.reply(cmd.Req, map[string]any{"ok": true}, nil)
	default:
		c.reply(cmd.Req, nil, fmt.Errorf(
			"ws command %q does not exist: this socket carries events, ping, subscribe and snapshot, and every action is REST (POST /api/session/{id}/…)",
			cmd.Type))
	}
}
