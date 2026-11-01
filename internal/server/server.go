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
	// pprof is the operator's "serve Go's runtime profiles on the API port too" switch (EnablePprof).
	pprof bool
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
	mux.HandleFunc("GET /api/session/{id}/tools", s.handleTools)
	mux.HandleFunc("GET /api/session/{id}/turns", s.handleTurns)
	mux.HandleFunc("POST /api/session/{id}/tools", s.handleToolsSet)
	mux.HandleFunc("POST /api/session/{id}/send", s.handleSend)
	mux.HandleFunc("POST /api/session/{id}/interrupt", s.handleInterrupt)
	mux.HandleFunc("POST /api/session/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /api/session/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/session/{id}/compact", s.handleCompact)
	mux.HandleFunc("POST /api/session/{id}/gate", s.handleGate)
	mux.HandleFunc("POST /api/session/{id}/elicit", s.handleElicit)
	mux.HandleFunc("POST /api/session/{id}/entry/patch", s.handlePatchEntry)
	mux.HandleFunc("POST /api/session/{id}/entry/delete", s.handleDeleteEntry)
	mux.HandleFunc("POST /api/session/{id}/tip", s.handleTip)
	mux.HandleFunc("POST /api/session/{id}/reload", s.handleReload)
	mux.HandleFunc("POST /api/session/{id}/flags", s.handleFlags)
	mux.HandleFunc("GET /api/session/{id}/events", s.handleSSE)
	mux.HandleFunc("GET /ws", s.handleWS)

	// The profiler, when asked for: its own mux keeps the /debug/pprof/ prefix intact (pprof.Index
	// reads the profile name out of the URL), and it wins over the catch-all below because ServeMux
	// prefers the more specific pattern.
	if s.pprof {
		mux.Handle("/debug/pprof/", PprofHandler())
	}

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
	cfg.ID = strings.TrimSpace(cfg.ID)
	// An id is a directory name, so the server never invents one: a stray submit used to leave a
	// session named after the clock in the data directory, which nobody asked for.
	if cfg.ID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("a session id is required"))
		return
	}
	if err := agent.ValidID(cfg.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.mgr.Create(r.Context(), cfg)
	if err != nil {
		// The usual case is that the directory already exists: the directory is the identity.
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

// handleGet is the transcript endpoint, and the only place a window is chosen.
//
// Without a limit it answers with the whole path, which is what it has always promised: the cost panel
// makes one unparameterised call and prices the entire path from it, so a default page would quote a
// part of a session as if it were the whole bill. What changed is that the answer is the *path* — the
// sequence that gets sent and the sequence the transcript draws — instead of every entry in the log
// including the branches nobody is on, which the browser used to receive and discard.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	o, err := pageQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	page, err := ir.PageOf(sess.Path(), o)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Each entry is encoded once and handed on as raw bytes: encoding/json passes a json.RawMessage
	// through unchanged, so adding up those lengths prices the page without encoding it twice.
	entries := make([]json.RawMessage, 0, len(page.Entries))
	size := 0
	for _, e := range page.Entries {
		raw, err := json.Marshal(e)
		if err != nil {
			// An entry that will not encode is a broken log, not a bad request.
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		size += len(raw)
		entries = append(entries, raw)
	}
	oldest, newest := "", ""
	if len(page.Entries) > 0 {
		oldest, newest = page.Entries[0].ID, page.Entries[len(page.Entries)-1].ID
	}
	cfg := sess.Config()
	cfg.LLM.APIKey = "" // the key is never echoed back to the browser
	writeJSON(w, map[string]any{
		"config":  cfg,
		"status":  sess.Status(),
		"entries": entries,
		"pending": sess.PendingIDs(),
		// window is always present, so the client has one code path for a page and for a whole path.
		"window": map[string]any{
			"oldestId":  oldest,
			"newestId":  newest,
			"hasOlder":  page.HasOlder,
			"hasNewer":  page.HasNewer,
			"count":     len(page.Entries),
			"total":     page.Total,
			"bytes":     size,
			"limit":     page.Limit,
			"maxLimit":  ir.MaxPageEntries,
			"clamped":   page.Clamped,
			"truncated": page.Truncated,
		},
	})
}

// pageQuery reads the window parameters out of a request. What they mean — including which
// combinations are refused and what the ceiling is — is decided in ir.PageOf, where one rule about
// paging has one home and is testable without a server; this only turns strings into types and
// refuses a limit that is not a number.
func pageQuery(r *http.Request) (ir.PageOptions, error) {
	q := r.URL.Query()
	o := ir.PageOptions{}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return o, fmt.Errorf("limit must be a number, got %q", v)
		}
		// Zero and negatives mean "no limit" inside ir, and a browser control that lands on 0 should
		// not silently ask for the whole session instead.
		if n < 1 {
			return o, fmt.Errorf("limit must be at least 1, or left out for the whole path, got %d", n)
		}
		o.Limit = n
	}
	switch v := q.Get("all"); v {
	case "", "0", "false":
	case "1", "true":
		o.All = true
	default:
		return o, fmt.Errorf("all must be 1 or 0, got %q", v)
	}
	// An empty value reads as absent: ?before=&limit=20 is what a client that always serialises both
	// fields sends, and "no id" from an empty field is the same request as no field. A *bad* id is
	// refused in ir.PageOf, which is also where an anchor with no size is refused.
	o.Before = q.Get("before")
	o.Anchor = q.Get("anchor")
	return o, nil
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

// flagsRequest is the body of POST /api/session/{id}/flags. Either flag on its own is a complete request,
// and a field the body does not carry is a field nobody mentioned - which is why these are pointers and
// not booleans: a false in the body is an order, and its absence is not.
type flagsRequest struct {
	Pinned   *bool `json:"pinned"`
	Archived *bool `json:"archived"`
}

// handleFlags is the one writer of the two list flags, and the only route here that acts on a session
// without opening it: pinning is four bytes of config.json, and opening is reading a transcript.
func (s *Server) handleFlags(w http.ResponseWriter, r *http.Request) {
	var q flagsRequest
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if q.Pinned == nil && q.Archived == nil {
		writeErr(w, http.StatusBadRequest, errors.New("say at least one of pinned, archived"))
		return
	}
	// why refused here rather than left to the merge: one body that asks to be both has not decided what it
	// wants, and guessing which half to drop is how a list starts disagreeing with the person reading it.
	// Archiving a session that is already pinned is a different thing - a change of mind - and that one
	// Manager.SetFlags resolves by dropping the other half.
	if q.Pinned != nil && q.Archived != nil && *q.Pinned && *q.Archived {
		writeErr(w, http.StatusBadRequest, errors.New("a session is either pinned or archived, and this asked for both"))
		return
	}
	id := r.PathValue("id")
	if err := s.mgr.SetFlags(id, q.Pinned, q.Archived); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The answer is the row itself, made from the same two files GET /api/sessions reads: a caller can
	// repaint one line without asking twice, and the row says closed:true for a session this route just
	// pinned, which is the whole point of the route.
	for _, sum := range s.mgr.List() {
		if sum.ID == id {
			writeJSON(w, map[string]any{"ok": true, "session": sum})
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- actions

// sendRequest is the body of /send and /interrupt.
type sendRequest struct {
	Text   string        `json:"text"`
	Images []ir.Part     `json:"images,omitempty"`
	Files  []uploadImage `json:"files,omitempty"`
	Label  string        `json:"label,omitempty"`
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

// handleResume continues a run from the current tip: no new message is appended, the path already ends
// on a boundary that expects an answer (a user message, a tool result, a compaction summary). This is
// what to press when a run was interrupted half way through the loop.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if err := sess.Resume(r.Context()); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "status": sess.Status()})
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

// toolsRequest is the body of POST /api/session/{id}/tools: one server's tool switches, and/or
// the prefix its tools are published under. Absent fields are not changes — which is what lets the
// drawer send one small object instead of the whole config.
type toolsRequest struct {
	Server        string   `json:"server"`
	DisabledTools []string `json:"disabledTools"`
	ToolPrefix    *string  `json:"toolPrefix"`
}

// handleTools is the tools drawer: every tool the session's MCP servers offer, what each one is
// estimated to cost, how often this session used it, and whether a change is still waiting for the
// end of the run. It answers in any phase and never asks an MCP server anything.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, sess.Tools())
}

// handleToolsSet is the narrow alternative to PATCH /api/session/{id}: it changes only the tool
// policy of one MCP server, and it works while the session is running, because the change waits
// for the end of the run instead of invalidating the payload in flight. The whole-config PATCH
// still refuses mid-run on purpose — it can change the model or the assembler, which is a
// different promise. See README (§tools).
func (s *Server) handleToolsSet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var q toolsRequest
	if err := decodeJSON(r, &q); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(q.Server) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("server is required"))
		return
	}
	view, err := sess.SetToolPolicy(strings.TrimSpace(q.Server), q.DisabledTools, q.ToolPrefix)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, view)
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

// handleTurns answers the shape of a transcript without its content: one row per turn of the path,
// oldest first, taken from the newest end when they do not all fit.
//
// It exists because the alternative is reading the whole transcript to find your way around it. A
// client that wants to offer "jump back to that question from three days ago" needs a few hundred ids
// and first lines; getting them out of GET /api/session/{id} carries every word of the answers along,
// which is the same reason that endpoint learned to page.
func (s *Server) handleTurns(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	limit, err := turnLimit(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	list := ir.TurnsOf(sess.Path(), limit)
	writeJSON(w, map[string]any{
		"turns":    list.Turns,
		"count":    len(list.Turns),
		"total":    list.Total,
		"hasOlder": list.HasOlder,
		// The transcript's own words, so a client learns one vocabulary: limit is the ceiling in force,
		// maxLimit the largest it will ever apply, clamped the request that ran into it.
		"limit":    list.Limit,
		"maxLimit": ir.MaxTurnRows,
		"clamped":  list.Clamped,
	})
}

// turnLimit reads the one parameter this endpoint has. What the transcript endpoint takes by name are
// refused here rather than ignored: a client that sends before= or anchor= is asking for a slice of
// turns this endpoint cannot give, and answering with the newest turns anyway is how a ruler ends up
// drawing the start of a path it has only seen the end of.
func turnLimit(r *http.Request) (int, error) {
	q := r.URL.Query()
	for _, name := range []string{"before", "anchor", "all"} {
		// An empty value reads as absent, which is what this server already does with those fields: a
		// client that always serialises them all is not asking for anything.
		if v := q.Get(name); v != "" {
			return 0, fmt.Errorf("turns does not take %s=%q: it lists whole turns from the newest end", name, v)
		}
	}
	v := q.Get("limit")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("limit must be a number, got %q", v)
	}
	// Zero and negatives mean "no limit" inside ir, and a browser control that lands on 0 should not
	// silently ask for the whole session instead.
	if n < 1 {
		return 0, fmt.Errorf("limit must be at least 1, or left out for as many as the ceiling allows, got %d", n)
	}
	return n, nil
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

// Cache policy for the UI. Asset names are not content-hashed, so a cache lifetime is the only
// staleness budget — short enough that an upgrade self-heals, long enough to skip the body:
// every response carries an ETag, so revalidation costs a 304 and nothing else.
const (
	assetCache = "public, max-age=60, must-revalidate"
	pageCache  = "no-cache, must-revalidate"
)

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
	// index.html is the entry point that names every asset, so it is never reused without asking;
	// that is what makes an edited UI, or an embedded one from a new binary, show up on navigation.
	s.serveFile(w, r, file, strings.HasSuffix(file, "index.html"))
}

// serveFile writes one static file with an ETag. The identity is size plus modification time, which
// is exactly what changes when a file on disk is edited; an embed.FS can hand over a content hash
// here instead and nothing else about the protocol changes.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path string, isPage bool) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%x-%x"`, st.ModTime().Unix(), st.Size())
	w.Header().Set("ETag", etag)
	if isPage {
		w.Header().Set("Cache-Control", pageCache)
	} else {
		w.Header().Set("Cache-Control", assetCache)
	}
	if match := r.Header.Get("If-None-Match"); match != "" {
		for _, want := range strings.Split(match, ",") {
			if strings.TrimPrefix(strings.TrimSpace(want), "W/") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	// ServeContent adds Last-Modified, honours If-Modified-Since and Range, and sets the content type
	// from the extension — a module script has to arrive as JavaScript to be imported.
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

// logWrap logs one line per request.
func logWrap(logf func(string, ...any), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" || strings.HasPrefix(r.URL.Path, "/debug/pprof/") {
			// The pprof path is in the list because on the shared port a 30-second CPU profile leaves
			// no other trace; the profiler's own listener is not wrapped at all.
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
