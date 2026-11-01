package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"

	"bareloop/internal/ir"
)

// The tests in this file are the first ones ever written for the store, and they were written for the
// index: a derived file that nothing reads yet is a file whose only proof is that it agrees with the log.
// So most of them say the same thing three ways - the tree Replay returns, the rows the index holds, and
// the bytes of log.jsonl at the address a row gives - and one of them is a differential test against the
// fold the index replaced.

func openTestLog(t *testing.T) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, dir
}

// at is the timestamp the index can carry: a row stores seconds, which is all a tick bar and a session
// list can show anyway.
func at(i int) time.Time {
	return time.Date(2026, 11, 1, 9, 0, i, 0, time.UTC)
}

func put(t *testing.T, l *Log, kind, parent, content string, setTip bool, ts time.Time) *ir.Entry {
	t.Helper()
	e := &ir.Entry{Kind: kind, Role: kind, ParentID: parent, Content: content, Ts: ts}
	if kind == ir.KindCompaction {
		e.Role = "user"
	}
	if err := l.AppendEntry(e, setTip); err != nil {
		t.Fatalf("AppendEntry %s: %v", content, err)
	}
	return e
}

func replay(t *testing.T, l *Log) *State {
	t.Helper()
	st, err := l.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return st
}

// readBack reads the record a row points at, straight out of the log, and returns it. This is what S2-b is
// going to do for one entry on a page, and it is the whole reason the offsets are in the row.
func readBack(t *testing.T, dir string, sk SkEntry) *ir.Entry {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	buf := make([]byte, sk.Len)
	if _, err := f.ReadAt(buf, sk.Off); err != nil {
		t.Fatalf("ReadAt(%d,%d) for %s: %v", sk.Off, sk.Len, sk.ID, err)
	}
	var r Record
	if err := json.Unmarshal(buf, &r); err != nil {
		t.Fatalf("record at %d for %s is not the record: %v (%q)", sk.Off, sk.ID, err, buf)
	}
	if r.Entry == nil || r.Entry.ID != sk.ID {
		t.Fatalf("record at %d is about %v, not %s", sk.Off, r.Entry, sk.ID)
	}
	return r.Entry
}

func TestSkeletonPointsAtTheLatestRecordOfEveryEntry(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "hello", true, at(1))
	b := put(t, l, ir.KindAssistant, a.ID, "thinking", true, at(2))
	// A patch rewrites b in place: the row has to move to the new record, and the entry keeps its parent.
	patched := &ir.Entry{ID: b.ID, ParentID: a.ID, Kind: ir.KindAssistant, Role: "assistant", Content: "an answer", Ts: at(3)}
	if err := l.Patch(patched); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	c := put(t, l, ir.KindTool, b.ID, "ok", true, at(4))

	st := replay(t, l)
	sk := l.Skeleton()
	if sk == nil {
		t.Fatal("no skeleton for a log that was read and then written to")
	}
	if sk.Tip() != st.Tip {
		t.Errorf("skeleton tip %q, replay tip %q", sk.Tip(), st.Tip)
	}
	if got, want := len(sk.Path()), len(st.Entries); got != want {
		t.Errorf("%d entries in the skeleton, %d in the tree", got, want)
	}
	for _, s := range sk.Path() {
		e := st.Entries[s.ID]
		if e == nil {
			t.Fatalf("skeleton holds %s, the tree does not", s.ID)
		}
		if e.ParentID != "" && e.ParentID != s.ID && sk.Parent(s.ID) != e.ParentID {
			t.Errorf("%s: skeleton parent %q, tree parent %q", s.ID, sk.Parent(s.ID), e.ParentID)
		}
		if !s.Ts.Equal(e.Ts.Truncate(time.Second)) {
			t.Errorf("%s: skeleton ts %s, entry ts %s", s.ID, s.Ts, e.Ts)
		}
		back := readBack(t, dir, s)
		if back.Content != e.Content {
			t.Errorf("%s: record at %d holds %q, the tree holds %q", s.ID, s.Off, back.Content, e.Content)
		}
	}
	// The patched entry is two records and one entry, which is the difference the sidebar has to get right:
	// three records put an entry, and one more row is the patch that rewrote one of them.
	if got := sk.Records(); got != 3 {
		t.Errorf("Records: %d, want 3 (a patch is not a record that puts an entry)", got)
	}
	if got := sk.Patches(); got != 1 {
		t.Errorf("Patches: %d, want 1", got)
	}
	if got, want := sk.Lines(), int64(4); got != want {
		t.Errorf("Lines: %d, want %d", got, want)
	}
	if got, want := sk.Deletes()+sk.Skipped(), int64(0); got != want {
		t.Errorf("Deletes+Skipped: %d, want %d", got, want)
	}
	// And the tool result really is below the assistant entry it answers.
	if got := sk.Parent(c.ID); got != b.ID {
		t.Errorf("%s parent %q, want %q", c.ID, got, b.ID)
	}
}

