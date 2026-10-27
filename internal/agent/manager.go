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

	"bareloop/internal/compact"
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
	// Compaction parameters: a session that did not specify any gets the manager defaults;
	// a session that did specify some keeps its own, with Normalise() filling the rest.
	if cfg.Compact == (compact.Params{}) && m.defaults.Compact != (compact.Params{}) {
		cfg.Compact = m.defaults.Compact
	}
	cfg.Compact = cfg.Compact.Normalise()
	return cfg
}

func (m *Manager) dir(id string) string { return filepath.Join(m.root, id) }

// List returns the known sessions (running ones first-hand, plus on-disk ones not opened yet).
func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []Summary
	for id, s := range m.sessions {
		seen[id] = true
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
			out = append(out, Summary{ID: de.Name(), Closed: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Summary is one row of the session list.
type Summary struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Model   string `json:"model,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Closed  bool   `json:"closed,omitempty"`
	Entries int    `json:"entries"`
	Tip     string `json:"tip,omitempty"`
}

func (s *Session) summary() Summary {
	cfg := s.Config()
	return Summary{ID: s.ID, Name: cfg.Name, Model: cfg.LLM.Model, Phase: phaseToString(s.phaseNow()), Entries: len(s.Entries()), Tip: s.tipNow()}
}

func (s *Session) phaseNow() phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

func (s *Session) tipNow() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Tip
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
	return m.open(ctx, cfg)
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
	if _, err := os.Stat(filepath.Join(m.dir(cfg.ID), "log.jsonl")); err == nil {
		return nil, fmt.Errorf("agent: session %q already exists", cfg.ID)
	}
	if err := m.saveConfig(cfg); err != nil {
		return nil, err
	}
	return m.open(ctx, cfg)
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
	if cfg.ID == "" {
		cfg.ID = id
	}
	cfg.Compact = cfg.Compact.Normalise()
	return cfg, nil
}

func (m *Manager) open(ctx context.Context, cfg Config) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.sessions[cfg.ID]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()
	s, err := openSession(ctx, m, cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[cfg.ID] = s
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
		if _, err := m.open(ctx, cfg); err != nil {
			m.logf("open session %s: %v", de.Name(), err)
		}
	}
	return nil
}

// Remove closes a session and deletes its directory.
func (m *Manager) Remove(id string) error {
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

// validID keeps session ids filesystem-safe.
func validID(id string) error {
	if id == "" {
		return errors.New("agent: empty id")
	}
	if len(id) > 64 {
		return errors.New("agent: id too long")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("agent: id %q contains an unsupported character %q", id, r)
		}
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return fmt.Errorf("agent: id %q is not allowed", id)
	}
	return nil
}


