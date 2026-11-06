package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
	"bareloop/internal/mcp"
	"bareloop/internal/store"
)

// Session phases.
const (
	PhaseIdle       = "idle"
	PhaseRunning    = "running"
	PhaseCompacting = "compacting"
)

// Message is one user message. Images travel as ir.Part with Type=image and base64 Data.
type Message struct {
	Text   string    `json:"text,omitempty"`
	Images []ir.Part `json:"images,omitempty"`
	// Label annotates the entry in the UI (e.g. "sent during compaction").
	Label string `json:"label,omitempty"`
}

// phase is the session's exclusive-work state (§1.1: compaction never runs mid-stream).
type phase int

const (
	phaseIdle phase = iota
	phaseRunning
	phaseCompacting
)

// gateDecision is the answer to the oversized-summary gate (§3.4).
type gateDecision struct {
	answer GateAnswer
}

// Session is one conversation: its own path in storage, its own MCP server set, system
// prompt, context parameters and compaction parameters (multi-session requirement).
type Session struct {
	ID  string
	mgr *Manager

	log *store.Log

	mu  sync.Mutex
	st  *store.State
	cfg Config

	phase phase
	// cancel is a cause-carrying one because a wait has to be able to tell "the clock ended me" from
	// "a person ended me and their message follows" — see builtin.ErrUserInterrupt and Stop/Interrupt.
	cancel context.CancelCauseFunc
	done   chan struct{}
	// park is the wait this daemon is holding instead of a run (park.go). It is the whole answer to
	// "is this session asleep", which is why there is no second parked flag to keep in step.
	park *parked
	// held is the tool call whose answer has not been written yet, and is why anything sent now is held
	// rather than appended: appended now it would sit between that call and its result, and the assembler
	// answers that shape by inventing a failure for a tool that is merely still waiting. It is a separate
	// field from park because the wake that answers the call is a live run with no park, and the hole a
	// message could fall into is open for as long as held is set rather than for as long as a timer is.
	// The queue it protects lives only here: a restart drops it, which is what docs/ says out loud.
	held  string
	queue []Message
	// awaitingReply is the third shape of the debt park.go describes: a payload has gone out and its
	// reply is not on the path yet. Appending anything in that window puts the message in front of the
	// answer that never saw it — and when that turn ends without tool calls nothing answers it at all,
	// because the reply is committed at the tip and the loop's own "is an answer owed" test reads only
	// the tip. Opened by markSent, closed the moment that reply exists (loop.go stopAwaiting).
	awaitingReply bool
	// generating is true only while an LLM stream is open. Compaction may run between turns
	// of a run (the loop calls it itself) but never while tokens are flowing (§1.1).
	generating bool
	// forceRun is a promise made to an interrupting message: the run being aborted cannot be the run
	// that answers it, because that run's context is about to be dead and every request built from it
	// fails on the way out. Its epilogue starts the run that does.
	forceRun bool
	// acceptedWhileBusy is the same promise for every other message: one reached this session from a
	// person while a run owned it, so the end of that run is not allowed to be the end of its story.
	// Without it only an interrupting message was ever answered at run end, and any other accepted message
	// - a plain send that landed in the last instant of a run, or one written out of the queue behind a
	// summary the person stopped - reached the path with nobody left owing it an answer.
	//
	// It is written and collected under s.mu, and that is the whole argument that a message cannot fall
	// between the two: a send either happens while the phase still names a run, and is then counted before
	// that run's epilogue puts the phase back to idle, or it happens after and starts a run of its own.
	//
	// Three hands spend it, because those are the three things that can own the message: the run's epilogue
	// (which may start the run that answers), endManualCompaction (which flushes and starts one), and Stop -
	// stopping is the person deciding that nobody says anything more.
	acceptedWhileBusy bool
	turn              int
	// sentTip is the path tip that was included in the payload we last sent. Everything on
	// the path after it is "pending": a message the user typed while the loop was running.
	sentTip  string
	streamNo int
	lastRun  *RunInfo
	ctxUsed  int
	ctxKnown bool
	nctx     int // effective window: see nctx.go (never persisted)
	// nctxCfg is what config.json says (the operator's ceiling); nctxServer is what the server
	// last reported. Keeping them apart is the only way to explain a decision, and nctx itself is
	// derived from the two on every probe.
	nctxCfg     int
	nctxServer  int
	nctxProbed  bool // asked since this process started
	nctxProbing bool // probe in flight (they are cheap but they are HTTP)
	updated_at  time.Time

	// compaction lock (§1.4): taken before the first await, covers summary + gate wait.
	compMu     sync.Mutex
	compacting bool
	// compactCancel is the one hand on a manual summary that is in flight, and Stop() is the only
	// fingers on it. It exists because a summary belongs to the session rather than to the request
	// that asked for it (CompactNow), and a thing that outlives its request needs somebody else to be
	// able to end it. Nil while nothing is summarising.
	compactCancel context.CancelCauseFunc
	gate          chan gateDecision
	gateID        string

	// pendingElicit waits for the UI to answer an MCP input request.
	pmu           sync.Mutex
	pendingElicit map[string]chan mcp.ElicitResponse

	reg      *registry
	provider llm.Provider

	// lastSent is the payload the session actually sent last, reused verbatim as the prefix
	// of the summary request (spec §5.2). lastIDs names the entry behind each of those messages
	// (asm.EntryIDs), which is the only way to know whether that payload is still a prefix of the
	// path — a KV cache is keyed on bytes, so a payload that differs anywhere is worth nothing.
	lastSent  []asm.Message
	lastIDs   []string
	lastTools []llm.Tool

	// tmu guards toolSnapshot. It is deliberately not mu: the tool snapshot is read by build(), and
	// build() is also called from paths that hold mu (the summary request), so a nested mu would
	// be a deadlock.
	tmu sync.Mutex
	// toolSnapshot is the tool array the current run sends on every turn, snapshotted when the run
	// starts so a policy change cannot slip into a run in flight. nil means "no run: use the live
	// published set". Whether a policy change is still waiting is never stored: it is derived from
	// "config vs registry" (registry.PolicyPending), so it cannot go stale.
	toolSnapshot []llm.Tool
}

