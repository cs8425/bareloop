package store

import (
	"errors"
	"testing"
	"time"

	"bareloop/internal/ir"
)

// The readers the resident spine (r101 B0) exists for: ReplayMeta answers the tree's
// shape without opening a payload, Contents inflates only the named rows, Skeleton and
// ToolUses carry one row's worth instead of a session's, and after Close every one of
// them answers ErrClosed rather than a nil map. These are the fences the split paid for.

func mustAppend(t *testing.T, l *Log, e *ir.Entry) {
	t.Helper()
	if err := l.AppendEntry(e, true); err != nil {
		t.Fatalf("append %s: %v", e.Content, err)
	}
}

// TestReplayMetaReadsTheShapeNotTheSaying is the load-bearing claim of the spine: the
// scan that rebuilds the tree never parses a payload — burning every payload but the
// compaction rows' changes what the entries say and nothing of what they are. The one
// payload the scan does open is a summary's own record, because the boundary lives inside
// it; corrupt one of those and the scan has to notice rather than walk past a hole.
func TestReplayMetaReadsTheShapeNotTheSaying(t *testing.T) {
	l := openTest(t)
	root := user("root")
	mustAppend(t, l, root)
	bid := root.ID
	cmp := &ir.Entry{Kind: ir.KindCompaction, Role: "user", Content: "the summary", Ts: time.Now().UTC(),
		Compaction: &ir.CompactionMeta{BoundaryID: &bid, Replaced: 3, SummarySize: 42,
			Summary: []ir.SummaryAttempt{{Attempt: 1, Usage: &ir.Usage{PromptTokens: 10, HasPrompt: true}}}}}
	cmp.ParentID = root.ID
	mustAppend(t, l, cmp)
	tail := user("tail")
	tail.ParentID = cmp.ID
	mustAppend(t, l, tail)

	// Valid JSON, and JSON nothing the spine reads could mistake for an entry: if the scan
	// ever parses a payload again, it fails here rather than in front of a user.
	res, err := l.db.Exec("UPDATE entry SET payload = '{\"corrupt\":true}' WHERE kind <> ?", ir.KindCompaction)
	if err != nil {
		t.Fatalf("burning payloads: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("burned %d payloads, want the 2 non-compaction rows", n)
	}

	m, err := l.ReplayMeta()
	if err != nil {
		t.Fatalf("ReplayMeta with every non-summary payload burned: %v", err)
	}
	if len(m.Entries) != 3 {
		t.Fatalf("spine holds %d entries, want 3", len(m.Entries))
	}
	if m.Tip != tail.ID || m.Seq != 3 {
		t.Errorf("tip %q seq %d, want %q and 3", m.Tip, m.Seq, tail.ID)
	}
	if got := m.Entries[cmp.ID]; got.ParentID != root.ID || got.Kind != ir.KindCompaction {
		t.Errorf("the summary row misread: %+v", got)
	}
	if got := m.Entries[cmp.ID].Compaction; got == nil || got.BoundaryID == nil || *got.BoundaryID != root.ID || got.SummarySize != 42 {
		t.Errorf("the summary record did not survive: %+v", got)
	}
	if m.Entries[root.ID].Ts.IsZero() || m.Entries[tail.ID].Ts.IsZero() {
		t.Error("a spine row lost its timestamp — the turn list would draw 1970")
	}

	// The summary row is the rule's admitted exception: blank its payload and the scan
	// must fail, not walk a path whose boundary quietly vanished.
	if _, err := l.db.Exec("UPDATE entry SET payload = 'GONE' WHERE kind = ?", ir.KindCompaction); err != nil {
		t.Fatalf("burning the summary payload: %v", err)
	}
	if _, err := l.ReplayMeta(); err == nil {
		t.Fatal("ReplayMeta accepted an unparsable compaction record — the scan that never reads payloads would be a lie")
	}
}

// TestContentsAnswersByPresence is the lookup a window and a page share: deleted and
// unknown ids are absent from the map rather than errors, and the expand knob alone
// decides whether an image travels as its reference or as its bytes.
func TestContentsAnswersByPresence(t *testing.T) {
	l := openTest(t)
	img := []byte("png-bytes")
	pic := imgEntry("look", img)
	mustAppend(t, l, pic)
	gone := user("was here, then deleted")
	mustAppend(t, l, gone)
	if err := l.Delete(gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := l.Contents([]string{pic.ID, gone.ID, "0000099"}, false)
	if err != nil {
		t.Fatalf("Contents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("map holds %d entries, want the 1 that exists: %v", len(got), got)
	}
	if e := got[pic.ID]; e == nil || len(e.Parts) == 0 || e.Parts[0].Data != BlobPrefix+shaOfBytes(img) {
		t.Errorf("expand=false kept no reference: %+v", e)
	}

	full, err := l.Contents([]string{pic.ID}, true)
	if err != nil {
		t.Fatalf("Contents(expand): %v", err)
	}
	if e := full[pic.ID]; e == nil || len(e.Parts) == 0 || e.Parts[0].Data == "" ||
		e.Parts[0].Data == BlobPrefix+shaOfBytes(img) {
		t.Errorf("expand=true sent no bytes: %+v", e)
	}

	empty, err := l.Contents([]string{"0000099", "0000098"}, false)
	if err != nil || len(empty) != 0 {
		t.Errorf("all-ghost lookup = %v, %v; want an empty map and no error", empty, err)
	}
}

// TestSkeletonRowsTheLivePathOnly: the ruler row list walks the tip's chain and nothing
// else — a branch nobody stands on loses its row — and each row carries one entry's
// numbers rather than the session's payloads.
func TestSkeletonRowsTheLivePathOnly(t *testing.T) {
	l := openTest(t)
	u1 := user("one")
	mustAppend(t, l, u1)
	a1 := &ir.Entry{Kind: ir.KindAssistant, Role: "assistant", Content: "answer", Ts: time.Now().UTC(),
		Usage: &ir.Usage{PromptTokens: 120, CompletionTokens: 5, HasPrompt: true, HasCompletion: true}}
	a1.ParentID = u1.ID
	mustAppend(t, l, a1)
	off := user("a branch nobody stands on")
	off.ParentID = u1.ID
	if err := l.AppendEntry(off, false); err != nil { // not the tip: the path never visits it
		t.Fatalf("append branch: %v", err)
	}
	bid := u1.ID
	cmp := &ir.Entry{Kind: ir.KindCompaction, Role: "user", Content: "sum", Ts: time.Now().UTC(),
		Compaction: &ir.CompactionMeta{BoundaryID: &bid,
			Summary: []ir.SummaryAttempt{{Attempt: 1}, {Attempt: 2, Failed: "cap refused"}}}}
	cmp.ParentID = a1.ID
	mustAppend(t, l, cmp)

	rows, err := l.Skeleton()
	if err != nil {
		t.Fatalf("Skeleton: %v", err)
	}
	want := []string{u1.ID, a1.ID, cmp.ID}
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, id := range want {
		if rows[i].ID != id {
			t.Errorf("row %d is %s, want %s", i, rows[i].ID, id)
		}
	}
	if rows[1].Usage == nil || rows[1].Usage.PromptTokens != 120 || rows[1].Usage.CompletionTokens != 5 {
		t.Errorf("the assistant row lost its usage: %+v", rows[1].Usage)
	}
	if rows[0].Usage != nil {
		t.Errorf("a user row grew a usage: %+v", rows[0].Usage)
	}
	if len(rows[2].Summary) != 2 {
		t.Errorf("the summary row carries %d attempts, want 2 (refused ones included)", len(rows[2].Summary))
	}
}

// TestToolUsesCountsTheNamedRows: one row per id, tools counted by name and stamped with
// their newest use, everything that is not a tool (and everything the spine still names
// after a delete) simply not in the answer.
func TestToolUsesCountsTheNamedRows(t *testing.T) {
	l := openTest(t)
	t1 := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	tool := func(name string, ts time.Time) *ir.Entry {
		return &ir.Entry{Kind: ir.KindTool, Role: "tool", Tool: name, Content: "out", Ts: ts}
	}
	first := tool("exec", t1)
	mustAppend(t, l, first)
	later := tool("exec", t1.Add(time.Hour))
	mustAppend(t, l, later)
	reader := tool("read", t1)
	mustAppend(t, l, reader)
	quiet := user("no tool here")
	mustAppend(t, l, quiet)

	uses, last, err := l.ToolUses([]string{first.ID, later.ID, reader.ID, quiet.ID, "0000099"})
	if err != nil {
		t.Fatalf("ToolUses: %v", err)
	}
	if len(uses) != 2 || uses["exec"] != 2 || uses["read"] != 1 {
		t.Errorf("uses = %v, want exec 2 and read 1", uses)
	}
	if last["exec"] != later.Ts.Format(time.RFC3339) {
		t.Errorf("last exec %q, want the later use %q", last["exec"], later.Ts.Format(time.RFC3339))
	}
	if _, ok := last["read"]; !ok {
		t.Error("read lost its last use")
	}
}

// TestClosedReadersAnswerErrClosed: after Close the spine readers refuse rather than
// answering from a closed handle — a nil map that means "nothing exists" is exactly what
// must never be a lie about a store that has everything.
func TestClosedReadersAnswerErrClosed(t *testing.T) {
	l := openTest(t)
	e := user("one")
	mustAppend(t, l, e)
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := l.ReplayMeta(); !errors.Is(err, ErrClosed) {
		t.Errorf("ReplayMeta = %v, want ErrClosed", err)
	}
	if _, err := l.Contents([]string{e.ID}, false); !errors.Is(err, ErrClosed) {
		t.Errorf("Contents = %v, want ErrClosed", err)
	}
	if _, err := l.Skeleton(); !errors.Is(err, ErrClosed) {
		t.Errorf("Skeleton = %v, want ErrClosed", err)
	}
	if _, _, err := l.ToolUses([]string{e.ID}); !errors.Is(err, ErrClosed) {
		t.Errorf("ToolUses = %v, want ErrClosed", err)
	}
}
