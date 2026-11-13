package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"

	"bareloop/internal/ir"
)

// The behaviour suite for the SQLite store: what the agent relies on the log to have
// been — ids, tip, atomic append, patch history, delete re-parenting, blob accounting.

func openAt(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := Open(dir, false)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	return l
}

func openTest(t *testing.T) *Log {
	t.Helper()
	return openAt(t, t.TempDir())
}

func user(text string) *ir.Entry {
	return &ir.Entry{Kind: ir.KindUser, Role: "user", Content: text, Ts: time.Now().UTC()}
}

func imgEntry(text string, img []byte) *ir.Entry {
	return &ir.Entry{
		Kind: ir.KindUser, Role: "user", Content: text, Ts: time.Now().UTC(),
		Parts: []ir.Part{
			{Type: ir.PartImage, MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(img), Name: "p.png"},
			{Type: ir.PartText, Text: text},
		},
	}
}

func shaOfBytes(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// TestImportStreamSettlesAtTheEnd is the living transaction's whole contract: rows
// arrive one at a time, the two whole-file verdicts (the tip among the rows, every
// parent a row that exists) answer at Finish over the rows as written, and every
// failure — a ghost tip, a hole in the tree, an explicit Abort — leaves the store
// exactly as it was found. A duplicate id fails on the spot, on the key's own
// sentence.
func TestImportStreamSettlesAtTheEnd(t *testing.T) {
	l := openTest(t)
	imported := func() int {
		st, err := l.Replay()
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		return len(st.Entries)
	}

	tx, err := l.ImportStream()
	if err != nil {
		t.Fatalf("ImportStream: %v", err)
	}
	if err := tx.Entry(&ir.Entry{ID: "i1", Kind: ir.KindUser, Role: "user", Content: "a"}); err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if err := tx.Entry(&ir.Entry{ID: "i1", Kind: ir.KindUser, Role: "user"}); err == nil {
		t.Error("a duplicate id was accepted, want the key's refusal")
	}
	if err := tx.Abort(); err != nil {
		t.Fatalf("Abort after the refusal: %v", err)
	}
	if got := imported(); got != 0 {
		t.Fatalf("the refused import left %d rows, want 0", got)
	}

	tx, err = l.ImportStream()
	if err != nil {
		t.Fatalf("ImportStream: %v", err)
	}
	if err := tx.Entry(&ir.Entry{ID: "i1", Kind: ir.KindUser, Role: "user"}); err != nil {
		t.Fatalf("Entry: %v", err)
	}
	if err := tx.Finish("ghost"); err == nil || !strings.Contains(err.Error(), "not among the entries") {
		t.Fatalf("Finish over a ghost tip = %v, want the tip refusal", err)
	}
	if got := imported(); got != 0 {
		t.Fatalf("the ghost-tipped import left %d rows, want 0", got)
	}

	tx, err = l.ImportStream()
	if err != nil {
		t.Fatalf("ImportStream: %v", err)
	}
	tx.Entry(&ir.Entry{ID: "i1", Kind: ir.KindUser, Role: "user"})
	if err := tx.Entry(&ir.Entry{ID: "i2", ParentID: "ghost", Kind: ir.KindUser, Role: "user"}); err != nil {
		t.Fatalf("Entry hole: %v", err)
	}
	if err := tx.Finish("i2"); err == nil || !strings.Contains(err.Error(), "never carries") {
		t.Fatalf("Finish over a hole = %v, want the parent refusal", err)
	}
	if got := imported(); got != 0 {
		t.Fatalf("the holed import left %d rows, want 0 (all rows or none)", got)
	}

	img := []byte("png-bytes")
	tx, err = l.ImportStream()
	if err != nil {
		t.Fatalf("ImportStream: %v", err)
	}
	imgE := imgEntry("look", img)
	imgE.ID = "9"
	if err := tx.Entry(imgE); err != nil {
		t.Fatalf("Entry image: %v", err)
	}
	if err := tx.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := imported(); got != 0 {
		t.Fatalf("the aborted import left %d rows, want 0", got)
	}

	// The committed shape: no tip named means the last row stands, images arrive
	// under their sha, and seq moved past the imported numeric ids — the next fresh
	// id is exactly one past the highest one imported. (Non-numeric ids keep seq
	// where it is, the rule ImportAll has always had: only a number can move a
	// counter.)
	tx, err = l.ImportStream()
	if err != nil {
		t.Fatalf("ImportStream: %v", err)
	}
	for _, e := range []*ir.Entry{
		imgE,
		{ID: "10", ParentID: "9", Kind: ir.KindUser, Role: "user", Content: "next"},
	} {
		if err := tx.Entry(e); err != nil {
			t.Fatalf("Entry: %v", err)
		}
	}
	if err := tx.Finish(""); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	st, err := l.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(st.Entries) != 2 || st.Tip != "10" {
		t.Fatalf("imported %d rows tip %q, want 2 and 10 (the last row stands)", len(st.Entries), st.Tip)
	}
	got := st.Entries["9"].Parts[0].Data
	if want := base64.StdEncoding.EncodeToString(img); got != want {
		t.Error("the imported image is not the bytes the file carried")
	}
	if nid := l.NextID(); nid != "0000011" {
		t.Fatalf("NextID %q, want 0000011 (seq moved one past the highest imported id)", nid)
	}
}

// replayAll reads the tree back the way a session opening would.
func replayAll(t *testing.T, l *Log) *State {
	t.Helper()
	st, err := l.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return st
}

func TestAppendIdsAndSeq(t *testing.T) {
	l := openTest(t)
	e1, e2, e3 := user("one"), user("two"), user("three")
	for _, e := range []*ir.Entry{e1, e2, e3} {
		if err := l.AppendEntry(e, true); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
	}
	if e1.ID != "0000001" || e2.ID != "0000002" || e3.ID != "0000003" {
		t.Errorf("ids %s %s %s, want 0000001 0000002 0000003", e1.ID, e2.ID, e3.ID)
	}
	st := replayAll(t, l)
	if len(st.Entries) != 3 {
		t.Fatalf("entries %d, want 3", len(st.Entries))
	}
	if st.Tip != e3.ID {
		t.Errorf("tip %s, want %s", st.Tip, e3.ID)
	}
	if st.Seq != 3 {
		t.Errorf("seq %d, want 3", st.Seq)
	}
	// An imported entry that names its own id keeps the counter monotonic past it.
	e4 := user("imported")
	e4.ID = "0000005"
	if err := l.AppendEntry(e4, true); err != nil {
		t.Fatalf("AppendEntry explicit id: %v", err)
	}
	e5 := user("after import")
	if err := l.AppendEntry(e5, true); err != nil {
		t.Fatalf("AppendEntry after import: %v", err)
	}
	if e5.ID != "0000006" {
		t.Errorf("id after an explicit 0000005 is %s, want 0000006", e5.ID)
	}
	// NextID hands out an id for a caller that must name its own entry before storing it.
	got := l.NextID()
	if got != "0000007" {
		t.Errorf("NextID %s, want 0000007", got)
	}
	e6 := user("preassigned")
	e6.ID = got
	if err := l.AppendEntry(e6, true); err != nil {
		t.Fatalf("AppendEntry preassigned: %v", err)
	}
	if e6.ID != "0000007" {
		t.Errorf("preassigned id %s, want 0000007", e6.ID)
	}
	e7 := user("after")
	if err := l.AppendEntry(e7, true); err != nil {
		t.Fatalf("AppendEntry after: %v", err)
	}
	if e7.ID != "0000008" {
		t.Errorf("id after 0000007 is %s, want 0000008", e7.ID)
	}
}

func TestChainAndTipMoves(t *testing.T) {
	l := openTest(t)
	root := user("root")
	if err := l.AppendEntry(root, true); err != nil {
		t.Fatalf("AppendEntry root: %v", err)
	}
	mid := user("mid")
	mid.ParentID = root.ID
	if err := l.AppendEntry(mid, true); err != nil {
		t.Fatalf("AppendEntry mid: %v", err)
	}
	leaf := user("leaf")
	leaf.ParentID = mid.ID
	if err := l.AppendEntry(leaf, true); err != nil {
		t.Fatalf("AppendEntry leaf: %v", err)
	}
	st := replayAll(t, l)
	if got := len(ir.Path(st.Entries, st.Tip)); got != 3 {
		t.Errorf("path length %d, want 3", got)
	}
	if err := l.SetTip(mid.ID); err != nil {
		t.Fatalf("SetTip: %v", err)
	}
	if st = replayAll(t, l); st.Tip != mid.ID {
		t.Errorf("tip after SetTip %s, want %s", st.Tip, mid.ID)
	}
	if err := l.SetTip(""); err != nil { // "no tip" stands the session on its newest entry
		t.Fatalf("SetTip empty: %v", err)
	}
	if st = replayAll(t, l); st.Tip != leaf.ID {
		t.Errorf("tip after empty SetTip %s, want the newest entry %s", st.Tip, leaf.ID)
	}
}

func TestPatchKeepsRevisions(t *testing.T) {
	l := openTest(t)
	e := user("v1")
	if err := l.AppendEntry(e, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	for _, v := range []string{"v2", "v3"} {
		cp := *e
		cp.Content = v
		if err := l.Patch(&cp); err != nil {
			t.Fatalf("Patch %s: %v", v, err)
		}
	}
	if st := replayAll(t, l); st.Entries[e.ID].Content != "v3" {
		t.Errorf("live content %q, want v3", st.Entries[e.ID].Content)
	}
	rows, err := l.db.Query("SELECT seq, payload FROM entry_rev WHERE entry_id = ? ORDER BY seq", e.ID)
	if err != nil {
		t.Fatalf("rev query: %v", err)
	}
	var versions []string
	var lastSeq int64 = -1
	for rows.Next() {
		var seq int64
		var payload string
		if err := rows.Scan(&seq, &payload); err != nil {
			t.Fatalf("rev scan: %v", err)
		}
		if seq <= lastSeq {
			t.Fatalf("rev seq %d did not increase past %d", seq, lastSeq)
		}
		lastSeq = seq
		var got ir.Entry
		// PayloadText because a big revision is stored deflated like any other payload —
		// the archive is a byte stream, not a promise of readable text.
		if err := json.Unmarshal([]byte(PayloadText(payload)), &got); err != nil {
			t.Fatalf("rev payload: %v", err)
		}
		versions = append(versions, got.Content)
	}
	rows.Close()
	if strings.Join(versions, ",") != "v1,v2" {
		t.Errorf("revisions %v, want [v1 v2]", versions)
	}
	// Patching an unknown id is a no-op, not an error (the log used to take the line silently).
	ghost := &ir.Entry{ID: "9999999", Kind: ir.KindUser, Role: "user"}
	if err := l.Patch(ghost); err != nil {
		t.Errorf("Patch of an unknown id: %v, want nil", err)
	}
}

func TestImagesDedupPinAndGc(t *testing.T) {
	l := openTest(t)
	imgA, imgB := []byte("png-bytes-A"), []byte("png-bytes-B")
	bA := base64.StdEncoding.EncodeToString(imgA)

	e1 := imgEntry("first", imgA)
	e2 := imgEntry("second", imgA)
	e3 := imgEntry("third", imgB)
	for _, e := range []*ir.Entry{e1, e2, e3} {
		if err := l.AppendEntry(e, true); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
	}
	shaOf := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	var n int
	var rc int
	l.db.QueryRow("SELECT COUNT(*) FROM blob").Scan(&n)
	if n != 2 {
		t.Fatalf("blob rows %d, want 2 (one per distinct image)", n)
	}
	l.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", shaOf(imgA)).Scan(&rc)
	if rc != 2 {
		t.Errorf("blob A refcount %d, want 2", rc)
	}
	// The bytes cross back byte-identically.
	st := replayAll(t, l)
	if got := st.Entries[e2.ID].Parts[0].Data; got != bA {
		t.Error("replayed image is not the bytes that went in")
	}
	// A patch moves a reference, it does not kill one: A stays pinned by the revision.
	cp := *e1
	cp.Parts = append([]ir.Part(nil), e1.Parts...)
	cp.Parts[0].Data = base64.StdEncoding.EncodeToString(imgB)
	if err := l.Patch(&cp); err != nil {
		t.Fatalf("Patch to image B: %v", err)
	}
	l.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", shaOf(imgA)).Scan(&rc)
	if rc != 2 {
		t.Errorf("blob A refcount after patch %d, want 2 (entry e2 + revision)", rc)
	}
	// Deleting the entry whose revision-pinned blob nothing else on the path needs keeps
	// the blob (the revision still names it)...
	if err := l.Delete(e2.ID); err != nil {
		t.Fatalf("Delete e2: %v", err)
	}
	l.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", shaOf(imgA)).Scan(&rc)
	if rc != 1 {
		t.Errorf("blob A refcount after delete e2 %d, want 1 (pinned by the revision)", rc)
	}
	// ...while the last entry holding B's only non-revision reference goes, B stays at 1
	// (e1 holds it), and only when e1 goes too does B leave with it.
	if err := l.Delete(e3.ID); err != nil {
		t.Fatalf("Delete e3: %v", err)
	}
	if err := l.Delete(e1.ID); err != nil {
		t.Fatalf("Delete e1: %v", err)
	}
	l.db.QueryRow("SELECT COUNT(*) FROM blob WHERE sha256 = ?", shaOf(imgB)).Scan(&n)
	if n != 0 {
		t.Errorf("blob B survived its last holder, with %d rows left", n)
	}
	l.db.QueryRow("SELECT COUNT(*) FROM blob WHERE sha256 = ?", shaOf(imgA)).Scan(&n)
	if n != 1 {
		t.Errorf("blob A rows %d, want 1 — a revision still references it", n)
	}
}

// TestContentsWithImagesIsNotDeadlockedByOneConn is a regression test in the literal
// sense: the store owns a single connection (SetMaxOpenConns(1)), so an inflation that
// chases a blob reference must not start while the payload scan still holds it — the
// shape that did asked the connection for itself and waited forever. Only an entry
// with an image part ever reaches the blob query, which is how the shape survived
// every pictureless test until a session with screenshots hung the daemon on open.
func TestContentsWithImagesIsNotDeadlockedByOneConn(t *testing.T) {
	l := openTest(t)
	img := []byte("png-bytes")
	b64 := base64.StdEncoding.EncodeToString(img)
	withImg := imgEntry("screenshot", img)
	plain := user("plain")
	for _, e := range []*ir.Entry{plain, withImg} {
		if err := l.AppendEntry(e, true); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
	}
	got, err := l.Contents([]string{withImg.ID, plain.ID, "9999999"}, true)
	if err != nil {
		t.Fatalf("Contents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("contents %d entries, want 2 — the ghost answers by absence", len(got))
	}
	if d := got[withImg.ID].Parts[0].Data; d != b64 {
		t.Error("Contents(true) did not re-attach the image bytes")
	}
	// expandBlobs=false keeps "blob:<sha>" where the bytes were — the shape a reader
	// that never sends a picture answers with, and it never opens the blob table.
	refs, err := l.Contents([]string{withImg.ID}, false)
	if err != nil {
		t.Fatalf("Contents(false): %v", err)
	}
	if d := refs[withImg.ID].Parts[0].Data; !strings.HasPrefix(d, BlobPrefix) {
		t.Errorf("Contents(false) data %q, want a %s reference", d, BlobPrefix)
	}
	// A window that repeats a screenshot pays for it once: two references, one
	// point query, one encoding — the bill is distinct images, not references
	// (the shape a model pays per picture it never sent twice).
	repeat := imgEntry("again", img)
	if err := l.AppendEntry(repeat, true); err != nil {
		t.Fatalf("AppendEntry repeat: %v", err)
	}
	before := l.blobLookups
	both, err := l.Contents([]string{withImg.ID, repeat.ID}, true)
	if err != nil {
		t.Fatalf("Contents pair: %v", err)
	}
	if got := l.blobLookups - before; got != 1 {
		t.Errorf("blob lookups for two references of one image = %d, want 1", got)
	}
	if both[withImg.ID].Parts[0].Data != b64 || both[repeat.ID].Parts[0].Data != b64 {
		t.Error("the cached encoding is not the bytes that went in")
	}
}

// TestInflateSingleRow is the export cursor's one-row shape: one call answers
// one entry with its pictures attached, and a vanished id answers by absence
// rather than error. The cache is the caller's to own: hand nil and every
// reference reads the blob table for itself — the trade a stream that passes
// each row once makes against holding a session's distinct pictures resident;
// hand one in and the shared shape holds, two references one ask.
func TestInflateSingleRow(t *testing.T) {
	l := openTest(t)
	img := []byte("png-bytes")
	b64 := base64.StdEncoding.EncodeToString(img)
	withImg := imgEntry("screenshot", img)
	if err := l.AppendEntry(withImg, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	got, err := l.Inflate(withImg.ID, nil)
	if err != nil || got == nil {
		t.Fatalf("Inflate = %v, %v", got, err)
	}
	if got.Parts[0].Data != b64 {
		t.Error("Inflate did not re-attach the image bytes")
	}
	before := l.blobLookups
	cache := map[string]string{}
	repeat := imgEntry("again", img)
	if err := l.AppendEntry(repeat, true); err != nil {
		t.Fatalf("AppendEntry repeat: %v", err)
	}
	for _, id := range []string{withImg.ID, repeat.ID} {
		if _, err := l.Inflate(id, cache); err != nil {
			t.Fatalf("Inflate %s: %v", id, err)
		}
	}
	if got := l.blobLookups - before; got != 1 {
		t.Errorf("blob lookups through the caller's cache = %d, want 1", got)
	}
	if e, err := l.Inflate("9999999", nil); err != nil || e != nil {
		t.Errorf("Inflate of a ghost = %v, %v, want absence without error", e, err)
	}
}

// TestBlobBytesMoveBetweenStores is the fork pipeline's byte movement without the
// fork: bytes leave one store by sha, land in another untouched, and the payload
// that names them arrives as a reference — never re-encoded, never re-hashed. The
// ordering it enforces is the pipeline's: bytes first, name second; a payload
// naming an unseen sha is refused rather than answered with an invented empty row.
func TestBlobBytesMoveBetweenStores(t *testing.T) {
	src := openTest(t)
	img := []byte("png-bytes")
	b64 := base64.StdEncoding.EncodeToString(img)
	e := imgEntry("shot", img)
	if err := src.AppendEntry(e, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	sha := shaOfBytes(img)

	raw, mime, found, err := src.BlobBytes(sha)
	if err != nil || !found || mime != "image/png" || !bytes.Equal(raw, img) {
		t.Fatalf("BlobBytes = %v,%v,%q,%v", raw, found, mime, err)
	}
	if _, _, found, err := src.BlobBytes("deadbeef"); err != nil || found {
		t.Errorf("BlobBytes of an unknown sha = %v,%v, want not-found without error", found, err)
	}

	dst := openTest(t)
	refEntry := func() *ir.Entry {
		return &ir.Entry{Kind: ir.KindUser, Role: "user", Ts: time.Now().UTC(),
			Parts: []ir.Part{{Type: ir.PartImage, MimeType: mime, Data: BlobPrefix + sha}}}
	}
	if err := dst.AppendEntry(refEntry(), false); err == nil {
		t.Error("a reference to bytes never put here was accepted, want a refusal")
	}
	if err := dst.PutBlob(sha, mime, raw); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	first := refEntry()
	if err := dst.AppendEntry(first, false); err != nil {
		t.Fatalf("AppendEntry reference: %v", err)
	}
	if err := dst.AppendEntry(refEntry(), false); err != nil {
		t.Fatalf("AppendEntry second reference: %v", err)
	}
	var rc int
	dst.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", sha).Scan(&rc)
	if rc != 2 {
		t.Errorf("refcount after two reference payloads = %d, want 2 (references are owned by payloads, PutBlob pins none)", rc)
	}
	// The same bytes handed again land on the existing row and charge nothing:
	// the sha is the identity, and a batch copy that repeats a sha repeats no cost.
	if err := dst.PutBlob(sha, mime, raw); err != nil {
		t.Fatalf("PutBlob again: %v", err)
	}
	dst.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", sha).Scan(&rc)
	if rc != 2 {
		t.Errorf("refcount after PutBlob again = %d, want 2", rc)
	}
	// The reference reads back in both shapes: named in false, bytes in true.
	refs, err := dst.Contents([]string{first.ID}, false)
	if err != nil || refs[first.ID].Parts[0].Data != BlobPrefix+sha {
		t.Errorf("Contents(false) = %v, %v, want the reference kept", refs[first.ID], err)
	}
	full, err := dst.Contents([]string{first.ID}, true)
	if err != nil || full[first.ID].Parts[0].Data != b64 {
		t.Errorf("Contents(true) = %v, %v, want the moved bytes back as base64", full[first.ID], err)
	}
	// And the ordinary path still works beside it: a base64 entry of the very same
	// image joins the store, the sha is recomputed, and one more reference pins.
	if err := dst.AppendEntry(imgEntry("ordinary", img), false); err != nil {
		t.Fatalf("AppendEntry base64 beside the reference: %v", err)
	}
	dst.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", sha).Scan(&rc)
	if rc != 3 {
		t.Errorf("refcount after the base64 path = %d, want 3", rc)
	}
}

// TestBlobSize is the read behind the assembler's text-only note: the byte count of
// one blob, answered from the size column without the bytes column being touched.
func TestBlobSize(t *testing.T) {
	l := openTest(t)
	img := bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 5001) // 15003 raw bytes, no base64 in sight
	if err := l.AppendEntry(imgEntry("sized", img), true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	size, found, err := l.BlobSize(shaOfBytes(img))
	if err != nil || !found || size != len(img) {
		t.Fatalf("BlobSize = %d,%v,%v, want %d,true,nil", size, found, err, len(img))
	}
	if _, found, err := l.BlobSize("deadbeef"); err != nil || found {
		t.Errorf("BlobSize of an unknown sha = %v,%v, want not-found without error", found, err)
	}
}

// TestImportTxSpeaksThroughItsOwnTransaction is the fork's pair of primitives: ids
// are issuable and blobs landable while the import transaction holds the store's
// lock — where the plain Log methods would deadlock against it — and everything
// arrives together at Finish, refcount included: the PutBlob-and-name pair charges
// exactly what AppendEntry charges for one entry, no more.
func TestImportTxSpeaksThroughItsOwnTransaction(t *testing.T) {
	l := openTest(t)
	img := bytes.Repeat([]byte{0x42}, 4096)
	it, err := l.ImportStream()
	if err != nil {
		t.Fatalf("ImportStream: %v", err)
	}
	head := it.NextID()
	if head != "0000001" {
		t.Fatalf("NextID = %q, want 0000001", head)
	}
	if err := it.PutBlob(shaOfBytes(img), "image/png", img); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	named := imgEntry("look", img)
	named.ID = head
	if err := it.Entry(named); err != nil {
		t.Fatalf("Entry: %v", err)
	}
	tail := it.NextID()
	second := user("second")
	second.ID = tail
	second.ParentID = head
	if err := it.Entry(second); err != nil {
		t.Fatalf("Entry second: %v", err)
	}
	if err := it.Finish(tail); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	st := replayAll(t, l)
	if len(st.Entries) != 2 || st.Tip != tail {
		t.Fatalf("committed copy = %d entries, tip %s; want 2, tip %s", len(st.Entries), st.Tip, tail)
	}
	if b, _, found, err := l.BlobBytes(shaOfBytes(img)); err != nil || !found || !bytes.Equal(b, img) {
		t.Fatalf("blob after commit = %d bytes, found %v, err %v; want %d,true,nil", len(b), found, err, len(img))
	}
	var refcount int
	l.db.QueryRow("SELECT refcount FROM blob WHERE sha256 = ?", shaOfBytes(img)).Scan(&refcount)
	if refcount != 1 {
		t.Fatalf("refcount = %d, want 1: the naming row charges it inside the same transaction", refcount)
	}
}

func TestDeleteReparentsAndRewritesPayload(t *testing.T) {
	l := openTest(t)
	root := user("root")
	if err := l.AppendEntry(root, true); err != nil {
		t.Fatalf("AppendEntry root: %v", err)
	}
	mid := user("mid")
	mid.ParentID = root.ID
	if err := l.AppendEntry(mid, true); err != nil {
		t.Fatalf("AppendEntry mid: %v", err)
	}
	leaf := user("leaf")
	leaf.ParentID = mid.ID
	if err := l.AppendEntry(leaf, true); err != nil {
		t.Fatalf("AppendEntry leaf: %v", err)
	}
	if err := l.Delete(mid.ID); err != nil {
		t.Fatalf("Delete mid: %v", err)
	}
	st := replayAll(t, l)
	if got := st.Entries[leaf.ID].ParentID; got != root.ID {
		t.Errorf("leaf's payload says parent %s, want %s", got, root.ID)
	}
	var col string
	l.db.QueryRow("SELECT parent FROM entry WHERE id = ?", leaf.ID).Scan(&col)
	if col != root.ID {
		t.Errorf("parent column says %s, want %s — payload and edge must agree", col, root.ID)
	}
	path := ir.Path(st.Entries, st.Tip)
	if len(path) != 2 || path[0].ID != root.ID || path[1].ID != leaf.ID {
		t.Errorf("path %v, want root→leaf", path)
	}
	// Deleting the tip leaves the session standing on the newest surviving entry.
	if err := l.Delete(leaf.ID); err != nil {
		t.Fatalf("Delete leaf: %v", err)
	}
	if st = replayAll(t, l); st.Tip != root.ID {
		t.Errorf("tip after deleting it %s, want the newest entry %s", st.Tip, root.ID)
	}
}

func TestReopenPersistenceAndInspect(t *testing.T) {
	dir := t.TempDir()
	l := openAt(t, dir)
	e := imgEntry("with image", []byte("some bytes"))
	if err := l.AppendEntry(e, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l2 := openAt(t, dir)
	st := replayAll(t, l2)
	if st.Entries[e.ID].Parts[0].Data != e.Parts[0].Data {
		t.Error("the image did not survive a reopen byte-identically")
	}
	info, err := l2.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.Entries != 1 || info.Rows != 1 || info.Depth != 1 || info.Tip != e.ID || info.LogBytes == 0 || info.LastTs.IsZero() {
		t.Errorf("Read %+v, want one entry, one write, depth 1, tip %s, bytes, a time", info, e.ID)
	}
	l2.Close()
	// A closed session still lists its numbers; a directory without a store lists none.
	cold, err := Inspect(dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if cold.Entries != 1 || cold.Tip != e.ID {
		t.Errorf("cold Inspect %+v, want the same one entry", cold)
	}
	if _, err := Inspect(filepath.Join(t.TempDir(), "nothing")); err == nil {
		t.Error("Inspect of a directory with no store returned numbers, want an error")
	}
}

func TestSchemaVersionGuard(t *testing.T) {
	dir := t.TempDir()
	l := openAt(t, dir)
	if _, err := l.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	l.Close()
	if _, err := Open(dir, false); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("Open of a foreign schema: %v, want a schema version error", err)
	}
}

func TestMetaDoorbells(t *testing.T) {
	dir := t.TempDir()
	l := openAt(t, dir)
	if err := l.SetMeta("open", "1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if v, ok := l.Meta("open"); !ok || v != "1" {
		t.Errorf("Meta = %q,%v, want \"1\",true", v, ok)
	}
	// A doorbell rings across handles: another process — the daemon walking its data
	// directory at startup — reads it without any handle of its own in play.
	if v, ok := MetaOf(dir, "open"); !ok || v != "1" {
		t.Errorf("MetaOf = %q,%v, want \"1\",true", v, ok)
	}
	// It survives the process, which is the whole reason it is a row and not a field.
	l.Close()
	if v, ok := MetaOf(dir, "open"); !ok || v != "1" {
		t.Fatalf("MetaOf after reopen = %q,%v, want \"1\",true", v, ok)
	}
	// Taking the bell off is deleting the row, and an absent key answers like an absent key.
	l = openAt(t, dir)
	if err := l.SetMeta("open", ""); err != nil {
		t.Fatalf("SetMeta clear: %v", err)
	}
	if _, ok := l.Meta("open"); ok {
		t.Error("cleared bell still reads as rung")
	}
	// The store's own keys are not the caller's namespace.
	if err := l.SetMeta(metaTip, "0000001"); err == nil {
		t.Error("SetMeta of the store's tip key succeeded, want an error")
	}
	l.Close()
}

// TestPayloadCompressionRoundTrip is the compression contract, run once per codec: a
// big payload is stored in its codec's shape and reads back byte-identically; the floor
// keeps short rows plain text; the "none" promise keeps long rows plain too; a store
// holding compressed, floor-excluded and knob-excluded shapes at once replays and
// searches as one; and a patch under compression still archives a readable revision.
func TestPayloadCompressionRoundTrip(t *testing.T) {
	t.Run("gzip", func(t *testing.T) { payloadRoundTrip(t, "gzip", []byte{0x1f, 0x8b}) })
	t.Run("zstd", func(t *testing.T) { payloadRoundTrip(t, "zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}) })
	t.Run("none", func(t *testing.T) { payloadRoundTrip(t, "none", nil) })
}

func payloadRoundTrip(t *testing.T, codec string, magic []byte) {
	old := Compressor
	defer func() { Compressor = old }()
	Compressor = codec

	long := strings.Repeat("deflate 測試 payload 內容 ", 120) // comfortably past the floor
	l := openTest(t)

	sealed := user(long)
	if err := l.AppendEntry(sealed, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	var raw []byte
	if err := l.db.QueryRow("SELECT payload FROM entry WHERE id = ?", sealed.ID).Scan(&raw); err != nil {
		t.Fatalf("raw scan: %v", err)
	}
	if magic == nil {
		if !bytes.HasPrefix(raw, []byte("{")) {
			t.Fatalf(`Compressor="none" stored %d bytes not opening a JSON object`, len(raw))
		}
	} else {
		if !bytes.HasPrefix(raw, magic) {
			t.Fatalf("payload stored as %x does not carry %s's magic", raw, codec)
		}
		if len(raw) >= len(long) {
			t.Errorf("stored %d bytes for %d of text, want the smaller", len(raw), len(long))
		}
	}

	// The floor: short stays plain text the shell can read.
	below := user("short")
	if err := l.AppendEntry(below, true); err != nil {
		t.Fatalf("AppendEntry below floor: %v", err)
	}
	var typ string
	if err := l.db.QueryRow("SELECT typeof(payload) FROM entry WHERE id = ?", below.ID).Scan(&typ); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if typ != "text" {
		t.Errorf("short payload stored as %s, want text", typ)
	}

	// The knob: promised plain, long rows arrive plain too.
	Compressor = "none"
	plain := user(long + " (knob-excluded)")
	if err := l.AppendEntry(plain, true); err != nil {
		t.Fatalf("AppendEntry knob: %v", err)
	}
	if err := l.db.QueryRow("SELECT typeof(payload) FROM entry WHERE id = ?", plain.ID).Scan(&typ); err != nil {
		t.Fatalf("typeof knob row: %v", err)
	}
	if typ != "text" {
		t.Errorf(`Compressor="none" payload stored as %s, want text`, typ)
	}

	// Mixed store: sealed, floor-excluded, knob-excluded — Replay speaks all three.
	st := replayAll(t, l)
	if st.Entries[sealed.ID].Content != long {
		t.Error("inflated content is not the text that went in")
	}
	if st.Entries[plain.ID].Content != long+" (knob-excluded)" {
		t.Error("plain row came back altered")
	}

	// Search crosses the shapes without being told which is which.
	hits, err := l.Search("測試 payload 內容", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits %d, want 2 (the sealed row and the plain one)", len(hits))
	}
	for _, h := range hits {
		if !strings.Contains(h.Snippet, "測試 payload 內容") {
			t.Errorf("hit %s snippet does not hold the needle: %q", h.ID, h.Snippet)
		}
	}

	// A patch under the codec: the archive is bytes, and PayloadText reads them back.
	Compressor = codec
	revised := *sealed
	revised.Content = strings.Repeat("修訂版 content ", 120)
	if err := l.Patch(&revised); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	var revp string
	if err := l.db.QueryRow("SELECT payload FROM entry_rev WHERE entry_id = ? ORDER BY seq DESC LIMIT 1", sealed.ID).Scan(&revp); err != nil {
		t.Fatalf("rev scan: %v", err)
	}
	var got ir.Entry
	if err := json.Unmarshal([]byte(PayloadText(revp)), &got); err != nil {
		t.Fatalf("rev payload through PayloadText: %v", err)
	}
	if got.Content != long {
		t.Error("the archived version is not the one it replaced")
	}
	if got := replayAll(t, l).Entries[sealed.ID].Content; got != revised.Content {
		t.Error("the live row did not take the revision")
	}
}

// TestFts5TrigramAvailable is a hand raised by the build's SQLite: fts5 with the
// trigram tokenizer is the only in-tree answer for CJK substring search, and plan B
// (an index over many sessions) is only on the table if this passes. Skipped, not
// failed, when the amalgam says no — the answer belongs in the test log.
func TestFts5TrigramAvailable(t *testing.T) {
	l := openTest(t)
	if _, err := l.db.Exec(`CREATE VIRTUAL TABLE f USING fts5(body, tokenize='trigram')`); err != nil {
		t.Skipf("this build's SQLite has no fts5/trigram: %v", err)
	}
	if _, err := l.db.Exec(`INSERT INTO f(body) VALUES ('繁中搜尋測試段落')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var hits int
	if err := l.db.QueryRow(`SELECT COUNT(*) FROM f WHERE f MATCH '"中搜尋"'`).Scan(&hits); err != nil {
		t.Fatalf("trigram match: %v", err)
	}
	if hits != 1 {
		t.Errorf("trigram hits %d, want 1 — the tokenizer does not speak CJK as this build hoped", hits)
	}
}

// TestCorpusScan prices plan A on real data: stored bytes, inflated bytes, and the
// wall time of one search scan per store. Set BLSTORE to a data directory (and
// optionally BLQUERY to a needle) to run it; the numbers answer "can we live on
// on-the-fly inflation" with the corpus's own voice rather than this file's guesses.
func TestCorpusScan(t *testing.T) {
	root := os.Getenv("BLSTORE")
	if root == "" {
		t.Skip("set BLSTORE to a data directory to price the on-the-fly scan")
	}
	needle := os.Getenv("BLQUERY")
	if needle == "" {
		needle = "content"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	var totalStored, totalText, totalEntries int64
	t0 := time.Now()
	for _, de := range dirs {
		dir := filepath.Join(root, de.Name())
		if _, err := os.Stat(filepath.Join(dir, DBFileName)); err != nil {
			continue
		}
		l, err := Open(dir, false)
		if err != nil {
			t.Errorf("open %s: %v", dir, err)
			continue
		}
		var stored int64
		if err := l.db.QueryRow("SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM entry").Scan(&stored); err != nil {
			t.Errorf("stored bytes of %s: %v", de.Name(), err)
		}
		var texts int64
		rows, err := l.db.Query("SELECT payload FROM entry")
		if err == nil {
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err == nil {
					texts += int64(len(payloadText(p)))
				}
			}
			rows.Close()
		}
		scanStart := time.Now()
		hits, err := l.Search(needle, 200)
		scan := time.Since(scanStart)
		if err != nil {
			t.Errorf("search %s: %v", de.Name(), err)
		}
		var entries int64
		l.db.QueryRow("SELECT COUNT(*) FROM entry").Scan(&entries)
		t.Logf("%s: entries=%d stored=%d text=%d hits=%d scan=%v", de.Name(), entries, stored, texts, len(hits), scan.Round(time.Millisecond))
		totalStored, totalText, totalEntries = totalStored+stored, totalText+texts, totalEntries+entries
		l.Close()
	}
	t.Logf("BLSTORE %s: %d entries, stored %d bytes over %d of text, one scan of everything in %v",
		root, totalEntries, totalStored, totalText, time.Since(t0).Round(time.Millisecond))
}

// TestCorpusDict asks the per-session dictionary question in corpus bytes: for each
// store, zstd without a dictionary vs zstd with a dictionary trained from that store's
// own payloads vs zstd with one global dictionary trained from a bounded sample of
// every store. The dictionary's bytes are part of the bill — one that earns back less
// than it costs is not a win — so local columns report both with and without it. The
// break-even threshold is meant to be read off these numbers, not guessed above them.
func TestCorpusDict(t *testing.T) {
	root := os.Getenv("BLSTORE")
	if root == "" {
		t.Skip("set BLSTORE to the uncompressed data directory to price per-session dictionaries")
	}
	// A dictionary is tailored to the level it was built for; this is the production one.
	const level = zstd.SpeedBetterCompression

	type storeData struct {
		name  string
		texts [][]byte
	}
	var stores []storeData
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, de := range dirs {
		dir := filepath.Join(root, de.Name())
		if _, err := os.Stat(filepath.Join(dir, DBFileName)); err != nil {
			continue
		}
		l := openAt(t, dir)
		rows, err := l.db.Query("SELECT payload FROM entry ORDER BY seq")
		if err != nil {
			l.Close()
			t.Fatalf("%s: %v", de.Name(), err)
		}
		var s storeData
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				break
			}
			s.texts = append(s.texts, []byte(payloadText(p)))
		}
		rows.Close()
		l.Close()
		s.name = de.Name()
		stores = append(stores, s)
	}
	if len(stores) == 0 {
		t.Fatalf("no stores under %s", root)
	}

	// The honest global: a per-store dictionary trained WITHOUT that store — what a
	// stranger's corpus gets from a dictionary shipped in the binary. (The in-sample
	// global, for reference, scored 5,777,506; heldout minus that is the outsider tax.)
	// Dictionary cost is not billed per store here: one seed book per released binary.
	opts := dict.Options{MaxDictSize: 32 << 10, HashBytes: 6, ZstdLevel: level}

	noDict, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
	if err != nil {
		t.Fatal(err)
	}
	defer noDict.Close()

	var totalText, totalNo, totalLocal, totalHeld int64
	for _, s := range stores {
		var nd, lc, hc int64
		for _, p := range s.texts {
			nd += int64(len(noDict.EncodeAll(p, nil)))
			totalText += int64(len(p))
		}
		var hsample [][]byte
		for _, o := range stores {
			if o.name == s.name {
				continue
			}
			sum := 0
			for _, p := range o.texts {
				if sum >= 128<<10 {
					break
				}
				if len(p) > 2<<10 {
					p = p[:2<<10]
				}
				hsample = append(hsample, p)
				sum += len(p)
			}
		}
		hd, err := dict.BuildZstdDict(hsample, opts)
		if err != nil {
			t.Fatalf("heldout dict for %s: %v", s.name, err)
		}
		henc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderDict(hd))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range s.texts {
			hc += int64(len(henc.EncodeAll(p, nil)))
		}
		henc.Close()
		// The trainer wants pre-truncated samples; beyond ~32KB the tail of a sample
		// adds nothing it could not already match on.
		samples := make([][]byte, len(s.texts))
		for j, p := range s.texts {
			if len(p) > 32<<10 {
				p = p[:32<<10]
			}
			samples[j] = p
		}
		dT0 := time.Now()
		d, derr := dict.BuildZstdDict(samples, opts)
		trainMS := time.Since(dT0).Milliseconds()
		if derr != nil {
			// too little content to train on is the threshold speaking, not a failure
			totalLocal += nd
			totalNo += nd
			totalHeld += hc
			t.Logf("%-22s text=%7d nodict=%7d local=n/a(%v,%dms) heldout=%7d", s.name, totalTextOf(s.texts), nd, derr, trainMS, hc)
			continue
		}
		le, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderDict(d))
		if err != nil {
			t.Fatalf("%s: dict encoder: %v", s.name, err)
		}
		for _, p := range s.texts {
			lc += int64(len(le.EncodeAll(p, nil)))
		}
		le.Close()
		totalNo += nd
		totalLocal += lc + int64(len(d)) // the dictionary lives on disk too
		totalHeld += hc
		t.Logf("%-22s text=%7d nodict=%7d local+dict=%6d(train %dms) heldout=%7d",
			s.name, totalTextOf(s.texts), nd, lc+int64(len(d)), trainMS, hc)
	}
	t.Logf("TOTAL text=%d | nodict=%d | local(incl dicts)=%d | heldout=%d | gzip reference=6635884 (in-sample global was 5777506)",
		totalText, totalNo, totalLocal, totalHeld)
}

func totalTextOf(texts [][]byte) int64 {
	var n int64
	for _, p := range texts {
		n += int64(len(p))
	}
	return n
}

// TestZstdDictionaryRoundTrip writes with a trained dictionary installed: the row must
// come back as a zstd frame stamped by that book, PayloadText must open it while the
// pool holds the book, and must fail loudly — raw bytes, not invented text — once the
// book is gone. A dictionary is data: this is that sentence tested.
func TestZstdDictionaryRoundTrip(t *testing.T) {
	// A book of the kind the seed will be: trained on the payload's own shape.
	var samples [][]byte
	for i := 0; i < 200; i++ {
		samples = append(samples, []byte(fmt.Sprintf(`{"schema":1,"id":"%07d","kind":"user","parts":[{"type":"text","text":"dictionary round trip sample %d, padded with common words to train against"}]}`, i, i)))
	}
	d, err := dict.BuildZstdDict(samples, dict.Options{MaxDictSize: 4 << 10, HashBytes: 6, ZstdLevel: ZstdLevel})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	if err := SetZdict(d); err != nil {
		t.Fatalf("SetZdict: %v", err)
	}
	defer SetZdict(nil)

	old := Compressor
	Compressor = "zstd"
	defer func() { Compressor = old }()

	l := openAt(t, t.TempDir())
	defer l.Close()
	text := strings.Repeat("dictionary round trip payload with the shipped book. ", 30) // over the floor
	e := &ir.Entry{Kind: ir.KindUser, Parts: []ir.Part{{Type: ir.PartText, Text: text}}}
	if err := l.AppendEntry(e, true); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := l.db.QueryRow("SELECT payload FROM entry WHERE id = ?", e.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) < 4 || raw[0] != 0x28 || raw[1] != 0xb5 || raw[2] != 0x2f || raw[3] != 0xfd {
		t.Fatalf("payload is not a zstd frame: %.40q", raw)
	}
	if got := PayloadText(raw); !strings.Contains(got, "shipped book") {
		t.Errorf("PayloadText with the book: %.80q", got)
	}
	SetZdict(nil)
	if got := PayloadText(raw); got != raw {
		t.Errorf("a reader without the book invented text: %.80q", got)
	}
}

// TestSetZdictRejectsNonDictionary: the book is validated before it is installed, so
// -zdict pointing at anything else is a startup death, not a store that cannot open.
func TestSetZdictRejectsNonDictionary(t *testing.T) {
	if err := SetZdict([]byte("this is not a zstd dictionary")); err == nil {
		t.Fatal("SetZdict accepted a non-dictionary")
	}
	if err := SetZdict(nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
}

// TestSetZdictRejectsOversizedDictionary: the format requires a dictionary to fit
// inside the encoder window; without this refusal the failure would be NewWriter
// erroring and buildZstdPair answering the only way it can — no compression, in
// silence. The size check precedes the content check: oversized junk is refused
// for the size that actually kills the encoder, not for its dictionary magic.
func TestSetZdictRejectsOversizedDictionary(t *testing.T) {
	if err := SetZdict(make([]byte, zstdWindow+1)); err == nil {
		t.Fatal("SetZdict accepted a dictionary larger than the encoder window")
	}
	if err := SetZdict(nil); err != nil {
		t.Fatalf("clearing: %v", err)
	}
}

// TestTreeShape reads the whole tree the way the branch navigator does: every edge,
// every child count, and the live path lit among the branches — and lit at its new
// place when the tip moves there. Shape is the promise: no content is asked, and the
// rows answer the navigator's two questions ("where do I stand", "what else hangs
// here") for every branch, not just the walked one.
func TestTreeShape(t *testing.T) {
	l := openTest(t)
	// The user's example tree: root forks (b1 branch / n2 branch); d1 forks (e1 / e2);
	// the tip stands on f1 deep in the e2 branch.
	root := user("root")
	if err := l.AppendEntry(root, true); err != nil {
		t.Fatalf("AppendEntry root: %v", err)
	}
	add := func(parent string) *ir.Entry {
		e := user("child of " + parent)
		e.ParentID = parent
		if err := l.AppendEntry(e, true); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
		return e
	}
	b1 := add(root.ID)
	n2 := add(root.ID) // the abandoned fork
	c1 := add(b1.ID)
	d1 := add(c1.ID)
	e1 := add(d1.ID) // tip moves here later
	e2 := add(d1.ID)
	f1 := add(e2.ID) // the tip

	byID := func(rows []TreeRow) map[string]TreeRow {
		m := map[string]TreeRow{}
		for _, r := range rows {
			m[r.ID] = r
		}
		return m
	}
	rows, err := l.Tree(0)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(rows) != 8 {
		t.Fatalf("tree has %d rows, want 8", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Seq >= rows[i].Seq {
			t.Fatalf("rows not seq-ordered at %d", i)
		}
	}
	m := byID(rows)
	if m[root.ID].NKids != 2 {
		t.Errorf("root nkids %d, want 2 (the fork and its abandoned twin)", m[root.ID].NKids)
	}
	if m[d1.ID].NKids != 2 {
		t.Errorf("d1 nkids %d, want 2", m[d1.ID].NKids)
	}
	if m[f1.ID].NKids != 0 {
		t.Errorf("leaf nkids %d, want 0", m[f1.ID].NKids)
	}
	onPath := func(r TreeRow) bool { return r.OnPath }
	for id, want := range map[string]bool{
		root.ID: true, b1.ID: true, c1.ID: true, d1.ID: true, e2.ID: true, f1.ID: true,
		n2.ID: false, e1.ID: false,
	} {
		if got := onPath(m[id]); got != want {
			t.Errorf("entry %s onPath %v, want %v", id, got, want)
		}
	}
	if !m[f1.ID].IsTip {
		t.Error("f1 is not flagged the tip")
	}
	for id, row := range m {
		if row.IsTip && id != f1.ID {
			t.Errorf("entry %s claims the tip beside %s", id, f1.ID)
		}
	}

	// The tip walks to the e1 branch: the lights move, the geometry does not.
	if err := l.SetTip(e1.ID); err != nil {
		t.Fatalf("SetTip: %v", err)
	}
	rows, err = l.Tree(0)
	if err != nil {
		t.Fatalf("Tree after SetTip: %v", err)
	}
	m = byID(rows)
	for id, want := range map[string]bool{
		root.ID: true, b1.ID: true, c1.ID: true, d1.ID: true, e1.ID: true,
		n2.ID: false, e2.ID: false, f1.ID: false,
	} {
		if got := onPath(m[id]); got != want {
			t.Errorf("after SetTip: entry %s onPath %v, want %v", id, got, want)
		}
	}
	if !m[e1.ID].IsTip || m[f1.ID].IsTip {
		t.Error("the tip flag did not move with the tip")
	}
	if m[root.ID].NKids != 2 || m[d1.ID].NKids != 2 {
		t.Error("moving the tip moved a child count: geometry is not the tip's to change")
	}

	// A bounding limit keeps the first rows in seq order and the walk honest: ancestors
	// before the window's first row cannot claim OnPath, and that is the truthful answer.
	rows, err = l.Tree(3)
	if err != nil {
		t.Fatalf("Tree limit: %v", err)
	}
	if len(rows) != 3 || rows[0].ID != root.ID {
		t.Fatalf("Tree(3) = %d rows starting %s, want 3 starting at the root", len(rows), rows[0].ID)
	}

	// An empty store answers empty, not null-error.
	fresh := openTest(t)
	if rows, err := fresh.Tree(0); err != nil || len(rows) != 0 {
		t.Fatalf("empty Tree = %d rows err %v, want 0 and nil", len(rows), err)
	}
}

// TestSnippetsPreviewsRows prices the navigator's lazy half: opening lines only, one
// display line whatever the answer wrote, ids that vanished mid-flight skipped without
// answering a hole, and the caller's order kept.
func TestSnippetsPreviewsRows(t *testing.T) {
	l := openTest(t)
	long := user(strings.Repeat("word ", 100)) // 500 characters: the clip and its mark
	multi := user("line one\nline\ttwo")
	quiet := &ir.Entry{Kind: ir.KindTool, Role: "tool", Tool: "fs_read", Ts: time.Now().UTC()}                                                                                                                              // a failed call with no text
	thinking := &ir.Entry{Kind: ir.KindAssistant, Role: "assistant", Reasoning: "pondering which branch to grow", ToolCalls: []ir.ToolCall{{ID: "c1", Name: "fs_read"}, {ID: "c2", Name: "fs_grep"}}, Ts: time.Now().UTC()} // a thinking model's reply: content empty, the words elsewhere
	hollow := &ir.Entry{Kind: ir.KindAssistant, Role: "assistant", Ts: time.Now().UTC()}                                                                                                                                    // interrupted before anything landed
	for _, e := range []*ir.Entry{long, multi, quiet, thinking, hollow} {
		if err := l.AppendEntry(e, true); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
	}
	snips, err := l.Snippets([]string{long.ID, "9999999", multi.ID, quiet.ID, thinking.ID, hollow.ID})
	if err != nil {
		t.Fatalf("Snippets: %v", err)
	}
	if len(snips) != 5 {
		t.Fatalf("got %d snippets for 5 living ids and 1 ghost, want 5", len(snips))
	}
	if snips[0].ID != long.ID || snips[1].ID != multi.ID || snips[2].ID != quiet.ID || snips[3].ID != thinking.ID || snips[4].ID != hollow.ID {
		t.Errorf("the answer shuffled the caller's order: %v", snips)
	}
	first := []rune(snips[0].Snippet)
	if len(first) != 121 || string(first[len(first)-1]) != "…" {
		t.Errorf("long snippet is %d runes ending %q, want 120 runes and a cut mark", len(first), string(first[len(first)-1]))
	}
	if snips[1].Snippet != "line one line two" {
		t.Errorf("multi-line preview %q, want one collapsed line", snips[1].Snippet)
	}
	if snips[2].Snippet != "⟨tool fs_read⟩" {
		t.Errorf("textless tool row %q, want the tool's name to name it", snips[2].Snippet)
	}
	// A row whose content is empty still names itself: calls first, thinking next, nothing last.
	if snips[3].Snippet != "⟨fs_read +1⟩" {
		t.Errorf("tool-calling row with no content %q, want its calls to name it", snips[3].Snippet)
	}
	if snips[4].Snippet != "⟨no text⟩" {
		t.Errorf("hollow row %q, want the plain admission", snips[4].Snippet)
	}
	reasoning := &ir.Entry{Kind: ir.KindAssistant, Role: "assistant", Reasoning: "weighing the options quietly", Ts: time.Now().UTC()}
	if err := l.AppendEntry(reasoning, false); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	sr, err := l.Snippets([]string{reasoning.ID})
	if err != nil || len(sr) != 1 {
		t.Fatalf("reasoning snippets: %v err %v", sr, err)
	}
	if sr[0].Snippet != "⟨thinking⟩ weighing the options quietly" {
		t.Errorf("thinking-only row %q, want the reasoning preview to name it", sr[0].Snippet)
	}
	// An empty ask answers empty — no query, no error.
	if s, err := l.Snippets(nil); err != nil || len(s) != 0 {
		t.Fatalf("Snippets(nil) = %d err %v, want 0 and nil", len(s), err)
	}
}

// benchFilled opens a store with n entries in one branchy tree, each carrying a
// paragraph: the shape both halves of the navigator read, at a session the product
// has measured rather than a toy.
func benchFilled(b *testing.B, n int) (*Log, []string) {
	b.Helper()
	l := openBench(b, b.TempDir())
	ids := make([]string, 0, n)
	tip := ""
	for i := 0; i < n; i++ {
		e := &ir.Entry{Kind: ir.KindUser, Role: "user", Content: strings.Repeat("filler ", 30), Ts: time.Now().UTC()}
		e.ParentID = tip
		if err := l.AppendEntry(e, false); err != nil {
			b.Fatalf("AppendEntry: %v", err)
		}
		tip = e.ID
		ids = append(ids, e.ID)
		if i%10 == 9 { // every tenth entry forks, as a lived session does
			fork := &ir.Entry{Kind: ir.KindUser, Role: "user", Content: "abandoned attempt", Ts: time.Now().UTC()}
			fork.ParentID = e.ID
			if err := l.AppendEntry(fork, false); err != nil {
				b.Fatalf("AppendEntry fork: %v", err)
			}
		}
	}
	if err := l.SetTip(tip); err != nil {
		b.Fatalf("SetTip: %v", err)
	}
	return l, ids
}

// openBench is openAt for benchmarks, which share no helper with the T world.
func openBench(b *testing.B, dir string) *Log {
	b.Helper()
	l, err := Open(dir, false)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { l.Close() })
	return l
}

// BenchmarkTree prices the shape-only read: one row per entry, no payload opened.
func BenchmarkTree(b *testing.B) {
	l, _ := benchFilled(b, 1000)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.Tree(0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnippets prices the lazy half: the opening lines of one screenful.
func BenchmarkSnippets(b *testing.B) {
	l, ids := benchFilled(b, 1000)
	screen := ids[:50]
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.Snippets(screen); err != nil {
			b.Fatal(err)
		}
	}
}
