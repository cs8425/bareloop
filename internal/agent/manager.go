package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/store"
)

// Manager owns every session: each session is one directory under root holding its append-only
// log (the path) and its config.json (per-session system prompt, MCP set and context params).
type Manager struct {
	mu       sync.Mutex
	root     string
	fsync    bool
	sessions map[string]*Session
	fanout   func(Event)
	logfn    func(string, ...any)
	seq      atomic.Int64
	// defaults are used when a request omits them (per-session config still wins).
	defaults Config
}

// NewManager creates a manager rooted at dir.
func NewManager(root string, fsync bool, defaults Config, fanout func(Event), logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if fanout == nil {
		fanout = func(Event) {}
	}
	defaults.Compact = defaults.Compact.Normalise()
	return &Manager{
		root: root, fsync: fsync, sessions: map[string]*Session{},
		fanout: fanout, logfn: logf, defaults: defaults,
	}
}

func (m *Manager) logf(format string, args ...any) { m.logfn(format, args...) }

func (m *Manager) publish(e Event) {
	e.Seq = m.seq.Add(1)
	m.fanout(e)
}

// Root returns the data directory.
func (m *Manager) Root() string { return m.root }

// Defaults returns the base configuration new sessions start from.
func (m *Manager) Defaults() Config { return m.defaults }

// MergeDefaults fills empty fields of cfg from the manager defaults.
func (m *Manager) MergeDefaults(cfg Config) Config {
	if strings.TrimSpace(cfg.SystemPrompt) == "" {
		cfg.SystemPrompt = m.defaults.SystemPrompt
	}
	if len(cfg.MCP) == 0 {
		cfg.MCP = m.defaults.MCP
	}
	if strings.TrimSpace(cfg.LLM.BaseURL) == "" {
		cfg.LLM.BaseURL = m.defaults.LLM.BaseURL
	}
	if strings.TrimSpace(cfg.LLM.Model) == "" {
		cfg.LLM.Model = m.defaults.LLM.Model
	}
	if strings.TrimSpace(cfg.LLM.APIKey) == "" {
		cfg.LLM.APIKey = m.defaults.LLM.APIKey
	}
	if cfg.LLM.NCTX == 0 {
		cfg.LLM.NCTX = m.defaults.LLM.NCTX
	}
	// A price tag belongs to the endpoint, so a session that says nothing about money inherits the
	// daemon's. A session that says {"input":0,...} is saying "this one is free", which is an answer
	// and is kept — hence the pointer rather than a zero check.
	if cfg.LLM.Pricing == nil {
		cfg.LLM.Pricing = m.defaults.LLM.Pricing
	}
	// Trust is a property of the endpoint as much as its price is, so a daemon whose default endpoint is
	// a private https server can state the answer once instead of in every session. It is inherited as a
	// pair rather than field by field, because a create body has no way to say "no caFile" — an empty
	// string means "inherit", as it does for apiKey and model. Inheriting the default's file next to a
	// session's own insecureTLS would therefore be a contradiction the caller could not talk their way
	// out of; a session that says anything about certificates replaces the daemon's answer entirely.
	if cfg.LLM.InsecureTLS == nil && cfg.LLM.CAFile == "" {
		cfg.LLM.InsecureTLS = m.defaults.LLM.InsecureTLS
		cfg.LLM.CAFile = m.defaults.LLM.CAFile
	}
	if cfg.Temperature == nil {
		cfg.Temperature = m.defaults.Temperature
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = m.defaults.MaxTokens
	}
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = m.defaults.MaxTurns
	}
	if len(cfg.Extra) == 0 {
		cfg.Extra = m.defaults.Extra
	}
	// These three only apply when creating a session (a session config that exists on disk is
	// authoritative and is never re-merged), so "unset means take the default" is safe here.
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = m.defaults.ReasoningEffort
	}
	if cfg.ASM == (asm.Options{}) {
		cfg.ASM = m.defaults.ASM
	}
	if !cfg.NotifyTools {
		cfg.NotifyTools = m.defaults.NotifyTools
	}
	// Like ReasoningEffort above: only new sessions get the daemon's structuredContent mode, because a
	// session config that exists on disk is never re-merged.
	if strings.TrimSpace(cfg.StructuredContent) == "" {
		cfg.StructuredContent = m.defaults.StructuredContent
	}
	// Compaction parameters: a session that did not specify any gets the manager defaults;
	// a session that did specify some keeps its own, with Normalise() filling the rest.
	if cfg.Compact == (compact.Params{}) && m.defaults.Compact != (compact.Params{}) {
		cfg.Compact = m.defaults.Compact
	}
	cfg.Compact = cfg.Compact.Normalise()
	// The daemon's own tools are inherited as a list, not per name: the list IS the policy, and an
	// empty list in a create body is the answer "none of them", which is kept rather than replaced by
	// the daemon's defaults. A session that says nothing gets whatever the daemon decided to offer.
	if cfg.BuiltinTools == nil {
		cfg.BuiltinTools = m.defaults.BuiltinTools
	}
	// And the timezone, for the same reason as the price tag above: it is a daemon-wide answer about
	// the machine the daemon runs on, and writing it once in config.json beats writing it per session.
	if strings.TrimSpace(cfg.Timezone) == "" {
		cfg.Timezone = m.defaults.Timezone
	}
	return cfg
}