// openSession opens (creating if needed) the session stored in the directory named id. The directory
// is the identity, so cfg.ID is set from it rather than trusted.
func openSession(ctx context.Context, mgr *Manager, id string, cfg Config) (*Session, error) {
	cfg.Compact = cfg.Compact.Normalise()
	if id == "" {
		return nil, errors.New("agent: session id is required")
	}
	cfg.ID = id
	dir := filepath.Join(mgr.root, id)
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	log, err := store.Open(dir, mgr.fsync)
	if err != nil {
		return nil, err
	}
	// An index problem is never a reason a session cannot open, so the only thing the log can tell this
	// program is what went wrong with it - and that is worth one line, once, in the same shape as every other
	// line about this session. It is set before the read, because the read is where an index is rebuilt.
	log.SetLogf(func(format string, args ...any) {
		mgr.logf("session "+id+": "+format, args...)
	})
	st, err := log.Replay()
	if err != nil {
		_ = log.Close()
		return nil, fmt.Errorf("replay %s: %w", cfg.ID, err)
	}
	// The endpoint is dialled through the proxy resolved for it — its own, else the session's, else whatever
	// the environment says. llmDial is a copy, so cfg and the config on disk keep saying only what was
	// configured there (proxy.go).
	prov, err := llm.New(cfg.llmDial())
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	s := &Session{
		ID:            id,
		mgr:           mgr,
		log:           log,
		cfg:           cfg,
		st:            st,
		provider:      prov,
		pendingElicit: map[string]chan mcp.ElicitResponse{},
	}
	s.reg = newRegistry(s.emit, s.logf, cfg.NotifyTools)
	s.reg.askElicit = s.askElicit
	// Install the tool policy before any server is listed, so tools are filtered from the first
	// tools/list rather than published for one turn and taken back.
	s.reg.SetPolicy(cfg)
	s.sentTip = tipOfPending(st)
	// The configured window is the starting point only: before the session sends anything the
	// server is asked once and the effective value is re-derived (nctx.go). Nothing probed is ever
	// written back to config.json.
	s.nctxCfg = cfg.LLM.NCTX
	s.nctx = cfg.LLM.NCTX
	for _, id := range st.Adopted {
		s.logf("adopted orphan compaction entry %s onto the path tip (§3.10)", id)
	}
	// MCP servers are started in the background: a slow or broken server must not block
	// opening a session, and the tools appear as soon as they are ready.
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.reg.connect(cctx, s.Config().mcpDial())
		s.emitState()
	}()
	// The previous process may have been holding a wait when it went away, and the log is the only
	// record of it: this either re-arms the wake-up or answers the call that came due meanwhile. It is
	// after the tool policy above because the question it asks is "is this an unanswered builtin call".
	s.recoverPark()
	s.logStructuredMode(cfg)
	s.logPolicy(cfg)
	return s, nil
}

// tipOfPending is the initial "already sent" watermark: everything on an existing path has
// been sent at some point, so nothing shows as pending when a session is reopened.
func tipOfPending(st *store.State) string { return st.Tip }

// Log exposes the session log (used by the HTTP layer for imports).
func (s *Session) Log() *store.Log { return s.log }

// Config returns a copy of the session configuration.
func (s *Session) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *Session) logf(format string, args ...any) {
	if s.mgr != nil {
		s.mgr.logf("session %s: "+format, append([]any{s.ID}, args...)...)
	}
}

// emit publishes one event, stamped with the session id.
func (s *Session) emit(e Event) {
	e.Session = s.ID
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}
	if s.mgr != nil {
		s.mgr.publish(e)
	}
}

// ---------------------------------------------------------------- path access

// path returns the current root->tip path.
func (s *Session) path() []*ir.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ir.Path(s.st.Entries, s.st.Tip)
}

// appendEntry appends one entry at the tip (single writer per session, tip advance in the
// same record, so no half-committed state is observable).
func (s *Session) appendEntry(e *ir.Entry) (*ir.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(e)
}

// appendLocked is appendEntry with s.mu already held, which exists so one caller can commit more than
// one entry in a single critical section: flushHeld writes the queue it has just emptied, and a message
// sent meanwhile has to land after all of them rather than between two (ordering, park.go).
func (s *Session) appendLocked(e *ir.Entry) (*ir.Entry, error) {
	if e.ParentID == "" {
		e.ParentID = s.st.Tip
	}
	if err := s.log.AppendEntry(e, true); err != nil {
		return nil, err
	}
	cp := *e
	s.st.Entries[e.ID] = &cp
	s.st.Tip = e.ID
	s.updated_at = e.Ts
	return &cp, nil
}

// effective returns the path the model actually sees (spec §3.2: planning input and
// assembler input are produced by the same function, structurally identical).
func (s *Session) effective() (path []*ir.Entry, kept []*ir.Entry, cmp *ir.Entry) {
	s.mu.Lock()
	path = ir.Path(s.st.Entries, s.st.Tip)
	kept, cmp = ir.Effective(s.st.Entries, path)
	s.mu.Unlock()
	return path, kept, cmp
}

// build runs the single assembler over the effective path (§5.4 choke point).
func (s *Session) build() (*asm.Built, *asm.Sizes, []llm.Tool) {
	_, kept, cmp := s.effective()
	built := asm.Assemble(asm.Payload{System: s.Config().SystemPrompt, Kept: kept, Summary: cmp}, s.cfgOptions())
	sizes := asm.DeriveSizes(built)
	return built, sizes, s.visibleTools()
}

// visibleTools is the tool array of the payload being built: the run's snapshot while a run is in
// flight, the published set otherwise. The snapshot is what makes "a tool switch never affects a
// request that is already on the wire" true by construction rather than by timing.
func (s *Session) visibleTools() []llm.Tool {
	s.tmu.Lock()
	snap := s.toolSnapshot
	s.tmu.Unlock()
	if snap != nil {
		return snap
	}
	return s.reg.Tools()
}

// cfgOptions is the assembler input: the asm block, plus the structuredContent modes — which live
// with the servers and the session, not in the asm block (see asm.Options.Structured).
func (s *Session) cfgOptions() asm.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.cfg.ASM
	t := structuredTargets(s.cfg)
	o.Structured = &t
	o.StructuredMaxBytes = s.cfg.StructuredMaxBytes
	return o
}

