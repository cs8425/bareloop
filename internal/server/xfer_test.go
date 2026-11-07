package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
	"bareloop/internal/store"
	"bareloop/internal/xfer"
)

// Export and import are tested as HTTP, because that is where they live: headers that
// promise the right file, gates that answer 413 before the parser sees a byte, and a
// round trip that must reproduce the source tree row for row, id for id.

func xferHandler(t *testing.T) (*agent.Manager, http.Handler) {
	t.Helper()
	// A manager with a default LLM endpoint, because that is what a daemon always has:
	// a file that carries no config (every webui file) inherits it through MergeDefaults,
	// exactly like POST /api/sessions with no body.
	def := agent.Config{LLM: llm.Config{BaseURL: "http://127.0.0.1:1", Model: "test-model"}}
	m := agent.NewManager(t.TempDir(), false, def, nil, nil)
	return m, New(m, nil, nil, "", nil).Handler()
}

// seedTree writes the same little forest the fork tests walk: three siblings under s3 and
// a compaction whose boundary is the root, tip on s8.
func seedTree(t *testing.T, m *agent.Manager, id string) {
	t.Helper()
	if _, err := m.Create(t.Context(), agent.Config{ID: id}); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	if _, err := m.Unload(id); err != nil {
		t.Fatalf("unload %s: %v", id, err)
	}
	l, err := store.Open(filepath.Join(m.Root(), id), false)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	defer l.Close()
	b := "s1"
	mk := func(eid, parent, kind, role, content string) *ir.Entry {
		return &ir.Entry{ID: eid, ParentID: parent, Kind: kind, Role: role, Content: content}
	}
	entries := []*ir.Entry{
		mk("s1", "", ir.KindUser, "user", "remember the list"),
		mk("s2", "s1", ir.KindAssistant, "assistant", "noted"),
		mk("s3", "s2", ir.KindUser, "user", "repeat it"),
		mk("s4", "s3", ir.KindAssistant, "assistant", "the list"),
		mk("s5", "s3", ir.KindAssistant, "assistant", "the list again"),
		mk("s6", "s3", ir.KindAssistant, "assistant", "the list, third"),
		{ID: "s7", ParentID: "s6", Kind: ir.KindCompaction, Role: "user", Content: "[Conversation summary]",
			Compaction: &ir.CompactionMeta{BoundaryID: &b, Replaced: 2}},
		mk("s8", "s7", ir.KindUser, "user", "and now?"),
	}
	for _, e := range entries {
		if err := l.AppendEntry(e, false); err != nil {
			t.Fatalf("append %s: %v", e.ID, err)
		}
	}
	if err := l.SetTip("s8"); err != nil {
		t.Fatalf("tip: %v", err)
	}
}

func replayOf(t *testing.T, m *agent.Manager, id string) *store.State {
	t.Helper()
	l, err := store.Open(filepath.Join(m.Root(), id), false)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	defer l.Close()
	st, err := l.Replay()
	if err != nil {
		t.Fatalf("replay %s: %v", id, err)
	}
	return st
}

