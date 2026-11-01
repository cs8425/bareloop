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
//
// The second file (log.idx): every record of the log also gets a fixed 24-byte row in log.idx beside it -
// what it was, where it lies, how long it is - so that one entry or one page of a transcript is a read at
// a known address instead of a scan of the whole log. It is a derived file and stays one: log.jsonl is the
// only truth, the rows are rebuilt whenever the log is read, and an index that cannot be written is
// dropped rather than costing the log a record. index.go is what that file is for.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bareloop/internal/ir"
)

// Ops of a log record.
const (
	OpEntry  = "entry"  // append (or replace) one entry; Tip (if set) moves in the same line
	OpTip    = "tip"    // move the path tip
	OpPatch  = "patch"  // rewrite one entry in place (edit summary text, move boundary)
	OpDelete = "delete" // remove one entry, re-attach its children to its parent
)

// Record is one line of the log.
type Record struct {
	Op    string    `json:"op"`
	Seq   int64     `json:"seq,omitempty"`
	Entry *ir.Entry `json:"entry,omitempty"`
	Tip   string    `json:"tip,omitempty"`
	ID    string    `json:"id,omitempty"`
}

// State is the result of replaying a log.
type State struct {
	Entries map[string]*ir.Entry
	Tip     string
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

	// size is the end of the log, which is where the next record starts - and what an index row records
	// the offset of. It is the end of the file and not the end of what the index describes: a tail torn by
	// a crash is behind the append and in front of the rows.
	size int64

	// ix is this log's log.idx, nil once it has been given up on (see ixErr).
	ix    *indexState
	ixErr error
	logf  func(format string, args ...any) // how an index problem becomes visible; nil means silent
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
	l := &Log{dir: dir, path: path, f: f, w: bufio.NewWriterSize(f, 1<<16), fsync: fsync}
	// why the size is taken here and not from the first Replay: it is where the next record starts, and the
	// offset an index row has to name. Nothing is indexed before the first read - no index file is open yet,
	// and an index cannot describe a log it did not watch from the first byte - so the appends of a session
	// nothing has read yet land unindexed, and the first Replay indexes the log from its first byte.
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
	return l, nil
}

// SetLogf gives the log a way to say what is wrong with its index. Without it an index problem is still
// never a log problem, it is only silent.
func (l *Log) SetLogf(fn func(format string, args ...any)) { l.logf = fn }

// IndexError is why this log is not being indexed, and nil when it is. It is a fact to log and to show in
// a debug panel, never a reason to fail a request: the log works without an index, and the next Replay
// rebuilds one.
func (l *Log) IndexError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ixErr
}

// Skeleton is the log as its index describes it: every live entry, where its record is, and what the log
// adds up to - with none of its content. It returns nil when this log has no index, which is the only
// honest answer for a log that is not indexed: the alternative is a second reader of log.jsonl, and the
// point of the index is that there is one reader.
//
// It folds the rows on every call, which costs a pass over the rows and one map of the live entries. That
// is deliberate: a session's skeleton is cheap to make and cheap to drop, and a Log that kept one would be
// a second copy of the tree to keep in step with the one the session already holds.
func (l *Log) Skeleton() *Skeleton {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ix == nil {
		return nil
	}
	return foldRows(l.ix.rows, nil, nil).skeleton()
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
	off := l.size
	// One Write call for one record: this is the "transaction" boundary.
	if _, err := l.w.Write(b); err != nil {
		return err
	}
	if err := l.w.Flush(); err != nil {
		return err
	}
	if l.fsync {
		if err := l.f.Sync(); err != nil {
			return err
		}
	}
	l.size += int64(len(b))
	l.indexRecord(*r, b, off)
	return nil
}

// indexRecord appends the row of a record that has just made it into the log file.
//
// Its errors are never the write's. That ordering is the whole trade: the record is in the log, which is
// the thing that must not be lost, and the row is a derived fact about it that the next Replay can
// recompute. The one thing that would make the trade wrong is silence, so the reason is remembered
// (IndexError) and said once through logf.
func (l *Log) indexRecord(r Record, line []byte, off int64) {
	if l.ix == nil {
		return // why: this log has no index, and every row of it is rebuilt at the next read either way
	}
	// why the entry is dropped here: the row is the whole of what an append has to leave behind, and a live
	// tree is the session's business, not the log's.
	row, _, err := rowIndex(r, off, len(line)-1, numberedIDs) // the newline is covered but not addressed
	if err != nil {
		l.giveUpIndex(l.ix, err)
		return
	}
	if err := l.ix.put(row, line); err != nil {
		l.giveUpIndex(l.ix, err)
	}
}