func TestBlankAndBrokenLinesAreCoveredAndNotRowed(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	b := put(t, l, ir.KindAssistant, a.ID, "two", true, at(2))
	replay(t, l)

	// A blank line and two lines that are not JSON at all - a truncated object and a bare word - written the
	// way a hand-edit or a bad flush would.
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log for writing: %v", err)
	}
	if _, err := f.WriteString("\n{ \"op\": \nanother\n"); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	f.Close()
	c := put(t, l, ir.KindTool, b.ID, "three", true, at(3))

	st := replay(t, l)
	if len(st.Entries) != 3 {
		t.Fatalf("%d entries after junk lines, want the three real ones: %v", len(st.Entries), st.Tip)
	}
	sk := l.Skeleton()
	// Three lines of the log are not records, so they have no rows - and are covered all the same, which is
	// what the digest of the whole log has to add up to. header.lines is where they are counted, and it counts
	// every line the file holds: three records, a blank line, a truncated object, a bare word.
	if got, want := sk.Lines(), int64(3); got != want {
		t.Errorf("rows: %d, want %d, because a row describes a record and neither of those lines is one", got, want)
	}
	if got, want := l.ix.h.lines, int64(6); got != want {
		t.Errorf("lines: %d, want %d (three records and three lines that are not records)", got, want)
	}
	if got, want := sk.Deletes()+sk.Skipped(), int64(0); got != want {
		t.Errorf("Deletes+Skipped: %d, want %d", got, want)
	}
	if sk.Tip() != c.ID {
		t.Errorf("tip %q, want %q", sk.Tip(), c.ID)
	}
	if err := l.IndexError(); err != nil {
		t.Errorf("a junk line cost the index: %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := sha256.Sum256(logBytes[:l.ix.h.cover]); l.ix.h.ck != got {
		t.Error("the junk lines are not inside the digest the rows are pinned by")
	}
}

// TestRecordsThisBuildCannotActOnAreSkippedAndKept is about the rows that hold nothing: a record this build
// has no op for is a row it cannot act on, and one row of that kind no longer ends the read - which is what
// a log written by a newer build deserves, since the part this build can read is still the whole of its
// beginning.
func TestRecordsThisBuildCannotActOnAreSkippedAndKept(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	replay(t, l)

	lines := []Record{
		{Op: "wobble", Seq: 2, Entry: &ir.Entry{ID: "0000002", Kind: ir.KindUser, Content: "from a later build", Ts: at(2)}},
		{Op: OpEntry, Seq: 3, Entry: &ir.Entry{ID: "0000003", Kind: ir.KindAssistant, ParentID: a.ID, Content: "two", Ts: at(3)}, Tip: "0000003"},
		{Op: OpEntry, Seq: 4, Tip: "0000003"}, // an entry record with no entry in it
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log for writing: %v", err)
	}
	for _, r := range lines {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	f.Close()

	st := replay(t, l)
	if len(st.Entries) != 2 {
		t.Fatalf("%d entries, want the two this build can hold: %v", len(st.Entries), st.Tip)
	}
	if st.Tip != "0000003" {
		t.Errorf("tip %q, want the entry the log tipped last", st.Tip)
	}
	sk := l.Skeleton()
	if got, want := sk.Skipped(), int64(2); got != want {
		t.Errorf("Skipped: %d, want %d", got, want)
	}
	if got, want := sk.Records(), int64(2); got != want {
		t.Errorf("Records: %d, want %d", got, want)
	}
	if got, want := sk.Lines(), int64(4); got != want {
		t.Errorf("rows: %d, want %d, because four records were read", got, want)
	}
	if err := l.IndexError(); err != nil {
		t.Errorf("a record this build cannot read cost the index: %v", err)
	}
}

func TestTornTailIsNotCoveredAndStopsTheRead(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "whole", true, at(1))
	put(t, l, ir.KindAssistant, a.ID, "half", true, at(2))
	// The second record is cut in half, the way a crash leaves it.
	p := filepath.Join(dir, "log.jsonl")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if err := os.WriteFile(p, b[:len(b)-4], 0o644); err != nil {
		t.Fatalf("truncate log: %v", err)
	}
	l.size = int64(len(b) - 4)

	st := replay(t, l)
	if len(st.Entries) != 1 {
		t.Fatalf("%d entries, want the one whole record", len(st.Entries))
	}
	sk := l.Skeleton()
	if got := sk.Lines(); got != 1 {
		t.Errorf("Lines: %d, want 1", got)
	}
	// what the rows describe, and no more: the torn bytes are behind the next append and in front of the
	// rows, and a digest that reached into them could never be taken again.
	want := int64(bytes.Index(b, []byte("\n")) + 1) // the whole of the first record, and none of the second
	if got := l.ix.h.cover; got != want {
		t.Errorf("cover %d, want %d: the first record and none of the torn one", got, want)
	}
	if got := l.ix.h.cover; got >= l.size {
		t.Errorf("a torn log is covered to %d of its %d bytes", got, l.size)
	}
	if got := l.ix.h.lines; got != 1 {
		t.Errorf("lines: %d, want 1, because the torn tail is not a line the rows describe", got)
	}
}

