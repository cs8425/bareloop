package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"bareloop/internal/asm"
)

// A server that refuses a body does it before reading a token, so the only number that predicts the
// refusal is how many bytes the body would be. These tests hold the two functions here to one rule:
// the number must be the body, not a guess about it.

func sizer(t *testing.T, model string) BodySizer {
	t.Helper()
	p, err := New(Config{BaseURL: "http://127.0.0.1:1/v1", Model: model})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bs, ok := p.(BodySizer)
	if !ok {
		t.Fatalf("%T cannot measure a request body", p)
	}
	return bs
}

func tool(name string) Tool {
	return Tool{Name: name, Description: "one tool", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
}

func TestTheEnvelopeIsTheBodyWithTheConversationErased(t *testing.T) {
	bs := sizer(t, "m")
	sys := asm.Message{Role: "system", Content: strings.Repeat("instructions ", 40)}
	live := Request{Model: "m", Stream: true, Tools: []Tool{tool("fs_read"), tool("fs_grep")},
		Messages: []asm.Message{sys,
			{Role: "user", Content: strings.Repeat("a conversation of some length. ", 200)},
			{Role: "assistant", Content: "thinking hard"},
			{Role: "user", Content: "and then another turn, longer still " + strings.Repeat("z", 5000)}}}
	quiet := live
	quiet.Messages = []asm.Message{sys, {Role: "user", Content: ""}}

	env, err := bs.EnvelopeBytes(live)
	if err != nil {
		t.Fatalf("EnvelopeBytes: %v", err)
	}
	want, err := bs.BodyBytes(quiet)
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	if env != want {
		t.Fatalf("the envelope is %d bytes and the same request with one empty turn is %d: the two must be the same bytes", env, want)
	}
	full, err := bs.BodyBytes(live)
	if err != nil {
		t.Fatalf("BodyBytes(live): %v", err)
	}
	if full <= env {
		t.Fatalf("a body carrying the conversation (%d) is no bigger than the envelope alone (%d)", full, env)
	}
	// And the envelope is the small part of a working window — the reason a caller can cache it and
	// count messages separately.
	if full < 2*env {
		t.Fatalf("the conversation added only %d of %d bytes; the ratio this test is about is gone", full-env, full)
	}
}

func TestTheEnvelopeMovesWithWhatBelongsToIt(t *testing.T) {
	sys := asm.Message{Role: "system", Content: "the session's instructions"}
	base := Request{Model: "m", Stream: true, Messages: []asm.Message{sys}}
	bs := sizer(t, "m")
	env := func(r Request) int {
		t.Helper()
		n, err := bs.EnvelopeBytes(r)
		if err != nil {
			t.Fatalf("EnvelopeBytes: %v", err)
		}
		return n
	}
	floor := env(base)

	// A conversation, however long, is not part of the envelope: that is what makes it cacheable.
	long := base
	long.Messages = []asm.Message{sys, {Role: "user", Content: strings.Repeat("long ", 40000)}}
	if got := env(long); got != floor {
		t.Fatalf("a 200 KB conversation changed the envelope from %d to %d", floor, got)
	}

	// A tool is part of it, schemas and all: enabling one tool is one of the ways a request grows
	// without the conversation doing anything.
	one := base
	one.Tools = []Tool{tool("fs_read")}
	if got := env(one); got <= floor {
		t.Fatalf("adding a tool changed the envelope from %d to %d", floor, got)
	}
	two := one
	two.Tools = []Tool{tool("fs_read"), tool("fs_grep")}
	if got := env(two); got <= env(one) {
		t.Fatalf("the second tool changed nothing: %d then %d", env(one), got)
	}
	// A schema edit that changes no name still changes the wire — which is why the session's cache
	// fingerprint counts schema bytes and not just names.
	wide := one
	wide.Tools = []Tool{{Name: "fs_read", Description: "one tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"deep":{"type":"boolean"}}}`)}}
	if got := env(wide); got == env(one) {
		t.Fatal("a wider schema left the envelope alone")
	}

	// The system prompt rides inside it, and so does everything the endpoint is told beyond the
	// message list.
	other := base
	other.Messages = []asm.Message{{Role: "system", Content: "a longer set of instructions for the session, by far"}}
	if got := env(other); got <= floor {
		t.Fatalf("a longer system prompt changed the envelope from %d to %d", floor, got)
	}
	named := base
	named.Model = "a-model-with-a-longer-name"
	if got := env(named); got <= floor {
		t.Fatalf("a longer model name changed the envelope from %d to %d", floor, got)
	}
	// The base is a streaming request, so the comparison is with the same request buffered:
	// stream_options is on the body and belongs in the envelope.
	buffered := base
	buffered.Stream = false
	if env(buffered) >= floor {
		t.Fatal("stream_options is on the body and must count")
	}
	extra := base
	extra.Extra = map[string]any{"top_k": 40, "mirostat": 2}
	if got := env(extra); got <= floor {
		t.Fatalf("provider-specific fields changed the envelope from %d to %d", floor, got)
	}
}

func TestAnEmptyEndpointStillHasAnEnvelope(t *testing.T) {
	// A request with no tools and no system prompt is not zero bytes: model, stream flag and the one
	// message the protocol insists on are all on the body. A caller reading 0 as "free" would be
	// wrong here, which is why the number is never allowed to be 0.
	n, err := sizer(t, "").EnvelopeBytes(Request{Model: "m", Messages: []asm.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("EnvelopeBytes: %v", err)
	}
	if n < 40 {
		t.Fatalf("the envelope of an empty request is %d bytes: even an empty body names its model, its stream flag and one message", n)
	}
}