func (m *Manager) dir(id string) string { return filepath.Join(m.root, id) }

// List returns the known sessions (running ones first-hand, plus on-disk ones not opened yet). Reading the
// list reads no transcript: an open session answers from the rows of its index in memory and a closed one
// from the same rows on disk.
//
// Why the manager's lock is let go before a single row is made: making one asks a session for its numbers,
// and the answer waits behind whatever that session is doing. The lock is only ever needed to copy the map,
// and holding it across the walk would let one busy session hold up every request that needs the map at all.
// What it costs is that a session opened while the walk of the directories is running can be listed as
// closed, and the next listing says otherwise.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	open := make([]*Session, 0, len(m.sessions))
	seen := make(map[string]bool, len(m.sessions))
	for id, s := range m.sessions {
		seen[id] = true
		open = append(open, s)
	}
	m.mu.Unlock()

	out := make([]Summary, 0, len(open))
	for _, s := range open {
		out = append(out, s.summary())
	}
	entries, err := os.ReadDir(m.root)
	if err == nil {
		for _, de := range entries {
			if !de.IsDir() {
				continue
			}
			if seen[de.Name()] {
				continue
			}
			if _, err := os.Stat(filepath.Join(m.root, de.Name(), "log.jsonl")); err != nil {
				continue
			}
			out = append(out, m.diskSummary(de.Name()))
		}
	}
	// The order is by id: the only order two listings of the same directories agree on without asking a
	// session anything, and the one a caller can page against. What a reader shows first - pinned, or the
	// newest lastTs - is that reader's decision, made with the fields this fills in.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Summary is one row of the session list, and every number in it comes from two files rather than from a
