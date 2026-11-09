package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
)

// Server wires the HTTP API to the session manager.
type Server struct {
	mgr  *agent.Manager
	hub  *Hub
	logf func(string, ...any)
	// web is the UI as an fs.FS and dir is where it came from: a directory on disk when one was named,
	// "" when it is the copy embedded in the binary. Serving it is the only thing that reads them, and
	// the two sources differ in exactly one respect — whether a file has a usable modification time.
	web fs.FS
	dir string
	// etags caches the ETag of an embedded file by name, because its content cannot change while the
	// process runs and hashing it once beats hashing it per request. Files from a directory are never
	// cached here: their identity has to be re-derived every request or an edit goes unnoticed.
	// The table is keyed by name alone, and only one filesystem is ever immutable (the copy inside the
	// binary), so names cannot collide across sources.
	etags sync.Map
	// testdata is the development mount: a directory on disk served under /testdata/, or nil when no
	// directory was named (ServeTestdata). It is never the copy inside the binary — a page that is still
	// being written has no business shipping — so its files always carry a modification time.
	testdata fs.FS
	// pprof is the operator's "serve Go's runtime profiles on the API port too" switch (EnablePprof).
	pprof bool
	// auth is the login gate around every route when one was configured
	// (EnableAuth); nil means this daemon answers anonymously, which is the
	// shape of every config written before the gate existed.
	auth *Auth
	// fps remembers the TLS fingerprint of each connecting address. It exists
	// whether or not TLS is on — without TLS nothing ever fills it, and the
	// rate-limit keys fall back to addresses on their own.
	fps *fpRecorder
}

// New creates the HTTP handler set.
//
// embedded is the UI compiled into the binary, rooted at the UI itself (fs.Sub(bareloop.WebUI, "web")),
// and it is what gets served when webDir is empty. webDir names a directory on disk to serve instead —
// that is the -web flag and the config's "web" key, and it is what makes an edit under web/ show up
// without a rebuild.
func New(mgr *agent.Manager, hub *Hub, embedded fs.FS, webDir string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{mgr: mgr, hub: hub, web: embedded, dir: webDir, logf: logf, fps: newFPRecorder(logf)}
	if webDir != "" {
		s.web = os.DirFS(webDir)
	}
	return s
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "time": time.Now().UTC()})
	})
	mux.HandleFunc("GET /api/sessions", s.handleList)
	mux.HandleFunc("POST /api/sessions", s.handleCreate)
	mux.HandleFunc("POST /api/sessions/import", s.handleImport)
	mux.HandleFunc("GET /api/session/{id}", s.handleGet)
	mux.HandleFunc("PATCH /api/session/{id}", s.handlePatch)
	mux.HandleFunc("DELETE /api/session/{id}", s.handleDelete)
	// The lighter sibling of DELETE: it gives back the RAM a conversation costs and touches nothing on disk.
	mux.HandleFunc("POST /api/session/{id}/unload", s.handleUnload)
	mux.HandleFunc("POST /api/session/{id}/fork", s.handleFork)
	mux.HandleFunc("GET /api/session/{id}/inspect", s.handleInspect)
	mux.HandleFunc("GET /api/session/{id}/payload", s.handlePayload)
	mux.HandleFunc("GET /api/session/{id}/export", s.handleExport)
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

	// Development files, when a directory was named (ServeTestdata). Off unless asked for is the point:
	// a released binary serves the UI and nothing else. It wins over the catch-all below for the same
	// reason the profiler does.
	if s.testdata != nil {
		mux.HandleFunc("/testdata/{rest...}", s.handleTestdata)
	}

	mux.HandleFunc("/", s.handleStatic)
	h := logWrap(s.logf, mux)
	if s.auth != nil {
		// Outside the log wrapper on purpose: the gate's own refusals are the
		// requests an operator most wants to see reasoned about, and they are
		// already logged inside the gate with their bucket counts.
		h = s.auth.Middleware(h)
	}
	return h
}

// EnableAuth installs the login gate. Like EnablePprof it has to happen before
// Handler() is called — the wrapper is chosen while the handler is built.
func (s *Server) EnableAuth(a *Auth) {
	s.auth = a
	a.fps = s.fps // failed logins charge TLS fingerprints when the daemon can see them
	a.startCleaner()
}