// giveUpIndex drops the index, remembering why and saying it through the daemon's logger. The log keeps
// working without it: every row of it is rebuilt from log.jsonl at the next read.
//
// The file is emptied rather than left where it stopped - hence the state in hand, whose file is the one to
// empty. A file that describes a prefix of the log is a thing a later reader could resume from, and one day
// will, but not while nothing can tell a prefix that is merely behind from a prefix that ran into an error.
func (l *Log) giveUpIndex(ix *indexState, why error) {
	if ix == nil {
		// why the worst way is the good way here: the file may not be open at all, and emptying it by path is
		// the same one call.
		if f, err := os.OpenFile(l.idxPath(), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644); err == nil {
			f.Close()
		}
	} else {
		if ix.f != nil {
			_ = ix.f.Truncate(0)
		}
		ix.close()
	}
	l.ixErr = why
	l.ix = nil
	if l.logf != nil {
		l.logf("log.idx of %s dropped: %v (the log works without it; the next read rebuilds it)", l.dir, why)
	}
}

// idxPath is where this session keeps its index.
func (l *Log) idxPath() string { return filepath.Join(l.dir, indexFileName) }

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

// Close flushes and closes the files.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ix.close()
	l.ix = nil
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

// Replay reads the whole log and rebuilds the tree, and rebuilds the index of the same file while it is
// reading it.
//
// What the tree is made of is index.go's business: Replay is this much reading, and one fold.
func (l *Log) Replay() (*State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil, errors.New("store: log closed")
	}
	// What the index file said last time is read before the log is, because the digest of the bytes the rows
	// describe is cheapest taken while reading them - and because "is the index still the truth" can only be
	// asked with both in hand. An index is never a reason a session cannot be opened, so a file that cannot be
	// opened is remembered and the read goes on without it.
	//
	// why the handle in hand is closed first: reading a log twice in one process opens the index file twice,
	// and the file is read afresh rather than trusted to be the rows the last Replay wrote - it may have been
	// written over by another process, or by nothing at all.
	l.ix.close()
	// why the handle is dropped here and not in every exit below: from this line on, the index the log keeps
	// is the one rebuildIndex hands back at the very end, and every other way out of Replay is a way out
	// without one.
	l.ix = nil
	prev, err := l.openIndex()
	if err != nil {
		l.giveUpIndex(nil, err)
		prev = nil
	}
	var hashed hash.Hash = sha256.New()
	if prev != nil {
		hashed = prev.ckh
	}
	s, err := scanLog(l.path, hashed)
	if errors.Is(err, os.ErrNotExist) {
		// What Replay always returned for a log that is not there: an empty tree, and no word about the
		// sequence number either, which is the sequence number of whatever is written next.
		prev.close()
		return &State{Entries: map[string]*ir.Entry{}}, nil
	}
	if err != nil {
		prev.close()
		return nil, err
	}
	st := foldRows(s.rows, s.contents, s.names).state()
	st.Seq = s.maxSeq
	l.seq = st.Seq
	l.ix = l.rebuildIndex(prev, s, st.Tip)
	return st, nil
}

// rebuildIndex makes log.idx describe the log that was just read, and returns the index the log keeps
// afterwards - nil when there is no file to write, or when the rows have not earned one. The digest of the
// log it writes was taken by scanLog, into this same state, one function call ago.
//
// The rows are the ones the scan derived from the log, so "does the index agree" is answered by comparing
// them with the rows that were in hand: a check of the file written by whatever read this log last against
// the log itself, and the only disagreement that can be noticed before something asks the index for an entry.
// A disagreement is reported and overwritten, never argued with.
//
// Nothing is done to avoid the rebuild, because nothing in this build can yet prove that an index file
// describes the log beside it: the header's digest is what will make that provable, and S2-b is the layer
// that reads it and can then decide whether to believe it.
func (l *Log) rebuildIndex(prev *indexState, s scan, tip string) *indexState {
	if prev == nil {
		return nil // why nothing is said here: the reason there is no index is already remembered and logged
	}
	if why := rowDiff(prev.rows, s.rows); why != "" && l.logf != nil {
		l.logf("log.idx of %s %s; rebuilt from log.jsonl", l.dir, why)
	}
	if s.why != nil {
		// why the rows are not worth a file is the scan's own reason: a hand-written id, or a record the read
		// had to stop at. Either way the transcript above still got its tree from these rows.
		l.giveUpIndex(prev, s.why)
		return nil
	}
	prev.rebuild()
	for _, r := range s.rows {
		prev.count(r)
	}
	// why taken from the scan and not from the file: the rows describe the log up to `covered`, and a tail
	// torn by a crash is past what they describe - which is also why cover is not l.size.
	prev.h.cover, prev.h.lines = s.covered, s.lines
	// why taken from the fold: the header's tip is a convenience for a reader that will not fold, and the tip
	// the session gets is the one the fold settled on - tip records, adoption and all.
	prev.h.tip = tip
	if err := prev.writeAll(); err != nil {
		l.giveUpIndex(prev, err)
		return nil
	}
	return prev
}