// structuredTargets resolves how every server's structuredContent is sent: the server's own value
// wins, then the session default, then asm.StructuredAuto.
//
// A value that cannot be parsed is treated as unset rather than as an error, because this runs on
// every payload: a hand-edited data/<id>/config.json must not be able to stop a session from
// answering. The live path cannot store one — Create and UpdateConfig both reject it.
func structuredTargets(cfg Config) asm.StructuredTargets {
	t := asm.StructuredTargets{Default: asm.StructuredAuto}
	if m, err := asm.ParseStructuredMode(cfg.StructuredContent); err == nil {
		t.Default = m
	}
	for _, mc := range cfg.MCP {
		if mc.StructuredContent == "" {
			continue // no override: this server follows the session
		}
		if m, err := asm.ParseStructuredMode(mc.StructuredContent); err == nil {
			if t.ByServer == nil {
				t.ByServer = map[string]asm.StructuredMode{}
			}
			t.ByServer[mc.Name] = m
		}
	}
	return t
}

// logStructuredMode says the one thing the setting itself cannot show: the word is applied while
// assembling, so it rewrites the tool results a session already has. A session with history whose
// turns ran under a different word loses its KV prefix on the next request — worth one line at the
// two moments anyone learns about it, which are boot and the patch that changed it.
func (s *Session) logStructuredMode(cfg Config) {
	def := structuredTargets(cfg).Default
	if def != asm.StructuredAuto {
		s.logf("structuredContent = %s: every tool result in the history is folded with it, so a session whose turns ran under another word re-prefills on the next request", def)
	}
	for _, mc := range cfg.MCP {
		mode, err := asm.ParseStructuredMode(mc.StructuredContent)
		if err == nil && mode != def {
			s.logf(`structuredContent for MCP server %q = %s, overriding %q: its results are refolded the same way`, mc.Name, mode, def)
		}
	}
}

// pendingIDs lists user messages that are on the path but were not part of a payload we
// sent yet — the queue the UI shows while the loop is running or compaction is in flight.
func (s *Session) pendingIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked()
}

func (s *Session) pendingLocked() []string {
	path := ir.Path(s.st.Entries, s.st.Tip)
	sent := s.sentTip
	var out []string
	for _, e := range path {
		if e.Kind != ir.KindUser {
			continue
		}
		if sent == "" || e.ID > sent {
			out = append(out, e.ID)
		}
	}
	return out
}

// markSent records that this payload covered the path up to the tip — and opens the debt it creates:
// from here until its reply is committed, the path is standing on a request whose answer does not
// exist yet, so a message appended now would travel ahead of it (Session.awaitingReply).
func (s *Session) markSent() {
	s.mu.Lock()
	s.sentTip = s.st.Tip
	s.awaitingReply = true
	s.mu.Unlock()
}

// stopAwaiting closes the debt one request opened. It is called the moment that request's reply is on
// the path, and also when it is certain no reply is coming — never earlier, because the reply is what
// anything held in the queue is waiting to be behind.
func (s *Session) stopAwaiting() {
	s.mu.Lock()
	s.awaitingReply = false
	s.mu.Unlock()
}

// ---------------------------------------------------------------- status

// Status reports the session status for the UI progress bar (contextUsed comes from real
// usage — the same source the compaction trigger uses, spec §0).
func (s *Session) Status() *Status {
	s.mu.Lock()
	cfg := s.cfg
	phase := phaseToString(s.phase)
	tip := s.st.Tip
	entries := len(s.st.Entries)
	turn := s.turn
	used, known := s.ctxUsed, s.ctxKnown
	nctx, cfgN, srvN := s.nctx, s.nctxCfg, s.nctxServer
	var (
		waiting bool
		wakeAt  time.Time
		queued  int
	)
	if s.park != nil {
		waiting, wakeAt = true, s.park.until
	}
	// What is held is reported whenever it exists rather than only while parked: the wake is a live run
	// with an unanswered call, and a queue nobody can see during those seconds is a queue that reads lost.
	queued = len(s.queue)
	s.mu.Unlock()

	_, _, cmp := s.effective()
	published, enabled, total := s.reg.counts()
	pendingTools := s.reg.PolicyPending(cfg)
	if !known {
		// Fall back to the last real usage on the path (never an estimate), stopping at the active
		// compaction entry: usage from before it describes a payload that no longer exists (1.5),
		// so the honest answer for that turn is contextKnown=false and a dashed-down bar.
		stop := cmp != nil && cmp.Compaction != nil && !cmp.Compaction.Broken
		if u := ir.LastUsage(s.path(), stop); u != nil && u.HasPrompt {
			used, known = u.ContextUsed(), true
		}
	}
	st := &Status{
		Session:      s.ID,
		Phase:        phase,
		Name:         cfg.Name,
		Model:        cfg.LLM.Model,
		NCTX:         nctx,
		NCTXSource:   nctxSource(cfgN, srvN),
		NCTXConfig:   cfgN,
		ContextUsed:  used,
		ContextKnown: known,
		ThresholdPct: cfg.Compact.ThresholdPct,
		Tip:          tip,
		Entries:      entries,
		Summary:      cmp != nil,
		Turn:         turn,
		AutoCompact:  cfg.Compact.Auto,
		InLoop:       cfg.Compact.UseInLoop(),
		Tools:        published,
		ToolsEnabled: enabled,
		ToolsTotal:   total,
		ToolsPending: pendingTools,
		Queued:       queued,
		Servers:      s.reg.statusSnapshot(),
		UpdatedAt:    time.Now().UTC(),
	}
	if waiting && !wakeAt.IsZero() {
		// UTC whatever the session's timezone is: this is a moment the UI renders in the reader's own
		// words, and the wait was due at the same instant in every zone on earth.
		st.WakeAt = wakeAt.UTC().Format(time.RFC3339)
	}
	if cmp != nil && cmp.Compaction != nil {
		st.Broken = cmp.Compaction.Broken
	}
	if nctx > 0 && known {
		st.UsedPct = used * 100 / nctx
	}
	st.Pending = len(s.pendingIDs())
	return st
}

func phaseToString(p phase) string {
	switch p {
	case phaseRunning:
		return PhaseRunning
	case phaseCompacting:
		return PhaseCompacting
	}
	return PhaseIdle
}

