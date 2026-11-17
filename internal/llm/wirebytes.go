package llm

import "bareloop/internal/asm"

// BodySizer is the optional half of Provider that answers "how big would this be?" without anyone
// sending it. It is optional rather than part of Provider because the answer is a property of a wire
// format — only the transport owns that — and a fake provider in a test has no format to speak of;
// asking every one of them to invent a number would manufacture a figure that lies.
//
// The reason it exists at all is 413. A server with a body ceiling refuses a request before it has
// read a token of it, so no token count — nobody's prompt, nobody's estimate — predicts the refusal.
// Only bytes do, and the one code that cannot get bytes wrong is the one that writes them.
type BodySizer interface {
	// BodyBytes is the body this provider would put on the wire for req: the model default
	// resolved, the tool array built, the provider-specific fields merged. Exact, not estimated —
	// it is the same call encodeBody makes before the bytes go out, and it is the number that
	// requestBrief prints when a server answers without a body of its own.
	BodyBytes(req Request) (int, error)
	// EnvelopeBytes is BodyBytes with the conversation taken out: the model name, the sampling
	// fields, the tool array with every schema in it, the system prompt when the payload leads with
	// one, and one empty user turn standing in for "there is always at least one message".
	//
	// It is worth caching per session, because it moves when the system prompt or the tool array
	// moves and for no other reason — a conversation growing by a hundred turns does not touch it.
	// The per-entry sizes (asm.Sizes) count message payloads and no JSON punctuation, so adding an
	// envelope to those sums is the estimate to compare against a byte ceiling; it runs about forty
	// bytes per message light, which is a floor, which is the direction that fails safe.
	EnvelopeBytes(req Request) (int, error)
}

// BodyBytes implements BodySizer over the real encoder. Nothing here assembles a struct of its own:
// an estimate that lives beside the encoder drifts the day a field is added, and this one cannot.
func (p *openai) BodyBytes(req Request) (int, error) {
	body, err := p.encodeBody(req, req.Stream)
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

// EnvelopeBytes implements BodySizer by asking the same encoder about a request whose only message is
// an empty one. The leading system message is kept when the payload has one, because the system
// prompt is the largest thing in the envelope that is nobody's message; a payload that carries its
// instructions inside a user turn has no such leading message and is measured without one.
func (p *openai) EnvelopeBytes(req Request) (int, error) {
	quiet := req
	quiet.Messages = envelopeMessages(req.Messages)
	body, err := p.encodeBody(quiet, req.Stream)
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

// envelopeMessages is the payload with its conversation erased: at most the leading system message,
// then one empty user turn. The empty turn is not subtracted from the total anywhere else, so the
// scaffolding of a minimal message is inside the number — which is the honest direction, since every
// real payload carries at least one message and this one must not claim a body with no messages
// costs the same as a body with one.
func envelopeMessages(msgs []asm.Message) []asm.Message {
	out := make([]asm.Message, 0, 2)
	if len(msgs) > 0 && msgs[0].Role == "system" {
		out = append(out, msgs[0])
	}
	return append(out, asm.Message{Role: "user", Content: ""})
}
