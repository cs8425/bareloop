// index.go owns log.idx, the second file of a session directory.
//
// # What it is for
//
// A session directory used to hold one record file and nothing else: to answer "what is in this
// session", or to show a session's transcript at all, the log had to be read from the first line to the
// last, in full, and every entry of it kept in RAM. The index is what removes that: one fixed 32-byte
// row per record of log.jsonl, in the same order as the log, saying what that record was and where it
// lies - offset and length - so that the content of one entry, or of a page of them, is a ReadAt at a
// known address instead of a scan.
//
// The rows are not a copy of the tree, and that is the whole reason they can be cheap. A patch changes a
// row's offset and length - which record is the latest word about that entry - and nothing else. A
// delete leaves its row exactly as it was and is applied at read time. So writing a record never
// rewrites another row, which is what lets a row be appended in the same breath as the record it
// describes. The tree, with its parent pointers, is what folding the rows yields - the fold Replay has
// always done, now written once and shared, and runnable without any content.
//
// # Why the index cannot be trusted, and what that costs
//
// log.jsonl is the only truth. The index is checked - its rows are compared with the ones just derived
// from the log, and its header carries a digest of exactly the log bytes they describe - and a
// disagreement is not argued with: the index is rebuilt, and the fact is said out loud. If the index
// cannot be written at all, the index is dropped and the log keeps taking writes. An index that a log
// write was lost for is a broken trade, and a broken trade is worse than no index.
//
// # The records of the log in one place
//
// Replay used to hold the rules that fold put/patch/tip/delete into a tree, and those rules are the log
// format: a second copy of them in the file that reads that format would mean a transcript that differs
// depending on which file was asked. So the rules are in this file, and both callers share them - Replay
// folds rows plus contents into the in-memory tree it always returned, and Skeleton folds the rows alone
// into the same shape minus the content. The rules that stay outside the fold are the ones that are not
// records: a log whose tip was never moved sits at its newest entry, and an off-path compaction whose
// parent is the tip is adopted onto it.
//
// # What is not here yet
//
// Nothing that answers a request goes near log.idx yet: S2-a builds it, S2-b reads a page of a session out
// of it, S2-c lists a session from it, and S2-d decides when a log is not resident at all. The only thing it
// is read for today is the comparison that decides whether one line is worth saying about it (openIndex). The
// header therefore already carries what a reader will want without the log open - a count of records, of
// patches, of deletes, of tip moves, of unreadable records, of lines, of bytes covered - and carries nothing
// else, because every field of a file that is written but never read is a field that cannot be noticed being
// wrong.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"bareloop/internal/ir"
)

const (
	indexFileName = "log.idx"

	// indexRowSize is why no row is ever rewritten: an entry's whole history fits in one row, and a
	// patch only has to move the fields that say where its latest record lies.
	indexRowSize = 32

	// The header gets a whole block. It is rewritten on every append, and a file system that cannot write
	// 4096 bytes in one go is not one this log can be trusted on either - the log's own append relies on the
	// same thing.
	indexHeaderSize = 4096
	indexVersion    = 1
	indexTipMax     = 32 // bytes of the header's tip field

	// indexMagic names the file, so a truncated or misplaced file is refused rather than read as rows.
	indexMagic = "BLIX"
)

var (
	// errIndexBadShape and errIndexVersion are refusals to resume: an index that is not the shape this
	// build wrote, or not the version it wrote, is rebuilt from the log and not repaired.
	errIndexBadShape = errors.New("store: log.idx is not the shape this build writes")
	errIndexVersion  = errors.New("store: log.idx is of another version")

	// errNotNumbered is why a log whose ids are not numbers has no index: a row names an entry by its
	// number, and an id that is not a number has no number to be found by. Such a log is still read, still
	// folded, and loses nothing but the index file - and this is all it says about that.
	errNotNumbered = errors.New("store: log.jsonl holds ids this store did not write, so it has no log.idx")
)

// header is log.idx's first block, and log.idx is the only file in this package with a binary format of
// its own - because it is the only file that has to be *seekable*: the point of an index is reading one
// row at a known address, which JSON forfeits. Everything else in a session directory is still one
// hand-written line of JSON on disk, so the format this package writes is still one line at a time; what
// a second file format costs is paid for here and nowhere else.
//
// All fields little-endian. Field order is the on-disk layout, and bytes 8..11 are what stays reserved
// for the next version:
//
//	  0 magic   "BLIX"
//	  4 version  uint32
//	  8 crc32   IEEE of bytes 12..4095, with the crc field itself zeroed
//	 12 ck      sha256 of log.jsonl[0:cover]
//	 44 cover   int64  how many bytes of the log the rows describe
//	 52 lines   int64  lines of log.jsonl inside cover, rows and all
//	 60 rows    int64  one row per record of the log
//	 68 records int64  rows that put an entry
//	 76 patches int64  rows that update one
//	 84 deletes int64  rows that remove one
//	 92 tips    int64  rows that only move the tip
//	100 skips   int64  rows this build cannot act on
//	108 tip     32 B   the tip's id, 0-padded, empty if the log left it unset
//
// ck is what lets a later build trust the rows without reading the log: it pins the first `cover` bytes,
// and the rows describe exactly those bytes. A log whose first change is an append still hashes the
// same, so the digest survives normal use; a rewrite of any earlier byte does not, and the index is
// rebuilt.
//
// cover is not the size of the log: a tail torn by a crash covers nothing, and a record appended after
// one is a thing S2-b has to read past. It is the length of what the rows do describe. lines is more than
// rows by every line that was never a record at all - a blank line, or a line that is not JSON - which is
// why the two are counted apart.
//
// The five row counters are a partition of rows: records+patches+deletes+tips+skips is every row, and
// records is the one the sidebar calls a session's log size (D54). Keeping them apart is what lets S2-c
// print a session's numbers without folding the rows or parsing the log at all.
type header struct {
	cover   int64
	lines   int64
	rows    int64
	records int64
	patches int64
	deletes int64
	tips    int64
	skips   int64
	ck      [sha256.Size]byte
	tip     string
}

