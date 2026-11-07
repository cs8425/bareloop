package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestPayloadGzipRoundTrip is the compression contract in one sitting: a big payload
// is stored deflated and reads back byte-identically; the floor and the knob keep
// short and refused rows plain; a store holding all three shapes at once replays and
// searches as one; and a patch under compression still archives a readable revision.
func TestPayloadGzipRoundTrip(t *testing.T) {
	old := Compress
	defer func() { Compress = old }()

	long := strings.Repeat("deflate 測試 payload 內容 ", 120) // comfortably past the floor
	l := openTest(t)

	deflated := user(long)
	if err := l.AppendEntry(deflated, true); err != nil {
		t.Fatalf("AppendEntry deflated: %v", err)
	}
	var raw []byte
	if err := l.db.QueryRow("SELECT payload FROM entry WHERE id = ?", deflated.ID).Scan(&raw); err != nil {
		t.Fatalf("raw scan: %v", err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("payload stored as %d bytes without gzip's magic, want a deflate stream", len(raw))
	}
	if len(raw) >= len(long) {
		t.Errorf("stored %d bytes for %d of text, want the smaller", len(raw), len(long))
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
	Compress = false
	plain := user(long + " (knob-excluded)")
	if err := l.AppendEntry(plain, true); err != nil {
		t.Fatalf("AppendEntry knob: %v", err)
	}
	if err := l.db.QueryRow("SELECT typeof(payload) FROM entry WHERE id = ?", plain.ID).Scan(&typ); err != nil {
		t.Fatalf("typeof knob row: %v", err)
	}
	if typ != "text" {
		t.Errorf("Compress=false payload stored as %s, want text", typ)
	}

	// Mixed store: inflated, floor-excluded, knob-excluded — Replay speaks all three.
	st := replayAll(t, l)
	if st.Entries[deflated.ID].Content != long {
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
		t.Fatalf("hits %d, want 2 (the deflated row and the plain one)", len(hits))
	}
	for _, h := range hits {
		if !strings.Contains(h.Snippet, "測試 payload 內容") {
			t.Errorf("hit %s snippet does not hold the needle: %q", h.ID, h.Snippet)
		}
	}

	// A patch under compression: the archive is bytes, and PayloadText reads them back.
	Compress = true
	revised := *deflated
	revised.Content = strings.Repeat("修訂版 content ", 120)
	if err := l.Patch(&revised); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	var revp string
	if err := l.db.QueryRow("SELECT payload FROM entry_rev WHERE entry_id = ? ORDER BY seq DESC LIMIT 1", deflated.ID).Scan(&revp); err != nil {
		t.Fatalf("rev scan: %v", err)
	}
	var got ir.Entry
	if err := json.Unmarshal([]byte(PayloadText(revp)), &got); err != nil {
		t.Fatalf("rev payload through PayloadText: %v", err)
	}
	if got.Content != long {
		t.Error("the archived version is not the one it replaced")
	}
	if got := replayAll(t, l).Entries[deflated.ID].Content; got != revised.Content {
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
