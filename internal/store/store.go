// Package store keeps one session's conversation tree, in SQLite, behind the same
// Log-shaped API the agent has always used (r83).
//
// Four tables (schema version 1, PRAGMA user_version):
//
//	entry      the tree as it stands: one row per live entry, parent the tree edge
//	entry_rev  one row per patch: the payload it replaced, stamped with its seq —
//	           the version archive the path-picker UI (deferred) will read
//	blob       image bytes once, content-addressed by sha256; payload holds "blob:<sha>"
//	meta       tip, seq_next — control, not history
//
// Why SQLite (r83): the append-only log needed an index the size of itself to answer
// "one entry by id" or "the newest version of a patched entry"; SQLite answers both with
// an index it maintains. WAL covers what the log covered by append-only: a torn commit is
// a rollback, not a repaired file. entry_rev is where "edits keep history readable" lives —
// the log's superseded lines had no reader; these rows are one SELECT from the tree UI.
//
// Crash safety: every write is one IMMEDIATE transaction (BEGIN IMMEDIATE is the driver's
// default since v0.37). fsync=true maps to synchronous=FULL, fsync=false to NORMAL.
//
// Image parts cross the boundary here and nowhere else: AppendEntry/Patch split every
// base64 image into the blob table and store "blob:<sha256>" in its payload; Replay
// re-attaches the bytes so callers see the byte-identical ir.Entry they stored (spec §5.4).
// refcount counts live references in entry AND entry_rev — append adds, patch moves one
// (entry→rev, count unchanged, plus any genuinely new ref), delete subtracts; a row of
// zero is deleted in the same transaction. A revision pins its blobs: that is its job.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"bareloop/internal/ir"
)

// DBFileName is the one file of a session's store. SQLite adds store.db-wal (and -shm
// while open) beside it; Close checkpoints the WAL away so a resting directory holds one file.
const DBFileName = "store.db"

// schemaVersion is what PRAGMA user_version this build writes, and the only one it reads.
const schemaVersion = 1

// meta keys — control state, never history (history is entry_rev).
const (
	metaTip     = "tip"
	metaSeqNext = "seq_next"
)

// blobPrefix marks a payload's image data as a reference into the blob table.
const blobPrefix = "blob:"

// ddl runs one statement per Exec: the ncruces driver takes a single statement per call.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS entry(
	   id      TEXT PRIMARY KEY,
	   parent  TEXT,
	   kind    TEXT NOT NULL,
	   seq     INTEGER NOT NULL,
	   ts      REAL NOT NULL,
	   payload TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS entry_parent ON entry(parent)`,
	`CREATE INDEX IF NOT EXISTS entry_seq ON entry(seq)`,
	`CREATE TABLE IF NOT EXISTS entry_rev(
	   entry_id TEXT NOT NULL,
	   seq      INTEGER NOT NULL,
	   payload  TEXT NOT NULL,
	   PRIMARY KEY(entry_id, seq)) WITHOUT ROWID`,
	`CREATE TABLE IF NOT EXISTS blob(
	   sha256   TEXT PRIMARY KEY,
	   mime     TEXT,
	   size     INTEGER,
	   refcount INTEGER NOT NULL DEFAULT 0,
	   bytes    BLOB NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
}

// Info is what a session lists about itself without anyone reading its transcript:
// the numbers of a summary row (manager.Summary).
type Info struct {
	// LogBytes is what the store costs the disk right now, WAL included.
	LogBytes int64
	// Rows is the number of write events (appends and patches) the store has taken —
	// the seq it has reached. The old log counted every line, tip moves and deletes
	// included; those control moves are single updates now and cost nothing to count.
	Rows int64
	// Skipped exists for the summary row and is always 0: a build cannot fail to
	// understand its own tables the way it could fail on a future log line.
	Skipped int64
	Entries int64
	Depth   int
	Tip     string
	LastTs  time.Time
}

