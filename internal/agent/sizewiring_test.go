package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bareloop/internal/ir"
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

	head := "the system head as the assembler wrote it"
	one := []llm.Tool{{Name: "fs_read", Description: "read a file", Parameters: json.RawMessage(`{"a":1}`)}}
	first := s.envelopeBytes(head, one)
	if first == 0 {
		t.Fatal("the envelope was never measured")
	}
	if again := s.envelopeBytes(head, one); again != first {
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
	if n := s.envelopeBytes(head, two); n <= first {
		t.Fatalf("a second tool left the envelope at %d (it was %d)", n, first)
	}
	if stub.asks != 2 {
		t.Fatalf("changing the tool array measured %d times in total, want 2", stub.asks)
	}

	// So is the system head — the assembled one, which is what the body carries rather than the
	// config field it started from.
	long := strings.Repeat("instructions for the session ", 100)
	if n := s.envelopeBytes(long, two); n <= first {
		t.Fatalf("a 2.9 KB system head left the envelope at %d", n)
	}
	if stub.asks != 3 {
		t.Fatalf("a new system head measured %d times in total, want 3", stub.asks)
	}
	before := stub.asks
	if n := s.envelopeBytes(long, two); n == 0 || stub.asks != before {
		t.Fatalf("the envelope is being measured on every call: %d bytes, asks %d", n, stub.asks)
	}
}

func TestASessionThatCannotMeasureSaysSo(t *testing.T) {
	s, _ := chainFixture(t, "no-sizer", 8192)
	s.provider = &chainProvider{} // a double with no wire format of its own
	if n := s.envelopeBytes("a head", nil); n != 0 {
		t.Fatalf("a provider that cannot measure a body answered %d; 0 is the only honest answer, and it means \"no idea\"", n)
	}
}

// TestTheInspectorReportsLogWeightAndWireApart is the debug surface: the two byte numbers have to be
// visible side by side per message and in total, because their disagreement is the whole story of a
// payload a server refuses.
func TestTheInspectorReportsLogWeightAndWireApart(t *testing.T) {
	s, cp := chainFixture(t, "inspect-bytes", 8192)
	raw := make([]byte, 3000)
	for i := range raw {
		raw[i] = byte('a' + i%26)
	}
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	if err := s.log.PutBlob(sha, "image/png", raw); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if _, err := s.appendEntry(&ir.Entry{Kind: ir.KindUser, Role: "user", Ts: time.Now().UTC(),
		Parts: []ir.Part{{Type: ir.PartImage, MimeType: "image/png", Data: ir.RefPrefix + sha}}}); err != nil {
		t.Fatalf("append: %v", err)
	}

	ins := s.Inspect()
	if len(ins.Messages) != 1 {
		t.Fatalf("the inspector shows %d rows, want the one entry", len(ins.Messages))
	}
	row := ins.Messages[0]
	if row.Bytes != len(ir.RefPrefix+sha) {
		t.Fatalf("row.Bytes = %d, want the reference the log holds (%d)", row.Bytes, len(ir.RefPrefix+sha))
	}
	const wire = len("data:image/png;base64,") + 4*((3000+2)/3)
	if row.Wire != wire {
		t.Fatalf("row.Wire = %d, want the data URL the wire carries (%d)", row.Wire, wire)
	}
	if ins.TotalBytes != row.Bytes || ins.WireBytes != row.Wire {
		t.Fatalf("totals disagree with the row: log %d/%d, wire %d/%d",
			ins.TotalBytes, row.Bytes, ins.WireBytes, row.Wire)
	}
	// Nothing is missing, and that is a property of the store rather than of this table: the log
	// refuses to write an entry whose blob is absent (store_test.go guards it), so an answer here is
	// always 0 for a session's own payload. wireMissing is the tripwire for a payload that arrived by
	// another door — rows imported without their bytes — and its absence is what makes wireBytes an
	// estimate rather than a floor.
	if ins.WireMissing != 0 {
		t.Fatalf("wireMissing = %d on a payload this store wrote itself", ins.WireMissing)
	}
	// A provider with no wire format is not a provider whose body is free.
	if ins.EnvelopeBytes != 0 || ins.RequestBody != 0 {
		t.Fatalf("the fixture provider cannot measure a body and answered envelope %d, request %d",
			ins.EnvelopeBytes, ins.RequestBody)
	}

	// With one that can, the request number is the two halves added — which is the sum the byte
	// ceiling gets cut against.
	s.provider = &envelopeStub{chainProvider: cp}
	ins = s.Inspect()
	if ins.EnvelopeBytes <= 0 {
		t.Fatalf("a measuring provider still reports no envelope")
	}
	if ins.RequestBody != ins.EnvelopeBytes+ins.WireBytes {
		t.Fatalf("requestBytes = %d, envelope %d + wire %d = %d",
			ins.RequestBody, ins.EnvelopeBytes, ins.WireBytes, ins.EnvelopeBytes+ins.WireBytes)
	}
}

// TestTheInspectorCountsWhatItCannotPrice is gone on purpose, and the reason is worth keeping: the
// store refuses to write an entry whose blob is not in it (store.go's guard, guarded in turn by
// store_test.go), so an agent-level fixture cannot produce a dangling reference at all. What is left
// of that case — a resolver that cannot price a reference — is covered where the semantics live, in
// internal/asm's TestWireCountsEveryImageAndRepeatsTheOnesSentTwice and
// TestWithoutAResolverAReferencePricesAtItsWrapper.
