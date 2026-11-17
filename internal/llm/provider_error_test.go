package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"bareloop/internal/asm"
)

// A provider that refuses a request almost never says what it refused: llama.cpp answers an oversized body
// with the status and nothing else, a proxy answers with HTML, and the payload that caused it no longer
// exists by the time anyone reads the line. So every non-2xx carries the shape of what we sent — message
// count, tools, and the size of the finished body — and these tests pin that the sentence is there, in a
// form a person can compare against a limit.

var sentBrief = regexp.MustCompile(`\[\d+ msgs, [\d.]+ (B|KB|MB|GB) sent`)

func refused(t *testing.T, call func() error, tools int) string {
	t.Helper()
	err := call()
	if err == nil {
		t.Fatal("the 413 came back as no error at all")
	}
	if !strings.HasPrefix(err.Error(), "llm: 413 ") {
		t.Fatalf("the retry classifier keys on this prefix, and it is not there: %v", err)
	}
	if !sentBrief.MatchString(err.Error()) {
		t.Fatalf("no message count and byte size in: %v", err)
	}
	if want := ", 1 tools]"; tools > 0 != strings.Contains(err.Error(), want) {
		t.Fatalf("tools sentence %q, want it present=%v in: %v", want, tools > 0, err)
	}
	return err.Error()
}

func TestStreamRefusalNamesTheRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge) // the body is empty on purpose: that is the real answer
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Model:    "m",
		Messages: []asm.Message{{Role: "user", Content: strings.Repeat("x", 4096)}},
		Tools:    []Tool{{Name: "fs_read"}},
	}
	got := refused(t, func() error { _, err := p.Stream(context.Background(), req); return err }, 1)
	// 4096 x's plus the JSON envelope is past a kilobyte: the number is the body's, not the content's.
	if strings.Contains(got, "4096 msgs") {
		t.Fatalf("the count should be messages, not bytes: %v", got)
	}

	// The same request with no tools must not claim any.
	noTools := req
	noTools.Tools = nil
	refused(t, func() error { _, err := p.Stream(context.Background(), noTools); return err }, 0)
}

func TestCompleteRefusalNamesTheRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), Request{
		Model:    "m",
		Messages: []asm.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "go"}},
	})
	if err == nil {
		t.Fatal("the 413 came back as no error at all")
	}
	if !strings.Contains(err.Error(), "2 msgs") {
		t.Fatalf("the summary path counts its own messages, and both are missing: %v", err)
	}
	if !sentBrief.MatchString(err.Error()) {
		t.Fatalf("no message count and byte size in: %v", err)
	}
}

// The size has to be comparable by eye: a request reported as 124151703 bytes is not something anyone
// weighs against a server's cap while deciding what to cut.
func TestHumanBytesReadsAsASize(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{{512, "512 B"}, {2048, "2.0 KB"}, {118 * 1024 * 1024, "118.0 MB"}, {3 * 1024 * 1024 * 1024, "3.0 GB"}} {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
