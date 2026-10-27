// Package store is an append-only JSONL log of conversation records.
//
// Why not a DB: the model this app needs is a tree of immutable records plus a tip
// pointer (spec §0 "the path is the single source of truth"). An append-only log gives
// that directly, is crash-safe with one record per line, is greppable/diffable, and keeps
// the dependency count at one (coder/websocket). Everything is hidden behind Log, so a
// bbolt or ncruces/go-sqlite3 backend can be swapped in if that ever becomes necessary.
//
// Crash safety (spec §3.10):
//  1. atomic commit  — "create the entry" and "advance the path tip" are one line, so a
//     crash either leaves both or neither (a torn last line is ignored on load).
//  2. adopt orphans  — Replay attaches a compaction entry that is off the path but whose
//     parent is the path tip.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bareloop/internal/ir"
)

// Ops of a log record.
const (
	OpEntry   = "entry"   // append (or replace) one entry; Tip (if set) moves in the same line
	OpTip     = "tip"     // move the path tip
	OpPatch   = "patch"   // rewrite one entry in place (edit summary text, move boundary)
	OpDelete  = "delete"  // remove one entry, re-attach its children to its parent
	OpSession = "session" // session display metadata
)

// Record is one line of the log.
type Record struct {
	Op    string          `json:"op"`
	Seq   int64           `json:"seq,omitempty"`
	Entry *ir.Entry       `json:"entry,omitempty"`
	Tip   string          `json:"tip,omitempty"`
	ID    string          `json:"id,omitempty"`
	Key   string          `json:"key,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// State is the result of replaying a log.
type State struct {
	Entries map[string]*ir.Entry
	Tip     string
	Meta    map[string]json.RawMessage
	Seq     int64
	// Adopted lists compaction entries that were off the path and got adopted (§3.10).
	Adopted []string
}

// Log is one session's append-only log. Safe for concurrent use.
type Log struct {
	mu    sync.Mutex
	dir   string
	path  string
	f     *os.File
	w     *bufio.Writer
	seq   int64
	fsync bool
}

// Open opens (creating if needed) the log inside dir.
func Open(dir string, fsync bool) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "log.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{dir: dir, path: path, f: f, w: bufio.NewWriterSize(f, 1<<16), fsync: fsync}, nil
}

// Dir returns the session directory.
func (l *Log) Dir() string { return l.dir }

// NextID returns the next record id (ids sort lexicographically).
func (l *Log) NextID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return fmt.Sprintf("%07d", l.seq)
}

func (l *Log) write(r *Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// One Write call for one record: this is the "transaction" boundary.
	if _, err := l.w.Write(b); err != nil {
		return err
	}
	if err := l.w.Flush(); err != nil {
		return err
	}
	if l.fsync {
		return l.f.Sync()
	}
	return nil
}

// AppendEntry appends an entry. When setTip is true the path tip moves to the new entry
// in the same record (atomic commit). An empty e.ID gets a fresh id.
func (l *Log) AppendEntry(e *ir.Entry, setTip bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.ID == "" {
		l.seq++
		e.ID = fmt.Sprintf("%07d", l.seq)
	} else if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil && n > l.seq {
		// keep ids monotonic when an imported entry carries an explicit id
		l.seq = n
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}
	r := Record{Op: OpEntry, Seq: l.seq, Entry: e}
	if setTip {
		r.Tip = e.ID
	}
	return l.write(&r)
}

// SetTip moves the path tip.
func (l *Log) SetTip(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.write(&Record{Op: OpTip, Seq: l.seq, Tip: id})
}

// Patch rewrites one entry in place (keeps the compaction entry's id, so boundaries that
// point at other messages stay valid).
func (l *Log) Patch(e *ir.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.write(&Record{Op: OpPatch, Seq: l.seq, Entry: e})
}

// Delete removes an entry and re-attaches its children to its parent.
func (l *Log) Delete(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.write(&Record{Op: OpDelete, Seq: l.seq, ID: id})
}

// Close flushes and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.w.Flush()
	if e := l.f.Close(); err == nil {
		err = e
	}
	l.f = nil
	return err
}

// Replay reads the whole log and rebuilds the tree.
func (l *Log) Replay() (*State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil, errors.New("store: log closed")
	}
	f, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{Entries: map[string]*ir.Entry{}, Meta: map[string]json.RawMessage{}}, nil
		}
		return nil, err
	}
	defer f.Close()

	st := &State{Entries: map[string]*ir.Entry{}, Meta: map[string]json.RawMessage{}}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := readLine(br)
		if len(strings.TrimSpace(line)) > 0 {
			var r Record
			if e := json.Unmarshal([]byte(line), &r); e != nil {
				// torn last line after a crash: ignore and stop (nothing valid follows)
				if err != nil {
					break
				}
			} else {
				apply(st, &r)
				if r.Seq > st.Seq {
					st.Seq = r.Seq
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if err == io.ErrUnexpectedEOF {
				break // torn tail
			}
			return nil, err
		}
	}
	l.seq = st.Seq

	// Adopt orphan compaction entries (§3.10 defence #2).
	st.Adopted = adoptOrphans(st)
	if st.Tip == "" {
		// No tip (fresh log or deleted tip): sit at the newest entry by id order.
		st.Tip = newest(st)
	}
	return st, nil
}

// apply folds one record into the state.
func apply(st *State, r *Record) {
	switch r.Op {
	case OpEntry:
		if r.Entry == nil {
			return
		}
		cp := *r.Entry
		st.Entries[cp.ID] = &cp
		if r.Tip != "" {
			st.Tip = r.Tip
		}
	case OpPatch:
		if r.Entry == nil {
			return
		}
		old, ok := st.Entries[r.Entry.ID]
		cp := *r.Entry
		if ok {
			cp.ParentID = old.ParentID // patches never move a node
		}
		st.Entries[cp.ID] = &cp
	case OpTip:
		st.Tip = r.Tip
	case OpDelete:
		del, ok := st.Entries[r.ID]
		if !ok {
			return
		}
		newTip := ""
		if st.Tip == r.ID {
			newTip = del.ParentID
		}
		delete(st.Entries, r.ID)
		for _, e := range st.Entries {
			if e.ParentID == r.ID {
				e.ParentID = del.ParentID
			}
		}
		if st.Tip == r.ID {
			st.Tip = newTip
		}
	case OpSession:
		if r.Key != "" {
			st.Meta[r.Key] = r.Value
		}
	}
}

// adoptOrphans finds compaction entries that are not on the path but whose parent is the
// path tip, and moves the tip onto them. Returns the adopted ids.
func adoptOrphans(st *State) []string {
	if st.Tip == "" {
		return nil
	}
	onPath := map[string]bool{}
	for _, e := range ir.Path(st.Entries, st.Tip) {
		onPath[e.ID] = true
	}
	var adopted []string
	changed := true
	for changed {
		changed = false
		ids := make([]string, 0, len(st.Entries))
		for id := range st.Entries {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			e := st.Entries[id]
			if !e.IsCompaction() || onPath[id] {
				continue
			}
			if e.ParentID == st.Tip {
				st.Tip = id
				onPath[id] = true
				adopted = append(adopted, id)
				changed = true
			}
		}
	}
	return adopted
}

func newest(st *State) string {
	best := ""
	for id := range st.Entries {
		if id > best {
			best = id
		}
	}
	return best
}

// SetSessionMeta records a session-level display key.
func (l *Log) SetSessionMeta(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.write(&Record{Op: OpSession, Seq: l.seq, Key: key, Value: raw})
}

// readLine returns one '\n'-terminated line. The final line may lack its newline; in that
// case it is returned together with io.EOF (or io.ErrUnexpectedEOF when clearly torn).
func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		sb.Write(chunk)
		if err == nil {
			return sb.String(), nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return sb.String(), err
	}
}
