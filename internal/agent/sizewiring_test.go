package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"bareloop/internal/llm"
	"bareloop/internal/store"
)

// Two byte numbers are wired here: what an image weighs (from the blob table's size column, cached)
// and what the request around the conversation weighs (measured by the transport, cached behind a
// fingerprint). Both exist for the one failure no token number predicts — a server that refuses a
// body before reading it — and both get asked far more often than the conversation changes them,
// which is what the caching is for.

func TestBlobSizesComeFromTheStoreAndAreRemembered(t *testing.T) {
	l, err := store.Open(filepath.Join(t.TempDir(), "blobs"), false)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	raw := []byte("12345")
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	if err := l.PutBlob(sha, "application/octet-stream", raw); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	r := newRefSizes(l)
	if n, ok := r.Size(sha); !ok || n != 5 {
		t.Fatalf("Size = %d,%v, want 5,true — the raw bytes, since the size column is written where the bytes are stored", n, ok)
	}
	// A closed store is the only way to tell the cache from a fresh query: the answer has to keep
	// arriving with nobody left to ask. A sha names its own bytes, so nothing here can go stale.
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if n, ok := r.Size(sha); !ok || n != 5 {
		t.Fatalf("the second ask went looking for the store: Size after closing = %d,%v", n, ok)
	}
	// A sha the store never held is 0,false rather than an error: the size table prices the wrapper
	// around nothing, and ExpandMessages is the code that refuses to send it.
	if n, ok := r.Size(strings.Repeat("f", 64)); ok || n != 0 {
		t.Fatalf("an unknown sha answered %d,%v, want 0,false", n, ok)
	}
}

// envelopeStub measures a request body the way a transport would — by adding up the parts that belong
// in the envelope — and counts how often it was asked. The arithmetic is deliberately not the real
// encoder's, which internal/llm's own test pins; what is under test here is when a session asks, and
// what it is willing to remember.
type envelopeStub struct {
	*chainProvider
	asks int
}

func (p *envelopeStub) BodyBytes(req llm.Request) (int, error) { return 1000 + len(req.Messages), nil }

func (p *envelopeStub) EnvelopeBytes(req llm.Request) (int, error) {
	p.asks++
	n := 100 + len(req.Model)
	for _, m := range req.Messages {
		if m.Role == "system" {
			if text, ok := m.Content.(string); ok {
				n += len(text)
			}
		}
	}
	for _, tool := range req.Tools {
		n += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	return n, nil
}

func TestTheEnvelopeIsMeasuredOnceAndOnlyAgainWhenItMoves(t *testing.T) {
	s, cp := chainFixture(t, "envelope", 8192)
	stub := &envelopeStub{chainProvider: cp}
	s.provider = stub

	one := []llm.Tool{{Name: "fs_read", Description: "read a file", Parameters: json.RawMessage(`{"a":1}`)}}
	first := s.envelopeBytes(one)
	if first == 0 {
		t.Fatal("the envelope was never measured")
	}
	if again := s.envelopeBytes(one); again != first {
		t.Fatalf("the same session measured twice and disagreed: %d then %d", first, again)
	}
	if stub.asks != 1 {
		t.Fatalf("the envelope was measured %d times while nothing moved; one measurement per shape is the whole point", stub.asks)
	}

	// Tools are a reason to ask again: a schema edit moves the wire without moving a name, which is
	// why the fingerprint counts schema bytes.
	two := []llm.Tool{
		{Name: "fs_read", Description: "read a file", Parameters: json.RawMessage(`{"a":1,"b":2}`)},
		{Name: "fs_grep", Description: "search"},
	}
	if n := s.envelopeBytes(two); n <= first {
		t.Fatalf("a second tool left the envelope at %d (it was %d)", n, first)
	}
	if stub.asks != 2 {
		t.Fatalf("changing the tool array measured %d times in total, want 2", stub.asks)
	}

	// The system prompt rides in the envelope, so a config change that touches it moves the number —
	// and moves it through the cache rather than around it.
	if err := s.UpdateConfig(context.Background(), Config{
		SystemPrompt: strings.Repeat("instructions for the session ", 100),
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if n := s.envelopeBytes(two); n <= first {
		t.Fatalf("a 2.9 KB system prompt left the envelope at %d", n)
	}
	if stub.asks != 3 {
		t.Fatalf("a new system prompt measured %d times in total, want 3", stub.asks)
	}
	before := stub.asks
	if n := s.envelopeBytes(two); n == 0 || stub.asks != before {
		t.Fatalf("the envelope is being measured on every call: %d bytes, asks %d", n, stub.asks)
	}
}

func TestASessionThatCannotMeasureSaysSo(t *testing.T) {
	s, _ := chainFixture(t, "no-sizer", 8192)
	s.provider = &chainProvider{} // a double with no wire format of its own
	if n := s.envelopeBytes(nil); n != 0 {
		t.Fatalf("a provider that cannot measure a body answered %d; 0 is the only honest answer, and it means \"no idea\"", n)
	}
}
