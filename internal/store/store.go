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
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

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

// BlobPrefix marks a payload's image data as a reference into the blob table. Exported
// because a copier that moves entries between stores reads references without
// inflating them (agent.Fork) and must recognise the shape it is moving. The one
// string lives in ir, which is where both sides of the reference already meet.
const BlobPrefix = ir.RefPrefix

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

	// agg is the cached answer of queryInfo, trusted only while the store's file stamps
	// agree with aggKey; every write that commits drops it, and so does the next open.
	agg    *Info
	aggKey stamps

	// blobLookups counts the point queries inflate has asked the blob table for.
	// Unexported and read only from tests one package over: it is the instrument
	// behind "one call, one fetch per distinct image", which is otherwise
	// invisible to its callers.
	blobLookups int64

	logf func(format string, args ...any) // how something odd gets said; nil means silent
}

// Open opens (creating if needed) the store inside dir.
func Open(dir string, fsync bool) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// why these five pragmas and nothing else: WAL is the crash story (a torn commit is
	// a rollback, no torn-tail rule to implement); synchronous(NORMAL) is what fsync=false
	// always meant (the bench's 0.8ms write; FULL for fsync=true); busy_timeout is insurance
	// for a second reader; mmap_size(0) because the WAL shm mapping is the only mapping we
	// want the page cache deciding about (r83: 300MB lessons); cache_size(-512) because the
	// default 2MB per handle is generous at the scale this store plans for — the r83 A/B
	// measured the default costing +20MB of RSS at 17 open sessions, and hundreds are planned.
	syncMode := "NORMAL"
	if fsync {
		syncMode = "FULL"
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, DBFileName)) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(" + syncMode +
		")&_pragma=busy_timeout(5000)&_pragma=mmap_size(0)&_pragma=cache_size(-512)"
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
			if sha, isRef := strings.CutPrefix(p.Data, BlobPrefix); isRef && sha != "" {
				// A reference arrives as a reference: the bytes belong to whoever
				// copied them in (the fork pipeline puts them one step ahead of the
				// payload that names them), and a sha256 of bytes already named by
				// their own hash is rent paid twice. Pin, do not deliver.
				refs = append(refs, blobRef{sha: sha})
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
			p.Data = BlobPrefix + hex.EncodeToString(sum[:])
			refs = append(refs, blobRef{sha: hex.EncodeToString(sum[:]), mime: p.MimeType, size: len(raw), data: raw})
		}
	}
	// v2-native on purpose: the compat layer escapes < > & (and U+2028/29 besides) on the
	// way to disk while native writes what JSON itself requires — 5 bytes per special
	// character saved in every stored text field, permanently, which in a code-heavy
	// transcript is most of the log. Deterministic is insurance: ir.Entry carries no map
	// fields today, and if one grows, rewriteParent's re-marshal must not shuffle bytes.
	// This is a write-shape change, not a format change — old escaped payloads read on.
	b, err := jsonv2.Marshal(&out, jsonv2.Deterministic(true))
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
		if p.Type == ir.PartImage && strings.HasPrefix(p.Data, BlobPrefix) {
			out = append(out, strings.TrimPrefix(p.Data, BlobPrefix))
		}
	}
	return out
}

// Payload compression: TOAST's stance, taken at this store's one choke point. A payload
// column holds whichever shape is smaller — plain JSON text, or the same bytes compressed
// by the codec the operator picked — and every codec's output carries its own magic, so
// a reader never asks a flag what a row is: a few bytes say. The knob below is an answer
// about new writes only; every store reads every store.
//
// zstd is the default because the trained dictionary made it the smaller and the faster
// answer on every corpus measured (S8.7/S8.8): ~9% under gzip on disk, ~2× the scan
// speed, with the book itself costing 32KB (docs/zstd-dictionary.md).
var Compressor = "zstd" // "gzip" | "zstd" | "none" — each CLI's -compress flag wires it

// gzipFloor is the TOAST threshold shrunk to this store's page: under half a kilobyte
// of JSON, a compressed stream's header alone has eaten the difference. It guards both
// codecs — zstd's small-data penalty is smaller, not absent.
const gzipFloor = 512

var gzWriters = sync.Pool{New: func() any {
	w, err := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	if err != nil {
		panic(err) // gzip only fails on a bad level, and this one is a constant
	}
	return w
}}

var gzReaders = sync.Pool{New: func() any { return new(gzip.Reader) }}

// The zstd pair is built lazily and shared, keyed by a fingerprint of the dictionary
// pool (see SetZdict): swapping a dictionary mid-process costs one fresh pair, never a
// silent misread of frames stamped by the old one. EncodeAll/DecodeAll are safe for
// concurrent use on one instance, and a gzip-default process never pays either one's
// buffers. The encoder window is this codec's real RAM note — ~80MB resident at
// the pinned 1MB window, ~67MB of it the fixed tables of the chosen encoder (see
// zstdWindow) — which is why the window is pinned and why the import is lazy.
type zstdPairVal struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

var (
	zstdMu    sync.Mutex
	zstdPairs = map[string]zstdPairVal{}

	// userDict is the operator's own trained dictionary (CLI -zdict); seedDict is the
	// shipped one (seeddict.go). A dictionary is data, not configuration: frames it
	// stamped stay readable only while the book is kept, which is what the pool and
	// frame dict_ids are for.
	userDict []byte
	userTag  = "-"
	seedTag  = "-"
	seedOnce sync.Once
)

// ZstdLevel is the one level the store writes with and the level dictionaries must
// be trained for — the trainer tailors a dictionary to its level, so the two live
// behind one constant on purpose.
const ZstdLevel = zstd.SpeedBetterCompression