func (h header) encode(buf []byte) {
	copy(buf[0:4], indexMagic)
	binary.LittleEndian.PutUint32(buf[4:8], indexVersion)
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	copy(buf[12:44], h.ck[:])
	binary.LittleEndian.PutUint64(buf[44:52], uint64(h.cover))
	binary.LittleEndian.PutUint64(buf[52:60], uint64(h.lines))
	binary.LittleEndian.PutUint64(buf[60:68], uint64(h.rows))
	binary.LittleEndian.PutUint64(buf[68:76], uint64(h.records))
	binary.LittleEndian.PutUint64(buf[76:84], uint64(h.patches))
	binary.LittleEndian.PutUint64(buf[84:92], uint64(h.deletes))
	binary.LittleEndian.PutUint64(buf[92:100], uint64(h.tips))
	binary.LittleEndian.PutUint64(buf[100:108], uint64(h.skips))
	copy(buf[108:108+indexTipMax], h.tip) // 0-padded; a tip is an id, so it is always 7 bytes
	binary.LittleEndian.PutUint32(buf[8:12], crc32.ChecksumIEEE(buf[12:]))
}

// decodeHeader is the whole trust boundary of log.idx: a file whose magic, version or block CRC is wrong
// comes back as an error, and every caller treats that as "read the log". Whether the body holds the rows
// the header counts is readIndex's to check, because it is the only one that reads the body.
func decodeHeader(buf []byte) (header, error) {
	if string(buf[0:4]) != indexMagic {
		return header{}, errIndexBadShape
	}
	if v := binary.LittleEndian.Uint32(buf[4:8]); v != indexVersion {
		return header{}, fmt.Errorf("%w: file says %d, this build writes %d", errIndexVersion, v, indexVersion)
	}
	if crc32.ChecksumIEEE(buf[12:]) != binary.LittleEndian.Uint32(buf[8:12]) {
		return header{}, errors.New("store: log.idx header fails its own CRC")
	}
	return header{
		cover:   int64(binary.LittleEndian.Uint64(buf[44:52])),
		lines:   int64(binary.LittleEndian.Uint64(buf[52:60])),
		rows:    int64(binary.LittleEndian.Uint64(buf[60:68])),
		records: int64(binary.LittleEndian.Uint64(buf[68:76])),
		patches: int64(binary.LittleEndian.Uint64(buf[76:84])),
		deletes: int64(binary.LittleEndian.Uint64(buf[84:92])),
		tips:    int64(binary.LittleEndian.Uint64(buf[92:100])),
		skips:   int64(binary.LittleEndian.Uint64(buf[100:108])),
		ck:      shaSum(buf[12:44]),
		tip:     cstring(buf[108 : 108+indexTipMax]),
	}, nil
}

// rowKind is one byte for every kind of record the log can hold. The four entry kinds and the four
// patches of those kinds are what a reader needs to tell an entry from an update of one; the two
// unknown kinds are for a record whose kind this build has no name for, which is legal in the log and
// simply leaves the kind to the record itself; and rowBad is a record this build cannot act on at all.
type rowKind uint8

const (
	rowNone rowKind = iota // what a row of all zeroes reads as: nothing. Never written by this build.
	rowTip                 // a move of the tip, which is not an entry
	rowDelete
	rowEntryUnknown // an entry of a kind this build has no name for
	rowPatchUnknown // an update of an entry, naming no kind this build knows
	rowBad          // a record this build cannot act on: no entry where one was needed, an op from a later build
	rowUser
	rowAssistant
	rowTool
	rowCompaction
	rowPatchUser
	rowPatchAssistant
	rowPatchTool
	rowPatchCompaction
)

// kindRow is the row kind of one record. It is a table rather than arithmetic so that adding an op or a
// kind cannot quietly shift a stored byte.
func kindRow(op, kind string) rowKind {
	switch op {
	case OpEntry:
		switch kind {
		case ir.KindUser:
			return rowUser
		case ir.KindAssistant:
			return rowAssistant
		case ir.KindTool:
			return rowTool
		case ir.KindCompaction:
			return rowCompaction
		default:
			return rowEntryUnknown
		}
	case OpPatch:
		switch kind {
		case ir.KindUser:
			return rowPatchUser
		case ir.KindAssistant:
			return rowPatchAssistant
		case ir.KindTool:
			return rowPatchTool
		case ir.KindCompaction:
			return rowPatchCompaction
		default:
			return rowPatchUnknown
		}
	case OpTip:
		return rowTip
	case OpDelete:
		return rowDelete
	}
	return rowBad
}

