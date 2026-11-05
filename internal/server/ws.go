package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
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

// beatEvery is how often a connection is told where the daemon's event counter stands. The interval is
// the one the control ping used, so an idle socket's traffic does not change shape — only its content
// does: a page can now tell "nothing happened" from "something happened that I never received".
const beatEvery = 20 * time.Second

// beatJitter is how far a beat may be early or late, as a fraction of the interval. Two tabs opened in
// the same second would otherwise beat in step, and so would anything else in the process that happens
// to run on a twenty second clock; the spread also keeps a beat from landing in lockstep with the work
// it is reporting on.
const beatJitter = 0.25

// nextBeat returns one jittered interval. The random value comes in as an argument so this stays a pure
// function: 0 is the earliest possible beat, a value just under 1 the latest.
func nextBeat(r float64) time.Duration {
	return time.Duration(float64(beatEvery) * (1 + beatJitter*(2*r-1)))
}

// heartbeatFrame is the "this is how far the counter has got" frame of one connection.
//
// It is written straight by the pump rather than published, which is the whole point of it: it is not an
// event, so it takes no stamp of its own, joins no session's replay buffer, and cannot be mistaken for
// something that happened in a conversation. The number it carries is the counter's value at the moment
// the pump wrote the frame.
//
// Marshalling a map of a string, an int64 and a string cannot fail; if it somehow did, the fallback frame
// carries no number at all, which a page reads as "nothing to compare" rather than as a gap.
func heartbeatFrame(seq int64) []byte {
	b, err := json.Marshal(map[string]any{
		"type": "heartbeat",
		"seq":  seq,
		"ts":   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return []byte(`{"type":"heartbeat"}`)
	}
	return b
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
		// lost is how a dying connection says so. A failed write is the only news this goroutine ever gets
		// that the peer is gone, and until now it arrived in silence: the pump stopped, the socket closed,
		// and nothing in the log could tell a closed laptop lid apart from a frame the browser was never
		// given. An already-cancelled request is the ordinary goodbye, so it stays out of the log.
		lost := func(err error) {
			if ctx.Err() == nil {
				s.logf("ws: write failed, closing the connection: %v", err)
			}
		}
		// The heartbeat is a data frame, not a control frame, because JS only ever sees data frames. It
		// replaces the control ping rather than joining it: that ping had one job — find out a peer is gone
		// by failing to write — and this is a write, with the same effect when it fails (the pump returns,
		// the connection closes, the client reconnects and re-reads what it missed).
		timer := time.NewTimer(nextBeat(rand.Float64()))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sc.closed:
				return
			case <-timer.C:
				wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
				err := c.Write(wctx, websocket.MessageText, heartbeatFrame(s.mgr.Seq()))
				wcancel()
				if err != nil {
					lost(err)
					return
				}
				timer.Reset(nextBeat(rand.Float64()))
			case b, ok := <-cli.raw:
				if !ok {
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					lost(err)
					return
				}
			case e, ok := <-sc.out:
				if !ok {
					return
				}
				b, err := json.Marshal(e)
				if err != nil {
					// Skipping an event that cannot be encoded is what this code has always done; this line is
					// what makes it findable. The stamp was spent before the event was ever queued, so a skip
					// here is a hole that every subscriber of this socket keeps — a page cannot tell an event it
					// was never handed from an event that never happened. Encoding one of these is not expected
					// to fail (the field that makes failure possible at all is `Data any`), which is exactly why
					// this is a log line rather than a redesign: if it never prints, nothing is being skipped.
					s.logf("ws: dropping event %s/%s seq %d, it cannot be encoded: %v", e.Session, e.Type, e.Seq, err)
					continue
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err = c.Write(wctx, websocket.MessageText, b)
				wcancel()
				if err != nil {
					lost(err)
					return
				}
			}
		}
	}()

	if !push(sc, agent.Event{Type: agent.EvtHello, Info: "connected"}) {
		s.logf("ws: dropped the hello frame: the reply queue of a brand new connection is already full")
	}
	cli.readLoop(ctx)

	cancel()
	<-pumpDone
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// push is a non-blocking publish used by this file (the hub does the blocking-free fan-out).
//
// It reports whether the frame was queued, because the alternative is a frame that vanishes without a
// trace: the event it carries has already spent its stamp by the time it gets here, so dropping it on a
// full queue is a hole in that connection's stream. Callers cannot do anything better than drop — a
// browser that is not draining 512 frames will not drain more — so the least this file can do is say so.
func push(sc *conn, e agent.Event) bool {
	select {
	case sc.out <- e:
		return true
	case <-sc.closed:
		return false
	default:
		return false
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
		// The replay is the one place this file can lose a hundred frames in a row without noticing: the
		// queue is bounded and the pump that drains it may be sitting in one slow write. Dropping is still
		// the right call, but it is a hole this page would otherwise stare at forever — so it is counted,
		// and one line says how deep it got rather than making the reader guess from a stale transcript.
		var dropped, total int
		for _, e := range c.srv.hub.Snapshot(cmd.Session) {
			total++
			if !push(c.sess, e) {
				dropped++
			}
		}
		if dropped > 0 {
			c.srv.logf("ws: snapshot of %s dropped %d of %d replayed frames, the reply queue is full",
				cmd.Session, dropped, total)
		}
		c.reply(cmd.Req, map[string]any{"ok": true}, nil)
	default:
		c.reply(cmd.Req, nil, fmt.Errorf(
			"ws command %q does not exist: this socket carries events, ping, subscribe and snapshot, and every action is REST (POST /api/session/{id}/…)",
			cmd.Type))
	}
}
