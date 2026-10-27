package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
)

// Server wires the HTTP API to the session manager.
type Server struct {
	mgr  *agent.Manager
	hub  *Hub
	web  string
	logf func(string, ...any)
}

// New creates the HTTP handler set.
func New(mgr *agent.Manager, hub *Hub, webDir string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{mgr: mgr, hub: hub, web: webDir, logf: logf}
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "time": time.Now().UTC()})
	})
	mux.HandleFunc("GET /api/sessions", s.handleList)
	mux.HandleFunc("POST /api/sessions", s.handleCreate)
	mux.HandleFunc("GET /api/session/{id}", s.handleGet)
	mux.HandleFunc("PATCH /api/session/{id}", s.handlePatch)
	mux.HandleFunc("DELETE /api/session/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/session/{id}/inspect", s.handleInspect)
	mux.HandleFunc("GET /api/session/{id}/payload", s.handlePayload)
	mux.HandleFunc("POST /api/session/{id}/send", s.handleSend)
	mux.HandleFunc("POST /api/session/{id}/interrupt", s.handleInterrupt)
	mux.HandleFunc("POST /api/session/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /api/session/{id}/compact", s.handleCompact)
	mux.HandleFunc("POST /api/session/{id}/gate", s.handleGate)
	mux.HandleFunc("POST /api/session/{id}/elicit", s.handleElicit)
	mux.HandleFunc("POST /api/session/{id}/entry/patch", s.handlePatchEntry)
	mux.HandleFunc("POST /api/session/{id}/entry/delete", s.handleDeleteEntry)
	mux.HandleFunc("POST /api/session/{id}/tip", s.handleTip)
	mux.HandleFunc("POST /api/session/{id}/reload", s.handleReload)
	mux.HandleFunc("GET /api/session/{id}/events", s.handleSSE)
	mux.HandleFunc("GET /ws", s.handleWS)

	mux.HandleFunc("/", s.handleStatic)
	return logWrap(s.logf, mux)
}

// ---------------------------------------------------------------- sessions

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"sessions": s.mgr.List()})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var cfg agent.Config
	if err := decodeJSON(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("s%d", time.Now().Unix())
	}
	sess, err := s.mgr.Create(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"id": sess.ID, "status": sess.Status()})
}

// session resolves {id} to an open session.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*agent.Session, bool) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing session id"))
		return nil, false
	}
	sess, err := s.mgr.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, false
	}
	return sess, true
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	cfg := sess.Config()
	cfg.LLM.APIKey = "" // the key is never echoed back to the browser
	entries := sess.Entries()
	sortEntries(entries)
	writeJSON(w, map[string]any{
		"config":  cfg,
		"status":  sess.Status(),
		"entries": entries,
		"pending": sess.PendingIDs(),
	})
}

// sortEntries orders entries by their numeric id, which is the order they were appended.
// store.Entries iterates a map, so without this the browser renders a shuffled transcript.
func sortEntries(entries []*ir.Entry) {
	sort.Slice(entries, func(i, j int) bool { return entryNum(entries[i].ID) < entryNum(entries[j].ID) })
}