// zstdWindow is the encoder's match history. Measured on the heap profile (the
// docs only mention the 8MB default): the history buffer scales with the window —
// 68MB at 8MB, 8MB at 1MB — and whatever else the better-dict encoder carries
// resident per pool slot does not follow the window. The library default arrived
// as ~135MB on this machine; pinning 1MB lands the floor at ~80MB, which is the
// fixed cost per pool slot times the slot count — see zstdEncoderSlots for the
// other half of that product, and neither WithLowerEncoderMem (it only trims the
// history, worth ~1MB here) nor WithDecoderLowmem (already the default — true)
// has anything left to give.
//
// One row per EncodeAll frame is how payloads are written, and a frame never
// matches outside itself — the window only has to cover the largest single
// payload, and 1MB does that with headroom (measured: reforking the corpus store
// byte-for-byte under the new window — same 282,624 path bytes, so no ratio
// change). A dictionary must fit inside the window (the format requires it), which
// SetZdict enforces loudly instead of letting NewWriter fail into "store text"
// silence. Old frames carry their own window size in every frame header, so
// shrinking this constant decodes the existing corpus unchanged.
const zstdWindow = 1 << 20

// ZstdSlots pins the EncodeAll pool depth, and pinning it is the difference
// between a RAM figure and a rumor: the library sizes that pool at GOMAXPROCS —
// one full set of better-dict tables (~10MB) per core, born when the first row big
// enough to compress is written and held forever — so an unpinned encoder costs 80MB on this 8-core machine and would
// cost 300MB+ on a 32-core one. The store cannot use the extra slots: every
// AppendEntry holds its Log's mutex across the compression, so only cross-store
// concurrent writes reach the pool at all — two slots cover a session writing
// while an import or fork runs, and past that writers queue on the channel,
// costing latency in microseconds while costing megabytes not to have.
// Zero or negative means auto — leave the library's GOMAXPROCS sizing alone —
// which is what -zstd-slots spells for anyone whose trade is different.
var ZstdSlots = 2

// SetZdict installs the operator's zstd dictionary (nil or empty clears it, leaving
// the seed). The bytes are validated here so a wrong file fails at startup rather
// than at the first frame that quietly needed it.
func SetZdict(b []byte) error {
	if len(b) > 0 {
		if len(b) > zstdWindow {
			return fmt.Errorf("store: zstd dictionary is %d bytes, over the %d byte encoder window it must fit inside", len(b), zstdWindow)
		}
		if _, err := zstd.InspectDictionary(b); err != nil {
			return fmt.Errorf("store: not a zstd dictionary: %w", err)
		}
	}
	userDict = b
	userTag = dictTag(b)
	return nil
}

func dictTag(b []byte) string {
	if len(b) == 0 {
		return "-"
	}
	s := sha256.Sum256(b)
	return fmt.Sprintf("%d-%x", len(b), s[:4])
}

// zdictPool orders the dictionary pool seed-first, user-last: WithDecoderDicts lets
// the last dictionary registered win on an id collision, so the operator's copy of a
// book deliberately outranks the shipped one; the encoder takes the last book too.
func zdictPool() [][]byte {
	seedOnce.Do(func() { seedTag = dictTag(seedDict) })
	var pool [][]byte
	if len(seedDict) > 0 {
		pool = append(pool, seedDict)
	}
	if len(userDict) > 0 {
		pool = append(pool, userDict)
	}
	return pool
}

func zstdPair() (*zstd.Encoder, *zstd.Decoder) {
	pool := zdictPool() // also settles seedTag
	zstdMu.Lock()
	defer zstdMu.Unlock()
	key := seedTag + "|" + userTag
	p, ok := zstdPairs[key]
	if !ok {
		p = buildZstdPair(pool)
		zstdPairs[key] = p
	}
	return p.enc, p.dec
}

func buildZstdPair(pool [][]byte) zstdPairVal {
	// SpeedBetterCompression — the library's doc says ≈ zstd level 7–8 at ~2–3×
	// default CPU: conversion happens offline, and the stance here is disk over
	// anything the machine keeps in RAM. Only the four named levels exist (the
	// FromZstd helper just picks the closest one), and the library warns that the
	// constants' mapping itself may shift between versions — for a store whose
	// bytes live on disk for years, that makes the go.mod pin load-bearing and an
	// upgrade worth a fresh du of the corpus.
	eopts := []zstd.EOption{zstd.WithEncoderLevel(ZstdLevel), zstd.WithWindowSize(zstdWindow)}
	if ZstdSlots > 0 {
		eopts = append(eopts, zstd.WithEncoderConcurrency(ZstdSlots))
	}
	var dopts []zstd.DOption
	if len(pool) > 0 {
		eopts = append(eopts, zstd.WithEncoderDict(pool[len(pool)-1]))
		dopts = append(dopts, zstd.WithDecoderDicts(pool...))
	}
	enc, err1 := zstd.NewWriter(nil, eopts...)
	dec, err2 := zstd.NewReader(nil, dopts...)
	if err1 != nil || err2 != nil {
		return zstdPairVal{} // callers treat nil as "compress nothing, store text"; SetZdict has already rejected bad books
	}
	return zstdPairVal{enc: enc, dec: dec}
}