// kindString is the entry kind a row names, and whether it names an entry at all. A patch names the kind
// it updated, which is why the two families answer the same way.
func kindString(k rowKind) (string, bool) {
	switch k {
	case rowUser, rowPatchUser:
		return ir.KindUser, true
	case rowAssistant, rowPatchAssistant:
		return ir.KindAssistant, true
	case rowTool, rowPatchTool:
		return ir.KindTool, true
	case rowCompaction, rowPatchCompaction:
		return ir.KindCompaction, true
	}
	return "", false
}

// isPatchRow is the one thing the fold has to know about the shape of the kinds: whether a row updates
// an entry or puts one. It is a range test, and the range is the four patch kinds plus the unknown one.
func isPatchRow(k rowKind) bool {
	return k >= rowPatchUser && k <= rowPatchCompaction || k == rowPatchUnknown
}

// rowFlags is one byte of "and what else", of which there is one fact: a record that also moved the tip
// onto its own entry. It is a flag because a row that carried a second id would need seven more bytes to
// say what one bit says - and because the only writer of that case is AppendEntry, which tips the entry
// it just wrote or nothing.
type rowFlags uint8

const tipSet rowFlags = 1 << 0

// row is one index row: one line of log.jsonl, and the entry it holds.
//
// It holds no parent chain and no id, because both are derived: a patch can be the latest word about an
// entry without moving it, a delete re-parents what was below it, and a row that had to be rewritten for
// either would make an append cost more than a write. The numbers in target and parent are ids - the
// log's own ids, less the padding - which is what lets one row name another without a string. Zero means
// "no such thing": a root has no parent, and a tip of zero is no tip.
//
// off and n address the entry's *latest* record - the one that is the whole current entry - which is
// what a page or a single-entry read wants. n excludes the record's newline, so off+n is the byte just
// before the next line begins. A row of a line that held no record has no address at all.
//
// The fields are ordered by alignment rather than by the file's order, which lands the struct on exactly
// indexRowSize bytes: the two bytes the format reserves at the end of a row are the two the compiler pads it
// with (TestARowIsTheWidthTheIndexSeeksBy holds it to that). The order they are stored in is the encoder's
// business, is fixed by the version of the file, and is not the order below - nothing in this package maps a
// field to a byte offset by position, and a caller that wants to treat a []row as the body of log.idx is
// holding a file whose format it has not read.
type row struct {
	off    int64
	ts     int64
	target int32 // id of the entry the record is about: written, updated, deleted, or the new tip
	parent int32 // id of the parent the record named, 0 for a root
	n      uint32
	kind   rowKind
	flags  rowFlags
}

// encode writes the row into buf, in the order the format names its fields - which is not the order of the
// fields above.
//
// why buf is trusted to be a row wide: both writers here hand over exactly that (put a [indexRowSize]byte it
// just zeroed, writeAll one row's slice of the body it just made), and a caller that handed over less would
// panic in the runtime rather than write a row across the wrong bytes. A format this positional is not going
// to be defensive about the one thing its own callers cannot get wrong quietly.
func (r row) encode(buf []byte) {
	buf[0] = byte(r.kind)
	buf[1] = byte(r.flags)
	binary.LittleEndian.PutUint32(buf[2:6], uint32(r.target))
	binary.LittleEndian.PutUint32(buf[6:10], uint32(r.parent))
	binary.LittleEndian.PutUint64(buf[10:18], uint64(r.off))
	binary.LittleEndian.PutUint32(buf[18:22], r.n)
	binary.LittleEndian.PutUint64(buf[22:30], uint64(r.ts))
	// bytes 30..31 are reserved and written as zero
}

func decodeRows(body []byte) []row {
	rows := make([]row, 0, len(body)/indexRowSize)
	for i := 0; i+indexRowSize <= len(body); i += indexRowSize {
		b := body[i : i+indexRowSize]
		rows = append(rows, row{
			kind:   rowKind(b[0]),
			flags:  rowFlags(b[1]),
			target: int32(binary.LittleEndian.Uint32(b[2:6])),
			parent: int32(binary.LittleEndian.Uint32(b[6:10])),
			off:    int64(binary.LittleEndian.Uint64(b[10:18])),
			n:      binary.LittleEndian.Uint32(b[18:22]),
			ts:     int64(binary.LittleEndian.Uint64(b[22:30])),
		})
	}
	return rows
}

// rowDiff is what two row lists disagree about, and "" when they do not. It is a sentence and not an
// error because a disagreement is never fatal: the index is rebuilt either way, and the only thing at
// stake is whether anyone hears about it.
func rowDiff(was, now []row) string {
	if len(was) == 0 {
		// why nothing is said about an index of no rows: that is a session being indexed for the first time,
		// and a first time is not a disagreement.
		return ""
	}
	if len(was) != len(now) {
		return fmt.Sprintf("covers %d rows and the log holds %d", len(was), len(now))
	}
	for i := range was {
		if was[i] != now[i] {
			return fmt.Sprintf("differs from the log at row %d (kind %d, target %d: the log says kind %d, target %d)",
				i+1, was[i].kind, was[i].target, now[i].kind, now[i].target)
		}
	}
	return ""
}

// shaSum is 32 bytes as the array the header holds its digest in.
func shaSum(b []byte) [sha256.Size]byte {
	var out [sha256.Size]byte
	copy(out[:], b)
	return out
}