// Wrap puts the gate in front of a handler that does not come from Handler()
// — today that is one handler: the profiler on its own port, which shares the
// daemon's credentials rather than its anonymity.
func (s *Server) Wrap(h http.Handler) http.Handler {
	if s.auth == nil {
		return h
	}
	return s.auth.Middleware(h)
}

// FPHook is the tls.Config.GetConfigForClient that records client fingerprints
// (tlsfp.go). Wiring passes it to the server's TLS config when one exists; an
// operator later adding their own GetConfigForClient calls this one first.
func (s *Server) FPHook(chi *tls.ClientHelloInfo) (*tls.Config, error) {
	return s.fps.hook(chi)
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
		// A locked MCP list outranks the conflict guess below: the body asked for servers this
		// daemon will not host, and "409 the id exists" would send the caller off editing ids.
		if errors.Is(err, agent.ErrMCPLocked) {
			writeErr(w, http.StatusForbidden, err)
			return
		}
		// The usual case is that the directory already exists: the directory is the identity.
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"id": sess.ID, "status": sess.Status()})
}

// handleFork copies a session — the path it stands on (default), the path cut at an entry
// (?at in the body; "ctx" forks the window the next request carries), or the whole tree
// ("full": true) — into a new session whose id the caller picks: the daemon invents no
// directory names here either. Everything else about the copy is the daemon's business;
// no client downloads the world to hand it back.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var body struct {
		ID   string `json:"id"`
		At   string `json:"at,omitempty"`
		Full bool   `json:"full,omitempty"`
		Name string `json:"name,omitempty"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id, entries, err := s.mgr.Fork(r.Context(), sess.ID, agent.ForkOptions{
		ID:   strings.TrimSpace(body.ID),
		At:   strings.TrimSpace(body.At),
		Full: body.Full,
		Name: body.Name,
	})
	if err != nil {
		// A taken id is the create conflict wearing the fork's clothes; everything else
		// (an unknown at, an empty source) is this request's own mistake.
		if strings.Contains(err.Error(), "already exists") {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"id": id, "entries": entries})
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
// The default is a page, and the page is the ceiling of an answer: silence, a bare place name,
// an oversized limit — whatever the URL says, the largest transcript this endpoint hands over is
// one page (ir.MaxPageEntries at most). An endpoint that ships images inline must not answer to
// the size of the session: one session's unparameterised read measured 73 MB, and no request —
// silence included — is a promise to pay for it. The answer is the *path* (the sequence that gets
// sent and the sequence the transcript draws), not every entry in the log including the branches
// nobody is on.
//
// Two other shapes answer alone, each refusing to be combined with a window: ?config=1 returns
// config and status without walking the path (the settings dialog reads a config, not a
// transcript), and ?usage=1 returns the whole path as a skeleton — every row's usage and shape,
// no content and no images — which is all the cost panel has ever needed. The skeleton is the
// one whole-path answer the endpoint keeps, and keeps because a skeleton row is an order of
// small bytes: the bill of a session is bounded by its length, not by what was said in it.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	configOnly, err := triFlag(q, "config")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	usageOnly, err := triFlag(q, "usage")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if configOnly && usageOnly {
		writeErr(w, http.StatusBadRequest, errors.New("config=1 and usage=1 answer different questions; pick one"))
		return
	}
	// Why each alone-shaped request refuses the window names: an answer with no entries inside a
	// window is a page nobody asked for, and a skeleton has no pages to cut.
	combos := q.Get("limit") != "" || q.Get("all") != "" || q.Get("anchor") != "" || q.Get("before") != "" || q.Get("after") != ""
	if configOnly {
		if combos {
			writeErr(w, http.StatusBadRequest, errors.New("config=1 answers config and status alone; drop the window parameters"))
			return
		}
		cfg := sess.Config()
		cfg.LLM.APIKey = "" // the key is never echoed back to the browser
		writeJSON(w, map[string]any{"config": cfg, "status": sess.Status()})
		return
	}
	if usageOnly {
		if combos {
			writeErr(w, http.StatusBadRequest, errors.New("usage=1 answers the whole path as a skeleton; drop the window parameters"))
			return
		}
		// The skeleton arrives row by row from disk (store.Skeleton): the cost panel wants
		// arithmetic, and reading the session's whole conversation to answer it was never the deal.
		entries, err := sess.UsageRows()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		cfg := sess.Config()
		cfg.LLM.APIKey = "" // the key is never echoed back to the browser
		writeJSON(w, map[string]any{"config": cfg, "status": sess.Status(), "entries": entries, "count": len(entries)})
		return
	}
	o, err := pageQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The window is cut from the spine, and exactly the entries it names are inflated:
	// a page costs a page, however long the session has grown (ir.PageOfSpine).
	page, err := sess.SpinePage(o)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	found, err := sess.PageContents(page.IDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Each entry is encoded once and handed on as raw bytes: encoding/json passes a json.RawMessage
	// through unchanged, so adding up those lengths prices the page without encoding it twice.
	entries := make([]json.RawMessage, 0, len(page.IDs))
	size := 0
	for _, id := range page.IDs {
		e := found[id]
		if e == nil {
			continue // deleted between the cut and the read: the page loses a row, not a window
		}
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
	if len(page.IDs) > 0 {
		oldest, newest = page.IDs[0], page.IDs[len(page.IDs)-1]
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
			"count":     len(page.IDs),
			"total":     page.Total,
			"bytes":     size,
			"limit":     page.Limit,
			"maxLimit":  ir.MaxPageEntries,
			"clamped":   page.Clamped,
			"truncated": page.Truncated,
		},
	})
}

// SkeletonRow is one row of the ?usage=1 skeleton: what a request cost, not what it
// said — the cost panel's entire vocabulary, read off the spine row by row in
// store.Skeleton. The type lives there now; this endpoint only answers it. (kept as a
// pointer-comment for the vocabulary's home: store.SkeletonRow)

// triFlag reads one of this endpoint's picky boolean query parameters: absent, 0 or false is
// off, 1 or true is on, and anything else is a 400 — silently accepting "yes" is how two
// dialects accumulate until one of them eventually means the opposite.
func triFlag(q url.Values, name string) (bool, error) {
	switch v := q.Get(name); v {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be 1 or 0, got %q", name, v)
	}
}

// pageQuery reads the window parameters out of a request. What they mean — including which
// combinations are refused and what the ceiling is — is decided in ir.PageOf, where one rule about
// paging has one home and is testable without a server; this only turns strings into types,
// refuses a limit that is not a number, and sizes any request that names a place but no page.
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
	// An empty value reads as absent: ?before=&limit=20 is what a client that always serialises both
	// fields sends, and "no id" from an empty field is the same request as no field. A *bad* id is
	// refused in ir.PageOf, which is also where an anchor with no size is refused.
	// `after` is the empty value of the same kind: a client that always serialises its window sends
	// all three names, and an empty one is asking for no cut rather than for an id it does not have.
	o.Before = q.Get("before")
	o.After = q.Get("after")
	o.Anchor = q.Get("anchor")
	// The endpoint's size rule, applied here rather than inside ir: PageOf's zero value stays "no
	// limit", because that is the right answer for a library, but no shape a URL can take gets an
	// unbounded answer here. Silence gets the default page, and so does a bare before or after —
	// "everything older than here" is an unbounded slice wearing a cursor. No name on this
	// endpoint asks for a whole path at all (the one that did is retired, and it reads as any
	// unknown parameter now), so the largest transcript answer is one page whatever the request
	// says. (An anchor with no size stays ir's 400: naming a place asks for a page around it, and
	// inventing a size the caller never sized would be answering a question nobody asked.)
	if o.Limit == 0 && o.Anchor == "" {
		o.Limit = ir.DefaultPageEntries
	}
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
		// The lock is a policy refusal, not a malformed body: 403 says "this daemon decided so"
		// where 400 would say "your request is wrong", and the difference is what a client
		// retries — never the lock, possibly the JSON.
		if errors.Is(err, agent.ErrMCPLocked) {
			writeErr(w, http.StatusForbidden, err)
			return
		}
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

// handleUnload puts one session down without deleting it: the process closes its log handle, its MCP
// connections and its parsed conversation, while the directory stays exactly where it was. It is the
// other half of the route two lines above - GET /api/session/{id} opens a session as a side effect of
// looking at it, and this is how that side effect is undone without anybody losing a conversation.
//
// Like handleFlags it does not go through s.session on purpose: that helper opens whatever it resolves,
// and a route whose entire job is to close something cannot begin by opening it. So it works from {id}
// and the manager alone.
//
// A session with a run or a summary in flight is refused with a 400 naming the phase (see
// Manager.Unload), exactly as a config PATCH refuses mid-run: stop it first, then put it down. The body
// is the row of the session list this just produced - closed:true - so a caller repaints the line it
// acted on without asking a second question, and "unloaded":false is the honest answer for a session
// this process was not holding: it is shut, which is what was asked for.
func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wasOpen, err := s.mgr.Unload(id)
	if err != nil {
		// A typo and a busy session both land here, and the sentence says which, because the caller's
		// next move is completely different: correct the name, or press stop.
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if wasOpen {
		// The replay buffer is what catches a reconnecting page up on a session this process is
		// holding. There is no such session now, and everything the buffer held is on disk and comes
		// back with the next read - which is the whole promise of unloading.
		s.hub.DropSession(id)
	}
	for _, sum := range s.mgr.List() {
		if sum.ID == id {
			writeJSON(w, map[string]any{"ok": true, "unloaded": wasOpen, "session": sum})
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "unloaded": wasOpen})
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
	payload, err := sess.Payload()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, payload)
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
	// The ruler is cut from the spine and quoted by exactly the turn-start rows it keeps.
	list, err := sess.TurnRows(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
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
// refused here rather than ignored: a client that sends before=, after= or anchor= wants a slice of
// turns this endpoint cannot give, and answering with the newest turns anyway is how a ruler ends up
// drawing the start of a path it has only seen the end of.
func turnLimit(r *http.Request) (int, error) {
	q := r.URL.Query()
	for _, name := range []string{"before", "after", "anchor"} {
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

	// Replay what this client missed while it was away, in order and one at a time: the ring's own
	// sequence is what keeps replay and live delivery from racing, and a catch-up should not sit on
	// a buffer while the live stream waits behind it.
	for _, e := range s.hub.Snapshot(id) {
		if !s.writeSSE(w, e) {
			return
		}
		flusher.Flush()
	}

	// Live: one SSE record per event, written as it arrives. The per-subscriber channel may drop
	// frames under backpressure — counted and logged at the hub; the seq stamp lets a client
	// notice (and a page re-reads the session, which is the source of truth anyway).
	//
	// One write per event stays right here for a reason the websocket needed a pacer for: an LLM
	// streaming an answer is one record per token. What keeps that affordable is upstream — the
	// agent coalesces its deltas before emitting them (see internal/agent/delta.go), so this loop
	// sees a steady stream and not a token firehose.
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-c.out:
			if !ok {
				return
			}
			if !s.writeSSE(w, e) {
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

// writeSSE writes one event as one SSE frame. The bool is whether the stream may keep being used: a
// failed write means the peer is gone.
//
// An event that cannot be encoded costs only itself, so this returns true and the stream goes on — but it
// used to do that in silence, which made it a second place (after the websocket pump) where a page can
// stop being told something about a session and nobody, including the page, can tell. `Data any` is what
// makes encoding failure possible at all; if this line never prints, nothing is being dropped.
func (s *Server) writeSSE(w io.Writer, e agent.Event) bool {
	b, err := json.Marshal(e)
	if err != nil {
		s.logf("sse: dropping event %s/%s seq %d, it cannot be encoded: %v", e.Session, e.Type, e.Seq, err)
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
	if s.web == nil {
		http.Error(w, "no web UI: this binary carries no embedded copy and was given no -web directory", http.StatusServiceUnavailable)
		return
	}
	// The name of a file in an fs.FS is unrooted and slash-separated, and cleaning it against an
	// artificial root is what makes the handler safe by construction rather than by suspicion: "a/../b"
	// collapses to "b", and no spelling of ".." — encoded or not — ends up above the UI. Anything that
	// still tried to escape is therefore just some other file, which is to say a 404 or the entry point.
	name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/")), "/")
	if name == "" {
		name = "index.html"
	}
	if st, err := fs.Stat(s.web, name); err != nil || st.IsDir() {
		// Single page app: unknown paths fall back to index.html.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(s.web, "index.html"); err != nil {
			http.Error(w, s.noWeb(), http.StatusNotFound)
			return
		}
		name = "index.html"
	}
	// index.html is the entry point that names every asset, so it is never reused without asking;
	// that is what makes an edited UI, or an embedded one from a new binary, show up on navigation.
	s.serveFile(w, r, s.web, name, s.dir == "", name == "index.html")
}

// ServeTestdata mounts a directory of development files at /testdata/, and it has to be called before
// Handler because that is where the routes get built.
//
// It exists so a page that is still being written can be opened in a browser without being compiled
// into the binary, and without a second web server: fixtures too, which are the reason a test page
// cannot simply be a file:// URL (a module import and a fetch both refuse the scheme). The directory is
// never searched for: naming none is the default, and a released binary therefore has one fewer surface.
//
// What is served is whatever the operator pointed at, on a port with nothing in front of it, so cmd
// refuses the one spelling that turns that into an exposure — the directory bareloopd runs from, which
// is where config.json and the session logs live.
func (s *Server) ServeTestdata(dir string) { s.testdata = os.DirFS(dir) }

// handleTestdata serves the mount. It shares handleStatic's safety rule — a name is cleaned against an
// artificial root, so no spelling of ".." ends up above the mount — and its ETag machinery, but it makes
// three different decisions because this is a directory being edited rather than an application being
// delivered:
//
//   - a missing file is a 404 and never the entry point. The single-page fallback is what a route needs,
//     and what turns a mistyped fixture name into a response full of HTML;
//   - nothing is cacheable, because a staleness budget on a file you are editing reads as a browser that
//     will not refresh;
//   - and a directory is a 404 too: no listing, so a mount is a set of known URLs rather than a window
//     onto the disk.
func (s *Server) handleTestdata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	// The wildcard is whatever came after /testdata/, and cleaning it against a root the client cannot
	// name is what makes "/testdata/../../config.json" a file inside the mount or nothing at all.
	name := strings.TrimPrefix(path.Clean("/"+r.PathValue("rest")), "/")
	if st, err := fs.Stat(s.testdata, name); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, s.testdata, name, false, true)
}

// noWeb words the one way this can fail that the operator can fix: the named UI directory has no entry
// point in it. Both ways out are named, because the common cause is a binary that moved away from the
// tree it was built in and a config still pointing at the old place.
func (s *Server) noWeb() string {
	if s.dir == "" {
		return "web UI not found: the copy embedded in this binary has no index.html"
	}
	return fmt.Sprintf("web UI not found in %s: point -web at a directory holding an index.html, or drop it to serve the copy embedded in the binary", s.dir)
}

// serveFile writes one static file with an ETag. The two booleans are what differs between the sources
// that call it: immutable says the bytes cannot change while this process runs (only the copy inside
// the binary), and noCache asks for the entry point's answer — revalidate every time — which is also
// what the development mount wants.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string, immutable, noCache bool) {
	f, err := fsys.Open(name)
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
	etag, err := s.etag(fsys, name, st, immutable)
	if err != nil {
		s.logf("static %s: %v", name, err)
		http.Error(w, "static file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", etag)
	if noCache {
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
	body, ok := f.(io.ReadSeeker)
	if !ok {
		// Both sources in use — a directory and the embedded one — are seekable, so this is the fallback
		// and not the rule; a static file is small enough to buffer, which beats serving a body from the
		// wrong offset.
		b, err := io.ReadAll(f)
		if err != nil {
			s.logf("static %s: %v", name, err)
			http.Error(w, "static file", http.StatusInternalServerError)
			return
		}
		body = bytes.NewReader(b)
	}
	// ServeContent adds Last-Modified, honours If-Modified-Since and Range, and sets the content type
	// from the extension — a module script has to arrive as JavaScript to be imported. It reports nothing
	// back, which is fine: by then the response is on its way and logWrap has already seen the request.
	// An embedded file has no modification time, and a zero one leaves Last-Modified off, which is the
	// right answer — the ETag is what revalidates it.
	http.ServeContent(w, r, path.Base(name), st.ModTime(), body)
}

// etag is the identity of a static file, and it means one of two things depending on where the file
// lives: what is inside the binary cannot change while the process runs, and a file on disk can.
// A file that can change is its size plus its modification time, re-read on every request:
// that pair is exactly what an edit changes, and caching the answer is how an edit gets hidden. A file
// from the binary has no modification time to talk about (the archive does not carry one), so its
// identity is a hash of its bytes — and those cannot change while the process runs, so they are worth
// remembering. Either way the client sees the same one header and the same 304.
func (s *Server) etag(fsys fs.FS, name string, st fs.FileInfo, immutable bool) (string, error) {
	if !immutable {
		return fmt.Sprintf(`"%x-%x"`, st.ModTime().Unix(), st.Size()), nil
	}
	if cached, ok := s.etags.Load(name); ok {
		if et, ok := cached.(string); ok {
			return et, nil
		}
	}
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	etag := fmt.Sprintf(`"%x"`, sum[:12])
	s.etags.Store(name, etag)
	return etag, nil
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