func (s *Session) emitState() {
	s.emit(Event{Type: EvtState, State: s.Status()})
}

func (s *Session) emitPending() {
	s.emit(Event{Type: EvtPending, Pending: s.pendingIDs()})
}

// ---------------------------------------------------------------- user input

// errEmptyMessage is what the two verbs that take a message answer when it carries nothing, and it is the
// same value in both so the two can never drift into meaning different things again.
var errEmptyMessage = errors.New("agent: empty message")

// emptyMessage is the one rule that decides what "nothing" is: whitespace alone is nothing, and a picture is
// a message by itself (a screenshot with no caption is a complete thing to send). Send refuses such a
// message because the path has no shape for it; Interrupt refuses it because an interrupt with nothing in it
// is a stop, and Stop exists.
func emptyMessage(m Message) bool {
	return strings.TrimSpace(m.Text) == "" && len(m.Images) == 0
}

// Send appends a user message. If the loop is idle it starts a run; otherwise the message
// simply becomes part of the path and is picked up by the next turn (that is the whole
// point of "the path is truth"): it becomes pending, then is injected at the proper moment.
// Two moments hold it in memory instead of appending it, and holdMessageLocked is where that is decided.
func (s *Session) Send(ctx context.Context, msg Message) error {
	if emptyMessage(msg) {
		return errEmptyMessage
	}
	// Whatever the path is owed an answer for — the wait the loop handed over, or a call this run is between
	// committing and answering — cannot be answered by anything this message touches, and appending it now
	// would put a user message between a tool call and its result, which is the one shape the assembler
	// refuses to leave alone (answeringLocked, park.go). A manual summary is the other holder: the boundary
	// it is about to write was planned from a path without this message in it. Both wait here, in memory,
	// and docs/ says out loud that a restart drops them.
	if s.holdMessage(msg) {
		return nil
	}
	e, err := s.appendEntry(s.userEntry(msg))
	if err != nil {
		return err
	}
	s.emit(Event{Type: EvtEntry, Entry: e})
	s.emitPending()

	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseIdle:
		// Nothing is owed and nothing is generating, which is the only moment this message can be answered
		// now. A summary never reaches this line with a message in hand: holdMessage above has already taken
		// it, whether the summary announced itself in the phase or is running inside a run that owes an answer.
		s.startRunLocked(ctx, 0)
	default:
		// The message is on the path either way, and the loop reads it at the next turn boundary; this is
		// only a note about when the answer can arrive. "At the next turn boundary" is a promise this
		// session has to keep, though: the run being joined may already have decided it is done, so the
		// promise is recorded here and collected when that run ends (Session.acceptedWhileBusy).
		s.acceptedWhileBusy = true
		s.logf("message joins the path while phase %s: it is read at the next turn boundary", phaseToString(s.phase))
	}
	return nil
}

func (s *Session) userEntry(msg Message) *ir.Entry {
	e := &ir.Entry{
		Kind:    ir.KindUser,
		Role:    "user",
		Content: msg.Text,
		Parts:   msg.Images,
		Label:   msg.Label,
	}
	return e
}

// needsRun reports whether the path ends with something that expects an answer.
func (s *Session) needsRun() bool {
	path := s.path()
	if len(path) == 0 {
		return false
	}
	last := path[len(path)-1]
	switch last.Kind {
	case ir.KindUser, ir.KindTool:
		return true
	}
	return false
}

// endsWithUserMsg is the narrower question the compaction lock asks: is a user message
// sitting at the end of the path unanswered? If yes, a message arrived while we were busy
// and must be sent as soon as the lock is released (spec §1.4).
func (s *Session) endsWithUserMsg() bool {
	path := s.path()
	return len(path) > 0 && path[len(path)-1].Kind == ir.KindUser
}

// Stop and Interrupt live in loop.go.

// ---------------------------------------------------------------- entry editing

// DeleteEntry removes one entry (children re-attach to its parent).
func (s *Session) DeleteEntry(id string) error {
	if err := s.log.Delete(id); err != nil {
		return err
	}
	s.reload()
	s.emit(Event{Type: EvtRemoved, Info: id})
	s.emitState()
	return nil
}

// PatchEntry rewrites one entry's content (used by the UI's edit).
func (s *Session) PatchEntry(id, content string) error {
	s.mu.Lock()
	e, ok := s.st.Entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("agent: no such entry %q", id)
	}
	cp := *e
	cp.Content = content
	s.mu.Unlock()
	if err := s.log.Patch(&cp); err != nil {
		return err
	}
	s.reload()
	s.emit(Event{Type: EvtEntry, Entry: &cp})
	return nil
}

// SetTip moves the path tip (branching / "rewind").
func (s *Session) SetTip(id string) error {
	s.mu.Lock()
	if s.phase != phaseIdle {
		s.mu.Unlock()
		return errors.New("agent: cannot move the path tip while running")
	}
	s.mu.Unlock()
	if err := s.log.SetTip(id); err != nil {
		return err
	}
	s.reload()
	s.emitState()
	return nil
}

// Reload re-reads storage (after an import).
func (s *Session) Reload() error { return s.reload() }

func (s *Session) reload() error {
	st, err := s.log.Replay()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
	return nil
}

// SetFlags is this session's pin and archive pair: the config snapshot is taken under the lock and the
// disk write is not, and applyFlags is the one door the pair goes through. Renaming goes the same way,
// through UpdateConfig - both rewrite config.json whole, so neither needs a log record and neither leaves
// a stale copy for Replay to resurrect.
//
// There is no emitState at the end: a `state` event carries the status, and neither flag is in it. The
// session list is the thing that shows these two, and it is re-read by whoever asked - an event that had
// nothing new in it would only be a second way for the two to disagree.
func (s *Session) SetFlags(pinned, archived *bool) error {
	s.mu.Lock()
	applyFlags(&s.cfg, pinned, archived)
	cfg := s.cfg
	s.mu.Unlock()
	return s.mgr.SaveConfig(cfg)
}

// Path returns the current root->tip path (the sequence that gets sent).
func (s *Session) Path() []*ir.Entry {
	path := s.path()
	out := make([]*ir.Entry, 0, len(path))
	for _, e := range path {
		cp := *e
		out = append(out, &cp)
	}
	return out
}