// cstring is a 0-padded fixed-width field back as a string.
func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// idNum is an id as the number a row uses for it, and an error when the id is not of the shape this
// store writes.
//
// %07d is not a formatting preference: AppendEntry hands out nothing else, a row names an entry by that
// number, and a fold that derived an id from a row number has to get the string back. An id that was
// written by hand, or by the branch of AppendEntry that accepts a caller's id, is therefore not indexable
// at all - scanLog reads such a log with numbers of its own making (negative, so they cannot collide with
// a real id) and Replay leaves it without a log.idx.
func idNum(id string) (int32, error) {
	if len(id) != 7 {
		return 0, fmt.Errorf("node id %q is not the 7-digit id this log writes", id)
	}
	n, err := strconv.Atoi(id)
	// why the round trip and not the parse: 741 is three characters and 0000741 is seven, and only the second
	// is what AppendEntry wrote. The way back is skID, not the number printed bare - an id that does not
	// survive that trip is an entry no row of this log could ever be found by. The length is checked first
	// because the number has to fit the int32 a row holds it in.
	if err != nil || n <= 0 || skID(int32(n)) != id {
		return 0, fmt.Errorf("node id %q is not the 7-digit id this log writes", id)
	}
	return int32(n), nil
}

// skID is idNum backwards: a row number as the id the log writes.
func skID(n int32) string { return fmt.Sprintf("%07d", n) }

// idReader turns an id the log wrote into the number a row uses for it, and says false for an id that has
// no number - which is the log's business and not one record's, so a record answers with an error and its
// reader decides what a log like that is worth. scanLog hands over a reader that answers every id, the
// hand-written ones by number of its own making; the log being written right now hands over numberedIDs,
// because a log that is not numbering its entries is not being indexed.
type idReader func(id string) (int32, bool)

// numberedIDs is the reader of a log being written: an id is its number, or it is nothing.
func numberedIDs(id string) (int32, bool) {
	n, err := idNum(id)
	return n, err == nil
}

// rowIndex is the row for one record, at the offset and length it occupies in the log, with its ids read by
// as. The entry it hands back is what the caller keeps beside the row, to put in a tree.
//
// The two numbers the caller hands over are the line's: off is where the line starts and n is the record
// without its newline. A record this build cannot act on - no entry where one was needed, an entry with no
// id to be found by, an op from a later build - comes back as a row of kind rowBad, which is what folding
// it will do nothing about.
func rowIndex(r Record, off int64, n int, as idReader) (out row, held *ir.Entry, err error) {
	if n > math.MaxUint32 {
		return row{}, nil, fmt.Errorf("a record of %d bytes is past what one index row can address", n)
	}
	// why one helper for every id in the record: an id the reader cannot name is an id no row can be found
	// by, and there is no useful third answer between "a number" and "an error".
	idOf := func(what, s string) (int32, error) {
		if s == "" {
			return 0, nil // 0 is the log's own "no parent" and "no tip", and costs no lookup
		}
		n, ok := as(s)
		if !ok {
			return 0, fmt.Errorf("%s %q is not the 7-digit id this log writes", what, s)
		}
		return n, nil
	}
	out = row{kind: rowBad, off: off, n: uint32(n)}
	switch r.Op {
	case OpEntry, OpPatch:
		e := r.Entry
		if e == nil || e.ID == "" {
			// why a row and not a refusal: a record with no entry is data this build cannot act on, and an
			// entry with no id is an entry no row can be found by - so the row says the same nothing about
			// both, and the line stays covered by the index that describes the log.
			return out, nil, nil
		}
		out.kind = kindRow(r.Op, e.Kind)
		out.ts = e.Ts.Unix()
		if out.target, err = idOf("node id", e.ID); err != nil {
			return row{}, nil, err
		}
		// why a patch's parent is read the same way a put's is: a patch never moves a node, so the parent a
		// patch carries is only read for the one case of an entry the log never put - and the fold inherits
		// it from the node either way.
		if out.parent, err = idOf("parent id", e.ParentID); err != nil {
			return row{}, nil, err
		}
		if r.Op == OpEntry && r.Tip != "" {
			// why refused rather than written: one row has one id in it, and the only writer of this case is
			// AppendEntry, which tips the entry it just wrote or nothing. A log that did otherwise would need a
			// row that lies about which entry is the tip.
			if r.Tip != e.ID {
				return row{}, nil, fmt.Errorf("record %s tips a different entry than it writes (%s)", e.ID, r.Tip)
			}
			out.flags |= tipSet
		}
		return out, e, nil
	case OpTip:
		out.kind = rowTip
		if out.target, err = idOf("tip id", r.Tip); err != nil {
			return row{}, nil, err
		}
	case OpDelete:
		out.kind = rowDelete
		if out.target, err = idOf("node id", r.ID); err != nil {
			return row{}, nil, err
		}
	}
	return out, nil, nil
}

