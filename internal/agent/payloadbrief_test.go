package agent

import (
	"errors"
	"fmt"
	"testing"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

// What a refused request said about itself, in the log line that outlives it. The provider usually says
// nothing (a 413 arrives with an empty body), so the ids of the entries that were on their way out are the
// only way back to the payload — see the note above payloadBrief for why a range and not a list.
//
// No tool count in any expectation: that number belongs to the provider's half of the sentence, which
// counts the very slice this one was assembled beside.
func TestPayloadBrief(t *testing.T) {
	ents := func(ids ...string) []*ir.Entry {
		out := make([]*ir.Entry, 0, len(ids))
		for _, id := range ids {
			out = append(out, &ir.Entry{ID: id})
		}
		return out
	}
	for _, c := range []struct {
		name  string
		built *asm.Built
		want  string
	}{
		{"nothing to name", nil, ""},
		{"an empty payload", &asm.Built{}, "no entries"},
		{"one entry", &asm.Built{Entries: ents("0000007")}, "entry 0000007 (1)"},
		{"a window onto the path", &asm.Built{Entries: ents("0000003", "0000004", "0000146")}, "entries 0000003–0000146 (3)"},
		{"the head is named", &asm.Built{System: "you are", Entries: ents("0000001", "0000002")}, "entries 0000001–0000002 (2) +system"},
		// A synthesised tool result keeps the messages and entries parallel without having an id of
		// its own; it counts, and it does not pretend to be a range endpoint.
		{"synthetic entries count but are not named", &asm.Built{Entries: []*ir.Entry{{ID: ""}, {ID: "0000002"}, {ID: ""}}}, "entry 0000002 (3)"},
		{"an entry that is nil", &asm.Built{Entries: []*ir.Entry{nil, {ID: "0000009"}}}, "entry 0000009 (2)"},
	} {
		if got := payloadBrief(c.built); got != c.want {
			t.Errorf("%s: payloadBrief = %q, want %q", c.name, got, c.want)
		}
	}
}

// The brief rides on the very error the retry classifier sorts, and the classifier reads a prefix
// ("llm: 413 ") out of a sentence that is now longer and ends elsewhere. Both directions matter: a size
// refusal must not acquire a retry storm, and a busy server must not lose the retries that fix it.
func TestBriefDoesNotChangeHowAFailureSorts(t *testing.T) {
	wrap := func(msg string) error {
		built := &asm.Built{System: "you are", Entries: []*ir.Entry{{ID: "0000003"}, {ID: "0000004"}}}
		return fmt.Errorf("%w [%s]", errors.New(msg), payloadBrief(built))
	}
	oversized := wrap("llm: 413 Request Entity Too Large: (empty body) [142 msgs, 118.4 MB sent, 12 tools]")
	if transientLLMError(oversized) {
		t.Errorf("the same bytes will fail the same way, and the brief did not keep that verdict: %v", oversized)
	}
	if !transientLLMError(wrap("llm: 500 Internal Server Error: waiting for slot")) {
		t.Error("a busy server lost its retries somewhere in the wrapping")
	}
	if !transientLLMError(wrap("Post \"https://example/v1/chat/completions\": dial tcp 127.0.0.1:8080: connect: connection refused")) {
		t.Error("a request that never arrived is still worth another attempt")
	}
}