// PendingIDs is the exported pending queue (ids of user messages not yet part of a payload).
func (s *Session) PendingIDs() []string { return s.pendingIDs() }

// UpdateConfig applies a configuration change. It is only allowed while the session is idle:
// changing the tool set or the model in the middle of a run would produce a payload whose
// prefix no longer matches what was sent.
func (s *Session) UpdateConfig(ctx context.Context, patch Config) error {
	// Check everything that can be checked from the body alone before taking the lock. Below this
	// point the merge runs with s.mu held and releases it only once, further down, so an error return
	// in that window has to unlock by hand — and a forgotten unlock does not fail one request, it
	// leaves the mutex held forever: every later reader of this session, and GET /api/sessions (which
	// walks them all), hangs for the lifetime of the process.
	if patch.LLM.Pricing != nil {
		if err := patch.LLM.Pricing.Validate(); err != nil {
			return err
		}
	}
	// Every structuredContent mode in the body is checked here. asm.ParseStructuredMode is lenient
	// once a session is running (see structuredTargets), which is exactly why an unknown word has to
	// be refused now: a typo that is stored becomes an option that quietly does nothing.
	if _, err := asm.ParseStructuredMode(patch.StructuredContent); err != nil {
		return err
	}
	// The ceiling is checked from the body alone, exactly like the mode above it, and what has to come
	// back is the negative that meant "no limit" to whoever typed it and means the default here. Too
	// small a ceiling is not a refusal: it shows in the first tool result, and this call is the undo.
	if err := asm.ValidateStructuredMax(patch.StructuredMaxBytes); err != nil {
		return err
	}
	for _, mc := range patch.MCP {
		if _, err := asm.ParseStructuredMode(mc.StructuredContent); err != nil {
			return fmt.Errorf("mcp server %q: %w", mc.Name, err)
		}
	}
	// The transport policy the body carries is checked here for the same reason: a body that names a caFile it
	// cannot read, or a proxy address it cannot parse, should fail now — and the merged result is checked again
	// below, where it can also be a body's new policy combined with a stored file or a stored proxy.
	if err := validatePolicy(patch); err != nil {
		return err
	}
	s.mu.Lock()
	if s.phase != phaseIdle {
		s.mu.Unlock()
		return errors.New("agent: cannot change the config while the session is running")
	}
	old := s.cfg
	patch.ID = old.ID
	// The two list flags are the body's to read and never to write: a settings save PATCHes the whole
	// config it cached, and a cached copy taken before somebody pinned this session would unpin it by
	// saying nothing at all. POST /api/session/{id}/flags is the only writer of them.
	patch.Pinned, patch.Archived = old.Pinned, old.Archived
	// The API never echoes the key back, so an empty credential block means "keep".
	if strings.TrimSpace(patch.LLM.BaseURL) == "" {
		patch.LLM.BaseURL = old.LLM.BaseURL
	}
	if strings.TrimSpace(patch.LLM.Model) == "" {
		patch.LLM.Model = old.LLM.Model
	}
	if strings.TrimSpace(patch.LLM.APIKey) == "" {
		patch.LLM.APIKey = old.LLM.APIKey
	}
	if strings.TrimSpace(patch.LLM.Kind) == "" {
		patch.LLM.Kind = old.LLM.Kind
	}
	if patch.LLM.NCTX == 0 {
		patch.LLM.NCTX = old.LLM.NCTX
	}
	// The two TLS fields merge as one policy rather than as two fields, and only one of them can do it
	// by itself: insecureTLS is a pointer, so "absent" really is "nobody said anything" and an explicit
	// false is a statement, while caFile shares with apiKey and model the rule that empty means
	// unchanged. That leaves one thing a string cannot express — "no file" — so asking not to verify is
	// taken as the whole answer for how this endpoint is verified. Without that, a session that once
	// pinned a CA could never be moved off it over the API at all.
	if patch.LLM.InsecureTLS == nil {
		patch.LLM.InsecureTLS = old.LLM.InsecureTLS
	}
	if patch.LLM.CAFile == "" && !patch.LLM.Policy().Insecure() {
		patch.LLM.CAFile = old.LLM.CAFile
	}
	// The endpoint's proxy follows the same rule as its caFile: an empty string in a body is not a change. It
	// cannot express "nothing was ever said here" either, and unlike the pair above there is no second field to
	// carry that answer — "direct" is available and means no proxy at all, environment included, while back to
	// letting HTTP_PROXY decide is a hand-edit of data/<id>/config.json. The session-level proxy below is what
	// an endpoint left empty falls back to, so clearing one endpoint's proxy to "" is not how it is changed;
	// sending the new address in the same field is.
	if patch.LLM.Proxy == "" {
		patch.LLM.Proxy = old.LLM.Proxy
	}
	// Same rule as everything else in a patch: no pricing block means no change. Unlike the fields
	// above, this one can be turned back off — send {"pricing":{"input":0,"cachedInput":0,"output":0}}
	// and the session really means "free", which the UI then renders as 0 rather than hiding money.
	// The values themselves were validated before the lock, which is what keeps this a plain assignment.
	if patch.LLM.Pricing == nil {
		patch.LLM.Pricing = old.LLM.Pricing
	}
	if patch.Name == "" {
		patch.Name = old.Name
	}
	// Same rule as the fields above: what a patch does not carry is not a change. Without these,
	// PATCHing only a name would drop the system prompt and reset the assembler options, because
	// s.cfg is replaced wholesale below.
	if patch.SystemPrompt == "" {
		patch.SystemPrompt = old.SystemPrompt
	}
	// Assembler options merge field by field: a block that only carries sendImages must not reset
	// sendReasoning. Same rule as above — what a patch does not say is not a change. (The one plain
	// bool can only be switched on, never off, by a patch: send the whole asm block for that.)
	if patch.ASM.SendReasoning == nil {
		patch.ASM.SendReasoning = old.ASM.SendReasoning
	}
	if patch.ASM.SendImages == nil {
		patch.ASM.SendImages = old.ASM.SendImages
	}
	if !patch.ASM.OmitToolResultName {
		patch.ASM.OmitToolResultName = old.ASM.OmitToolResultName
	}
	if patch.ReasoningEffort == "" {
		patch.ReasoningEffort = old.ReasoningEffort
	}
	if len(patch.Extra) == 0 {
		patch.Extra = old.Extra
	}
	if !patch.NotifyTools {
		patch.NotifyTools = old.NotifyTools
	}
	// Same rule as the fields above: an absent mode is not a change. It cannot be turned back to
	// "unset" over the API either — send "auto", which is what unset means.
	if patch.StructuredContent == "" {
		patch.StructuredContent = old.StructuredContent
	}
	// Same rule as the fields around it: a ceiling the body does not carry is not a change. The cost of
	// that rule is that 0 is already spent on "unchanged", so there is no wire spelling for "back to the
	// factory default" — whoever wants the default types the default's number.
	if patch.StructuredMaxBytes == 0 {
		patch.StructuredMaxBytes = old.StructuredMaxBytes
	}
	if patch.MCP == nil {
		// An empty list is a change ("remove every server"); an absent key is not.
		patch.MCP = old.MCP
	}
	// Same rule as the fields above: an absent timezone is not a change, and going back to UTC is sent
	// as "UTC" rather than as an empty string, which means unchanged here as it does for the mode above.
	if patch.Timezone == "" {
		patch.Timezone = old.Timezone
	}
	// Same rule again: a session's proxy is not changed by a body that does not carry one. What this field is
	// worth noticing: it is the fallback the endpoints without a proxy of their own read, so PATCHing this one
	// line is how every such endpoint starts or stops going through a proxy at once — while each endpoint that
	// named one keeps it, which is the rule proxy.go exists to apply.
	if patch.Proxy == "" {
		patch.Proxy = old.Proxy
	}
	// An absent builtinTools key is not a change, while an empty list is a decision ("switch all of
	// them off") — the same line the MCP list draws just above between "did not say" and "said none".
	if patch.BuiltinTools == nil {
		patch.BuiltinTools = old.BuiltinTools
	}
	// compact is the one block that replaces instead of merging field by field, and Params is why: it is a
	// plain struct, so an absent key and a zero key are the same byte to the decoder, while 0 is a legal
	// answer for two of its fields (minKeptMessages, minKeptUserTurns — "no floor"). A field-by-field merge
	// could not express those 0s, so the block is either taken whole or not at all: nothing in it means
	// "unchanged", anything in it is the whole new answer. README.md documents that where PATCH is.
	if patch.Compact == (compact.Params{}) {
		patch.Compact = old.Compact
	}
	patch.Compact = patch.Compact.Normalise()
	if patch.MaxTurns == 0 {
		patch.MaxTurns = old.MaxTurns
	}
	if patch.Temperature == nil {
		patch.Temperature = old.Temperature
	}
	// The merged policy is validated too, not only the body: the rules above can combine a body's
	// insecureTLS with a stored caFile, or a body's proxy with a stored stdio entry, and this is the last
	// point where nothing has been written or applied yet. s.mu is held here, so the unlock on the error path
	// is by hand — see the note above the lock about what a forgotten one costs.
	if err := validatePolicy(patch); err != nil {
		s.mu.Unlock()
		return err
	}
	// The same rule for the daemon's own tools: the merged config is what would be written, so a
	// builtinTools name that does not exist or a timezone that cannot be resolved is refused here and
	// not discovered as a failed tool call an hour from now.
	if err := validateBuiltin(patch); err != nil {
		s.mu.Unlock()
		return err
	}
	// Both questions are asked of the resolved endpoints rather than the stored ones, and the difference is
	// the whole point: PATCHing only the session's proxy changes where every endpoint that names none is
	// dialled, and an identity comparison on the stored fields would report "nothing changed" for the one
	// change that moved the connection.
	reconnectMCP := !sameMCP(old.mcpDial(), patch.mcpDial())
	llmChanged := !old.llmDial().Same(patch.llmDial())
	s.cfg = patch
	s.mu.Unlock()

	if err := s.mgr.SaveConfig(patch); err != nil {
		return err
	}
	// Tool policy (which tools are on, under which prefix, and which of the daemon's own tools the
	// session asked for) travels with the config but is not a connection change, so it is installed
	// whatever else changed: before a reconnect, so a re-listed server is filtered from the first
	// tools/list, or right here if the servers were left alone. UpdateConfig only runs while idle, so
	// there is no run snapshot to wait for.
	if s.reg.SetPolicy(patch) {
		s.emitTools()
	}
	if llmChanged {
		prov, err := llm.New(patch.llmDial())
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.provider = prov
		s.nctxCfg = patch.LLM.NCTX
		s.nctx = patch.LLM.NCTX
		// A new endpoint may be a different server: forget what the old one reported and ask again
		// before the next send. The probe is deliberately not done here, so PATCH stays fast.
		s.nctxServer, s.nctxProbed, s.nctxProbing = 0, false, false
		s.mu.Unlock()
	}
	if reconnectMCP {
		s.reg.closeAll()
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s.reg.connect(cctx, patch.mcpDial())
			s.emitState()
		}()
	}
	// No separate rename here: the merged patch — whose Name was filled from old.Name above — is exactly
	// what SaveConfig just wrote, so the rename is already persisted.
	s.emitState()
	if patch.StructuredContent != "" || len(patch.MCP) > 0 {
		s.logStructuredMode(patch)
	}
	// Both flags already mean "an endpoint changed", which is exactly when the transport policy is worth
	// restating — including when it did not change, because the line is the notice that a deviation is still in
	// force. A PATCH of the session-level proxy lands here too: resolving is what makes it an endpoint change.
	if llmChanged || reconnectMCP {
		s.logPolicy(patch)
	}
	return nil
}