// scan is one pass over log.jsonl: a row for every record it held, the entry of every row that held one,
// the names the ids were written with when they were not numbers, how far into the log all of that reaches,
// and - should the rows not be worth a file - why not.
type scan struct {
	rows     []row
	contents []*ir.Entry
	names    map[int32]string
	covered  int64
	lines    int64
	maxSeq   int64

	// why is why these rows have not earned a log.idx: an id the log wrote by hand, or a record the read
	// had to stop at. nil says the rows describe the log and are findable by id.
	why error
}

// scanLog reads the log from its first byte and turns it into rows, feeding hashed every byte the rows
// describe and nothing they do not.
//
// The rules about lines are the ones Replay has always applied, and they are applied once, here: a blank
// line is nothing, a line that is not JSON is skipped and the read goes on, and a line that cannot be read
// whole and does not parse ends the read. That last rule is the log's and not the index's, and it means a
// record appended after a torn tail is never read at all; S2-b is where that stops being good enough, and
// the question is written down in _state rather than answered here.
//
// A line that parses but holds a record this build cannot act on - an op from a later build, an entry with
// no id - becomes a row of kind rowBad and the read goes on. The old reader stopped at the first of those,
// which is the one difference worth naming: a log written by a newer build loses its tail to this one
// either way, and now keeps the part this build could read.
//
// One pass, and a dictionary for the ids that are not numbers. One pass is enough because a record always
// names an entry that an earlier record wrote - an id is minted by the append that writes it, so nothing in
// a log can refer forward - and a dictionary is needed at all because a log edited by hand can name an
// entry by a string this store would never have handed out. Such an id gets a negative number of this
// scan's making, which keeps the transcript intact and costs the log its index file.
func scanLog(path string, hashed hash.Hash) (scan, error) {
	f, err := os.Open(path)
	if err != nil {
		return scan{}, err
	}
	defer f.Close()

	var (
		s     scan
		ids   = map[string]int32{} // hand-written ids only: the string → the number made up for it
		names = map[int32]string{} // and back, because a row has to be speakable by the name the log used
		hand  int32                // the next negative number, one per hand-written id
	)
	read := func(id string) (int32, bool) {
		if n, err := idNum(id); err == nil {
			return n, true // why tried first: every id this store writes is answered without touching a map
		}
		if n, ok := ids[id]; ok {
			return n, true
		}
		hand--
		// why negative, and why it costs the file: a positive number is an id, and this is not one - and a
		// reader that finds a row by id has nothing to look a made-up number up with.
		ids[id], names[hand] = hand, id
		if s.why == nil {
			s.why = errNotNumbered
		}
		return hand, true
	}

	br := bufio.NewReaderSize(f, 1<<20)
	for off := int64(0); ; {
		line, rerr := readLine(br)
		if len(strings.TrimSpace(line)) > 0 {
			body := strings.TrimSuffix(line, "\n")
			var r Record
			if e := json.Unmarshal([]byte(body), &r); e != nil {
				if rerr != nil {
					// torn last line after a crash: ignore it and stop. It is the one line the rows do not
					// describe, and so the one line the digest does not cover.
					break
				}
				// a whole line that is not JSON: Replay skipped it and went on. It is covered by the index of
				// the log around it and gets no row, because a row describes a record and this is not one.
			} else {
				out, held, err := rowIndex(r, off, len(body), read)
				if err != nil {
					// A record this build cannot make a row of, which is the same thing the old reader made of
					// it: stop, keep what was read, and write none of it to a file.
					s.why = err
					break
				}
				s.rows = append(s.rows, out)
				s.contents = append(s.contents, held)
				if r.Seq > s.maxSeq {
					s.maxSeq = r.Seq
				}
			}
		}
		if line != "" {
			// why counted only when there is a line: at the end of the file readLine hands back an empty
			// string with io.EOF, and that is the absence of a line, not a line of zero length.
			_, _ = hashed.Write([]byte(line))
			s.covered += int64(len(line))
			s.lines++
			off += int64(len(line))
		}
		if rerr == io.ErrUnexpectedEOF {
			// a tail that parsed is a line the rows describe; what it was read as does not matter
			rerr = io.EOF
		}
		if rerr != nil && rerr != io.EOF {
			return scan{}, rerr
		}
		if rerr == io.EOF {
			break
		}
	}
	if len(names) > 0 {
		s.names = names
	}
	return s, nil
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