// entryNum parses the per-session number of an entry id ("42" → 42, anything else → 0).
func entryNum(id string) int64 {
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var patch agent.Config
	if err := decodeJSON(r, &patch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	patch.ID = sess.ID
	if err := sess.UpdateConfig(r.Context(), patch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": sess.Status()})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Remove(id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.hub.DropSession(id)
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- actions

// sendRequest is the body of /send and /interrupt.
type sendRequest struct {
	Text   string    `json:"text"`
	Images []ir.Part `json:"images,omitempty"`
	Files  []uploadImage `json:"files,omitempty"`
	Label  string    `json:"label,omitempty"`
	// Req is echoed back so a client can match a reply; it is ignored over REST.
	Req string `json:"req,omitempty"`
}

type uploadImage struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

func (q sendRequest) message() agent.Message {
	m := agent.Message{Text: q.Text, Label: q.Label}
	for _, p := range q.Images {
		if p.Type == "" {
			p.Type = ir.PartImage
		}
		m.Images = append(m.Images, p)
	}
	for _, f := range q.Files {
		m.Images = append(m.Images, ir.Part{Type: ir.PartImage, MimeType: f.MimeType, Data: f.Data, Name: f.Name})
	}
	return m
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q sendRequest
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := sess.Send(r.Context(), q.message()); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": sess.Status(), "pending": sess.PendingIDs()})
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q sendRequest
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := sess.Interrupt(q.message()); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if err := sess.Stop(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q struct {
		Instruction string `json:"instruction"`
	}
	_ = decodeJSON(r, &q)
	res, err := sess.CompactNow(r.Context(), q.Instruction)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) handleGate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q struct {
		Id     string `json:"id"`
		Answer string `json:"answer"`
	}
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := sess.AnswerGate(q.Id, agent.GateAnswer(q.Answer)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleElicit(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q struct {
		Id       string         `json:"id"`
		Behavior string         `json:"behavior"`
		Content  map[string]any `json:"content"`
	}
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if q.Behavior == "" {
		q.Behavior = "accept"
	}
	if err := sess.AnswerElicitation(q.Id, q.Behavior, q.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePatchEntry(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q struct {
		Id      string `json:"id"`
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := sess.PatchEntry(q.Id, q.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q struct {
		Id string `json:"id"`
	}
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := sess.DeleteEntry(q.Id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleTip(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q struct {
		Id string `json:"id"`
	}
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := sess.SetTip(q.Id); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": sess.Status()})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if err := sess.Reload(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": sess.Status()})
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, sess.Inspect())
}

func (s *Server) handlePayload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, sess.Payload())
}

// ---------------------------------------------------------------- SSE (fallback transport)

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	id := r.PathValue("id")
	c := newConn(id == "", []string{id})
	s.hub.register(c)
	defer s.hub.unregister(c)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Replay what this client missed while it was away.
	for _, e := range s.hub.Snapshot(id) {
		if !writeSSE(w, e) {
			return
		}
	}
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-c.out:
			if !ok {
				return
			}
			if !writeSSE(w, e) {
				return
			}
			flusher.Flush()
		case <-time.After(25 * time.Second):
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, e agent.Event) bool {
	b, err := json.Marshal(e)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: agent\ndata: %s\n\n", b); err != nil {
		return false
	}
	return true
}

// ---------------------------------------------------------------- static

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	up := strings.TrimPrefix(r.URL.Path, "/")
	if up == "" {
		up = "index.html"
	}
	if strings.Contains(up, "..") {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	file := filepath.Join(s.web, filepath.Clean(up))
	st, err := os.Stat(file)
	if err != nil || st.IsDir() {
		// Single page app: unknown paths fall back to index.html.
		if filepath.Ext(up) != "" {
			http.NotFound(w, r)
			return
		}
		file = filepath.Join(s.web, "index.html")
		if _, err := os.Stat(file); err != nil {
			http.Error(w, "web UI not found: start the server with -web <dir>", http.StatusNotFound)
			return
		}
	}
	// The UI is served from disk and changes while the server runs. Revalidate every time (a 304 is
	// still cheap: ServeFile answers If-Modified-Since) so a reload always shows the edited file.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, file)
}

// logWrap logs one line per request.
func logWrap(logf func(string, ...any), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			logf("%s %s -> %d in %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes the connection through to the underlying ResponseWriter. A logging wrapper that
// forgets this breaks every WebSocket upgrade with "http.ResponseWriter does not implement
// http.Hijacker", because websocket.Accept receives the wrapper instead of the real writer.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("server: underlying ResponseWriter is not a http.Hijacker")
	}
	conn, buf, err := h.Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, buf, err
}

// Unwrap lets http.ResponseController reach the real ResponseWriter through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