// State is the whole tree as it stands, in the shape the session has always held.
type State struct {
	Entries map[string]*ir.Entry
	Tip     string
	Seq     int64
	// Adopted is always empty: adoption was a fold repair for a compaction appended
	// off the tip; with the tree stored as edges there is nothing to repair.
	Adopted []string
}

// Log is one session's store. Safe for concurrent use (one SQLite connection, so
// everything is serialised below the mutex anyway).
type Log struct {
	mu  sync.Mutex
	dir string
	db  *sql.DB // nil after Close
	seq int64   // the last seq handed out; NextID/AppendEntry raise it

	logf func(format string, args ...any) // how something odd gets said; nil means silent
}

// Open opens (creating if needed) the store inside dir.
func Open(dir string, fsync bool) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// why these four pragmas and nothing else: WAL is the crash story (a torn commit is
	// a rollback, no torn-tail rule to implement); synchronous(NORMAL) is what fsync=false
	// always meant (the bench's 0.8ms write; FULL for fsync=true); busy_timeout is insurance
	// for a second reader; mmap_size(0) because the WAL shm mapping is the only mapping we
	// want the page cache deciding about (r83: 300MB lessons).
	syncMode := "NORMAL"
	if fsync {
		syncMode = "FULL"
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, DBFileName)) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(" + syncMode +
		")&_pragma=busy_timeout(5000)&_pragma=mmap_size(0)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// why one connection: one daemon, one writer per session, and an SQLite handle is a
	// wasm sandbox — the bench priced each connection at ~330KB, so we keep exactly one.
	db.SetMaxOpenConns(1)
	l := &Log{dir: dir, db: db}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		db.Close()
		return nil, err
	}
	if v != 0 && v != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("store: %s has schema version %d, this build knows only %d", dir, v, schemaVersion)
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", dir, err)
		}
	}
	if v == 0 {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			db.Close()
			return nil, err
		}
	}
	// The seq in hand is the one every later id and every entry_rev stamp is counted from;
	// a store without one has handed out nothing yet.
	if err := db.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?", metaSeqNext).Scan(&l.seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	return l, nil
}

// SetLogf gives the store a way to say what it found odd (today: an image part whose
// base64 cannot be decoded, and a payload referencing a blob that is not there).
func (l *Log) SetLogf(fn func(format string, args ...any)) { l.logf = fn }

// IndexError is a kept-for-compatibility no-op: this store has no index to give up on.
func (l *Log) IndexError() error { return nil }

// Dir returns the session directory.
func (l *Log) Dir() string { return l.dir }

// NextID returns the next record id (ids sort lexicographically).
func (l *Log) NextID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	return fmt.Sprintf("%07d", l.seq)
}

// Close checkpoints and closes the store.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil
	}
	// why checkpoint before closing: a resting session directory should hold one file.
	// The checkpoint is best effort — a WAL left behind is a WAL replayed at the next open.
	_, _ = l.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	err := l.db.Close()
	l.db = nil
	return err
}