func TestIndexAgreesWithTheDigestOfTheLogItDescribes(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	put(t, l, ir.KindAssistant, a.ID, "two", false, at(2))
	if err := l.SetTip(a.ID); err != nil {
		t.Fatalf("SetTip: %v", err)
	}
	replay(t, l)

	logBytes, err := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	f, err := os.Open(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer f.Close()
	h, rows, err := readIndex(f)
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	if int64(len(rows)) != h.rows {
		t.Fatalf("header claims %d rows, the body holds %d", h.rows, len(rows))
	}
	if h.rows != 3 {
		t.Errorf("%d rows, want 3 (two entries and a tip move)", h.rows)
	}
	if h.tip != a.ID {
		t.Errorf("header tip %q, want %q", h.tip, a.ID)
	}
	if got := sha256.Sum256(logBytes[:h.cover]); h.ck != got {
		t.Errorf("header digest does not describe the first %d bytes of the log", h.cover)
	}
	if got := sha256.Sum256(logBytes[:h.cover-1]); h.ck == got {
		t.Error("the digest is not of the bytes it claims: dropping one byte still matches")
	}
	if h.cover != int64(len(logBytes)) {
		t.Errorf("cover %d, want the whole log at %d", h.cover, len(logBytes))
	}
	if got := h.records + h.patches + h.deletes + h.tips + h.skips; got != h.rows {
		t.Errorf("counters add up to %d, not the %d rows", got, h.rows)
	}
	if h.tips != 1 || h.records != 2 {
		t.Errorf("%d tips and %d records, want 1 and 2", h.tips, h.records)
	}
	if h.lines != h.rows {
		t.Errorf("%d lines for %d rows, and every line of this log is a record", h.lines, h.rows)
	}
}

func TestDecodeHeaderRefusesWhatItCannotTrust(t *testing.T) {
	var h header
	h.rows, h.lines, h.cover, h.tip = 2, 3, 100, "0000002"
	h.records, h.patches, h.deletes, h.tips, h.skips = 1, 0, 0, 1, 0
	var buf [indexHeaderSize]byte
	h.encode(buf[:])

	got, err := decodeHeader(buf[:])
	if err != nil {
		t.Fatalf("a header this build wrote: %v", err)
	}
	if got != h {
		t.Errorf("header did not come back: %+v, wrote %+v", got, h)
	}

	// One byte of the log's length, and the block no longer believes itself.
	var one [indexHeaderSize]byte
	copy(one[:], buf[:])
	one[44] ^= 0xff
	if _, err := decodeHeader(one[:]); err == nil {
		t.Error("a flipped byte in the covered block passed the CRC")
	}

	var magic [indexHeaderSize]byte
	copy(magic[:], buf[:])
	copy(magic[0:4], "BLOX")
	if _, err := decodeHeader(magic[:]); !errors.Is(err, errIndexBadShape) {
		t.Errorf("wrong magic: %v, want errIndexBadShape", err)
	}

	var ver [indexHeaderSize]byte
	copy(ver[:], buf[:])
	ver[4], ver[5], ver[6], ver[7] = 9, 0, 0, 0
	if _, err := decodeHeader(ver[:]); !errors.Is(err, errIndexVersion) {
		t.Errorf("version 9: %v, want errIndexVersion", err)
	}

	// Another instant to count the rows from, which would make every timestamp in the body another calendar.
	// The block has to be written over again to reach that check at all: the epoch is inside what the CRC
	// covers, and a header that does not believe itself is refused before it is asked what it counts from.
	var epoch [indexHeaderSize]byte
	copy(epoch[:], buf[:])
	binary.LittleEndian.PutUint64(epoch[140:148], uint64(indexEpoch+1))
	binary.LittleEndian.PutUint32(epoch[8:12], crc32.ChecksumIEEE(epoch[12:]))
	if _, err := decodeHeader(epoch[:]); !errors.Is(err, errIndexVersion) {
		t.Errorf("rows counting from another instant: %v, want errIndexVersion", err)
	}
}

// TestSkeletonAfterADelete is about the one row that does not move: a delete leaves its row and its record
// exactly where they were, and the tree is where the change happens.
func TestSkeletonAfterADelete(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	b := put(t, l, ir.KindAssistant, a.ID, "two", true, at(2))
	c := put(t, l, ir.KindTool, b.ID, "three", true, at(3))
	replay(t, l)

	if err := l.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	st := replay(t, l)
	if _, ok := st.Entries[b.ID]; ok {
		t.Fatal("the deleted entry is still in the tree")
	}
	if got := st.Entries[c.ID].ParentID; got != a.ID {
		t.Errorf("%s was re-parented to %q, want %q", c.ID, got, a.ID)
	}
	sk := l.Skeleton()
	if _, ok := sk.Entry(b.ID); ok {
		t.Error("the skeleton still holds the deleted entry")
	}
	if got := sk.Deletes(); got != 1 {
		t.Errorf("Deletes: %d, want 1", got)
	}
	if got := sk.Records(); got != 3 {
		t.Errorf("Records: %d, want 3, because a delete is not an undo: the log still holds three records", got)
	}
	// The record of the surviving tool result is still where its row says, and still its whole entry.
	s, ok := sk.Entry(c.ID)
	if !ok {
		t.Fatal("no row for the surviving entry")
	}
	if back := readBack(t, dir, s); back.ToolCallID != "" || back.Content != "three" {
		t.Errorf("record at %d came back as %+v", s.Off, back)
	}
}

// TestLogWorksWithoutAnIndexAtAll is the trade: an index that cannot be written must not cost the log a
// record, and must not stop a session from opening. The first read is where the index is opened, and so
// where it is given up on: before anything has been read there is nothing yet to have failed.
func TestLogWorksWithoutAnIndexAtAll(t *testing.T) {
	dir := t.TempDir()
	// log.idx is a directory, which is the cheapest way to be unable to open a file.
	if err := os.Mkdir(filepath.Join(dir, indexFileName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var said []string
	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	l.SetLogf(func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) })

	replay(t, l)
	if got := l.Skeleton(); got != nil {
		t.Errorf("a log with no index handed back %v", got)
	}
	if err := l.IndexError(); err == nil {
		t.Error("no index and no reason given")
	}
	if len(said) != 1 || !strings.Contains(said[0], indexFileName) {
		t.Errorf("%d lines said why: %v", len(said), said)
	}

	// The trade itself: the writes keep coming, none of them is indexed, and none of them is lost.
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	b := put(t, l, ir.KindAssistant, a.ID, "two", true, at(2))
	if l.Skeleton() != nil {
		t.Error("the index came back on its own")
	}
	st := replay(t, l)
	if len(st.Entries) != 2 || st.Tip != b.ID {
		t.Fatalf("the log itself is wrong without an index: %d entries, tip %q", len(st.Entries), st.Tip)
	}
}

// TestHandWrittenIDsAreReadAndNotIndexed is the corner the index refuses: a log edited by hand, with ids
// this store would never hand out. The transcript still opens, because the log is the truth, and there is
// no index, because a row names an entry by its number.
func TestHandWrittenIDsAreReadAndNotIndexed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log.jsonl")
	lines := []Record{
		{Op: OpEntry, Seq: 1, Entry: &ir.Entry{ID: "root", Kind: ir.KindUser, Role: "user", Content: "one", Ts: at(1)}, Tip: "root"},
		{Op: OpEntry, Seq: 2, Entry: &ir.Entry{ID: "leaf", Kind: ir.KindAssistant, Role: "assistant", ParentID: "root", Content: "two", Ts: at(2)}, Tip: "leaf"},
		{Op: OpDelete, Seq: 3, ID: "gone"},
	}
	var buf []byte
	for _, r := range lines {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	var said []string
	l.SetLogf(func(format string, args ...any) { said = append(said, fmt.Sprintf(format, args...)) })

	st := replay(t, l)
	if len(st.Entries) != 2 {
		t.Fatalf("%d entries, want 2", len(st.Entries))
	}
	if st.Tip != "leaf" {
		t.Errorf("tip %q, want leaf", st.Tip)
	}
	if got := st.Entries["leaf"].ParentID; got != "root" {
		t.Errorf("leaf's parent is %q, want the name the log wrote", got)
	}
	if l.Skeleton() != nil {
		t.Error("a log with names for ids has rows, and rows number them")
	}
	if err := l.IndexError(); err == nil {
		t.Error("no index and no reason given")
	}
	if len(said) != 1 {
		t.Errorf("%d lines said why: %v", len(said), said)
	}
}

func TestAppendAfterReadKeepsRowsInOrder(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	replay(t, l)

	b := put(t, l, ir.KindAssistant, a.ID, "two", true, at(2))
	c := put(t, l, ir.KindTool, b.ID, "three", true, at(3))
	appended := append([]row{}, l.ix.rows...) // the rows the appends wrote, one per record
	if got, want := len(appended), 3; got != want {
		t.Fatalf("%d rows for three records", got)
	}

	// The rows a second read derives have to be the rows the appends wrote, in the same order: that is what
	// rowDiff is for, and what a disagreement between the two means is one line in the daemon log.
	if why := rowDiff(appended, l.ix.rows); why != "" {
		t.Fatalf("the rows in hand are not what was just written: %s", why)
	}
	fresh := replay(t, l)
	if fresh.Tip != c.ID {
		t.Fatalf("tip %q, want %q", fresh.Tip, c.ID)
	}
	if why := rowDiff(appended, l.ix.rows); why != "" {
		t.Errorf("the rows the appends wrote are not the rows the log says: %s", why)
	}
	h := l.ix.h
	if h.rows != int64(len(l.ix.rows)) || h.records != 3 {
		t.Errorf("header says %d rows, %d records, for %d rows", h.rows, h.records, len(l.ix.rows))
	}
	if h.tip != c.ID {
		t.Errorf("header tip %q, want %q", h.tip, c.ID)
	}
	if h.lines != h.rows {
		t.Errorf("%d lines for %d rows, and every line of this log is a record", h.lines, h.rows)
	}
	// And the header on disk says the same as the one in hand.
	f, err := os.Open(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer f.Close()
	disk, rows, err := readIndex(f)
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	if disk != h {
		t.Errorf("header on disk %+v, in hand %+v", disk, h)
	}
	if why := rowDiff(l.ix.rows, rows); why != "" {
		t.Errorf("index file and in-hand rows disagree: %s", why)
	}
}

// TestRecordThatTipsAnotherEntryEndsTheRead is the one record shape a row cannot hold: an entry record that
// tips an entry other than the one it writes. No row can say which of the two is the tip, so rowIndex
// refuses the record, and a record this build cannot row is where the read stops - which is what the reader
// did with every record it could not make sense of before the index existed.
func TestRecordThatTipsAnotherEntryEndsTheRead(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	replay(t, l)

	lines := []Record{
		{Op: OpEntry, Seq: 2, Entry: &ir.Entry{ID: "0000002", Kind: ir.KindAssistant, ParentID: a.ID, Content: "two", Ts: at(2)}, Tip: a.ID},
		{Op: OpEntry, Seq: 3, Entry: &ir.Entry{ID: "0000003", Kind: ir.KindTool, ParentID: "0000002", Content: "three", Ts: at(3)}, Tip: "0000003"},
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log for writing: %v", err)
	}
	for _, r := range lines {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	f.Close()

	st := replay(t, l)
	if len(st.Entries) != 1 {
		t.Fatalf("%d entries, want the one before the record that could not be rowed", len(st.Entries))
	}
	if err := l.IndexError(); err == nil {
		t.Error("the rows stopped short of the log and no reason was given")
	}
	if l.Skeleton() != nil {
		t.Error("rows that stop short of the log were written out anyway")
	}
	b, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("%d bytes of index left behind by a read that stopped short", len(b))
	}
}

// oracleReplay is the reader the fold replaced: one Record at a time into a map, with the two rules that are
// not records applied at the end. It is written out here rather than left in a revision somewhere because
// the day the two disagree is the day every log.jsonl on disk has two answers about its own transcript, and
// a test that carries its own copy of the old rules is the only way that is noticed.
func oracleReplay(t *testing.T, recs []Record) *State {
	t.Helper()
	st := &State{Entries: map[string]*ir.Entry{}}
	for _, r := range recs {
		switch r.Op {
		case OpEntry:
			cp := *r.Entry
			st.Entries[cp.ID] = &cp
			if r.Tip != "" {
				st.Tip = r.Tip
			}
		case OpPatch:
			cp := *r.Entry
			if old, ok := st.Entries[cp.ID]; ok {
				cp.ParentID = old.ParentID // patches never move a node
			}
			st.Entries[cp.ID] = &cp
		case OpTip:
			st.Tip = r.Tip // which may be nothing at all
		case OpDelete:
			del, ok := st.Entries[r.ID]
			if !ok {
				break // a delete of what is not there changes nothing, not even the tip
			}
			if st.Tip == r.ID {
				st.Tip = del.ParentID
			}
			delete(st.Entries, r.ID)
			for _, e := range st.Entries {
				if e.ParentID == r.ID {
					e.ParentID = del.ParentID
				}
			}
		default:
			t.Fatalf("the oracle was handed op %q, which the reader it copies ends the log at", r.Op)
		}
		if r.Seq > st.Seq {
			st.Seq = r.Seq
		}
	}
	st.Adopted = oracleAdopt(t, st)
	if st.Tip == "" {
		st.Tip = oracleNewest(st)
	}
	return st
}

// oracleAdopt is Replay's orphan rule: an off-path compaction whose parent is the tip is adopted onto it,
// over and over until nothing new moves.
func oracleAdopt(t *testing.T, st *State) []string {
	t.Helper()
	// The same guard the fold carries, and the one rule this test cannot witness from the other side: "an
	// off-path compaction whose parent is the tip" needs a tip, and without it the only entries whose parent
	// matches are the roots. A session without a tip sits at its newest entry instead.
	if st.Tip == "" {
		return nil
	}
	onPath := map[string]bool{}
	for cur := st.Tip; cur != ""; {
		if onPath[cur] {
			break
		}
		onPath[cur] = true
		e, ok := st.Entries[cur]
		if !ok {
			break
		}
		cur = e.ParentID
	}
	var out []string
	for changed := true; changed; {
		changed = false
		for _, id := range oracleLive(st) {
			e := st.Entries[id]
			if !e.IsCompaction() || onPath[id] || e.ParentID != st.Tip {
				continue
			}
			st.Tip = id
			onPath[id] = true
			out = append(out, id)
			changed = true
		}
	}
	return out
}

// oracleNewest is where a log that never moved its tip sits, by string comparison because that is how ids
// sort.
func oracleNewest(st *State) string {
	var out string
	for id := range st.Entries {
		if id > out {
			out = id
		}
	}
	return out
}

// oracleLive is every live id, sorted, which is what makes the generator below reproducible.
func oracleLive(st *State) []string {
	out := make([]string, 0, len(st.Entries))
	for id := range st.Entries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// TestFoldAgreesWithTheReaderItReplaced puts a hundred and fifty thousand dice at the question that matters
// most in this package: the index moved the rules of the log format out of the reader and into a fold over
// rows, so a transcript must not depend on which of the two was asked. It generates records the way the
// session does - puts that replace, patches that name a parent they must not follow, tip moves to nothing,
// deletes of what is not there - runs them through the log and through the reader that is no longer there,
// and asks the skeleton a third time.
func TestFoldAgreesWithTheReaderItReplaced(t *testing.T) {
	l, _ := openTestLog(t)
	rng := rand.New(rand.NewSource(7))
	kinds := []string{ir.KindUser, ir.KindAssistant, ir.KindTool, ir.KindCompaction}
	pick := func(alive []string, none string) string {
		if len(alive) == 0 {
			return none
		}
		return alive[rng.Intn(len(alive))]
	}

	var recs []Record
	for seq := int64(1); seq <= 250; seq++ {
		alive := oracleLive(oracleReplay(t, recs))
		e := &ir.Entry{
			ID:      skID(int32(1 + rng.Intn(9))),
			Kind:    kinds[rng.Intn(len(kinds))],
			Content: fmt.Sprintf("round %d", seq),
			Ts:      at(int(seq % 60)),
		}
		n := rng.Intn(100)
		switch {
		case n < 40:
			// A put below something that is alive, three times in four, and a root otherwise.
			if rng.Intn(4) > 0 {
				e.ParentID = pick(alive, "")
			}
			rec := Record{Op: OpEntry, Seq: seq, Entry: e}
			if rng.Intn(2) == 0 {
				rec.Tip = e.ID // the only tip one record may name alongside its entry
			}
			recs = append(recs, rec)
		case n < 55:
			// A patch naming a parent it must refuse to follow, so the inheritance is what is tested.
			e.ParentID = pick(alive, skID(1))
			recs = append(recs, Record{Op: OpPatch, Seq: seq, Entry: e})
		case n < 65:
			// A tip move, and sometimes a move back to nothing, which is the log saying "newest then".
			recs = append(recs, Record{Op: OpTip, Seq: seq, Tip: pick(alive, "")})
		case n < 80:
			// A delete, of something alive or of something the log never wrote.
			recs = append(recs, Record{Op: OpDelete, Seq: seq, ID: pick(alive, skID(999))})
		default:
			// A tipped put, which is what every real append of this store's looks like.
			e.ParentID = pick(alive, "")
			recs = append(recs, Record{Op: OpEntry, Seq: seq, Entry: e, Tip: e.ID})
		}
	}
	for i := range recs {
		if err := l.write(&recs[i]); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	want, got := oracleReplay(t, recs), replay(t, l)
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("%d entries, want %d", len(got.Entries), len(want.Entries))
	}
	for id, we := range want.Entries {
		ge := got.Entries[id]
		if ge == nil {
			t.Errorf("%s: the reader holds it and the fold does not", id)
			continue
		}
		if ge.ParentID != we.ParentID {
			t.Errorf("%s: parent %q, want %q", id, ge.ParentID, we.ParentID)
		}
		if ge.Kind != we.Kind {
			t.Errorf("%s: kind %q, want %q", id, ge.Kind, we.Kind)
		}
		if ge.Content != we.Content {
			t.Errorf("%s: content %q, want %q", id, ge.Content, we.Content)
		}
	}
	for id := range got.Entries {
		if _, ok := want.Entries[id]; !ok {
			t.Errorf("%s: the fold holds an entry the reader never made", id)
		}
	}
	if got.Tip != want.Tip {
		t.Errorf("tip %q, want %q", got.Tip, want.Tip)
	}
	if got.Seq != want.Seq {
		t.Errorf("seq %d, want %d", got.Seq, want.Seq)
	}
	// Which orphans were adopted is the fact this compares; the order they were named in is settled by the
	// fold sorting them, and comparing sorted is what keeps this test about the tree and not about a slice.
	gotAdopted, wantAdopted := append([]string{}, got.Adopted...), append([]string{}, want.Adopted...)
	sort.Strings(gotAdopted)
	sort.Strings(wantAdopted)
	if strings.Join(gotAdopted, ",") != strings.Join(wantAdopted, ",") {
		t.Errorf("adopted %v, want %v", gotAdopted, wantAdopted)
	}

	// And the skeleton a third time, since it is the same fold with the contents left out.
	sk := l.Skeleton()
	if sk == nil {
		t.Fatal("no skeleton for a log this build read")
	}
	if sk.Tip() != want.Tip {
		t.Errorf("skeleton tip %q, want %q", sk.Tip(), want.Tip)
	}
	if len(sk.Path()) != len(want.Entries) {
		t.Errorf("%d entries in the skeleton, want %d", len(sk.Path()), len(want.Entries))
	}
	for id, we := range want.Entries {
		s, ok := sk.Entry(id)
		if !ok {
			t.Errorf("%s: the skeleton does not hold it", id)
			continue
		}
		if s.Kind != we.Kind {
			t.Errorf("%s: skeleton kind %q, want %q", id, s.Kind, we.Kind)
		}
		if got := sk.Parent(id); got != we.ParentID {
			t.Errorf("%s: skeleton parent %q, want %q", id, got, we.ParentID)
		}
	}
	if strings.Join(sortedStrings(sk.Live()), ",") != strings.Join(oracleLive(want), ",") {
		t.Errorf("skeleton holds %v, want %v", sk.Live(), oracleLive(want))
	}
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// TestARowIsTheWidthTheIndexSeeksBy is why the fields of a row are ordered by alignment: every seek in this
// package is indexHeaderSize+n*indexRowSize, and the same rows are held in RAM one per record of every session
// that is open. Those two facts only agree while the struct is exactly the row the file holds - and the round
// trip through encode and decodeRows is what says the order of the fields in Go is not the order on disk,
// which is the other half of what this pins.
//
// A row of this version has no byte that is not a field, so unlike the version before it this test has no
// reserved bytes to assert are zero: what keeps a future field out of a row is indexVersion, and the width
// asserted here is the number that a new version has to be weighed against.
func TestARowIsTheWidthTheIndexSeeksBy(t *testing.T) {
	// why unsafe and not reflect: this is the compiler's own arithmetic on the struct, and a constant.
	if got := unsafe.Sizeof(row{}); got != indexRowSize {
		t.Errorf("a row is %d bytes in memory and %d on disk, so a []row and the body of log.idx are two shapes",
			got, indexRowSize)
	}
	// Every field at a value that would notice a swapped offset - including the negative number a hand-written
	// id gets, a length whose top byte is set, an address past the four billion one half cannot name, and a
	// time before the epoch the row counts from.
	r := row{
		kind:   rowPatchCompaction,
		flags:  tipSet,
		target: -3,
		parent: 1234567,
		n:      0xffffff00,
		ts:     -(1 << 30),
	}
	r.setOffset(1<<40 + 7)
	var buf [indexRowSize]byte
	r.encode(buf[:])
	back := decodeRows(buf[:])
	if len(back) != 1 {
		t.Fatalf("%d rows decoded from one row's bytes", len(back))
	}
	if back[0] != r {
		t.Errorf("a row that went through the file came back as %+v, not %+v", back[0], r)
	}

	// And two rows in one body start where the other ended, which is all a seek claims to know. The width is
	// not a power of two any more, but 4096 and 24 share a factor of 8, so every row still begins on an
	// address that is a multiple of 8 - which nothing in this package depends on, and nothing should have to
	// check.
	pair := make([]byte, 2*indexRowSize)
	r.encode(pair)
	second := row{kind: rowTip, target: 7}
	second.setOffset(1)
	second.encode(pair[indexRowSize:])
	if two := decodeRows(pair); len(two) != 2 || two[0] != r || two[1] != second {
		t.Errorf("two rows in a body came back as %+v", two)
	}
}

// TestALowHalfIsNotTheWholeAddress is about the one mistake a field split in two makes without complaint:
// swapped halves still round-trip every offset a log is likely to reach, because under four gigabytes the two
// halves differ only in a zero. So the offsets here are not.
func TestALowHalfIsNotTheWholeAddress(t *testing.T) {
	for _, off := range []int64{0, 1, 1 << 31, 1 << 32, 1<<32 + 1, 1 << 47, maxRowOffset} {
		var r row
		r.setOffset(off)
		if r.offHi != uint16(off>>32) || r.off != uint32(off) {
			t.Errorf("offset %d was split into %d and %d, which is not low then high", off, r.off, r.offHi)
		}
		if got := r.offset(); got != off {
			t.Errorf("offset %d came back from the two halves as %d", off, got)
		}
		var buf [indexRowSize]byte
		r.encode(buf[:])
		if got := decodeRows(buf[:]); got[0].offset() != off {
			t.Errorf("offset %d came back from the file as %d", off, got[0].offset())
		}
	}
	// And one past what the 48 bits hold is refused where the row is made, rather than written as a row that
	// points at the start of the log again.
	if _, _, err := rowIndex(Record{Op: OpTip, Tip: "0000001"}, maxRowOffset+1, 10, numberedIDs); err == nil {
		t.Error("an offset past the address one row holds was accepted")
	}
}

// TestTheEpochIsTheInstantTheFormatNames holds the number the format is written with to the instant it is
// meant to be. It is a number and not a time.Date call so that nothing about a row depends on a zone, and a
// number is exactly what no reader can check by looking at it.
func TestTheEpochIsTheInstantTheFormatNames(t *testing.T) {
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(); indexEpoch != want {
		t.Errorf("indexEpoch is %d, which is %s, and 2026-01-01T00:00:00Z is %d", indexEpoch,
			time.Unix(indexEpoch, 0).UTC(), want)
	}
	// Both ends of the sixty-eight years, because the comment on indexEpoch says what four bytes reach and a
	// comment is not a check. At either end the time still has to come back; a second past either is flagged
	// rather than wrapped, which is the difference between losing a tick and inventing one.
	for _, end := range []int64{math.MinInt32, math.MaxInt32} {
		ts := time.Unix(indexEpoch+end, 0).UTC()
		stored, flags := rowTime(ts)
		if flags != 0 {
			t.Errorf("%s is at the edge of what a row holds and came back flagged unknown", ts)
			continue
		}
		if got := (row{ts: stored}).absTs(); got != ts.Unix() {
			t.Errorf("%s went through a row and came back %d", ts, got)
		}
	}
	for _, end := range []int64{math.MinInt32 - 1, math.MaxInt32 + 1} {
		ts := time.Unix(indexEpoch+end, 0).UTC()
		if _, flags := rowTime(ts); flags != tsUnknown {
			t.Errorf("%s is past what a row's four bytes hold and came back with flags %d, want tsUnknown", ts, flags)
		}
	}
}

// TestATimestampARowCannotHoldIsMissingAndNotWrong is the trade the four-byte timestamp makes, and the
// regression the row used to have written for: a record whose own timestamp is missing, or older or newer
// than the sixty-eight years a row reaches, loses that timestamp and nothing else. The row, the tree, the
// index file and every other entry's time are kept - which is only worth spelling out because reading back
// whatever Unix says about a zero time put the first second of 1970 in a row as though it had been meant.
func TestATimestampARowCannotHoldIsMissingAndNotWrong(t *testing.T) {
	l, _ := openTestLog(t)
	replay(t, l) // the first read, so the records below are indexed as they arrive and not only by a scan

	// Written as records and not through AppendEntry on purpose: that is the one path that stamps a time onto
	// an entry that had none, and a missing timestamp only reaches a row from a log written without it.
	recs := []Record{
		{Op: OpEntry, Seq: 1, Entry: &ir.Entry{ID: "0000001", Kind: ir.KindUser, Role: "user", Content: "no timestamp", Ts: time.Time{}}, Tip: "0000001"},
		{Op: OpEntry, Seq: 2, Entry: &ir.Entry{ID: "0000002", Kind: ir.KindAssistant, Role: "assistant", ParentID: "0000001", Content: "from 1901", Ts: time.Date(1901, 6, 1, 0, 0, 0, 0, time.UTC)}, Tip: "0000002"},
		{Op: OpEntry, Seq: 3, Entry: &ir.Entry{ID: "0000003", Kind: ir.KindTool, Role: "tool", ParentID: "0000002", Content: "in range", Ts: at(9)}, Tip: "0000003"},
	}
	for i := range recs {
		if err := l.write(&recs[i]); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := l.IndexError(); err != nil {
		t.Fatalf("a timestamp outside the row cost the index: %v", err)
	}

	// The row written at append time and the row the log describes on the way back have to be the same row:
	// rowTime is called from both, and two answers about one record is what rowDiff exists to notice.
	appended := append([]row{}, l.ix.rows...)
	replay(t, l)
	if why := rowDiff(appended, l.ix.rows); why != "" {
		t.Errorf("the row appended is not the row the log says: %s", why)
	}
	sk := l.Skeleton()
	if sk == nil {
		t.Fatal("the index was given up on over a timestamp")
	}
	if got := sk.Records(); got != 3 {
		t.Errorf("Records: %d, want 3, because a timestamp the format cannot hold is not an entry the log lost", got)
	}
	for _, id := range []string{"0000001", "0000002"} {
		s, ok := sk.Entry(id)
		if !ok {
			t.Fatalf("%s is not in the skeleton", id)
		}
		if !s.Ts.IsZero() {
			t.Errorf("%s came back timed at %s; a row that cannot hold a time says so by having none, which is not a date", id, s.Ts)
		}
	}
	s, ok := sk.Entry("0000003")
	if !ok {
		t.Fatal("the entry inside the range is not in the skeleton")
	}
	if !s.Ts.Equal(at(9)) {
		t.Errorf("0000003: %s, want %s", s.Ts, at(9))
	}
}