// conversation: config.json for what the session is called, log.idx for what it adds up to. Neither is a
// reason to read a transcript, which is what lets a session nothing has opened still have a row.
type Summary struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Model is the session's own config, and empty for a session whose config this build cannot read.
	Model  string `json:"model,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Closed bool   `json:"closed,omitempty"`
	// Path is how many entries the conversation is - the ones on the path the session stands on - and Log
	// how many records it took to get there. The two are only equal on a session that has never been
	// edited, rewound, or compacted, because a patch and a delete are records that add nothing. All three
	// numbers are omitted rather than zeroed when the session has no index to ask, because a zero is a
	// claim about an empty session and a missing figure is a claim about nothing.
	Path int   `json:"path,omitempty"`
	Log  int64 `json:"log,omitempty"`
	// Bytes is what the log costs the disk. Skipped is the records the index has a row for and this build
	// cannot act on: an ordinary fact of a session an older build wrote, and nothing to warn about.
	Bytes   int64  `json:"bytes,omitempty"`
	Skipped int64  `json:"skipped,omitempty"`
	Tip     string `json:"tip,omitempty"`
	// Pinned and Archived are where this session sits in a list (see Config). The daemon stores them and
	// nothing else: what a pinned session is worth is the reader's decision, and every reader gets the
	// same pair to decide with.
	Pinned   bool `json:"pinned,omitempty"`
	Archived bool `json:"archived,omitempty"`
	// LastTs is when the newest entry of this session happened - not when its log was last written, and
	// not when this process last opened it. A pointer because a session whose index has no time to read
	// is missing a figure rather than standing at the epoch, and encoding/json's omitempty cannot say
	// that about a struct: no lastTs in the response means nobody claimed one.
	LastTs *time.Time `json:"lastTs,omitempty"`
}

// fill is the one place Info becomes a row, so a session that is open and a session that is only a
// directory cannot drift apart over what a number means.
func (s *Summary) fill(info store.Info) {
	s.Path, s.Log, s.Bytes, s.Skipped, s.Tip = info.Depth, info.Rows, info.LogBytes, info.Skipped, info.Tip
	// why a zero time is dropped instead of carried: an index with no entry timestamp is a session that
	// cannot be timed, and a 1970 in the response is a date some reader will sort by and draw.
	if !info.LastTs.IsZero() {
		ts := info.LastTs
		s.LastTs = &ts
	}
}

// diskSummary is the row of a session this process has not opened, made from the files that do not need it
// to be open. A config that will not read or an index that will not be believed costs the row its numbers
// and not the row: a directory with a log in it is a session in the list whatever its files say about
// themselves.
func (m *Manager) diskSummary(id string) Summary {
	out := Summary{ID: id, Closed: true}
	if cfg, err := m.loadConfig(id); err == nil {
		out.Name, out.Model = cfg.Name, cfg.LLM.Model
		// The two list flags come along even though the session is shut: where it sits in a list is a
		// fact of its config.json, and a closed session that lost its pin when the daemon restarted
		// would be a pin with a secret dependency on this process having opened it.
		out.Pinned, out.Archived = cfg.Pinned, cfg.Archived
	}
	if info, err := store.Inspect(m.dir(id)); err == nil {
		out.fill(info)
	}
	return out
}

// summary is this session's row. Its numbers come from its log handle - the rows in memory once the log
// has been read, its index file before that - and only the phase has to be asked of the session, being the
// one number on the row no file can tell.
func (s *Session) summary() Summary {
	cfg := s.Config()
	out := Summary{ID: s.ID, Name: cfg.Name, Model: cfg.LLM.Model, Phase: phaseToString(s.phaseNow())}
	// Where a session sits in a list is its config's business: open or shut, this row and the one
	// diskSummary makes from config.json have to say the same thing, or a pin would depend on this
	// process happening to have the session open.
	out.Pinned, out.Archived = cfg.Pinned, cfg.Archived
	info, err := s.log.Read()
	if err != nil {
		// why the row is still worth a line: its name and its phase are true, and a session with no
		// log.idx - one never read, or one whose index was given up on - has no numbers to be wrong about.
		return out
	}
	out.fill(info)
	return out
}

func (s *Session) phaseNow() phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// Get returns an open session or opens it from disk.
func (m *Manager) Get(ctx context.Context, id string) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()
	cfg, err := m.loadConfig(id)
	if err != nil {
		return nil, err
	}
	return m.open(ctx, id, cfg)
}

// Peek returns an already-open session without opening anything.
func (m *Manager) Peek(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Create writes a new session's config and opens it.
func (m *Manager) Create(ctx context.Context, cfg Config) (*Session, error) {
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, errors.New("agent: session id required")
	}
	if err := validID(cfg.ID); err != nil {
		return nil, err
	}
	cfg = m.MergeDefaults(cfg)
	if err := cfg.LLM.Pricing.Validate(); err != nil {
		return nil, err
	}
	// A structuredContent mode is checked before anything is written: a session whose config says
	// "alway" would otherwise read as "auto" forever, and the operator would be hunting a typo in a
	// file the server rewrites on every settings save. Same rule UpdateConfig follows.
	if _, err := asm.ParseStructuredMode(cfg.StructuredContent); err != nil {
		return nil, err
	}
	// The ceiling on structuredContent goes through the same door, and it refuses exactly one thing: a
	// negative, which is what people type for "no limit" and what this process then reads back as the
	// default. A ceiling typed too small is a visible mistake one PATCH wide, not a stored lie, so it stays.
	if err := asm.ValidateStructuredMax(cfg.StructuredMaxBytes); err != nil {
		return nil, err
	}
	for _, mc := range cfg.MCP {
		if _, err := asm.ParseStructuredMode(mc.StructuredContent); err != nil {
			return nil, fmt.Errorf("session %q: mcp server %q: %w", cfg.ID, mc.Name, err)
		}
	}
	// Same rule as the two above: a certificate policy that cannot work — or names a file that cannot be
	// read — is refused before data/<id>/config.json exists, so whoever typed the path hears about it now
	// rather than as a failed request an hour later.
	if err := validateTLS(cfg); err != nil {
		return nil, err
	}
	// And the same rule one more time for the daemon's own tools: "a name that is not a builtin" and
	// "a zone this machine cannot resolve" are typos in a body the caller still has, and Create is the
	// last place that can say so without costing the session a turn.
	if err := validateBuiltin(cfg); err != nil {
		return nil, err
	}
	// The directory is the identity, so an existing directory means the id is taken - even when no log
	// has been written yet (a session interrupted before its first entry, or a directory someone made
	// by hand, used to be overwritten without a word because only log.jsonl was checked).
	if fi, err := os.Stat(m.dir(cfg.ID)); err == nil {
		if fi.IsDir() {
			return nil, fmt.Errorf("agent: session %q already exists (%s)", cfg.ID, m.dir(cfg.ID))
		}
		return nil, fmt.Errorf("agent: %s exists and is not a directory", m.dir(cfg.ID))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := m.saveConfig(cfg); err != nil {
		return nil, err
	}
	return m.open(ctx, cfg.ID, cfg)
}

// SaveConfig persists a session config (used before opening and on edits).
func (m *Manager) SaveConfig(cfg Config) error { return m.saveConfig(cfg) }

func (m *Manager) saveConfig(cfg Config) error {
	if err := os.MkdirAll(m.dir(cfg.ID), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.dir(cfg.ID), "config.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.dir(cfg.ID), "config.json"))
}

func (m *Manager) loadConfig(id string) (Config, error) {
	var cfg Config
	if err := validID(id); err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(filepath.Join(m.dir(id), "config.json"))
	if err != nil {
		return cfg, fmt.Errorf("agent: no session %q: %w", id, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("agent: bad config for %q: %w", id, err)
	}
	// The directory is the session's identity. It is what List, the HTTP route, Remove and the log
	// path all use, so a config file must not be able to re-home a session: two directories holding
	// "id": "test" used to be one session, and the other directory's log was never read at all.
	// A disagreeing file is reported and rewritten so it cannot mislead the next reader.
	stale := cfg.ID != id
	if stale && cfg.ID != "" {
		m.logf("session %q: config.json says id %q; the directory name wins, rewriting config.json", id, cfg.ID)
	}
	if stale {
		cfg.ID = id
	}
	cfg.Compact = cfg.Compact.Normalise()
	if stale {
		if err := m.saveConfig(cfg); err != nil {
			m.logf("session %q: could not fix the id in config.json: %v", id, err)
		}
	}
	return cfg, nil
}

// SetFlags is the whole cost of pinning or archiving a session: two booleans in its config.json.
//
// Why a route of its own and not two more fields of PATCH /api/session/{id}: that route answers {id} by
// opening the session, and opening one reads its whole log - so pinning a session this process has never
// opened would replay a transcript to change four bytes of a file. The manager can answer that question
// from config.json alone. A session already open is the case that must not be answered that way: its
// in-memory config is the authority, and a file written behind its back is one SaveConfig away from being
// undone.
//
// A nil is "no opinion about this flag". applyFlags is where the pair is decided, and it is the only door.
func (m *Manager) SetFlags(id string, pinned, archived *bool) error {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil {
		return s.SetFlags(pinned, archived)
	}
	cfg, err := m.loadConfig(id)
	if err != nil {
		return err
	}
	applyFlags(&cfg, pinned, archived)
	return m.saveConfig(cfg)
}

// applyFlags writes the two list flags, and it is the only place that writes them: "pinned and archived
// are never both true" is the kind of invariant that only holds while there is one door through which
// either of them is set.
//
// When what arrives would make the pair (true, true), the flag that was just asked for wins and the other
// one goes. That request is not a contradiction to litigate - it is how "put this one away" and "keep this
// one where I can see it" are said - and a row of the session list can only be drawn in one place.
func applyFlags(cfg *Config, pinned, archived *bool) {
	if pinned != nil {
		cfg.Pinned = *pinned
	}
	if archived != nil {
		cfg.Archived = *archived
	}
	if cfg.Pinned && cfg.Archived {
		// which of the two was just said is all this needs: a pair that is already (true, true) cannot be
		// stored, because every writer of it comes through here.
		if pinned != nil && *pinned {
			cfg.Archived = false
		} else {
			cfg.Pinned = false
		}
	}
}

// open returns the session of the directory id, opening it when needed. id is always the directory
// name; cfg.ID is kept as a copy of it, never the other way round.
func (m *Manager) open(ctx context.Context, id string, cfg Config) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()
	s, err := openSession(ctx, m, id, cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	go s.emitState()
	return s, nil
}

// LoadAll opens every session directory that has a config.
func (m *Manager) LoadAll(ctx context.Context) error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(m.root, 0o755)
		}
		return err
	}
	for _, de := range entries {
		if !de.IsDir() {
			continue
		}
		cfg, err := m.loadConfig(de.Name())
		if err != nil {
			m.logf("skip session %s: %v", de.Name(), err)
			continue
		}
		if _, err := m.open(ctx, de.Name(), cfg); err != nil {
			m.logf("open session %s: %v", de.Name(), err)
		}
	}
	return nil
}

// Remove closes a session and deletes its directory.
func (m *Manager) Remove(id string) error {
	// The route hands us {id} straight from the URL, and this is a RemoveAll: validate before deleting.
	if err := validID(id); err != nil {
		return err
	}
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if ok {
		_ = s.Close()
	}
	return os.RemoveAll(m.dir(id))
}

// Unload closes one session and forgets it, leaving its directory exactly as it was: what goes is the log
// handle, the MCP connections and the parsed conversation, and the row in List survives because that row
// is made from config.json and log.idx rather than from a live session (see List and diskSummary).
//
// This is the memory half of the story. An open session costs RAM for as long as it is open - the whole
// conversation parsed into entries, its index, one set of child processes and sockets per MCP server -
// and until now the only ways to give that back were deleting the conversation or restarting the daemon.
// Opening is a side effect of looking: every read route goes through Get, so a reader who clicked through
// six conversations was holding six of them until the process ended. Unload puts one back, and the next
// read of the id opens it again from disk (open emits state, so a watching UI learns about it by itself).
//
// What goes with it is what only this process had: a message held instead of written, and the in-memory
// half of a parked wait. That is the same bill a restart pays - Close gives up the same two things and
// says so where it stands - and the wait itself is not lost: it is recomputed from the log the moment the
// session is opened again (recoverPark reads the unanswered call off the path).
//
// A session with a run or a summary in flight is refused rather than cancelled: stopping a run is its own
// verb, and putting a conversation down while a provider is answering it is not something a memory button
// should decide. A session this process does not have open answers (false, nil) - the caller wanted it
// shut and it is - while an id with no config behind it is an error: that is a typo, and silence about a
// typo is how a script spends an hour unloading nothing while every answer says ok.
func (m *Manager) Unload(id string) (bool, error) {
	if err := validID(id); err != nil {
		return false, err
	}
	s, ok := m.Peek(id)
	if !ok {
		if _, err := m.loadConfig(id); err != nil {
			return false, err
		}
		return false, nil
	}
	// Asked outside m.mu for the reason List gives: a session answers questions about itself behind
	// whatever it is busy with, and the map lock is not the place to wait for that.
	if p := s.phaseNow(); p != phaseIdle {
		return false, fmt.Errorf("agent: session %q is %s; stop it before unloading", id, phaseToString(p))
	}
	// Close the instance that was just checked, and only that one. A Get that opened the id again in the
	// interval has stored a newer session, and closing that one would be closing a session nobody asked
	// about - so when the map no longer holds this pointer, this call has nothing left to do.
	m.mu.Lock()
	if m.sessions[id] == s {
		delete(m.sessions, id)
	} else {
		s = nil
	}
	m.mu.Unlock()
	if s == nil {
		return false, nil
	}
	// A failure here is logged rather than returned: the session is already out of the map, which is the
	// half the caller asked for, and a refusal now would leave a session that is neither open nor findable.
	if err := s.Close(); err != nil {
		m.logf("unload session %s: close: %v", id, err)
	}
	m.logf("session %s unloaded", id)
	return true, nil
}

// Close closes every session.
func (m *Manager) Close() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		all = append(all, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, s := range all {
		_ = s.Close()
	}
}

// DefaultSessionConfig builds the configuration for a brand new session.
func (m *Manager) DefaultSessionConfig(id, name string) Config {
	return m.MergeDefaults(Config{ID: id, Name: name})
}

// ValidID reports whether id can be used as a session id. An id is also the session's directory name
// and the key of every route, so this is the portability gate for the whole data directory.
func ValidID(id string) error { return validID(id) }

// validID keeps session ids filesystem-safe.
func validID(id string) error {
	if id == "" {
		return errors.New("agent: empty id")
	}
	// An id is a directory name, and directories are only case-insensitive on some filesystems
	// (Windows, macOS, exFAT drives, synced folders). Forcing lowercase makes "no two session ids
	// differ only in case" true by construction, so the filesystem's own uniqueness is enough on
	// every platform - and a hand-made data/Test/ directory cannot be opened under the name test.
	if lower := strings.ToLower(id); lower != id {
		return fmt.Errorf("agent: session ids are lowercase (did you mean %q?)", lower)
	}
	if len(id) > 64 {
		return errors.New("agent: id too long")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("agent: id %q contains an unsupported character %q", id, r)
		}
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("agent: id %q is not allowed", id)
	}
	// filepath.IsLocal is the standard library's own name gate: on Windows it rejects the reserved
	// device names (con, nul, com1, lpt1, ...) and the trailing dots and spaces that FAT cannot store,
	// and everywhere it rejects separators and the \\?\ namespace. Its rules belong to the running
	// system, so a data directory prepared on Linux can still hold a name Windows would refuse - the
	// same direction of trade that making ids lowercase makes in the other one.
	if !filepath.IsLocal(id) {
		return fmt.Errorf("agent: id %q is not a name this filesystem accepts", id)
	}
	return nil
}