func TestExportImportRoundTrip(t *testing.T) {
	m, h := xferHandler(t)
	seedTree(t, m, "src")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/src/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, ".jsonl.zst") {
		t.Fatalf("a zstd export must say so in the filename: %q", cd)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding must stay identity, the file in the client's hands is the file named: %q", ce)
	}
	raw := rec.Body.Bytes()

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/import?id=dst", bytes.NewReader(raw))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		ID      string `json:"id"`
		Format  string `json:"format"`
		Entries int    `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("import answer: %v", err)
	}
	if out.ID != "dst" || out.Format != "native" || out.Entries != 8 {
		t.Fatalf("import answer: %+v", out)
	}

	src, dst := replayOf(t, m, "src"), replayOf(t, m, "dst")
	if len(dst.Entries) != len(src.Entries) {
		t.Fatalf("dst has %d entries, src has %d", len(dst.Entries), len(src.Entries))
	}
	if dst.Tip != src.Tip {
		t.Fatalf("tip moved: %q vs %q (an import keeps ids, only a fork reissues)", dst.Tip, src.Tip)
	}
	for id, se := range src.Entries {
		de, ok := dst.Entries[id]
		if !ok {
			t.Fatalf("entry %s missing from the import", id)
		}
		if de.ParentID != se.ParentID || de.Content != se.Content || de.Kind != se.Kind {
			t.Fatalf("entry %s changed on the trip", id)
		}
	}
	c := dst.Entries["s7"]
	if c.Compaction == nil || c.Compaction.BoundaryID == nil || *c.Compaction.BoundaryID != "s1" {
		t.Fatalf("compaction boundary did not survive: %+v", c.Compaction)
	}
	if _, ok := dst.Entries["s1"]; !ok {
		t.Fatal("boundary target gone")
	}
}

func TestExportPathScopeAndRefusals(t *testing.T) {
	m, h := xferHandler(t)
	seedTree(t, m, "src")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/src/export?path=tip", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("path export: %d", rec.Code)
	}
	plain, closer, err := xfer.Uncompress(bytes.NewReader(rec.Body.Bytes()))
	if closer != nil {
		defer closer.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	stream, _, err := xfer.Sniff(plain)
	if err != nil {
		t.Fatal(err)
	}
	imp, err := xfer.DecodeNative(stream)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Tip != "s8" || len(imp.Entries) != 6 {
		t.Fatalf("path export: tip %q entries %d, want s8 and 6 (the siblings stay home)", imp.Tip, len(imp.Entries))
	}
	for _, e := range imp.Entries {
		if e.ID == "s4" || e.ID == "s5" {
			t.Fatalf("a sibling rode along on the path export")
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/src/export?path=ghost", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ghost path: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/session/src/export?codec=lz4", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown codec: %d", rec.Code)
	}
}

func TestImportWebUIFixture(t *testing.T) {
	m, h := xferHandler(t)
	body, err := os.ReadFile(filepath.Join("..", "xfer", "testdata", "webui_basic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions/import?id=webin&name=from%20webui", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if st := replayOf(t, m, "webin"); len(st.Entries) != 3 || st.Tip != "m3" {
		t.Fatalf("%d entries tip %q, want 3 and m3", len(st.Entries), st.Tip)
	}
	// the system root must have become the session's prompt, not a turn on the path
	sess, err := m.Get(t.Context(), "webin")
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.Config().SystemPrompt; got != "You are a careful assistant." {
		t.Fatalf("system prompt %q", got)
	}
	if got := sess.Config().Name; got != "from webui" {
		t.Fatalf("query name must outrank the file's: %q", got)
	}

	// The same file cannot land twice on one id, and junk cannot land at all.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions/import?id=webin", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second import onto the id: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions/import?id=junk", strings.NewReader("not a transcript\nat all\n")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("junk import: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "junk")); !os.IsNotExist(err) {
		t.Fatalf("the refused import left a directory behind")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions/import", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import without an id: %d", rec.Code)
	}
}

func TestImportCompressedBodyGate(t *testing.T) {
	m, h := xferHandler(t)
	old := xfer.MaxCompressedBody
	xfer.MaxCompressedBody = 16
	defer func() { xfer.MaxCompressedBody = old }()
	_ = m

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/import?id=g1", strings.NewReader(strings.Repeat("x", 64)))
	req.ContentLength = 64 // declared up front, refused before the body is touched
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize body: %d", rec.Code)
	}
	// An undeclared length still hits the MaxBytesReader on read.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/sessions/import?id=g2", strings.NewReader(strings.Repeat("y", 64)))
	req.ContentLength = -1
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("undeclared oversize body: %d", rec.Code)
	}
}

// A body that decodes and then fails validation (broken json mid-file) must leave no
// directory, the import equivalent of fork's half-copy undo.
func TestFailedImportLeavesNothing(t *testing.T) {
	m, h := xferHandler(t)
	body := `{"harness":"bareloop","format":"bareloop/1","tip":"x1","scope":"full","session":{}}` + "\n" +
		`{"id":"x1","kind":"user","role":"user"}` + "\n" + `{"id":"x2","parentId":"x1"` + "\n" // truncated
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions/import?id=half", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("broken file: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "half")); !os.IsNotExist(err) {
		t.Fatal("half an import left a directory")
	}
}
