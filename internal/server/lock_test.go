package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bareloop/internal/agent"
)

// The lock is a policy refusal, and the three routes that can write an MCP list say so with
// 403 where their neighbouring failures say 400 (bad body) or 409 (taken id): the request
// is not wrong, this daemon decided. The everyday bodies — the ones that never mention mcp,
// or re-save the list they were given — keep landing exactly as before.
func TestMCPLockAnswersForbidden(t *testing.T) {
	m, h := xferHandler(t)
	m.SetMCPLock(true) // the helper's defaults carry no servers, so any list is a foreign one

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions",
		strings.NewReader(`{"id":"evil","mcp":[{"name":"evil","command":"sh"}]}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create with a server: %d %s", rec.Code, rec.Body)
	}

	if _, err := m.Create(t.Context(), agent.Config{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/api/session/s",
		strings.NewReader(`{"mcp":[{"name":"evil","command":"sh"}]}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("patch with a server: %d %s", rec.Code, rec.Body)
	}
	// The settings-page re-save: a body that says nothing about mcp is not what the lock
	// speaks to.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/api/session/s", strings.NewReader(`{"name":"kept"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a body without mcp: %d %s", rec.Code, rec.Body)
	}

	// The import door: a bundle whose config header carries a server is the same refusal —
	// a route that could walk a server set past the lock would make the lock a sign on a
	// back door.
	bundle := `{"harness":"bareloop","format":"bareloop/1","tip":"x1","scope":"full",` +
		`"session":{"id":"imp","config":{"mcp":[{"name":"evil","command":"sh"}]}}}` + "\n" +
		`{"id":"x1","kind":"user","role":"user","content":"hi"}` + "\n"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sessions/import?id=imp", strings.NewReader(bundle)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("import with a server: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), "imp")); !os.IsNotExist(err) {
		t.Fatal("the refused import left a directory behind")
	}
}