// maybeCompress returns the value to store: compressed bytes when the chosen codec
// wins, the original text otherwise. Losing costs nothing and happens constantly — a
// short or already-entropic payload compresses to more than it started as — and losing
// means plaintext, not a marker. An unknown codec name reads as "none": plaintext is
// always the safe answer to a typo.
func maybeCompress(s string) any {
	if len(s) < gzipFloor {
		return s
	}
	switch Compressor {
	case "gzip":
		zw := gzWriters.Get().(*gzip.Writer)
		defer gzWriters.Put(zw)
		var buf bytes.Buffer
		zw.Reset(&buf)
		if _, err := zw.Write([]byte(s)); err != nil {
			return s
		}
		if err := zw.Close(); err != nil || buf.Len() >= len(s) {
			return s
		}
		return buf.Bytes()
	case "zstd":
		if enc, _ := zstdPair(); enc != nil {
			if b := enc.EncodeAll([]byte(s), nil); len(b) < len(s) {
				return b
			}
		}
	}
	return s
}

// payloadText is the read side of maybeCompress: each codec's magic answered in the
// negative and the bytes are the text already. Plain rows — the floor's, the knob's,
// and every old store's — travel this same line.
func payloadText(raw string) string {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr := gzReaders.Get().(*gzip.Reader)
		defer gzReaders.Put(zr)
		if err := zr.Reset(bytes.NewReader([]byte(raw))); err != nil {
			return raw // bytes that claim gzip and are not are somebody else's diagnosis
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err != nil {
			return raw
		}
		return string(out)
	}
	if len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd {
		if _, dec := zstdPair(); dec != nil {
			if out, err := dec.DecodeAll([]byte(raw), nil); err == nil {
				return string(out)
			}
		}
	}
	return raw
}

// PayloadText is the exported answer for anyone reading a payload column outside these
// functions — blimport's digest, which must weigh the same whether or not a store
// deflated, and the audit tool the corpus deserves.
func PayloadText(raw string) string { return payloadText(raw) }

// blobUpsert pins one reference: a new blob arrives with its first reference, an existing
// one gains a reference (its bytes are never rewritten — the sha is the identity). A
// pin-only reference (no bytes: the payload arrived naming a sha, fork's shape) must find
// its bytes already here — inventing an empty-bytes row for a sha this store has never
// seen would be a blob that lies.
func blobUpsert(tx *sql.Tx, r blobRef) error {
	if r.data == nil {
		res, err := tx.Exec(`UPDATE blob SET refcount = refcount + 1 WHERE sha256 = ?`, r.sha)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("store: entry references blob %s, whose bytes were never put here", r.sha)
		}
		return nil
	}
	_, err := tx.Exec(`INSERT INTO blob(sha256, mime, size, refcount, bytes) VALUES(?, ?, ?, 1, ?)
		ON CONFLICT(sha256) DO UPDATE SET refcount = refcount + 1`, r.sha, r.mime, r.size, r.data)
	return err
}

// BlobBytes hands over one blob's bytes and mime — the read half of moving bytes
// between stores (the fork pipeline). The statement is self-contained by the store's
// one-connection rule: callers may interleave these reads with any other statement.
func (l *Log) BlobBytes(sha string) (b []byte, mime string, found bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil, "", false, ErrClosed
	}
	err = l.db.QueryRow("SELECT bytes, mime FROM blob WHERE sha256 = ?", sha).Scan(&b, &mime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	return b, mime, true, nil
}

// BlobSize is one blob's byte count, answered from the size column the write path
// keeps beside the bytes — no bytes move. It exists so an assembler can price an
// image note (asm's text-only view says "~N KB") over a reference, where the
// base64 the note was always counted from is not in hand.
func (l *Log) BlobSize(sha string) (size int, found bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return 0, false, ErrClosed
	}
	err = l.db.QueryRow("SELECT size FROM blob WHERE sha256 = ?", sha).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return size, true, nil
}

// PutBlob lands one blob's bytes without touching any refcount: references are owned
// by the payloads that name them, and AppendEntry raises the count when such a payload
// arrives. A sha already here — holding, as it must, the very same bytes, the sha
// being the identity — is ignored, which is what lets a batch copy hand the same sha
// twice and charge nothing.
func (l *Log) PutBlob(sha, mime string, b []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return ErrClosed
	}
	_, err := l.db.Exec(`INSERT INTO blob(sha256, mime, size, refcount, bytes) VALUES(?, ?, ?, 0, ?)
		ON CONFLICT(sha256) DO NOTHING`, sha, mime, len(b), b)
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
		e.ID, nullString(e.ParentID), kind, l.seq, float64(e.Ts.Unix()), maybeCompress(payload)); err != nil {
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
	if err := tx.Commit(); err != nil {
		return err
	}
	l.agg = nil // the row's numbers just moved; the next Read asks the store
	return nil
}

