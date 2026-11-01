package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bareloop/internal/ir"
)

// These are the tests of the read side: log.idx as something a caller asks a question, rather than
// something the log writes and hopes nobody checks. The questions are the session list's, because the
// session list is all this build reads an index for.

// TestWhatTheIndexSaysIsWhatTheLogSays is the test the session list rests on: a number shown beside a
// session nobody has opened has to be the number that session would be run with. Every way those can come
// apart is in the log this makes - a patch adding a record and no entry, a branch living off the path, a tip
// that never moved onto it.
func TestWhatTheIndexSaysIsWhatTheLogSays(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	b := put(t, l, ir.KindAssistant, a.ID, "two", true, at(2))
	put(t, l, ir.KindTool, a.ID, "on another branch", false, at(3))
	patched := &ir.Entry{ID: b.ID, ParentID: a.ID, Kind: ir.KindAssistant, Role: "assistant", Content: "revised", Ts: at(4)}
	if err := l.Patch(patched); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	st := replay(t, l)

	warm, err := l.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if warm.Entries != len(st.Entries) {
		t.Errorf("Entries: %d, want the %d the tree leaves standing", warm.Entries, len(st.Entries))
	}
	onPath := ir.Path(st.Entries, st.Tip)
	if warm.Depth != len(onPath) {
		t.Errorf("Depth: %d, want the %d entries on the path ending at %s", warm.Depth, len(onPath), st.Tip)
	}
	if warm.Depth >= warm.Entries {
		t.Errorf("Depth %d is not less than Entries %d, and a branch off the path is the only thing that makes those numbers differ", warm.Depth, warm.Entries)
	}
	if warm.Tip != st.Tip {
		t.Errorf("Tip %q, replay tip %q", warm.Tip, st.Tip)
	}
	// Four records: two entries on the path, one entry on a branch, and a patch that rewrote one of them
	// without adding an entry to the count above.
	if warm.Rows != 4 {
		t.Errorf("Rows: %d, want 4", warm.Rows)
	}
	if warm.Skipped != 0 {
		t.Errorf("Skipped: %d, want 0", warm.Skipped)
	}
	fi, err := os.Stat(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if warm.LogBytes != fi.Size() {
		t.Errorf("LogBytes: %d, want the %d bytes the file holds", warm.LogBytes, fi.Size())
	}

	// The file answers what the memory answered, once the handle that wrote it is gone. That is the whole
	// claim of a session listed without being opened, and it is the claim this test is for.
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cold, err := Inspect(dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if cold != warm {
		t.Errorf("the file says %+v and reading the log said %+v", cold, warm)
	}
}

// TestARecordAppendedAfterAReadIsAnsweredTwice guards the ordering a listing depends on: a row goes into the
// index file in the same breath as the record goes into the log. Were it written at close, or at the next
// read, then every listing after a new message would be a record behind for a session still open.
func TestARecordAppendedAfterAReadIsAnsweredTwice(t *testing.T) {
	l, dir := openTestLog(t)
	first := put(t, l, ir.KindUser, "", "one", true, at(1))
	replay(t, l)
	before, err := l.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if before.Rows != 1 || before.Depth != 1 || before.Entries != 1 {
		t.Fatalf("a session of one entry reads as %+v", before)
	}

	put(t, l, ir.KindAssistant, first.ID, "two", true, at(2))
	after, err := l.Read()
	if err != nil {
		t.Fatalf("Read after an append: %v", err)
	}
	if after.Rows != 2 || after.Depth != 2 || after.Entries != 2 {
		t.Errorf("the new record is missing from %+v", after)
	}
	if after.LogBytes <= before.LogBytes {
		t.Errorf("LogBytes: %d after %d, and a record was appended between them", after.LogBytes, before.LogBytes)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cold, err := Inspect(dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if cold != after {
		t.Errorf("the file says %+v and the open log said %+v, so a row reached the file later than the record it describes", cold, after)
	}
}

// TestASessionWithNoIndexHasNoNumbers is the case the sidebar shows as a dash. A log this build has never
// read has no rows to count, and answering zero would be a claim about an empty session that no file in the
// directory made.
func TestASessionWithNoIndexHasNoNumbers(t *testing.T) {
	dir := t.TempDir()
	line := `{"op":"entry","seq":1,"entry":{"id":"0000001","kind":"user","role":"user","content":"by hand"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "log.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if _, err := Inspect(dir); err == nil {
		t.Fatal("Inspect answered with numbers for a session that has no log.idx")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Inspect: %v, want the index file to be what is missing", err)
	}
}

// TestRowsAboutBytesTheLogDoesNotHoldAreRefused is the one check the read side cannot take from the index
// file alone, and the only way a foreign log.idx can be caught while passing every check inside itself: it
// describes a log longer than the one beside it.
func TestRowsAboutBytesTheLogDoesNotHoldAreRefused(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(1))
	put(t, l, ir.KindAssistant, a.ID, "two", true, at(2))
	replay(t, l)

	p := filepath.Join(dir, "log.jsonl")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if err := os.WriteFile(p, b[:len(b)-30], 0o644); err != nil {
		t.Fatalf("shorten log: %v", err)
	}
	if _, err := Inspect(dir); err == nil {
		t.Fatal("Inspect believed rows describing bytes the log does not hold")
	} else if !errors.Is(err, errIndexBadShape) {
		t.Errorf("Inspect: %v, want the refusal to be the one about shape", err)
	}
}

// TestAParentChainThatCyclesIsWalkedOnceAround: a log.jsonl whose ids are numbers still gets an index, and
// nothing about such a file stops it naming two entries the parents of each other. The session list walks
// that chain for every session it lists, so the walk has a bound, and this is the test that says so - a
// hanging Depth is this test timing out rather than failing.
func TestAParentChainThatCyclesIsWalkedOnceAround(t *testing.T) {
	l, _ := openTestLog(t)
	// Explicit ids are how a log written by hand says which entry is which, and this is what it can say.
	for _, e := range []*ir.Entry{
		{ID: "0000001", Kind: ir.KindUser, Role: "user", ParentID: "0000002", Content: "a", Ts: at(1)},
		{ID: "0000002", Kind: ir.KindAssistant, Role: "assistant", ParentID: "0000001", Content: "b", Ts: at(2)},
	} {
		if err := l.AppendEntry(e, true); err != nil {
			t.Fatalf("AppendEntry %s: %v", e.ID, err)
		}
	}
	st := replay(t, l)
	sk := l.Skeleton()
	if sk == nil {
		t.Fatal("a log with a cycle in it cost the index")
	}
	if got, want := sk.Depth(), sk.Size(); got != want {
		t.Errorf("Depth: %d, want it bounded at the %d entries the tree holds", got, want)
	}
	// The same bound ir.Path uses, so the two walks of one tree cannot disagree about how deep it is.
	if got, want := len(ir.Path(st.Entries, st.Tip)), sk.Size(); got != want {
		t.Errorf("ir.Path walks %d entries, the skeleton bounds itself at %d", got, want)
	}
}

// TestNewestTimeAsksTheEntryRowsAndNobodyElse is the kind filter of newestTs written out. A row spends four
// of its 24 bytes on a timestamp, and they are filled from the entry the record wrote or patched; a tip
// move, a delete, and a record this build cannot act on have no entry to take a time from, so their rows
// leave ts zero *and* tsUnknown clear - absTs hands them back the epoch. newestTs therefore has to be told
// which rows to ask, and the answer is not "the newest row".
func TestNewestTimeAsksTheEntryRowsAndNobodyElse(t *testing.T) {
	// why 100 instead of a date: a row stores seconds after indexEpoch, and this is that arithmetic.
	want := time.Unix(indexEpoch+100, 0).UTC()
	if got := newestTs([]row{{kind: rowUser, ts: 100}, {kind: rowTip}, {kind: rowDelete}, {kind: rowBad}}); !got.Equal(want) {
		t.Errorf("newestTs: %s, want the user row's %s - the tip, delete and bad rows beside it have no timestamp to give", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// A patch row is asked too: it carries the timestamp of the entry it restated, which is the entry's
	// own and not the moment the patch was written (Log.Patch writes the caller's entry unchanged).
	if got := newestTs([]row{{kind: rowPatchTool, ts: 5}, {kind: rowCompaction, ts: 7}}); !got.Equal(time.Unix(indexEpoch+7, 0).UTC()) {
		t.Errorf("newestTs: %s, want the compaction row's 7 over the patch's 5", got.Format(time.RFC3339))
	}
	// A log of nothing but tip moves and deletes has no timestamp to tell anybody. Handing back the epoch
	// would be the lie this test exists for: it is a date, and the log never claimed one.
	if got := newestTs([]row{{kind: rowNone}, {kind: rowTip}, {kind: rowDelete}, {kind: rowBad}}); !got.IsZero() {
		t.Errorf("newestTs: %s, want the zero time for a log that holds no entry", got.Format(time.RFC3339))
	}
	// tsUnknown is a row saying out loud that it has no time, which is a different claim from a row of
	// zeroes - and both have to end at the zero time, by different routes.
	if got := newestTs([]row{{kind: rowAssistant, flags: tsUnknown}}); !got.IsZero() {
		t.Errorf("newestTs: %s, want the zero time for the one row there is and it carries no timestamp", got.Format(time.RFC3339))
	}
}

// TestANewestEntrySurvivesTheTipMoving is the session list's sorting question asked of a real log: what
// gets printed beside a session is when its newest message happened, and a tip moving back underneath it -
// a rewind, a time travel - is not the conversation having something new to say.
func TestANewestEntrySurvivesTheTipMoving(t *testing.T) {
	l, dir := openTestLog(t)
	a := put(t, l, ir.KindUser, "", "one", true, at(5))
	put(t, l, ir.KindAssistant, a.ID, "two", true, at(9))
	// The newest record in the log is now this tip move, and it is not an entry.
	if err := l.SetTip(a.ID); err != nil {
		t.Fatalf("SetTip: %v", err)
	}
	replay(t, l)
	warm, err := l.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !warm.LastTs.Equal(at(9)) {
		t.Errorf("LastTs: %s, want the assistant entry's %s and not the tip move that came after it", warm.LastTs.Format(time.RFC3339), at(9).Format(time.RFC3339))
	}
	// The file says what the memory said, which is what a session listed without being opened is betting
	// on; TestWhatTheIndexSaysIsWhatTheLogSays is the one that holds the two structs together as a whole.
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cold, err := Inspect(dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !cold.LastTs.Equal(warm.LastTs) {
		t.Errorf("the file says LastTs %s and reading the log said %s", cold.LastTs.Format(time.RFC3339), warm.LastTs.Format(time.RFC3339))
	}
}
