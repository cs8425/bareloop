// Package server exposes the agent manager over HTTP: a small JSON API, a websocket event
// stream (coder/websocket), an SSE fallback, and the static web UI.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"bareloop/internal/agent"
)

// Hub fans agent events out to connected browsers.
//
// One goroutine per connection writes every frame, which is what coder/websocket requires (a
// Conn accepts only one concurrent writer). A client that cannot keep up is closed rather
// than allowed to block the agent loop: a stalled browser must never slow the loop down.
type Hub struct {
	mu   sync.Mutex
	live map[*conn]struct{}
	logf func(string, ...any)
	// replay holds the most recent events per session so a page that reloads mid-run can
	// show what it missed instead of a blank screen.
	replay map[string][]agent.Event
	limit  int
}

// NewHub creates a hub.
func NewHub(logf func(string, ...any)) *Hub {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Hub{
		live:   map[*conn]struct{}{},
		logf:   logf,
		replay: map[string][]agent.Event{},
		limit:  400,
	}
}

// Broadcast publishes one event to every subscriber. It never blocks.
func (h *Hub) Broadcast(e agent.Event) {
	h.mu.Lock()
	if e.Session != "" {
		ring := h.replay[e.Session]
		ring = append(ring, e)
		if _, id := finishedStream(e); id != "" {
			// A finished stream must not be replayed. A page that reloads gets the committed entry
			// from GET /api/session/{id}, so replaying the fragments too renders the same answer a
			// second time. Subscribers received the fragments before this call, so nobody loses them.
			ring = dropStream(ring, id)
		}
		if len(ring) > h.limit {
			ring = ring[len(ring)-h.limit:]
		}
		h.replay[e.Session] = ring
	}
	slow := make([]*conn, 0, 1)
	for c := range h.live {
		if !c.wants(e) {
			continue
		}
		select {
		case c.out <- e:
		case <-c.closed:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		h.logf("closing a client that is not reading fast enough")
		h.unregister(c)
	}
}

// finishedStream reports the id of the stream a terminal fragment closes ("" for any other event).
func finishedStream(e agent.Event) (bool, string) {
	if e.Type != agent.EvtDelta || e.Delta == nil || !e.Delta.Done {
		return false, ""
	}
	return true, e.Delta.StreamID
}

// dropStream removes every fragment of one stream, reusing the backing array (no other slice
// shares it: Snapshot copies, and the events themselves are never mutated).
func dropStream(ring []agent.Event, id string) []agent.Event {
	out := ring[:0]
	for _, ev := range ring {
		if ev.Type == agent.EvtDelta && ev.Delta != nil && ev.Delta.StreamID == id {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// Snapshot returns the recent events of a session (for a page that just reconnected).
func (h *Hub) Snapshot(session string) []agent.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]agent.Event, len(h.replay[session]))
	copy(out, h.replay[session])
	return out
}

// DropSession forgets the replay buffer of a deleted session.
func (h *Hub) DropSession(session string) {
	h.mu.Lock()
	delete(h.replay, session)
	h.mu.Unlock()
}

func (h *Hub) register(c *conn) {
	h.mu.Lock()
	h.live[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	delete(h.live, c)
	h.mu.Unlock()
	c.close()
}

// conn is one browser connection.
type conn struct {
	out    chan agent.Event
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	all    bool
	sess   map[string]bool
}

// setSessions changes the subscription of a live connection (an empty list means "all").
func (c *conn) setSessions(sessions ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess = map[string]bool{}
	for _, s := range sessions {
		c.sess[s] = true
	}
	c.all = len(c.sess) == 0
}

func newConn(subscribeAll bool, sessions []string) *conn {
	c := &conn{out: make(chan agent.Event, 512), closed: make(chan struct{}), all: subscribeAll, sess: map[string]bool{}}
	for _, s := range sessions {
		c.sess[s] = true
	}
	if len(c.sess) == 0 {
		c.all = true
	}
	return c
}

func (c *conn) wants(e agent.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.all {
		return true
	}
	_, ok := c.sess[e.Session]
	return ok || e.Session == ""
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closed)
		close(c.out)
	})
}

// writeJSON replies with a JSON body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeErr replies with an error envelope.
func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

// decodeJSON reads a request body. Unknown fields are tolerated so the UI can send more than
// the server currently understands.
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("bad json: %w", err)
	}
	return nil
}
