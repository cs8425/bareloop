package agent

import (
	"context"
	"encoding/json"
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

	phase  phase
	cancel context.CancelFunc
	done   chan struct{}
	// generating is true only while an LLM stream is open. Compaction may run between turns
	// of a run (the loop calls it itself) but never while tokens are flowing (§1.1).
	generating bool
	forceMsg   *Message // set when the user interrupts with a message to send now
	turn       int
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
	gate       chan gateDecision
	gateID     string

	// pendingElicit waits for the UI to answer an MCP input request.
	pmu           sync.Mutex
	pendingElicit map[string]chan mcp.ElicitResponse

	reg      *registry
	provider llm.Provider

	// lastSent is the payload the session actually sent last, reused verbatim as the prefix
	// of the summary request (spec §5.2).
	lastSent  []asm.Message
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
	st, err := log.Replay()
	if err != nil {
		_ = log.Close()
		return nil, fmt.Errorf("replay %s: %w", cfg.ID, err)
	}
	if cfg.Name == "" {
		if raw, ok := st.Meta["name"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				cfg.Name = s
			}
		}
	}
	prov, err := llm.New(cfg.LLM)
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
	s.reg.SetPolicy(cfg.MCP)
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
		s.reg.connect(cctx, s.Config().MCP)
		s.emitState()
	}()
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

func (s *Session) cfgOptions() asm.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.ASM
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

func (s *Session) markSent() {
	s.mu.Lock()
	s.sentTip = s.st.Tip
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
	s.mu.Unlock()

	_, _, cmp := s.effective()
	published, enabled, total := s.reg.counts()
	pendingTools := s.reg.PolicyPending(cfg.MCP)
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
		Servers:      s.reg.statusSnapshot(),
		UpdatedAt:    time.Now().UTC(),
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

// Send appends a user message. If the loop is idle it starts a run; otherwise the message
// simply becomes part of the path and is picked up by the next turn (that is the whole
// point of "the path is truth"): it becomes pending, then is injected at the proper moment.
func (s *Session) Send(ctx context.Context, msg Message) error {
	if strings.TrimSpace(msg.Text) == "" && len(msg.Images) == 0 {
		return errors.New("agent: empty message")
	}
	e, err := s.appendEntry(s.userEntry(msg))
	if err != nil {
		return err
	}
	s.emit(Event{Type: EvtEntry, Entry: e})
	s.emitPending()

	s.mu.Lock()
	switch s.phase {
	case phaseIdle:
		if !s.compacting {
			s.startRunLocked(ctx, 0)
			s.mu.Unlock()
			return nil
		}
		// Messages sent while compaction runs are queued and auto-sent when it finishes
		// (§1.4); the compaction goroutine does that by re-checking needsRun.
		s.mu.Unlock()
		s.logf("message queued during compaction")
		return nil
	default:
		s.mu.Unlock()
		s.logf("message queued: loop is running (will be sent after the current turn)")
		return nil
	}
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

// Entries returns the whole tree (debug UI).
func (s *Session) Entries() []*ir.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*ir.Entry, 0, len(s.st.Entries))
	for _, e := range s.st.Entries {
		cp := *e
		out = append(out, &cp)
	}
	return out
}

// SetName renames the session.
func (s *Session) SetName(name string) error {
	s.mu.Lock()
	s.cfg.Name = name
	s.mu.Unlock()
	if err := s.log.SetSessionMeta("name", name); err != nil {
		return err
	}
	s.emitState()
	return nil
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
	s.mu.Lock()
	if s.phase != phaseIdle {
		s.mu.Unlock()
		return errors.New("agent: cannot change the config while the session is running")
	}
	old := s.cfg
	patch.ID = old.ID
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
	if patch.MCP == nil {
		// An empty list is a change ("remove every server"); an absent key is not.
		patch.MCP = old.MCP
	}
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
	reconnectMCP := !sameMCP(old.MCP, patch.MCP)
	llmChanged := !old.LLM.Same(patch.LLM)
	s.cfg = patch
	s.mu.Unlock()

	if err := s.mgr.SaveConfig(patch); err != nil {
		return err
	}
	// Tool policy (which tools are on, under which prefix) travels with the MCP list but is not a
	// connection change, so it is installed whatever else changed: before a reconnect, so a
	// re-listed server is filtered from the first tools/list, or right here if the servers were
	// left alone. UpdateConfig only runs while idle, so there is no run snapshot to wait for.
	if s.reg.SetPolicy(patch.MCP) {
		s.emitTools()
	}
	if llmChanged {
		prov, err := llm.New(patch.LLM)
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
			s.reg.connect(cctx, patch.MCP)
			s.emitState()
		}()
	}
	if err := s.SetName(patch.Name); err != nil {
		return err
	}
	s.emitState()
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

// Tools reports the tools drawer: every tool this session's MCP servers have listed, what each is
// estimated to cost, how often the transcript used it, and whether a change is still waiting for
// the end of the run. It works in any phase — a run in flight is exactly when you want to know
// which tool is eating the context — and it never asks a server anything, because the registry
// caches the last tools/list result of every server it knows about.
func (s *Session) Tools() *ToolsView {
	cfg := s.Config()
	view := s.reg.view(cfg.MCP)
	view.Session = s.ID
	view.UpdatedAt = time.Now().UTC()

	view.PendingChanges = s.reg.PolicyPending(cfg.MCP)
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

// SetToolPolicy is the drawer's write path: it switches tools of one MCP server off or on for this
// session and/or changes the prefix that server's tools are published under — both per session,
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
func (s *Session) SetToolPolicy(server string, disabled []string, prefix *string) (*ToolsView, error) {
	s.mu.Lock()
	cfg := s.cfg
	// s.cfg.MCP is the very slice we are about to change: copy it, so no half-written policy is
	// observable by a reader that only holds the lock briefly.
	mcpCfg := make([]mcp.Config, len(cfg.MCP))
	copy(mcpCfg, cfg.MCP)
	cfg.MCP = mcpCfg
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
		// character set function calling accepts — and to no silent fixups: a typo that quietly turns
		// "my/server" into "my_server" would rename tools behind their back.
		if raw != "" && sanitize(raw) != raw {
			s.mu.Unlock()
			return nil, fmt.Errorf("agent: a tool prefix may only use A-Z a-z 0-9 _ - and may not start or end with _ or - (got %q)", raw)
		}
		cfg.MCP[idx].ToolPrefix = raw
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
	} else if s.reg.SetPolicy(cfg.MCP) {
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
		s.cancel()
	}
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
	// The plan is computed from the very slice the assembler produced (§4).
	plan, ok := compact.Select(built.Entries, sizes, ins.NCTX, cfg.Compact, true)
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