// openIndex opens log.idx and reads what it claims, so that a rebuild has something to be compared with.
//
// An index file on disk is never an authority in this build - nothing can yet prove that it describes the log
// beside it - so what is read here is read to be checked and then written over. A file that cannot be opened
// is an error, and a header that cannot be trusted is not: an index is allowed to be empty, half-written, or
// written by a build that no longer exists, and what comes out of all of those is no rows and no complaint.
//
// The header is deliberately not kept. Its counters are derived from the rows a moment later, and keeping a
// number from a file that is about to be contradicted is how an index starts believing itself.
func (l *Log) openIndex() (*indexState, error) {
	f, err := os.OpenFile(l.idxPath(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	ix := newIndexState(f)
	if fi, err := f.Stat(); err == nil && fi.Size() >= indexHeaderSize {
		if _, rows, err := readIndex(f); err == nil {
			ix.rows = rows
		} else if l.logf != nil {
			l.logf("log.idx of %s not believed (%v); rebuilt from log.jsonl", l.dir, err)
		}
	}
	return ix, nil
}

// readIndex is the read side of log.idx: the header and the rows it claims, with every shape check that
// has to happen before a row is trusted. A failed check is an error, never a partial answer.
//
// It has no caller yet, and is the first thing S2-b will have one of: a rebuild is only worth avoiding if
// something can read the result.
func readIndex(f *os.File) (header, []row, error) {
	var hb [indexHeaderSize]byte
	if _, err := f.ReadAt(hb[:], 0); err != nil {
		return header{}, nil, fmt.Errorf("store: reading the index header: %w", err)
	}
	h, err := decodeHeader(hb[:])
	if err != nil {
		return header{}, nil, err
	}
	if h.rows < 0 {
		return header{}, nil, fmt.Errorf("%w: header claims %d rows", errIndexBadShape, h.rows)
	}
	want := h.rows * indexRowSize
	if fi, err := f.Stat(); err == nil {
		if have := fi.Size() - indexHeaderSize; have < want {
			return header{}, nil, fmt.Errorf("%w: header claims %d rows, the body holds %d", errIndexBadShape, h.rows, have/indexRowSize)
		}
	}
	body := make([]byte, want)
	if want > 0 {
		if _, err := f.ReadAt(body, indexHeaderSize); err != nil {
			return header{}, nil, fmt.Errorf("store: reading the index body: %w", err)
		}
	}
	return h, decodeRows(body), nil
}

// indexState is one session's log.idx: the rows written so far, the file they live in, and the running
// checksum of the log bytes they describe.
//
// The checksum is carried forward rather than recomputed, which means an indexState only ever describes a
// log it watched from the first byte. Nothing in this build resumes an index from its file, so that is
// not a limitation being papered over: resuming is the job of the reader that skips re-reading the log,
// and sha256's own MarshalBinary is what it will restore this from.
//
// Nothing here syncs the index file, in any mode. It is derived: the sync that has to be durable is the
// log's, and an index a crash left behind is one rebuild from correct - which is the same sentence the
// header's digest is there to make unnecessary one day, and not before.
type indexState struct {
	f    *os.File // the index file, open for reading and writing
	h    header
	rows []row
	ckh  hash.Hash // sha256 of exactly log.jsonl[0:h.cover]
}

// newIndexState starts an index for a log about to be read from its first byte, or for a log whose rows
// have just been read: either way, the file must be a log.idx, open for reading and writing, holding
// whatever a previous rebuild left in it.
func newIndexState(f *os.File) *indexState {
	return &indexState{f: f, ckh: sha256.New()}
}

// rebuild starts the same file over: the rows and every number of the header go back to nothing, because the
// caller has just derived all of them from the log again. The rows already in hand are the ones it is
// replacing, and they are what rowDiff was asked about before this was called.
//
// The digest is not touched, and that is the one thing a caller has to know: it is the digest of the log
// bytes the new rows describe, taken by the scan that produced them, and a rebuild that had its hasher reset
// would write a checksum of nothing. See Replay, which is the only caller.
func (ix *indexState) rebuild() {
	ix.h = header{}
	ix.rows = nil
}

// cover accounts for one line of the log: its bytes go into the digest and its length into what the rows
// are said to describe. line is exactly what belongs to header.cover - the record, its newline, and any
// line that was never a record at all - so that the checksum describes a byte range and not an intention.
// It is called apart from count because a rebuild hashes the log in one pass and adds its rows in another.
func (ix *indexState) cover(line []byte) {
	ix.h.cover += int64(len(line))
	ix.h.lines++
	_, _ = ix.ckh.Write(line)
}

// count accounts for one row in the header's numbers.
func (ix *indexState) count(r row) {
	ix.rows = append(ix.rows, r)
	ix.h.rows++
	switch r.kind {
	case rowNone:
		// why counted and not believed: rows go into the file one per record, with no hole to fill, so a row
		// of nothing is a row this build did not write - and the header's numbers are the least of what is
		// wrong in that case.
	case rowTip:
		ix.h.tips++
	case rowDelete:
		ix.h.deletes++
	case rowBad:
		// A skip is a record this build cannot act on: no entry where one was needed, or an op it has no
		// name for. It is a row, and folding it changes nothing.
		ix.h.skips++
	case rowPatchUser, rowPatchAssistant, rowPatchTool, rowPatchCompaction, rowPatchUnknown:
		ix.h.patches++
	default:
		ix.h.records++
	}
	if r.kind == rowTip || r.flags&tipSet != 0 {
		ix.h.tip = tipString(r.target)
	}
}

// tipString is a tip's row number as the log writes it, and empty for no tip at all - which a record can
// say, because a tip record that names nothing sends the session back to its newest entry. It is empty for
// a negative number too, and that is the one thing the header cannot tell: a number below zero is an id the
// log wrote by hand, and the string behind it lives in the rows' names, not here. Such a log has no index
// file either way (see errNotNumbered), so nothing reads this field of it.
func tipString(n int32) string {
	if n <= 0 {
		return ""
	}
	return skID(n)
}

// put accounts for one row and the log line it came from, and puts the row in the file.
//
// The row goes where its count says it goes, not at the append offset: this file is read and written
// alike, and what the last rebuild left of its body is a multiple of the row size.
func (ix *indexState) put(r row, line []byte) error {
	ix.count(r)
	ix.cover(line)
	var buf [indexRowSize]byte
	r.encode(buf[:])
	if _, err := ix.f.WriteAt(buf[:], indexHeaderSize+int64(len(ix.rows)-1)*indexRowSize); err != nil {
		return err
	}
	return ix.writeHeader()
}

// checksum is the log digest the header carries: the bytes the rows describe, and no others.
func (ix *indexState) checksum() [sha256.Size]byte {
	return shaSum(ix.ckh.Sum(nil))
}

// writeAll rewrites the whole index: rows first, then the header. In that order the file never claims
// more rows than it holds, so an interrupted rebuild leaves an index that is behind - which is a smaller
// index - rather than one that is wrong.
func (ix *indexState) writeAll() error {
	if err := ix.f.Truncate(indexHeaderSize); err != nil {
		return err
	}
	buf := make([]byte, len(ix.rows)*indexRowSize)
	for i, r := range ix.rows {
		r.encode(buf[i*indexRowSize:])
	}
	if len(buf) > 0 {
		if _, err := ix.f.WriteAt(buf, indexHeaderSize); err != nil {
			return err
		}
	}
	return ix.writeHeader()
}

// writeHeader writes the header over its whole block, with the running checksum and the CRC of the block
// itself in it. This is the one write that can leave the file claiming rows it does not hold, which is
// why put and writeAll put their rows down first.
func (ix *indexState) writeHeader() error {
	ix.h.ck = ix.checksum()
	var buf [indexHeaderSize]byte
	ix.h.encode(buf[:])
	_, err := ix.f.WriteAt(buf[:], 0)
	return err
}

// close leaves the file where it stands: valid or not, it is the log that is read again. Whoever closed it
// drops the indexState, so this does not pretend to make it unusable.
func (ix *indexState) close() {
	if ix != nil && ix.f != nil {
		_ = ix.f.Close()
	}
}

// skNode is one entry's place in the tree: what it was, where its latest record is, who its parent
// turned out to be once every later record had been read, and - for a fold that keeps content at all -
// the entry itself.
type skNode struct {
	id     string
	parent int32 // id number of the parent, 0 for a root
	kind   rowKind
	off    int64
	n      int
	ts     int64
	e      *ir.Entry // nil unless this fold was given the contents to keep
}

func (n *skNode) isCompaction() bool {
	k, _ := kindString(n.kind)
	return k == ir.KindCompaction
}

// SkEntry is one entry of a Skeleton: everything the log says about a record except its content.
type SkEntry struct {
	ID   string
	Kind string // "" when the latest record named a kind this build has no row for - the record wins
	Ts   time.Time
	Off  int64 // where the entry's latest record starts in log.jsonl …
	Len  int   // … and how many bytes it occupies there, its newline excluded
}

func (n *skNode) skEntry() SkEntry {
	e := SkEntry{ID: n.id, Off: n.off, Len: n.n}
	if k, ok := kindString(n.kind); ok {
		e.Kind = k
	}
	if n.ts != 0 {
		e.Ts = time.Unix(n.ts, 0).UTC()
	}
	return e
}

// folder holds the rules that fold records into a tree, and nothing else: read rows, get a tree.
//
// It is the only place those rules exist, because they are the log format. Replay folds rows plus the
// contents it read with them, and Skeleton folds the rows alone; both get the same tree, and a transcript
// that differed depending on which of the two was asked is the failure this prevents.
//
// Everything is addressed by id number - the log's id less its padding, or a negative number for an id the
// log wrote by hand - and 0 means "nothing": no parent, no tip. A row is addressed by its position, which
// is not the same numbering: a patch and a tip move consume a row and no id.
type folder struct {
	rows     []row
	contents []*ir.Entry
	names    map[int32]string // id number → the id the log wrote, only when an id was not a number

	nodes   map[int32]*skNode // id number → its place; a deleted entry is absent from it
	adopted []string
	tip     int32
}

// foldRows applies every record in order, exactly as Replay always did:
//
//   - an entry record puts the entry at the parent it names, replacing whatever stood there;
//   - a patch replaces the entry with what it carries, except that it never moves the node - a patch that
//     re-parented would silently re-parent everything below it;
//   - a tip record moves the tip, and an entry record that tipped its own entry moves it too;
//   - a delete removes the entry, re-parents everything that named it onto its own parent, and takes the
//     tip with it if the tip was the deleted entry.
//
// Then, and only then, the two rules that are not records: an off-path compaction whose parent is the tip
// is adopted onto it (§3.10 defence #2), and a log that never moved its tip sits at its newest entry.
//
// contents, if given, holds one entry per row - see scanLog - and a row that holds no entry is nil in it.
// names, if given, holds the id every number was written with, because it was not a number.
func foldRows(rows []row, contents []*ir.Entry, names map[int32]string) *folder {
	f := &folder{rows: rows, contents: contents, names: names, nodes: map[int32]*skNode{}}
	for i, r := range rows {
		switch {
		case r.kind == rowNone, r.kind == rowBad:
			// A row of nothing, or of a record this build cannot act on, changed nothing - which is the one
			// thing guaranteed of it, and what Replay did with the line.
		case r.kind == rowTip:
			f.tip = r.target
		case r.kind == rowDelete:
			f.remove(r.target)
		case isPatchRow(r.kind):
			f.patch(r.target, r, i)
		default:
			f.put(r.target, r, i)
		}
		if r.flags&tipSet != 0 {
			f.tip = r.target
		}
	}
	f.adopt()
	if f.tip == 0 {
		f.tip = f.newest()
	}
	return f
}

// name is an id number as the log wrote it.
func (f *folder) name(n int32) string { return idName(f.names, n) }

// idName is the whole rule for speaking of an id by number, shared by the fold that holds the tree and the
// skeleton that holds what it folded. Normally that is skID, and the point of keeping it a function is the
// log whose ids were written by hand: those entries still have to be spoken of by the names their records
// used, because that is the only name the rest of the program has ever had for them. A name is kept for a
// number the log never wrote an entry under too - a parent named before it existed, which is a number
// nothing puts anything at - because the entry below it has to keep pointing at that name.
func idName(names map[int32]string, n int32) string {
	if n == 0 {
		return ""
	}
	if s, ok := names[n]; ok {
		return s
	}
	if n < 0 {
		// why empty and not a guess: a negative number is one this build made up for an id it could not
		// number, and every one of them has a name in names - so reaching here means a fold or a skeleton
		// was handed numbers it was not built from, and inventing a name would be a name no transcript can
		// look up.
		return ""
	}
	return skID(n)
}

// parentString is a node's parent as the log writes one: the id on its own, or empty for a root. There is
// no separator in it, because ir.Path walks from an entry to its parent by looking the string up in the
// map, and has always done so.
func (f *folder) parentString(n *skNode) string { return f.name(n.parent) }

// content is the entry of the row at i, and nil for a fold that was given no contents at all.
func (f *folder) content(i int) *ir.Entry {
	if i < len(f.contents) {
		return f.contents[i]
	}
	return nil
}

func (f *folder) put(id int32, r row, i int) {
	f.nodes[id] = &skNode{id: f.name(id), parent: r.parent, kind: r.kind, off: r.off, n: int(r.n), ts: r.ts, e: f.content(i)}
}

func (f *folder) patch(id int32, r row, i int) {
	parent := r.parent
	if old, ok := f.nodes[id]; ok {
		// why inherited rather than taken from the record: patches never move a node, which is what lets a
		// tool's output be edited without moving the branch that grew below it.
		parent = old.parent
	}
	f.nodes[id] = &skNode{id: f.name(id), parent: parent, kind: r.kind, off: r.off, n: int(r.n), ts: r.ts, e: f.content(i)}
}

func (f *folder) remove(id int32) {
	del, ok := f.nodes[id]
	if !ok {
		// A delete of an entry that is not there changes nothing, not even the tip.
		return
	}
	// why before the walk: the walk re-parents what was below the deleted entry, and if the tip was the
	// deleted entry it goes to the deleted entry's parent - the one entry that cannot be reached from the
	// tip any more, and an entry that is about to be absent from f.nodes.
	if f.tip == id {
		f.tip = del.parent
	}
	delete(f.nodes, id)
	for _, e := range f.nodes {
		if e.parent == id {
			e.parent = del.parent
		}
	}
}

// adopt moves the tip onto a compaction entry that is off the path but whose parent is the tip, and is
// §3.10 defence #2 in full. It is part of the fold and not of Replay because it decides where the path
// is, and a skeleton that was tipped somewhere else would be a second truth about the session.
func (f *folder) adopt() {
	if f.tip == 0 {
		return
	}
	onPath := map[int32]bool{}
	for cur := f.tip; cur != 0; {
		if onPath[cur] {
			break // corrupt cycle: ir.Path stops the same way
		}
		onPath[cur] = true
		e, ok := f.nodes[cur]
		if !ok {
			break
		}
		cur = e.parent
	}
	for changed := true; changed; {
		changed = false
		var orphans []int32
		for id, e := range f.nodes {
			if e.isCompaction() && !onPath[id] && e.parent == f.tip {
				orphans = append(orphans, id)
			}
		}
		// why the orphans are sorted before any of them is adopted: two off-path compactions below the same
		// tip differ in more than which one is named first - the first ends up on the path, and the other
		// cannot follow it - so a session whose transcript depends on the order a map was walked is a session
		// that changes when it is reopened. Sorted ids, ascending, is what the reader this fold replaced
		// walked, and what every log.jsonl on disk was written under.
		sort.Slice(orphans, func(i, j int) bool { return f.name(orphans[i]) < f.name(orphans[j]) })
		for _, id := range orphans {
			e := f.nodes[id]
			// why the parent is looked at again here: adopting one orphan moves the tip, and an orphan below
			// the one that just moved is one that has to wait for the next round to be seen at all.
			if onPath[id] || e.parent != f.tip {
				continue
			}
			f.tip = id
			onPath[id] = true
			f.adopted = append(f.adopted, f.name(id))
			changed = true
		}
	}
}

// newest is where a log that never moved its tip sits: the greatest entry id, compared as a string because
// that is how the ids sort, and it is the id number of that entry that is handed back - because everything
// else in here is addressed by number.
func (f *folder) newest() int32 {
	var out int32
	for id, n := range f.nodes {
		if out == 0 || n.id > f.name(out) {
			out = id
		}
	}
	return out
}

// state folds the tree into the shape the whole program reads the log in. It belongs to a fold that was
// given its contents; a skeleton fold has the places of entries and no entries to hand back.
func (f *folder) state() *State {
	st := &State{Entries: make(map[string]*ir.Entry, len(f.nodes))}
	for _, n := range f.nodes {
		e := n.e
		if e == nil {
			// why an entry and not a skip: an entry whose body this fold does not carry is still an entry,
			// and an empty one with the right id is what Replay has always handed back.
			e = &ir.Entry{}
		}
		// why the id and the parent are taken from the fold and not trusted from the record: they are what
		// every later record and every other reader agreed they are, and a hand-edited log that disagreed
		// with itself has to be read the one way that the transcript is walked by.
		e.ID, e.ParentID = n.id, f.parentString(n)
		st.Entries[e.ID] = e
	}
	st.Tip = f.name(f.tip)
	st.Adopted = f.adopted
	return st
}

// skeleton is the same tree without the content: what S2-b reads a page from and S2-c counts a session
// with. A fold that was given content answers it too, so one fold serves both.
func (f *folder) skeleton() *Skeleton {
	sk := &Skeleton{rows: f.rows, nodes: f.nodes, names: f.names, tip: f.name(f.tip), adopted: f.adopted}
	if len(f.nodes) > 0 {
		sk.byID = make(map[string]int32, len(f.nodes))
		for n, node := range f.nodes {
			sk.byID[node.id] = n
		}
	}
	return sk
}

// Skeleton is a folded log index: every entry the log holds, where its record is, and what the log adds up
// to - with none of the content.
//
// It is a value, not a live handle: it holds the rows and nodes it was folded from, so it never changes
// under a reader, and a caller that wants the next record has to fold again. Folding is a pass over the
// rows and one map of live entries, which is what makes holding a skeleton for a session that is merely
// listed a decision and not a default.
type Skeleton struct {
	rows    []row
	nodes   map[int32]*skNode
	names   map[int32]string // the ids of a log that did not number them; nil for every other log
	byID    map[string]int32 // id → its node, so an entry is answerable by the name the log wrote
	tip     string
	adopted []string
}

// name is an id number as the log wrote it.
func (sk *Skeleton) name(n int32) string { return idName(sk.names, n) }

// Tip is the session's leaf: the entry the last tip record named, newest entry if the log never moved it.
func (sk *Skeleton) Tip() string { return sk.tip }

// Adopted lists the compaction entries that were off the path and got adopted onto it, in the order the
// fold adopted them - which is the order their ids sort, and what Replay reported.
func (sk *Skeleton) Adopted() []string { return sk.adopted }

// Path is every live entry, by id. It is the whole skeleton in a slice, and S2-b is the layer that learns
// to ask for less of it than that.
func (sk *Skeleton) Path() []SkEntry {
	out := make([]SkEntry, 0, len(sk.nodes))
	for _, n := range sk.nodes {
		out = append(out, n.skEntry())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Entry is one live entry by id, and whether the log still holds it. An id is looked up by the string the
// log wrote it with, which for every log this store writes is also its number.
func (sk *Skeleton) Entry(id string) (SkEntry, bool) {
	n, ok := sk.byID[id]
	if !ok {
		return SkEntry{}, false
	}
	return sk.nodes[n].skEntry(), true
}

// Parent is where an entry hangs, and "" is the root. A page of a transcript is drawn in path order, and
// the parent is the whole of what it needs to put a record back where it belongs without reading anything
// else. An id the log does not hold has no parent, and says so with "".
func (sk *Skeleton) Parent(id string) string {
	n, ok := sk.byID[id]
	if !ok {
		return ""
	}
	return sk.name(sk.nodes[n].parent)
}

// Live is every live entry's id, unsorted.
func (sk *Skeleton) Live() []string {
	out := make([]string, 0, len(sk.nodes))
	for _, n := range sk.nodes {
		out = append(out, n.id)
	}
	return out
}

// Records counts the rows that put an entry, which is not the same as the entries that are live: a patched
// entry is one entry and two rows, and a deleted entry is a row still. This is the number the sidebar is
// going to call the log, and Path and Live are the numbers about the tree.
func (sk *Skeleton) Records() int64 {
	return sk.count(rowUser, rowAssistant, rowTool, rowCompaction, rowEntryUnknown)
}

// Patches, Deletes, Tips and Skipped are the rest of the rows, and with Records they are a partition of
// them: see header. Skipped is every row this build cannot act on - a record with no entry where one was
// needed, or an op it has no name for - and not the blank and unreadable lines of the log, which never
// became rows at all (Lines says how many rows there are, and header.lines how many lines).
func (sk *Skeleton) Patches() int64 {
	return sk.count(rowPatchUser, rowPatchAssistant, rowPatchTool, rowPatchCompaction, rowPatchUnknown)
}
func (sk *Skeleton) Deletes() int64 { return sk.count(rowDelete) }
func (sk *Skeleton) Tips() int64    { return sk.count(rowTip) }
func (sk *Skeleton) Skipped() int64 { return sk.count(rowBad) }

// Lines is the rows the skeleton covers: one per record of log.jsonl, which is more than the entries that
// are live and fewer than the lines of the file. It is not the number the sidebar used to show as a
// session's size, and Records is not either: a session of three user messages and nine tool updates is
// twelve records, three entries, and twelve lines only until something patches one of them.
func (sk *Skeleton) Lines() int64 { return int64(len(sk.rows)) }

func (sk *Skeleton) count(kinds ...rowKind) int64 {
	var n int64
	for _, r := range sk.rows {
		for _, k := range kinds {
			if r.kind == k {
				n++
				break
			}
		}
	}
	return n
}