func sameMCP(a, b []mcp.Config) bool {
	if len(a) != len(b) {
		return false
	}
	// Identity is the transport plus whether the server is on: tool policy (disabledTools,
	// toolPrefix) is not part of it, so switching one tool off never takes a working MCP server
	// down with it (and never restarts a stdio child process). See mcp.Config.Identity.
	for i := range a {
		if a[i].Identity() != b[i].Identity() {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- tool policy

// Tools reports the tools drawer: every tool this session's MCP servers have listed and every tool
// the daemon answers itself, what each is estimated to cost, how often the transcript used it, and
// whether a change is still waiting for the end of the run. It works in any phase — a run in flight
// is exactly when you want to know which tool is eating the context — and it never asks a server
// anything, because the registry caches the last tools/list result of every server it knows about.
func (s *Session) Tools() *ToolsView {
	cfg := s.Config()
	view := s.reg.view(cfg)
	view.Session = s.ID
	view.UpdatedAt = time.Now().UTC()

	view.PendingChanges = s.reg.PolicyPending(cfg)
	s.mu.Lock()
	view.ContextUsed = s.ctxUsed
	s.mu.Unlock()

	// Uses and LastUsed come from the transcript: "never called in forty turns" is the argument
	// for switching a tool off, and the data is already in the log.
	uses := map[string]int{}
	lastUsed := map[string]string{}
	for _, e := range s.path() {
		if e.Kind != ir.KindTool || e.Tool == "" {
			continue
		}
		uses[e.Tool]++
		lastUsed[e.Tool] = e.Ts.UTC().Format(time.RFC3339)
	}
	total, visible := 0, 0
	for i := range view.Servers {
		for j := range view.Servers[i].Tools {
			t := &view.Servers[i].Tools[j]
			t.Uses, t.LastUsed = uses[t.Name], lastUsed[t.Name]
			total++
			if t.Enabled {
				visible++
			}
		}
	}
	view.Total, view.Visible = total, visible
	return view
}

// SetToolPolicy is the drawer's write path: it switches the tools of one group off or on for this
// session and/or changes the prefix that group's tools are published under — both per session,
// because both live in this session's config.json (mcp[].disabledTools, mcp[].toolPrefix).
//
// Unlike UpdateConfig, it is allowed while the session is running, and it still cannot affect a
// turn that is already on the wire: the decision goes to config.json immediately (a restart must
// not silently undo it), while the registry keeps publishing the previous array until the run
// ends. The reason is the one UpdateConfig gives for refusing outright — the tool block is
// rendered in front of the history, so the moment it changes is the moment the KV prefix of
// everything already generated stops matching.
//
// A nil disabled slice or a nil prefix pointer means "not a change"; an empty non-nil slice means
// "everything on again", which is what the drawer's "all tools" button sends, and an empty prefix
// means "no namespace", which is what a server without toolPrefix gets.
//
// The daemon's own tools are the same drawer and the same promise (server == BuiltinServer), with
// one difference that comes from their being off by default: the drawer sends what is switched off,
// and for these the config stores what is switched on, so the two views are translated here rather
// than in two places that can disagree.
func (s *Session) SetToolPolicy(server string, disabled []string, prefix *string) (*ToolsView, error) {
	s.mu.Lock()
	cfg := s.cfg
	// s.cfg.MCP is the very slice we are about to change: copy it, so no half-written policy is
	// observable by a reader that only holds the lock briefly.
	mcpCfg := make([]mcp.Config, len(cfg.MCP))
	copy(mcpCfg, cfg.MCP)
	cfg.MCP = mcpCfg
	if server == BuiltinServer {
		// A builtin is published under its own name or not at all (that name is what the
		// documentation quotes), so there is no prefix for this group to have — and no suffix is
		// invented for it either, which is what accepting the field would quietly mean.
		if prefix != nil {
			s.mu.Unlock()
			return nil, errors.New("agent: builtin tools are published under their own names; toolPrefix is for MCP servers")
		}
		if disabled != nil {
			cfg.BuiltinTools = builtinEnabledFromOff(disabled)
		}
	} else {
		idx := -1
		for i := range cfg.MCP {
			if cfg.MCP[i].Name == server {
				idx = i
				break
			}
		}
		if idx < 0 {
			s.mu.Unlock()
			return nil, fmt.Errorf("agent: session %s has no MCP server %q", s.ID, server)
		}
		if disabled != nil {
			clean := make([]string, 0, len(disabled))
			for _, d := range disabled {
				if d = strings.TrimSpace(d); d != "" {
					clean = append(clean, d)
				}
			}
			cfg.MCP[idx].DisabledTools = clean
		}
		if prefix != nil {
			raw := strings.TrimSpace(*prefix)
			if len(raw) > 32 {
				s.mu.Unlock()
				return nil, errors.New("agent: a tool prefix is at most 32 characters")
			}
			// The prefix becomes part of every tool name this session sends, so it is held to the
			// character set function calling accepts — and to no silent fixups: a typo that quietly
			// turns "my/server" into "my_server" would rename tools behind their back.
			if raw != "" && sanitize(raw) != raw {
				s.mu.Unlock()
				return nil, fmt.Errorf("agent: a tool prefix may only use A-Z a-z 0-9 _ - and may not start or end with _ or - (got %q)", raw)
			}
			cfg.MCP[idx].ToolPrefix = raw
		}
	}
	running := s.phase != phaseIdle
	s.cfg = cfg
	s.mu.Unlock()

	if err := s.mgr.SaveConfig(cfg); err != nil {
		return nil, err
	}
	if running {
		// The registry keeps publishing what this run is committed to; the change lands when the
		// run releases its snapshot (loop.go), which is also when the drawer's ⏳ disappears.
		s.logf("tool policy for %s changed during a run: it takes effect when the run ends", server)
	} else if s.reg.SetPolicy(cfg) {
		s.logf("tool policy for %s applied", server)
		s.emitTools()
	}
	s.emitState()
	return s.Tools(), nil
}

// emitTools publishes the drawer view, so every tab showing this session redraws its tool list.
func (s *Session) emitTools() {
	s.emit(Event{Type: EvtTools, Data: s.Tools()})
}

// Close stops everything this session owns.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel(nil)
	}
	// A timer outliving the session would wake a session whose log is already closed, and the wake's
	// run would find nothing to write to. Abandoning the park is honest about the rest: the wait's call
	// stays unanswered on the path, which is exactly what the next open looks for.
	s.stopTimerLocked()
	s.park = nil
	// What was held dies with the session rather than being written out on the way: nothing is going to
	// answer the call it was waiting for, and a restart losing it is the documented cost (docs/).
	s.held, s.queue, s.awaitingReply, s.forceRun = "", nil, false, false
	s.mu.Unlock()
	s.reg.closeAll()
	return s.log.Close()
}

// ---------------------------------------------------------------- elicitation

// askElicit forwards an MCP input request to the connected UI and waits for the answer.
func (s *Session) askElicit(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResponse, error) {
	ch := make(chan mcp.ElicitResponse, 1)
	id := fmt.Sprintf("elicit-%d-%d", time.Now().UnixNano(), len(req.Server))
	s.pmu.Lock()
	s.pendingElicit[id] = ch
	s.pmu.Unlock()
	defer func() {
		s.pmu.Lock()
		delete(s.pendingElicit, id)
		s.pmu.Unlock()
	}()

	s.emit(Event{Type: EvtRequest, Elicit: &ElicitAsk{
		ID: id, Server: req.Server, Tool: req.ToolName, Message: req.Message,
		Mode: req.Mode, Schema: req.Schema, Round: req.Round,
	}})
	defer s.emit(Event{Type: EvtRequestDone, Info: id})

	select {
	case <-ctx.Done():
		return mcp.ElicitResponse{Behavior: "cancel"}, ctx.Err()
	case resp := <-ch:
		return resp, nil
	case <-time.After(30 * time.Minute):
		return mcp.ElicitResponse{Behavior: "cancel"}, errors.New("agent: input request timed out")
	}
}

// AnswerElicitation delivers the user's answer to a waiting MCP input request. behavior is
// accept, decline or cancel.
func (s *Session) AnswerElicitation(id, behavior string, content map[string]any) error {
	if behavior == "" {
		behavior = "accept"
	}
	resp := mcp.ElicitResponse{Behavior: behavior, Content: content}
	s.pmu.Lock()
	ch, ok := s.pendingElicit[id]
	s.pmu.Unlock()
	if !ok {
		return fmt.Errorf("agent: no pending input request %q", id)
	}
	ch <- resp
	return nil
}

// ---------------------------------------------------------------- inspector (optional §4)

// Inspection is the "what will actually be sent" view: every entry of the payload with its
// derived size, plus the plan the compactor would produce right now.
type Inspection struct {
	Session      string       `json:"session"`
	NCTX         int          `json:"nCtx"`
	ContextUsed  int          `json:"contextUsed"`
	ContextKnown bool         `json:"contextKnown"`
	Budget       int          `json:"budget"`
	SummaryCap   int          `json:"summaryCap"`
	Summary      bool         `json:"summary"`
	BoundaryID   *string      `json:"boundaryId,omitempty"`
	Broken       bool         `json:"boundaryBroken,omitempty"`
	Messages     []InspectRow `json:"messages"`
	SystemBytes  int          `json:"systemBytes"`
	// System is the exact system head that will be sent. Empty means no system message is sent at
	// all, which is the difference between a steered model and whatever the model defaults to.
	System     string        `json:"system,omitempty"`
	TotalBytes int           `json:"totalBytes"`
	Synthetic  int           `json:"syntheticToolResults"`
	Plan       *compact.Plan `json:"plan,omitempty"`
	PlanReason string        `json:"planReason,omitempty"`
	Estimated  bool          `json:"estimated"`
	Tools      []string      `json:"tools,omitempty"`
}

// InspectRow is one payload entry in the inspector.
type InspectRow struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Role   string `json:"role"`
	Tokens int    `json:"tokens"`
	Real   bool   `json:"real"`
	Bytes  int    `json:"bytes"`
	Head   string `json:"head,omitempty"`
	InKept bool   `json:"inKept"`
}

