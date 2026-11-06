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

// serveMounted goes through the routes rather than calling a handler, because the two things that keep a
// mounted directory inside itself — which pattern wins, and what the path looks like by then — are the
// router's decisions as much as the handler's.
func serveMounted(t *testing.T, s *Server, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestTestdataMount(t *testing.T) {
	// The mount exists so a page that is still being written can be opened in a browser, fixtures included
	// — that is the part a file:// URL cannot do, since a module import and a fetch both refuse it. Its
	// failure modes are the UI's with different answers: a mistyped fixture name that comes back as the
	// single page instead of a 404 becomes a JSON parse error nobody can read, and an edit that never
	// arrives is a browser blamed for a cache header.
	dir := t.TempDir()
	fixture := filepath.Join(dir, "cases.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", fixture, err)
		}
	}
	write(`[{"in": "one"}]`)

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve me"), 0o644); err != nil {
		t.Fatalf("write %s: %v", secret, err)
	}
	secretDir := filepath.Base(filepath.Dir(secret))

	app := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>app")}}
	s := New(nil, nil, app, "", nil)
	s.ServeTestdata(dir)

	w := serveMounted(t, s, "GET", "/testdata/cases.json", nil)
	if w.Code != http.StatusOK || w.Body.String() != `[{"in": "one"}]` {
		t.Fatalf("GET /testdata/cases.json: got %d %q, want the file", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "json") {
		t.Errorf("content-type %q, want json: a page fetches a fixture as JSON", got)
	}
	// Nothing in a directory somebody is editing gets a cache lifetime, and its identity is the
	// modification time rather than a hash remembered for the life of the process — both are so that an
	// edit is on the screen after a reload.
	if got := w.Header().Get("Cache-Control"); got != pageCache {
		t.Errorf("cache-control %q, want %q", got, pageCache)
	}
	etag := w.Header().Get("ETag")
	write(`[{"in": "two and a half"}]`)
	changed := serveMounted(t, s, "GET", "/testdata/cases.json", nil).Header().Get("ETag")
	if changed == etag {
		t.Errorf("an edit left the ETag at %q, so a page holding the fixture keeps it", etag)
	}
	if again := serveMounted(t, s, "GET", "/testdata/cases.json", map[string]string{"If-None-Match": changed}); again.Code != http.StatusNotModified {
		t.Errorf("revalidating: got %d, want 304", again.Code)
	}

	// A name that is not there is a 404 and never the entry point: the single-page answer is what a route
	// needs, and here it is what turns a fixture typed one character wrong into a response full of HTML.
	if w := serveMounted(t, s, "GET", "/testdata/case.json", nil); w.Code != http.StatusNotFound ||
		strings.Contains(w.Body.String(), "<html>app") {
		t.Errorf("GET /testdata/case.json: got %d %q, want a 404 with no entry point in it", w.Code, w.Body.String())
	}
	// A directory is a 404 too: a mount is a set of known URLs, not a window onto the disk.
	for _, target := range []string{"/testdata", "/testdata/"} {
		if w := serveMounted(t, s, "GET", target, nil); w.Code == http.StatusOK {
			t.Errorf("GET %s: got 200, want no listing", target)
		}
	}
	// And nothing outside it, which is refused twice over — the router cleans the path before choosing a
	// handler and the handler cleans the name again against the mount — so the promise worth testing is
	// simply that the answer is never the file.
	for _, target := range []string{
		"/testdata/../" + secretDir + "/secret.txt",
		"/testdata/%2e%2e/" + secretDir + "/secret.txt",
		"/testdata/" + strings.Repeat("../", 8) + "etc/passwd",
	} {
		if w := serveMounted(t, s, "GET", target, nil); strings.Contains(w.Body.String(), "do not serve me") {
			t.Errorf("GET %s: the mount served a file from outside itself", target)
		}
	}

	// A client that does not normalise the path before sending it — `curl --path-as-is` — is the only
	// thing the handler's own cleaning is for, and the router never lets such a request reach it. So this
	// is the one test that hands handleTestdata a name still carrying "..", and what it pins is that the
	// answer is a 404 rather than a file reached by climbing out of the mount.
	dirty := httptest.NewRequest("GET", "/testdata/x", nil)
	dirty.SetPathValue("rest", "../../../../etc/passwd")
	wd := httptest.NewRecorder()
	s.handleTestdata(wd, dirty)
	if wd.Code != http.StatusNotFound || strings.Contains(wd.Body.String(), "do not serve me") {
		t.Errorf("a dirty name handed to the handler directly: got %d %q, want a 404", wd.Code, wd.Body.String())
	}

	// HEAD because GET is (asking whether a fixture exists is a thing a page does), and not POST because
	// there is nothing to write.
	if w := serveMounted(t, s, "HEAD", "/testdata/cases.json", nil); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: got %d with %d bytes, want 200 and no body", w.Code, w.Body.Len())
	}
	if w := serveMounted(t, s, "POST", "/testdata/cases.json", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d, want 405", w.Code)
	}

	// Without a named directory — which is every binary nobody is developing against — there is no mount,
	// and the catch-all answers exactly as it would for any other path.
	plain := New(nil, nil, app, "", nil)
	if w := serveMounted(t, plain, "GET", "/testdata/cases.json", nil); w.Code != http.StatusNotFound {
		t.Errorf("no mount: GET /testdata/cases.json got %d, want the 404 an unknown asset gets", w.Code)
	}
	if w := serveMounted(t, plain, "GET", "/testdata/anything", nil); w.Body.String() != "<html>app" {
		t.Errorf("no mount: GET /testdata/anything got %q, want the entry point", w.Body.String())
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
