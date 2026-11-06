package server

// Tests for the one handler that serves the UI, which has two sources: the copy compiled into the binary
// and a directory named by -web. What can go wrong differs between them, and none of it is loud. A client
// that re-downloads the whole UI every navigation looks like a slow network. An edit under web/ that never
// reaches a browser looks like a browser that refuses to refresh. A UI directory that names nothing looks
// like a 404 the daemon never once mentioned on stderr.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func serveStatic(t *testing.T, s *Server, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.handleStatic(w, r)
	return w
}

func TestStaticFromTheBinary(t *testing.T) {
	// An embedded file carries no modification time, so an ETag built from one would be the same string
	// for every file in the archive: the identity has to be the bytes. fstest.MapFS is the same shape —
	// an fs.FS whose files have no times at all.
	s := New(nil, nil, fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>hi")},
		"js/app.js":  &fstest.MapFile{Data: []byte("export const x = 1;")},
	}, "", nil)

	w := serveStatic(t, s, "GET", "/", nil)
	if w.Code != http.StatusOK || w.Body.String() != "<html>hi" {
		t.Fatalf("GET /: got %d %q, want the entry point", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != pageCache {
		t.Errorf("entry point cache-control %q, want %q", got, pageCache)
	}
	if got := w.Header().Get("Last-Modified"); got != "" {
		t.Errorf("Last-Modified %q, want nothing: there is no modification time to report", got)
	}
	etag := w.Header().Get("ETag")
	if !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") || strings.Contains(etag, "-") {
		t.Errorf("ETag %q, want a quoted hash of the bytes", etag)
	}
	if again := serveStatic(t, s, "GET", "/", nil).Header().Get("ETag"); again != etag {
		t.Errorf("a second read gave ETag %q, want the same %q", again, etag)
	}
	if w := serveStatic(t, s, "GET", "/", map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Errorf("revalidating the entry point: got %d with %d bytes, want 304 and no body", w.Code, w.Body.Len())
	}

	// A module has to arrive as JavaScript or the browser refuses the import, and an asset is allowed a
	// cache lifetime the entry point is refused one.
	w = serveStatic(t, s, "GET", "/js/app.js", nil)
	if !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("GET /js/app.js: content-type %q, want javascript", w.Header().Get("Content-Type"))
	}
	if got := w.Header().Get("Cache-Control"); got != assetCache {
		t.Errorf("asset cache-control %q, want %q", got, assetCache)
	}
	if other := w.Header().Get("ETag"); other == etag {
		t.Errorf("two different files share the ETag %q", other)
	}

	// A name is cleaned before it is used, so nothing can climb out of the UI: what is left of an attempt
	// is simply some other name, which for an unknown asset is a 404 and for everything else the entry
	// point — the single page answer a route needs.
	for _, target := range []string{"/js/none.js", "/../none.js", "/%2e%2e/none.js"} {
		if w := serveStatic(t, s, "GET", target, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", target, w.Code)
		}
	}
	for _, target := range []string{"/some/route", "/", "/js/", "/../../index.html"} {
		if w := serveStatic(t, s, "GET", target, nil); w.Body.String() != "<html>hi" {
			t.Errorf("GET %s: got %q, want the entry point", target, w.Body.String())
		}
	}
	if w := serveStatic(t, s, "POST", "/", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: got %d, want 405", w.Code)
	}

	// A binary with neither source is a misconfiguration, and it should say so instead of 404-ing.
	if w := serveStatic(t, New(nil, nil, nil, "", nil), "GET", "/", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no UI at all: got %d, want 503", w.Code)
	}
}

func TestStaticFromADirectory(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "index.html")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(page, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", page, err)
		}
	}
	write("<html>one")

	s := New(nil, nil, nil, dir, nil)
	etag := serveStatic(t, s, "GET", "/", nil).Header().Get("ETag")

	// This is the promise a directory makes and the binary cannot: an edit is seen. Size and modification
	// time are the identity here precisely because those are what an edit changes, and an answer cached
	// once per file is how the edit stays invisible to every client that already has the page.
	write("<html>two!")
	if again := serveStatic(t, s, "GET", "/", nil).Header().Get("ETag"); again == etag {
		t.Errorf("an edit left the ETag at %q, so a client holding the page keeps it forever", etag)
	}
	if w := serveStatic(t, s, "GET", "/some/route", nil); w.Body.String() != "<html>two!" {
		t.Errorf("an unknown route got %q, want the entry point", w.Body.String())
	}

	// A named directory with no entry point in it is the operator's own mistake, so the answer names both
	// ways out of it.
	empty := t.TempDir()
	if w := serveStatic(t, New(nil, nil, nil, empty, nil), "GET", "/whatever", nil); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), "embedded") {
		t.Errorf("a directory with no index.html: got %d %q, want a 404 that names the embedded copy", w.Code, w.Body.String())
	}
}