// Inspect renders the inspector view (§4: always available, input is the choke point's output).
func (s *Session) Inspect() *Inspection {
	built, sizes, tools := s.build()
	_, _, cmp := s.effective()
	ins := &Inspection{
		Session:     s.ID,
		NCTX:        s.nctxLocked(),
		Summary:     cmp != nil,
		SystemBytes: len(built.System),
		System:      built.System,
		TotalBytes:  built.Bytes(),
		Synthetic:   built.Synthetic,
	}
	if cmp != nil && cmp.Compaction != nil {
		ins.BoundaryID = cmp.Compaction.BoundaryID
		ins.Broken = cmp.Compaction.Broken
	}
	// Same rule as the trigger and the progress bar (1.5): stop at the active compaction entry,
	// because a usage from before it prices a payload that is no longer built.
	stop := cmp != nil && cmp.Compaction != nil && !cmp.Compaction.Broken
	if u := ir.LastUsage(s.path(), stop); u != nil && u.HasPrompt {
		ins.ContextUsed, ins.ContextKnown = u.ContextUsed(), true
	}
	cfg := s.Config()
	ins.Budget = cfg.Compact.Budget(ins.NCTX)
	ins.SummaryCap = cfg.Compact.SummaryCap(ins.NCTX)
	for _, e := range built.Entries {
		row := InspectRow{ID: e.ID, Kind: e.Kind, Role: roleOf(e), InKept: true}
		if sz, ok := sizes.Get(e.ID); ok {
			row.Tokens, row.Real, row.Bytes = sz.Tokens, sz.Real, sz.Bytes
			if !sz.Real {
				ins.Estimated = true
			}
		}
		row.Head = head(e.Content, 120)
		ins.Messages = append(ins.Messages, row)
	}
	for _, t := range tools {
		ins.Tools = append(ins.Tools, t.Name)
	}
	// The plan is computed from the very slice the assembler produced (§4). Select does not know
	// which trigger would run it, so this is also the cut an automatic compaction makes: what it
	// does not show is whether the trigger fires at all.
	plan, ok := compact.Select(built.Entries, sizes, ins.NCTX, cfg.Compact)
	if plan != nil {
		ins.Plan = plan
		if ok {
			ins.PlanReason = ""
		} else {
			ins.PlanReason = string(plan.Reason)
		}
	}
	return ins
}

// Payload is the exact request body the next request would carry: system head, messages (system
// first) and tool definitions. Inspect sizes them, this shows them; nothing about the prompt is
// hidden behind a byte count.
func (s *Session) Payload() map[string]any {
	built, _, tools := s.build()
	return map[string]any{
		"system":   built.System,
		"messages": built.All(),
		"tools":    tools,
	}
}

func (s *Session) nctxLocked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nctx
}

func roleOf(e *ir.Entry) string {
	if e.Kind == ir.KindCompaction {
		return "user"
	}
	return e.Role
}

// head clips to n runes: n bytes would cut multi-byte characters in half and the JSON encoder
// would turn the remainder into replacement characters.
func head(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