// upsertMeta writes one meta key inside a transaction.
func upsertMeta(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// blobRef is one image's bytes and the sha that names them.
type blobRef struct {
	sha  string
	mime string
	size int
	data []byte
}

// encodeEntry marshals an entry, splitting its image parts into blobRefs and leaving
// "blob:<sha256>" where the base64 was. The caller's entry is not modified.
func (l *Log) encodeEntry(e *ir.Entry) (string, []blobRef, error) {
	var refs []blobRef
	out := *e
	if len(out.Parts) > 0 {
		out.Parts = make([]ir.Part, len(e.Parts))
		copy(out.Parts, e.Parts)
		for i := range out.Parts {
			p := &out.Parts[i]
			if p.Type != ir.PartImage || p.Data == "" {
				continue
			}
			raw, err := decodeB64(p.Data)
			if err != nil {
				// why inline rather than fail: an image part this store was handed that is
				// not base64 is a bug upstream or a hand-written import, and losing the
				// entry outright would be the store lying about something it did receive.
				if l.logf != nil {
					l.logf("image part %d of entry %s is not base64; stored inline", i, e.ID)
				}
				continue
			}
			sum := sha256.Sum256(raw)
			p.Data = blobPrefix + hex.EncodeToString(sum[:])
			refs = append(refs, blobRef{sha: hex.EncodeToString(sum[:]), mime: p.MimeType, size: len(raw), data: raw})
		}
	}
	b, err := json.Marshal(&out)
	return string(b), refs, err
}

// decodeB64 reads the encoding image parts have always travelled in: standard base64,
// with padded and unpadded as the two shapes the wire has shown.
func decodeB64(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// payloadShas lists the blob references one stored payload holds.
func payloadShas(payload string) []string {
	var e ir.Entry
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return nil
	}
	var out []string
	for _, p := range e.Parts {
		if p.Type == ir.PartImage && strings.HasPrefix(p.Data, blobPrefix) {
			out = append(out, strings.TrimPrefix(p.Data, blobPrefix))
		}
	}
	return out
}

// blobUpsert pins one reference: a new blob arrives with its first reference, an existing
// one gains a reference (its bytes are never rewritten — the sha is the identity).
func blobUpsert(tx *sql.Tx, r blobRef) error {
	_, err := tx.Exec(`INSERT INTO blob(sha256, mime, size, refcount, bytes) VALUES(?, ?, ?, 1, ?)
		ON CONFLICT(sha256) DO UPDATE SET refcount = refcount + 1`, r.sha, r.mime, r.size, r.data)
	return err
}

// AppendEntry appends an entry. When setTip is true the path tip moves to the new entry
// in the same transaction (atomic commit). An empty e.ID gets a fresh id.
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
	payload, refs, err := l.encodeEntry(e)
	if err != nil {
		return err
	}
	kind := e.Kind
	if kind == "" {
		kind = "unknown" // the column names the row for SQL; the payload stays the truth
	}
	tx, err := l.db.Begin() // the driver's default since v0.37: BEGIN IMMEDIATE
	if err != nil {
		return err
	}
	for _, r := range refs {
		if err := blobUpsert(tx, r); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO entry(id, parent, kind, seq, ts, payload) VALUES(?, ?, ?, ?, ?, ?)`,
		e.ID, nullString(e.ParentID), kind, l.seq, float64(e.Ts.Unix()), payload); err != nil {
		tx.Rollback()
		return err
	}
	if setTip {
		if err := upsertMeta(tx, metaTip, e.ID); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := upsertMeta(tx, metaSeqNext, strconv.FormatInt(l.seq, 10)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetTip moves the path tip. An empty id says "no tip", and Replay then stands the
// session on its newest entry — the same promise the log's tipString made.
func (l *Log) SetTip(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	if err := upsertMeta(tx, metaTip, id); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Patch rewrites one entry in place (edit summary text, move boundary — and today, the
// UI's edit of a message). The payload it replaced goes to entry_rev stamped with a fresh
// seq, which is what keeps "which version was sent then" answerable; two patches of one
// entry take two seqs, so no revision ever overwrites another.
func (l *Log) Patch(e *ir.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.ID == "" {
		return errors.New("store: patch requires an id")
	}
	var old string
	err := l.db.QueryRow("SELECT payload FROM entry WHERE id = ?", e.ID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		// why a no-op and not an error: patching a deleted or unknown entry used to append
		// a line nobody would ever fold; a store that refuses to speak about nothing is the
		// same silence without the disk.
		return nil
	}
	if err != nil {
		return err
	}
	payload, refs, err := l.encodeEntry(e)
	if err != nil {
		return err
	}
	l.seq++
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO entry_rev(entry_id, seq, payload) VALUES(?, ?, ?)", e.ID, l.seq, old); err != nil {
		tx.Rollback()
		return err
	}
	// why the old payload's references are not touched: they did not die, they moved from
	// entry to entry_rev — the count is entry+rev together, and a revision pins its blobs.
	for _, r := range refs {
		if err := blobUpsert(tx, r); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec("UPDATE entry SET payload = ? WHERE id = ?", payload, e.ID); err != nil {
		tx.Rollback()
		return err
	}
	if err := upsertMeta(tx, metaSeqNext, strconv.FormatInt(l.seq, 10)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Delete removes an entry and re-attaches its children to its parent. The children's
// payloads are rewritten too — the parentId they carry must agree with the edge the tree
// says it has, in this store and not just in one column.
func (l *Log) Delete(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var parent, payload sql.NullString
	err := l.db.QueryRow("SELECT parent, payload FROM entry WHERE id = ?", id).Scan(&parent, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleting nothing used to write a line about nothing; same outcome
	}
	if err != nil {
		return err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	// Children first: rewrite each payload, then the edge, so no reader of either form
	// sees a child whose two parent pointers disagree.
	rows, err := tx.Query("SELECT id, payload FROM entry WHERE parent = ?", id)
	if err != nil {
		tx.Rollback()
		return err
	}
	type kid struct {
		id      string
		payload string
	}
	var kids []kid
	for rows.Next() {
		var k kid
		if err := rows.Scan(&k.id, &k.payload); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		kids = append(kids, k)
	}
	rows.Close()
	for _, k := range kids {
		newPayload, err := rewriteParent(k.payload, parent.String)
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec("UPDATE entry SET parent = ?, payload = ? WHERE id = ?", nullString(parent.String), newPayload, k.id); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, sha := range payloadShas(payload.String) {
		if _, err := tx.Exec("UPDATE blob SET refcount = refcount - 1 WHERE sha256 = ?", sha); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM entry WHERE id = ?", id); err != nil {
		tx.Rollback()
		return err
	}
	// why GC lives here: a reference that dies has nowhere else to be noticed, and a blob
	// nobody holds cannot survive the transaction that dropped its last holder.
	if _, err := tx.Exec("DELETE FROM blob WHERE refcount <= 0"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// rewriteParent swaps the parentId an entry's payload carries.
func rewriteParent(payload, parent string) (string, error) {
	var e ir.Entry
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return "", err
	}
	e.ParentID = parent
	b, err := json.Marshal(&e)
	return string(b), err
}

// Replay reads the store and returns the tree as it stands. The name is kept for the
// agent: it asks for "the session as it came off disk", and this answers that — only
// now the fold is a SELECT, because the tree was stored as edges, never as a story.
func (l *Log) Replay() (*State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var payloads []string
	rows, err := l.db.Query("SELECT payload FROM entry ORDER BY seq")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		payloads = append(payloads, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	st := &State{Entries: map[string]*ir.Entry{}}
	for _, p := range payloads {
		e, err := l.decodeEntry(p)
		if err != nil {
			return nil, err
		}
		if e.ID == "" {
			continue // a payload with no id can speak of no entry
		}
		st.Entries[e.ID] = e
	}
	st.Tip = l.effectiveTip()
	st.Seq = l.seq
	return st, nil
}

// decodeEntry re-attaches the blob bytes an entry's payload references, so callers get
// back the byte-identical ir.Entry AppendEntry was handed.
func (l *Log) decodeEntry(payload string) (*ir.Entry, error) {
	var e ir.Entry
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return nil, err
	}
	for i := range e.Parts {
		p := &e.Parts[i]
		if p.Type != ir.PartImage || !strings.HasPrefix(p.Data, blobPrefix) {
			continue
		}
		var raw []byte
		err := l.db.QueryRow("SELECT bytes FROM blob WHERE sha256 = ?", strings.TrimPrefix(p.Data, blobPrefix)).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			// why the reference survives its blob's absence: a payload that names a sha is
			// telling the truth about what was stored; a blob that vanished is corruption
			// worth a line, not worth losing the entry over.
			if l.logf != nil {
				l.logf("entry %s references blob %s, which is not there", e.ID, strings.TrimPrefix(p.Data, blobPrefix))
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		p.Data = base64.StdEncoding.EncodeToString(raw)
	}
	return &e, nil
}

// effectiveTip is the tip the session stands on: meta's, when it names a live entry;
// else the newest entry — "no tip" always meant "stand on the newest" (see SetTip).
func (l *Log) effectiveTip() string {
	var tip string
	err := l.db.QueryRow(`SELECT value FROM meta WHERE key = ? AND EXISTS(SELECT 1 FROM entry WHERE id = meta.value)`, metaTip).Scan(&tip)
	if err == nil && tip != "" {
		return tip
	}
	if err := l.db.QueryRow("SELECT id FROM entry ORDER BY seq DESC LIMIT 1").Scan(&tip); err != nil {
		return ""
	}
	return tip
}

// Read is the summary row's numbers, taken from a store this process has open.
func (l *Log) Read() (Info, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return queryInfo(l.db, l.dir)
}

// Inspect is the summary row's numbers of a session nobody has opened: one open, one
// query, one close — the cold open this store costs is a millisecond, not a replay.
func Inspect(dir string) (Info, error) {
	if _, err := os.Stat(filepath.Join(dir, DBFileName)); err != nil {
		return Info{}, err
	}
	l, err := Open(dir, false)
	if err != nil {
		return Info{}, err
	}
	defer l.Close()
	return queryInfo(l.db, dir)
}

// queryInfo folds the handful of aggregates a session list row is made of.
func queryInfo(db *sql.DB, dir string) (Info, error) {
	in := Info{}
	if err := db.QueryRow("SELECT COUNT(*) FROM entry").Scan(&in.Entries); err != nil {
		return Info{}, err
	}
	// Rows counts the write events reached by seq; tip moves and deletes are single
	// updates now, which is the number's new sentence (see Info.Rows).
	var rows int64
	if err := db.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?", metaSeqNext).Scan(&rows); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Info{}, err
	}
	in.Rows = rows
	// The depth of the tip's path: walk the edges up from the tip. A cycle is impossible
	// by construction (an edge only ever points at an entry of a lower seq), so the
	// recursive step always terminates.
	tip := ""
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = ? AND EXISTS(SELECT 1 FROM entry WHERE id = meta.value)`, metaTip).Scan(&tip); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Info{}, err
	}
	if tip == "" {
		if err := db.QueryRow("SELECT id FROM entry ORDER BY seq DESC LIMIT 1").Scan(&tip); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Info{}, err
		}
	}
	in.Tip = tip
	if tip != "" {
		var d int
		if err := db.QueryRow(`WITH RECURSIVE up(id, parent, d) AS (
				SELECT id, parent, 1 FROM entry WHERE id = ?
				UNION ALL
				SELECT e.id, e.parent, up.d + 1 FROM up JOIN entry e ON e.id = up.parent
			) SELECT MAX(d) FROM up`, tip).Scan(&d); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Info{}, err
		}
		in.Depth = d
	}
	var ts sql.NullFloat64
	if err := db.QueryRow("SELECT MAX(ts) FROM entry").Scan(&ts); err != nil {
		return Info{}, err
	}
	if ts.Valid {
		in.LastTs = time.Unix(int64(ts.Float64), 0).UTC()
	}
	// Bytes is the disk the session's directory actually spends on the store, WAL
	// included — the sidebar's number is what the disk says, not what a resting file says.
	if fi, err := os.Stat(filepath.Join(dir, DBFileName)); err == nil {
		in.LogBytes += fi.Size()
	}
	if fi, err := os.Stat(filepath.Join(dir, DBFileName+"-wal")); err == nil {
		in.LogBytes += fi.Size()
	}
	return in, nil
}

// nullString is the empty parent the way the table says "root": NULL.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