// ImportAll writes a prepared transcript as one transaction: an import is all entries or
// none, because half an imported tree is a path nobody can walk and a question nobody can
// answer. Ids are kept exactly as the file carries them — an import restores a history;
// only a fork reissues ids — and seq moves past every numeric id in the payload so NextID
// keeps handing out fresh ones. The tip moves in the same transaction: a session never
// exists with rows and no story to stand on. A failed import leaves the store exactly as
// it was found; the caller's rollback (delete the fresh directory) does the rest.
func (l *Log) ImportAll(entries []*ir.Entry, tip string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(entries) == 0 {
		return errors.New("store: nothing to import")
	}
	tx, err := l.db.Begin() // BEGIN IMMEDIATE, as everywhere
	if err != nil {
		return err
	}
	defer tx.Rollback() // harmlessly wrong after Commit
	for _, e := range entries {
		if e.ID == "" {
			return errors.New("store: imported entries must carry their id")
		}
		if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil && n > l.seq {
			l.seq = n // ids sort lexicographically; seq must not walk over them backwards
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
			kind = "unknown"
		}
		for _, r := range refs {
			if err := blobUpsert(tx, r); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO entry(id, parent, kind, seq, ts, payload) VALUES(?, ?, ?, ?, ?, ?)`,
			e.ID, nullString(e.ParentID), kind, l.seq, float64(e.Ts.Unix()), maybeCompress(payload)); err != nil {
			return err
		}
	}
	if tip != "" {
		if err := upsertMeta(tx, metaTip, tip); err != nil {
			return err
		}
	}
	if err := upsertMeta(tx, metaSeqNext, strconv.FormatInt(l.seq, 10)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.agg = nil
	return nil
}

// ImportTx is an import as a living transaction: rows arrive one at a time from a
// decoder and are written as they arrive, so the size of an import stops being a
// RAM bill. The all-or-nothing rule an import has always promised is the
// transaction's own — Finish settles at the end the two questions that used to
// need the whole file in hand (does the tip stand among the rows; is every parent
// a row that exists), each one SELECT over the rows as written, and a failed
// Finish or an Abort leaves the store exactly as it was found, the caller's
// rollback (delete the fresh directory) doing the rest.
//
// The store lock is held for the life of the transaction: under the single
// connection no other statement runs while a tx lives, and an import is aimed at
// a store nobody else is writing — a birth, not a merge. ImportAll stays for
// callers that already hold every entry (the blimport converter, and every test
// that asserts what an import is in one line); it is this type's batch shape.
type ImportTx struct {
	l    *Log
	tx   *sql.Tx
	n    int
	last string // id of the last row written: the tip when the file names none
}

// ImportStream opens the living transaction. As with ImportAll, a failed attempt
// may leave l.seq advanced in memory — the same caller (delete the directory)
// owns both forms of the undo.
func (l *Log) ImportStream() (*ImportTx, error) {
	l.mu.Lock()
	if l.db == nil {
		l.mu.Unlock()
		return nil, ErrClosed
	}
	tx, err := l.db.Begin() // BEGIN IMMEDIATE, as everywhere
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	return &ImportTx{l: l, tx: tx}, nil
}

// Entry writes one row, keeping the id exactly as the file carries it and moving
// seq past every numeric id — the same rules ImportAll applies, one row at a
// time. A duplicate id fails here on the spot, on the primary key's own sentence.
func (t *ImportTx) Entry(e *ir.Entry) error {
	if e.ID == "" {
		return errors.New("store: imported entries must carry their id")
	}
	if n, err := strconv.ParseInt(e.ID, 10, 64); err == nil && n > t.l.seq {
		t.l.seq = n // ids sort lexicographically; seq must not walk over them backwards
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now().UTC()
	}
	payload, refs, err := t.l.encodeEntry(e)
	if err != nil {
		return err
	}
	kind := e.Kind
	if kind == "" {
		kind = "unknown"
	}
	for _, r := range refs {
		if err := blobUpsert(t.tx, r); err != nil {
			return err
		}
	}
	if _, err := t.tx.Exec(`INSERT INTO entry(id, parent, kind, seq, ts, payload) VALUES(?, ?, ?, ?, ?, ?)`,
		e.ID, nullString(e.ParentID), kind, t.l.seq, float64(e.Ts.Unix()), maybeCompress(payload)); err != nil {
		return err
	}
	t.n++
	t.last = e.ID
	return nil
}

// NextID issues a fresh id inside the living transaction — what a fork hands its
// rows as it copies them. The plain Log.NextID would deadlock where this is needed:
// the transaction holds the store's lock for its whole life, and the copy must be
// able to name rows inside the transaction that owns the store.
func (t *ImportTx) NextID() string {
	t.l.seq++
	return fmt.Sprintf("%07d", t.l.seq)
}

// PutBlob lands one blob's bytes inside the living transaction — the fork's half
// of the table-to-table move. It makes Log.PutBlob's promises unchanged (an sha
// already here is ignored; the count is raised later by the payload that names
// it), with one more: the bytes commit in the same transaction as the rows that
// reference them, so no reader can ever see a reference whose bytes are missing.
func (t *ImportTx) PutBlob(sha, mime string, b []byte) error {
	_, err := t.tx.Exec(`INSERT INTO blob(sha256, mime, size, refcount, bytes) VALUES(?, ?, ?, 0, ?)
		ON CONFLICT(sha256) DO NOTHING`, sha, mime, len(b), b)
	return err
}

// Finish asks the two whole-file questions over the rows as written and commits.
// An empty tip stands on the last row, the way the format has always meant it.
func (t *ImportTx) Finish(tip string) error {
	defer t.l.mu.Unlock()
	defer t.tx.Rollback() // harmlessly wrong after Commit
	if t.n == 0 {
		return errors.New("store: nothing to import")
	}
	if tip == "" {
		tip = t.last
	}
	var one string
	err := t.tx.QueryRow("SELECT id FROM entry WHERE id = ?", tip).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: the tip %q is not among the entries", tip)
	}
	if err != nil {
		return err
	}
	var badID, badParent string
	err = t.tx.QueryRow(`SELECT e.id, e.parent FROM entry e
		WHERE e.parent IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM entry p WHERE p.id = e.parent) LIMIT 1`).Scan(&badID, &badParent)
	if err == nil {
		return fmt.Errorf("store: entry %q names parent %q that the import never carries", badID, badParent)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := upsertMeta(t.tx, metaTip, tip); err != nil {
		return err
	}
	if err := upsertMeta(t.tx, metaSeqNext, strconv.FormatInt(t.l.seq, 10)); err != nil {
		return err
	}
	if err := t.tx.Commit(); err != nil {
		return err
	}
	t.l.agg = nil
	return nil
}

// Rows counts the rows written so far — what Finish would commit, and what the
// route reports after it commits.
func (t *ImportTx) Rows() int { return t.n }

// Abort unwinds the living transaction; the store keeps whatever it held before
// the import began, which is the whole point of opening one.
func (t *ImportTx) Abort() error {
	defer t.l.mu.Unlock()
	err := t.tx.Rollback()
	t.l.agg = nil
	return err
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
	if err := tx.Commit(); err != nil {
		return err
	}
	l.agg = nil // a tip move is a path move is a depth move
	return nil
}

// SetMeta writes one caller's key into meta (empty value removes it). tip is the store's
// own key; this is the caller's namespace — "the daemon had this session open", "a wait
// is due at" — the kind of control state that has to survive exactly as long as the
// store, which is nothing else on this machine. Judgment stays where it was: these keys
// are doorbells a caller rings itself on the way in, and a doorbell that lied costs one
// unnecessary look, never one mistaken action.
func (l *Log) SetMeta(key, value string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if key == metaTip || key == metaSeqNext {
		return errors.New("store: " + key + " is the store's own meta key")
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	var opErr error
	if value == "" {
		_, opErr = tx.Exec("DELETE FROM meta WHERE key = ?", key)
	} else {
		opErr = upsertMeta(tx, key, value)
	}
	if opErr != nil {
		tx.Rollback()
		return opErr
	}
	return tx.Commit()
}

// Meta reads one caller's key from an open store.
func (l *Log) Meta(key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var v string
	err := l.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// MetaOf reads one meta key of a store nobody has opened — the doorbell read: open, one
// row, close, with the same stamp cache Inspect runs on. A false answer here is a store
// without that key, which is the same answer an open store gives.
func MetaOf(dir, key string) (string, bool) {
	l, err := Open(dir, false)
	if err != nil {
		return "", false
	}
	defer l.Close()
	return l.Meta(key)
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
	// why the revision keeps the bytes exactly as they were stored (plain or deflated):
	// they are archived, not read twice, and payloadText speaks one language to both.
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
	if _, err := tx.Exec("UPDATE entry SET payload = ? WHERE id = ?", maybeCompress(payload), e.ID); err != nil {
		tx.Rollback()
		return err
	}
	if err := upsertMeta(tx, metaSeqNext, strconv.FormatInt(l.seq, 10)); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.agg = nil // Rows moved, and LastTs may have
	return nil
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
		if _, err := tx.Exec("UPDATE entry SET parent = ?, payload = ? WHERE id = ?", nullString(parent.String), maybeCompress(newPayload), k.id); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, sha := range payloadShas(payloadText(payload.String)) {
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
	if err := tx.Commit(); err != nil {
		return err
	}
	l.agg = nil // entries, depth and the store's bytes all just moved
	return nil
}

// rewriteParent swaps the parentId an entry's payload carries.
func rewriteParent(payload, parent string) (string, error) {
	var e ir.Entry
	if err := json.Unmarshal([]byte(payloadText(payload)), &e); err != nil {
		return "", err
	}
	e.ParentID = parent
	// v2 like encodeEntry; the rewrite lands in the store's new write shape even when
	// the payload it read was old — parse-equivalent either way.
	b, err := jsonv2.Marshal(&e, jsonv2.Deterministic(true))
	return string(b), err
}

// Replay reads the store and returns the tree as it stands — every payload inflated,
// the whole tree in RAM at once. No production path calls it any more: the resident
// spine answers shape questions and Contents answers content questions, and the
// three copies (fork, export, window) all stream. It stays as the plainest reading
// of a store there is — which is exactly what a test wants to assert against.
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
	return l.inflate(payload, true)
}

// inflate reads one stored payload. expandBlobs decides whether image parts come
// back as base64 (the wire shape, what a model or a transcript page is owed) or
// stay "blob:<sha>" references (the shape a reader that never sends a picture can
// still answer with — sizes, counts, search).
func (l *Log) inflate(payload string, expandBlobs bool) (*ir.Entry, error) {
	return l.inflateWith(payload, expandBlobs, nil)
}

// inflateWith is inflate with a place to leave what it learned: a reader that
// inflates a batch (Contents) hands the same call-scoped map to every row, so a
// screenshot the window repeats crosses the wire once — one point query and one
// base64 encoding per distinct sha rather than per reference. Scoped to one call
// on purpose: blobs die (delete's GC), and a cache that outlives its call would
// some day speak for a sha that is no longer there.
func (l *Log) inflateWith(payload string, expandBlobs bool, cache map[string]string) (*ir.Entry, error) {
	var e ir.Entry
	if err := json.Unmarshal([]byte(payloadText(payload)), &e); err != nil {
		return nil, err
	}
	if !expandBlobs {
		return &e, nil
	}
	for i := range e.Parts {
		p := &e.Parts[i]
		if p.Type != ir.PartImage || !strings.HasPrefix(p.Data, BlobPrefix) {
			continue
		}
		sha := strings.TrimPrefix(p.Data, BlobPrefix)
		if cached, ok := cache[sha]; ok {
			p.Data = cached
			continue
		}
		var raw []byte
		l.blobLookups++
		err := l.db.QueryRow("SELECT bytes FROM blob WHERE sha256 = ?", sha).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			// why the reference survives its blob's absence: a payload that names a sha is
			// telling the truth about what was stored; a blob that vanished is corruption
			// worth a line, not worth losing the entry over.
			if l.logf != nil {
				l.logf("entry %s references blob %s, which is not there", e.ID, sha)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		p.Data = base64.StdEncoding.EncodeToString(raw)
		if cache != nil {
			cache[sha] = p.Data
		}
	}
	return &e, nil
}

// MetaState is the resident half of a session: the tree's shape and the facts a
// compaction leaves on it, and nothing else. One row per entry, every field a
// spine — this is what a session may hold in RAM for the length of a conversation,
// where holding the conversations themselves is what Replay costs and what an
// idle session should not be paying for (docs/design.md, memory).
type MetaState struct {
	Entries map[string]*ir.Meta
	Tip     string
	Seq     int64
}

// ErrClosed is what every reader answers once Close has run: the log handed its
// file back to the process, and a reader that raced the close (an emitState on a
// connect goroutine, say) is owed an error, not a nil pointer. Every statement
// below asks through it for that reason.
var ErrClosed = errors.New("store: log is closed")

// ReplayMeta reads the tree's shape without inflating a payload — except the
// compaction rows, whose boundary is a fact inside their JSON and whose count
// grows with the number of summaries rather than the length of the history.
// The rows arrive in seq order so the spine is assembled exactly as Replay
// would assemble the entries; Tip and Seq mean the same thing here as in State.
func (l *Log) ReplayMeta() (*MetaState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil, ErrClosed
	}
	// Two statements rather than one, and neither pays for the other's column:
	// the spine scan never touches the payload column at all (in a compressed
	// store that column is most of the file), and the payload scan is bounded by
	// the number of summaries the session ever made. IFNULL is the table's own
	// sentence — NULL is how it spells a root's parent.
	type spineRow struct {
		id, parent, kind string
		ts               float64
	}
	var spine []spineRow
	rows, err := l.db.Query("SELECT id, IFNULL(parent,''), kind, ts FROM entry ORDER BY seq")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r spineRow
		if err := rows.Scan(&r.id, &r.parent, &r.kind, &r.ts); err != nil {
			rows.Close()
			return nil, err
		}
		spine = append(spine, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	summaries := map[string]*ir.CompactionMeta{}
	crows, err := l.db.Query("SELECT id, payload FROM entry WHERE kind = ?", ir.KindCompaction)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var id, p string
		if err := crows.Scan(&id, &p); err != nil {
			crows.Close()
			return nil, err
		}
		var e ir.Entry
		if err := json.Unmarshal([]byte(payloadText(p)), &e); err != nil {
			crows.Close()
			return nil, err
		}
		summaries[id] = e.Compaction
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, err
	}
	crows.Close()
	st := &MetaState{Entries: map[string]*ir.Meta{}}
	for _, r := range spine {
		if r.id == "" {
			continue // a row with no id can speak of no entry
		}
		sec := int64(r.ts)
		m := &ir.Meta{ID: r.id, ParentID: r.parent, Kind: r.kind,
			Ts: time.Unix(sec, int64((r.ts-float64(sec))*float64(time.Second))).UTC()}
		m.Compaction = summaries[r.id] // nil unless this row is a summary
		st.Entries[m.ID] = m
	}
	st.Tip = l.effectiveTip()
	st.Seq = l.seq
	return st, nil
}

// Contents inflates exactly the named entries, in one statement, and reports the
// ones that are there: ids that were deleted answer by absence rather than error,
// because a reader that walked from the live tree can be between a write and a
// reload and the honest answer to "what is this id" is then "nothing". This is
// the store side of the resident spine: the session's window is kept + cmp ids,
// a page's is its own slice, and neither drags a whole history into RAM.
func (l *Log) Contents(ids []string, expandBlobs bool) (map[string]*ir.Entry, error) {
	if len(ids) == 0 {
		return map[string]*ir.Entry{}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil, ErrClosed
	}
	house := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		house[i] = "?"
		args[i] = id
	}
	rows, err := l.db.Query("SELECT payload FROM entry WHERE id IN ("+strings.Join(house, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	// Gather, release, then inflate — and this ordering is load-bearing, not
	// stylistic: the store runs on a single connection (SetMaxOpenConns(1)), so
	// an inflation that chases a blob reference needs the very connection the
	// scan above still holds. An entry with an image part asked for inside this
	// loop waits for itself forever (the shape Replay avoids by gathering its
	// payloads first, and one real session's screenshots were what it took to
	// find that out).
	var payloads []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		payloads = append(payloads, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*ir.Entry, len(ids))
	// One cache for the call: a window that sends the same screenshot in five
	// turns fetches and encodes it once. (No SQL join reaches this: the sha
	// lives inside the payload JSON, and the payload column is one codec's
	// bytes away from anything SQLite can look into — the scan above is the
	// last statement that knows ids, and inflation is the first that knows
	// parts.)
	var cache map[string]string
	if expandBlobs {
		cache = map[string]string{}
	}
	for _, p := range payloads {
		e, err := l.inflateWith(p, expandBlobs, cache)
		if err != nil {
			return nil, err
		}
		if e.ID != "" {
			out[e.ID] = e
		}
	}
	return out, nil
}

// Inflate reads one entry by id with its image parts expanded. A caller that
// walks rows one at a time — an export, which must not hold a page of pictures
// at once — gets one row's bytes live between this call and the next, and no
// larger residency than that. The cache is the caller's to own (see Contents
// for its one-call rule); nil means every image reference pays its own point
// query and encoding, which a one-pass stream happily trades for memory.
func (l *Log) Inflate(id string, cache map[string]string) (*ir.Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil, ErrClosed
	}
	var p string
	if err := l.db.QueryRow("SELECT payload FROM entry WHERE id = ?", id).Scan(&p); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // absence is answered, not errored, as Contents answers it
		}
		return nil, err
	}
	return l.inflateWith(p, true, cache)
}

// SkeletonRow is one row of the cost panel's view of the path: what a request
// cost, not what it said. Content, reasoning, tool calls and images are absent
// by construction — the panel's entire vocabulary, and nothing the transcript
// already answers.
type SkeletonRow struct {
	ID       string    `json:"id"`
	ParentID string    `json:"parentId,omitempty"`
	Kind     string    `json:"kind"`
	Role     string    `json:"role"`
	Usage    *ir.Usage `json:"usage,omitempty"`
	// Summary is the per-call list a compaction's Usage is the total of — the
	// refused retries included, so the panel can show the line items behind the row.
	Summary []ir.SummaryAttempt `json:"summaryRequests,omitempty"`
	Finish  string              `json:"finishReason,omitempty"`
	Ms      int64               `json:"ms,omitempty"`
	Ts      time.Time           `json:"ts"`
}

// Skeleton answers the cost panel for the live path without putting the path in
// RAM: the spine arrives in one cheap scan, then each payload is inflated and
// stripped one at a time, so the memory is a row and the bill is CPU — the same
// trade Search already makes, and the reason this reader exists next to Contents
// (which would happily hold a whole session's conversation to answer a table of
// numbers). Rows come back in path order, oldest first; a chain that walks into
// a missing parent stops at the prefix, as every path walk here does.
func (l *Log) Skeleton() ([]SkeletonRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil, ErrClosed
	}
	parent := map[string]string{}
	rows, err := l.db.Query("SELECT id, IFNULL(parent,'') FROM entry ORDER BY seq")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return nil, err
		}
		parent[id] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// walk the chain from the tip and reverse, then read each payload alone
	var chain []string
	seen := map[string]bool{}
	for id := l.effectiveTip(); id != ""; id = parent[id] {
		if seen[id] {
			break // corrupt cycle: the prefix is the honest answer
		}
		seen[id] = true
		if _, ok := parent[id]; !ok {
			break // the tip names an entry that is not there
		}
		chain = append(chain, id)
	}
	out := make([]SkeletonRow, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		var p string
		err := l.db.QueryRow("SELECT payload FROM entry WHERE id = ?", chain[i]).Scan(&p)
		if errors.Is(err, sql.ErrNoRows) {
			continue // deleted between the spine scan and the read: the panel loses a row, not a truth
		}
		if err != nil {
			return nil, err
		}
		e, err := l.inflate(p, false) // no blobs: a picture has no line in the cost panel
		if err != nil {
			return nil, err
		}
		row := SkeletonRow{ID: e.ID, ParentID: e.ParentID, Kind: e.Kind, Role: e.Role, Usage: e.Usage, Finish: e.Finish, Ms: e.Ms, Ts: e.Ts}
		if e.Compaction != nil {
			row.Summary = e.Compaction.Summary
		}
		out = append(out, row)
	}
	return out, nil
}

// ToolUses counts the tool entries named by ids — the drawer's "never called in
// forty turns" column — inflating one payload at a time, so the memory of the
// answer is a row rather than the path. That is the same trade Skeleton makes and
// for the same reason: the column wants a name and a timestamp from each row, and
// a session should not have to hold its conversation to say which tools it uses.
// Names come from the entries; timestamps from the column, which is where Replay
// reads them too.
func (l *Log) ToolUses(ids []string) (uses map[string]int, lastUsed map[string]string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return nil, nil, ErrClosed
	}
	uses, lastUsed = map[string]int{}, map[string]string{}
	for _, id := range ids {
		var (
			p  string
			ts float64
		)
		err := l.db.QueryRow("SELECT payload, ts FROM entry WHERE id = ?", id).Scan(&p, &ts)
		if errors.Is(err, sql.ErrNoRows) {
			continue // deleted since the spine walk: the count loses a use, not a truth
		}
		if err != nil {
			return nil, nil, err
		}
		e, err := l.inflate(p, false) // a tool result's name is not a picture
		if err != nil {
			return nil, nil, err
		}
		if e.Kind != ir.KindTool || e.Tool == "" {
			continue
		}
		uses[e.Tool]++
		sec := int64(ts)
		lastUsed[e.Tool] = time.Unix(sec, int64((ts-float64(sec))*float64(time.Second))).UTC().Format(time.RFC3339)
	}
	return uses, lastUsed, nil
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

// SearchHit is one entry whose payload holds what the caller was looking for, with a
// snippet cut from around the first match.
type SearchHit struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Seq     int64  `json:"seq"`
	Ts      int64  `json:"ts"`
	Snippet string `json:"snippet"`
}

// Search walks the live entries, inflating payloads in flight — plan A, the one that
// keeps no index to go stale and asks no tokenizer to understand Chinese: a substring
// is a substring in every language. The bill is CPU (an inflation per row per scan),
// priced honestly by the corpus test; an index is a later answer to a bigger corpus.
// The match is against the stored JSON, so a needle can hit a field name as well as a
// word — v0 accepts that, and says so here rather than pretending otherwise.
func (l *Log) Search(query string, limit int) ([]SearchHit, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	needle := strings.ToLower(query)
	rows, err := l.db.Query("SELECT id, kind, seq, ts, payload FROM entry ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var (
			h   SearchHit
			tsf float64
			raw string
		)
		if err := rows.Scan(&h.ID, &h.Kind, &h.Seq, &tsf, &raw); err != nil {
			return nil, err
		}
		text := payloadText(raw)
		at := strings.Index(strings.ToLower(text), needle)
		if at < 0 {
			continue
		}
		h.Ts = int64(tsf)
		h.Snippet = searchSnippet(text, at, len(needle))
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// searchSnippet clips the bytes around a match down to something a list row can show,
// then makes the edges valid characters rather than halves of one — the clip lands by
// byte, the reader reads by character.
func searchSnippet(text string, at, n int) string {
	const pad = 60
	lo, hi := at-pad, at+n+pad
	if lo < 0 {
		lo = 0
	}
	if hi > len(text) {
		hi = len(text)
	}
	s := strings.ToValidUTF8(text[lo:hi], "")
	if lo > 0 {
		s = "…" + s
	}
	if hi < len(text) {
		s += "…"
	}
	return s
}

// Read is the summary row's numbers, taken from a store this process has open.
//
// The numbers are cached: re-asking them is a statement of SQL, and at the number of
// open stores a busy daemon carries, refreshing a list should not have to ask them.
// The cache is trusted only while the store's files agree with the stamps it was taken
// at — which costs two stats and no query, because a write always dirties one of them.
func (l *Log) Read() (Info, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := statStamps(l.dir)
	if l.agg != nil && st.equal(l.aggKey) {
		in := *l.agg
		in.LogBytes = st.bytes()
		return in, nil
	}
	in, err := queryInfo(l.db)
	if err != nil {
		return Info{}, err
	}
	cp := in
	l.agg, l.aggKey = &cp, st
	in.LogBytes = st.bytes()
	return in, nil
}

// Inspect is the summary row's numbers of a session nobody has opened: one open, one
// query, one close — the cold open this store costs is a millisecond, not a replay —
// and after that first one, a package cache keyed by the same stamps Read uses. The
// hundred sessions nobody clicked cost a stat each per listing, not a query.
func Inspect(dir string) (Info, error) {
	if _, err := os.Stat(filepath.Join(dir, DBFileName)); err != nil {
		inspectCacheMu.Lock()
		delete(inspectCache, dir) // a session deleted out from under us loses its cached row
		inspectCacheMu.Unlock()
		return Info{}, err
	}
	st := statStamps(dir)
	inspectCacheMu.Lock()
	e, ok := inspectCache[dir]
	inspectCacheMu.Unlock()
	if ok && st.equal(e.st) {
		in := e.in
		in.LogBytes = st.bytes()
		return in, nil
	}
	l, err := Open(dir, false)
	if err != nil {
		return Info{}, err
	}
	defer l.Close()
	in, err := queryInfo(l.db)
	if err != nil {
		return Info{}, err
	}
	in.LogBytes = st.bytes()
	inspectCacheMu.Lock()
	inspectCache[dir] = inspectEntry{st: st, in: in}
	inspectCacheMu.Unlock()
	return in, nil
}

// stamps is the (mtime, size) pair of a store's two files. Any write that moves a
// summary number either dirties the WAL or is the checkpoint that folds it back —
// either way at least one of the four figures moves — so a matching stamp means the
// numbers are as current as the files the list is made of. The package cache below is
// bounded by the number of session directories, which is the order a listing already is.
type stamps struct {
	dbMod, walMod time.Time
	dbSize        int64
	walSize       int64
}

func statStamps(dir string) stamps {
	var s stamps
	if fi, err := os.Stat(filepath.Join(dir, DBFileName)); err == nil {
		s.dbMod, s.dbSize = fi.ModTime(), fi.Size()
	}
	if fi, err := os.Stat(filepath.Join(dir, DBFileName+"-wal")); err == nil {
		s.walMod, s.walSize = fi.ModTime(), fi.Size()
	}
	return s
}

func (s stamps) bytes() int64 { return s.dbSize + s.walSize }

func (s stamps) equal(o stamps) bool {
	return s.dbSize == o.dbSize && s.walSize == o.walSize &&
		s.dbMod.Equal(o.dbMod) && s.walMod.Equal(o.walMod)
}

var (
	inspectCacheMu sync.Mutex
	inspectCache   = map[string]inspectEntry{}
)

type inspectEntry struct {
	st stamps
	in Info
}

// queryInfo is the summary row in one statement: the tip (stated, else the newest entry),
// the depth of the path it stands on walked up the edges, the counts, and the newest
// timestamp. A cycle in the walk is impossible by construction (an edge only ever points
// at an entry of a lower seq), so the recursive step always terminates. The previous code
// asked six separate questions; at the number of stores one listing walks, that
// difference is the listing. Bytes is the caller's to stamp — what lives here is the
// store's side of the row, and Rows keeps the number's new sentence (see Info.Rows).
func queryInfo(db *sql.DB) (Info, error) {
	var (
		in    Info
		rows  sql.NullInt64
		tip   sql.NullString
		depth sql.NullInt64
		ts    sql.NullFloat64
	)
	err := db.QueryRow(`WITH
			t(v) AS (SELECT COALESCE(
					(SELECT value FROM meta WHERE key = ? AND EXISTS (SELECT 1 FROM entry WHERE id = meta.value)),
					(SELECT id FROM entry ORDER BY seq DESC LIMIT 1))),
			up(id, parent, d) AS (
					SELECT e.id, e.parent, 1 FROM entry e JOIN t ON e.id = t.v
					UNION ALL
					SELECT e.id, e.parent, up.d + 1 FROM up JOIN entry e ON e.id = up.parent)
		SELECT (SELECT COUNT(*) FROM entry),
			(SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?),
			(SELECT v FROM t),
			(SELECT MAX(d) FROM up),
			(SELECT MAX(ts) FROM entry)`, metaTip, metaSeqNext).
		Scan(&in.Entries, &rows, &tip, &depth, &ts)
	if err != nil {
		return Info{}, err
	}
	in.Rows, in.Tip, in.Depth = rows.Int64, tip.String, int(depth.Int64)
	if ts.Valid {
		in.LastTs = time.Unix(int64(ts.Float64), 0).UTC()
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
