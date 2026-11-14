// Package asm is the ONE assembler that turns "the path" into "the API payload".
//
// Spec §0 design position 2: the main send path, the in-loop rebuild, the summary
// request, the inspector and the rebuild after a reload all call this same function, so
// "what the inspector shows" == "what goes on the wire", and prefix-stability (KV cache)
// holds structurally instead of by careful copy-paste.
//
// Spec §5.4: tool results and user uploads share one parts builder, parts have one fixed
// order ([images..., text]), images use one encoding, and an assistant message that only
// carries tool_calls omits `content` — every rebuild path therefore produces exactly the
// same bytes as the live push.
package asm

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"bareloop/internal/ir"
)

// Content part types (OpenAI-compatible chat content array).
const (
	PartTypeText  = "text"
	PartTypeImage = "image_url"
)

// ImageURL is one image part.
type ImageURL struct {
	URL string `json:"url"`
}

// ContentPart is an element of a multi-part content array. Marshalling is explicit — see
// MarshalJSON: a text part always carries "text", an image part never does.
type ContentPart struct {
	Type     string
	Text     string
	ImageURL *ImageURL
}

// MarshalJSON writes exactly the two shapes a chat server expects.
//
// This exists because strict OpenAI-compatible servers reject a text part whose "text" key was
// left out: llama.cpp's server reads part.at("text") for every part and answers the whole request
// with 500 "key 'text' not found" — which an image with no text used to trigger, because
// `json:"text,omitempty"` dropped the key. Field order is fixed by the structs below, so a rebuild
// produces the same bytes (KV cache prefix).
//
// It is now the fallback, not the highway: an engine that sees MarshalJSONTo prefers it, so this
// method serves builds compiled with GOEXPERIMENT=nojsonv2 — and the golden table pins both
// methods to the same shapes so the escape hatch cannot rot.
func (p ContentPart) MarshalJSON() ([]byte, error) {
	if p.ImageURL != nil {
		return json.Marshal(struct {
			Type     string    `json:"type"`
			ImageURL *ImageURL `json:"image_url"`
		}{PartTypeImage, p.ImageURL})
	}
	typ := p.Type
	if typ == "" {
		typ = PartTypeText
	}
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{typ, p.Text})
}

// MarshalJSONTo is the v2-native twin of MarshalJSON — same shapes, same field order, same
// discriminator (the ImageURL pointer, never the Type string), written token by token into the
// engine's own encoder.
//
// It exists because of what the v1 method costs: handed only MarshalJSON, the v2 engine calls it,
// re-parses its output, reformats every byte for policy compliance and clones the result — per
// part, per turn, forever. That compat tax was the daemon's single largest allocation source.
// MarshalJSONTo streams into the encoder instead: the same bytes, no intermediates.
//
// Escaping is deliberately absent here. The encoder this method receives owns the escaping policy
// (the v1-compat layer of json.Marshal runs the HTML trio plus the JS pair on this build; the
// wire path through jsonv2.Marshal runs nothing beyond JSON's own rules), so this method writes
// logical strings and lets the caller's regime decide their bytes. Hand-escaping anywhere inside
// would double-escape the moment a caller already escapes — the golden table's two columns are
// the fence against exactly that class of bug.
func (p ContentPart) MarshalJSONTo(e *jsontext.Encoder) error {
	if p.ImageURL != nil {
		if err := e.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.String("type")); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.String(PartTypeImage)); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.String("image_url")); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.String("url")); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.String(p.ImageURL.URL)); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.EndObject); err != nil {
			return err
		}
		return e.WriteToken(jsontext.EndObject)
	}
	typ := p.Type
	if typ == "" {
		typ = PartTypeText
	}
	if err := e.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := e.WriteToken(jsontext.String("type")); err != nil {
		return err
	}
	if err := e.WriteToken(jsontext.String(typ)); err != nil {
		return err
	}
	// "text" is emitted unconditionally, exactly like the struct tag above: llama.cpp's 500
	// does not care which engine produced the bytes.
	if err := e.WriteToken(jsontext.String("text")); err != nil {
		return err
	}
	if err := e.WriteToken(jsontext.String(p.Text)); err != nil {
		return err
	}
	return e.WriteToken(jsontext.EndObject)
}

// ToolCallOut is a tool call as it appears in an assistant message.
type ToolCallOut struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the tool name and its JSON arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one chat message on the wire.
type Message struct {
	Role string `json:"role"`
	// Content is a string, a []ContentPart, or nil for omitted (assistant with only
	// tool_calls). Presence is decided by MarshalJSONTo, never by the tag: this build's v2
	// engine unwraps the interface and calls a held "" empty — measured, 2026-11-13 — so
	// `omitempty` silently deleted the key from a tool result that had stored nothing, and
	// a strict provider refused the whole payload ("All non-assistant messages must
	// contain 'content'") on every turn that carried the entry. The tag stays only to
	// spell the compat shape for engines that never reach either method.
	Content any `json:"content,omitempty"`
	// ReasoningContent is only sent when Options.SendReasoning is on; the same switch is
	// used by every caller, so the prefix stays identical either way (spec §5.4 #3).
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallOut `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	Name             string        `json:"name,omitempty"`
}

// checkShape refuses the message shapes a provider is known to punish, at the one place
// that still runs before the request. Which keys a message may carry is a question of its
// shape — role plus which fields are set — not of each field asking to be written: the
// 2026-11-13 dogfood 400 ("All non-assistant messages must contain 'content'") was a per-
// field value test ("set means written") meeting a role rule ("a tool result must carry
// content"), and the field lost. The two shapes that poison a session whole:
//
//   - a tool result with no content at all: with the old tag it reached the wire keyless
//     (the engine read an interface holding "" as empty) and every later turn died with it;
//   - an assistant message pairing tool_calls with an empty-string content — the
//     "tool call + content" combo strict parsers reject, and which the assembler cannot
//     produce (an empty assistant text assembles to nil, omitting the key) — so reaching
//     for it means a caller bypassed the choke point, which is exactly when to refuse.
//
// Empty strings stay legal where the spec says they are (a tool result that says "" is a
// result; a user "" is a message) — the rule protects required keys, not taste. An
// unrecognised role shape-checks as nothing: imported histories are the village's to
// deliver, the provider's to judge.
func (m Message) checkShape() error {
	switch m.Role {
	case "tool":
		if m.Content == nil {
			return fmt.Errorf("asm: tool message (tool_call_id %q) must carry content — an empty result sends \"\"", m.ToolCallID)
		}
		if m.ToolCallID == "" {
			return fmt.Errorf("asm: tool message must carry tool_call_id")
		}
	case "assistant":
		if s, ok := m.Content.(string); ok && s == "" && len(m.ToolCalls) > 0 {
			return fmt.Errorf("asm: assistant message pairs tool_calls with an empty content — say something or send nil")
		}
	case "user", "system":
		if m.Content == nil {
			return fmt.Errorf("asm: %s message must carry content", m.Role)
		}
	}
	return nil
}

// MarshalJSONTo writes a message token by token — the same hand ContentPart learned from
// llama.cpp, extended one level up after the 2026-11-13 dogfood proved the message layer
// had the same disease: what goes into `content` comes out, always. A nil Content is the
// one way to omit the key; an empty string is a value like any other and ships as "".
// Field order is the struct declaration order, so the KV prefix reads the same bytes the
// engine wrote before this method existed — the goldens are what prove "same bytes", not
// this sentence.
func (m Message) MarshalJSONTo(e *jsontext.Encoder) error {
	if err := m.checkShape(); err != nil {
		return err
	}
	if m.Content != nil {
		switch m.Content.(type) {
		case string, []ContentPart:
		default:
			return fmt.Errorf("asm: Message.Content is %T, want string or []ContentPart", m.Content)
		}
	}
	str := func(s string) error { return e.WriteToken(jsontext.String(s)) }
	key := func(k string) error { return str(k) }
	if err := e.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := key("role"); err != nil {
		return err
	}
	if err := str(m.Role); err != nil {
		return err
	}
	if m.Content != nil {
		if err := key("content"); err != nil {
			return err
		}
		switch c := m.Content.(type) {
		case string:
			if err := str(c); err != nil {
				return err
			}
		case []ContentPart:
			if err := e.WriteToken(jsontext.BeginArray); err != nil {
				return err
			}
			for _, p := range c {
				if err := p.MarshalJSONTo(e); err != nil {
					return err
				}
			}
			if err := e.WriteToken(jsontext.EndArray); err != nil {
				return err
			}
		}
	}
	if m.ReasoningContent != "" {
		if err := key("reasoning_content"); err != nil {
			return err
		}
		if err := str(m.ReasoningContent); err != nil {
			return err
		}
	}
	if len(m.ToolCalls) > 0 {
		if err := key("tool_calls"); err != nil {
			return err
		}
		if err := e.WriteToken(jsontext.BeginArray); err != nil {
			return err
		}
		for _, tc := range m.ToolCalls {
			if err := e.WriteToken(jsontext.BeginObject); err != nil {
				return err
			}
			if err := key("id"); err != nil {
				return err
			}
			if err := str(tc.ID); err != nil {
				return err
			}
			if err := key("type"); err != nil {
				return err
			}
			if err := str(tc.Type); err != nil {
				return err
			}
			if err := key("function"); err != nil {
				return err
			}
			if err := e.WriteToken(jsontext.BeginObject); err != nil {
				return err
			}
			if err := key("name"); err != nil {
				return err
			}
			if err := str(tc.Function.Name); err != nil {
				return err
			}
			if err := key("arguments"); err != nil {
				return err
			}
			if err := str(tc.Function.Arguments); err != nil {
				return err
			}
			if err := e.WriteToken(jsontext.EndObject); err != nil {
				return err
			}
			if err := e.WriteToken(jsontext.EndObject); err != nil {
				return err
			}
		}
		if err := e.WriteToken(jsontext.EndArray); err != nil {
			return err
		}
	}
	if m.ToolCallID != "" {
		if err := key("tool_call_id"); err != nil {
			return err
		}
		if err := str(m.ToolCallID); err != nil {
			return err
		}
	}
	if m.Name != "" {
		if err := key("name"); err != nil {
			return err
		}
		if err := str(m.Name); err != nil {
			return err
		}
	}
	return e.WriteToken(jsontext.EndObject)
}

// MarshalJSON is the v1 twin of MarshalJSONTo — the shape a GOEXPERIMENT=nojsonv2 build
// writes, and the method every v1-API caller sees on any build. Content's presence is
// branched in code here too rather than left to a tag: the fallback exists to be the
// boringly explicit answer, and a type whose whole contract is "what goes in comes out"
// may not reopen that question through omitempty on its way out of a struct literal.
// The golden table pins both twins to the same shapes so neither route can rot.
func (m Message) MarshalJSON() ([]byte, error) {
	if err := m.checkShape(); err != nil {
		return nil, err
	}
	if m.Content == nil {
		return json.Marshal(struct {
			Role             string        `json:"role"`
			ReasoningContent string        `json:"reasoning_content,omitempty"`
			ToolCalls        []ToolCallOut `json:"tool_calls,omitempty"`
			ToolCallID       string        `json:"tool_call_id,omitempty"`
			Name             string        `json:"name,omitempty"`
		}{m.Role, m.ReasoningContent, m.ToolCalls, m.ToolCallID, m.Name})
	}
	return json.Marshal(struct {
		Role             string        `json:"role"`
		Content          any           `json:"content"`
		ReasoningContent string        `json:"reasoning_content,omitempty"`
		ToolCalls        []ToolCallOut `json:"tool_calls,omitempty"`
		ToolCallID       string        `json:"tool_call_id,omitempty"`
		Name             string        `json:"name,omitempty"`
	}{m.Role, m.Content, m.ReasoningContent, m.ToolCalls, m.ToolCallID, m.Name})
}

// Blobs resolves the image references a session's entries carry: an image part whose
// data is "blob:<sha>" (ir.RefPrefix) names bytes that live in the store's blob table,
// not in the entry. The session injects an implementation over its log so the resident
// window can hold references and pay for the bytes only at the two moments that need
// them — pricing a text-only note (Size) and putting pixels on the wire (Get).
type Blobs interface {
	// Size is one blob's raw byte count. The text-only note quotes the attachment's
	// KB from the base64 it was always counted out of, and this answers without
	// moving any bytes.
	Size(sha string) (int, error)
	// Get is one blob's base64 (standard encoding, the one inflate has always
	// emitted) and the mime it was stored under ("" when the part carried none).
	Get(sha string) (b64, mime string, err error)
}

// Options controls the assembler. It is derived from session config and is identical for
// every consumer (main send, summary call, inspector, rebuild). A nil pointer means
// "not set in config.json", i.e. the documented default.
type Options struct {
	// SendReasoning re-sends the model's reasoning channel on the next turn. Default on, so
	// a reasoning-capable model keeps seeing its own thinking.
	SendReasoning *bool `json:"sendReasoning,omitempty"`
	// OmitToolResultName skips the `name` field on tool results.
	OmitToolResultName bool `json:"omitToolResultName,omitempty"`
	// SendImages puts image attachments on the wire as data URLs. Default on. Turn it off when
	// the model cannot see (a text-only llama.cpp has no projector loaded): the base64 then costs
	// neither the request nor the context, and the message says the image is there instead.
	SendImages *bool `json:"sendImages,omitempty"`
	// Structured decides what a tool's structuredContent does on the way to the model.
	// It is injected by the session from its own config (the session default plus the per-server
	// values in mcp[]), and it is deliberately not a key of the asm block: the servers live in the
	// mcp list, and Assemble must stay a pure function of (Payload, Options) like everything else here.
	// It is a pointer so that Options stays comparable — manager.Create still asks whether a config
	// carried an asm block at all by comparing against the zero Options — and nil simply means auto.
	Structured *StructuredTargets `json:"-"`
	// StructuredMaxBytes is how many bytes of a tool's structuredContent the model is shown before the
	// rest is cut off, with a note saying how much of it there was. 0 means DefaultStructuredMaxBytes.
	//
	// It sits beside the modes rather than inside the asm block because it belongs to the same
	// question — what is this field allowed to cost on the wire — and because it is session-wide by
	// design: a ceiling per server would be a second number for whoever is counting tokens to explain.
	// The tag is "-" for the same reason Structured's is, and with the same consequence: the only wire
	// spelling of this number is the session's own top-level field. A key inside "asm" would be
	// inherited whole by defaults.asm and then overwritten by cfgOptions a moment later — two doors,
	// one of them a lie about which one is in force. ValidateStructuredMax is what refuses a
	// nonsensical value before it is ever stored.
	StructuredMaxBytes int `json:"-"`
	// Blobs resolves image references while assembling (see imageNote). It is injected
	// by the session, never spelled in config.json — which is also why it is an
	// interface and not a store: the assembler stays a pure function of (Payload,
	// Options) over the village's types. A nil Blobs assembles b64-carrying parts
	// exactly as it always did; a reference appearing without one says so in its own
	// note rather than inventing a size.
	Blobs Blobs `json:"-"`
}

// structuredFor is the mode of the server that produced an entry ("" = the session default).
func (o Options) structuredFor(server string) StructuredMode { return o.Structured.For(server) }

// structuredMax is the byte ceiling in force for this payload.
//
// A negative number cannot reach here through the API, because ValidateStructuredMax refuses it when
// the config is written; a hand-edited config.json can still carry one, and it is read as the default
// rather than as "no limit". That choice is deliberate: the direction that mistake travels in is ten
// megabytes of context, which is the exact thing this ceiling exists to prevent.
func (o Options) structuredMax() int {
	if o.StructuredMaxBytes > 0 {
		return o.StructuredMaxBytes
	}
	return DefaultStructuredMaxBytes
}

// SendReasoningOn is the effective value of SendReasoning: on unless explicitly disabled.
func (o Options) SendReasoningOn() bool { return o.SendReasoning == nil || *o.SendReasoning }

// SendImagesOn is the effective value of SendImages: on unless explicitly disabled.
func (o Options) SendImagesOn() bool { return o.SendImages == nil || *o.SendImages }

// Built is the assembled payload plus the entries it came from.
type Built struct {
	// System is the system head (may be "").
	System string
	// StructuredCap is the byte ceiling this payload was assembled with. It rides on the result so that
	// a reader holding only the payload — DeriveSizes — counts the bytes the wire will actually carry.
	// Without it the size table prices a tool's JSON at its stored size, and every decision built on
	// that table (the pre-send guard, the compaction cut) charges a capped twenty kilobytes for what
	// the server really sent.
	StructuredCap int
	// Messages are the non-system messages, parallel to Entries.
	Messages []Message
	// Entries are the payload entries in send order. The compaction entry (summary) is
	// spliced in as the first entry with role user.
	Entries []*ir.Entry
	// Summary is the active compaction entry (nil when none).
	Summary *ir.Entry
	// Synthetic counts tool results synthesised to keep the payload valid.
	Synthetic int
}

// All returns the full messages array that goes on the wire (system head first).
func (b *Built) All() []Message {
	out := make([]Message, 0, len(b.Messages)+1)
	if b.System != "" {
		out = append(out, Message{Role: "system", Content: b.System})
	}
	return append(out, b.Messages...)
}

// EntryIDs names the entry behind each message of the payload: a fingerprint of the system prompt
// first when there is one, then one id per Built.Entries.
//
// It is the payload's own description of what it carries, which is the only way a later request can ask
// "is what I would like to reuse still the front of the path?" — a question no size can answer, because
// two different paths can be exactly as long. The system head is a fingerprint rather than an id because
// editing the system prompt changes every byte of the prompt while adding no entry at all, and a caller
// comparing ids would call that a prefix.
//
// Entries and Messages are parallel by construction (Assemble appends them in step; repair keeps them
// parallel when it drops orphans and when it synthesises a result), so this is a mapping and not a
// lookup — the same fact the compaction chain cuts its parts with.
func EntryIDs(b *Built) []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.Entries)+1)
	if b.System != "" {
		out = append(out, fingerprint(b.System))
	}
	for _, e := range b.Entries {
		if e == nil {
			out = append(out, "")
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// HeadLen is the counterpart of EntryIDs: how many of a payload's leading messages are not entries.
// One with a system head, zero without, and that is the whole rule — which is exactly why it lives
// here instead of being re-derived by subtracting an entry count from a message count somewhere else.
// Subtracting is what let a payload that was a prefix of the path — so shorter than the path, so
// holding fewer entries than the path has — read as a payload with no system head, and shift every
// slice of every part one message to the left: the summariser was then handed the system prompt where
// the conversation should have been, while the log kept crediting it with reading thousands of tokens.
func HeadLen(ids []string) int {
	if len(ids) > 0 && strings.HasPrefix(ids[0], systemPrefix) {
		return 1
	}
	return 0
}

// systemPrefix marks the one id of a payload that is not an entry id.
const systemPrefix = "system:"

// fingerprint is FNV-1a, because it needs no import and an entry id is not a security decision.
func fingerprint(s string) string {
	const prime = uint64(1099511628211)
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return systemPrefix + strconv.FormatUint(h, 16)
}

// Payload is the assembler input: "the effective path".
type Payload struct {
	System string
	// Kept is the verbatim suffix (without the compaction entry).
	Kept []*ir.Entry
	// Summary is the active compaction entry; its content is sent with role user.
	Summary *ir.Entry
}

// Assemble is the choke point. It must stay a pure function of (Payload, Options).
func Assemble(p Payload, opts Options) *Built {
	b := &Built{System: p.System, StructuredCap: opts.structuredMax()}
	if p.Summary != nil {
		s := *p.Summary
		s.Role = "user" // spec §3.7: the summary travels with the user role
		b.Summary = &s
		b.Entries = append(b.Entries, &s)
		b.Messages = append(b.Messages, messageFor(&s, opts))
	}
	for _, e := range p.Kept {
		b.Entries = append(b.Entries, e)
		b.Messages = append(b.Messages, messageFor(e, opts))
	}
	b.Synthetic = repair(b)
	return b
}

// messageFor converts one entry. role=tool and role=user share the parts builder (§5.4 #1).
func messageFor(e *ir.Entry, opts Options) Message {
	m := Message{Role: e.Role}
	switch e.Role {
	case "tool":
		// The structuredContent fold happens here and nowhere else: it is a tool-result field, so an
		// entry of another role that carries one is left alone rather than guessed at.
		m.Content = content(foldStructured(e, opts.structuredFor(e.Server), opts.structuredMax()), opts)
		m.ToolCallID = e.ToolCallID
		if !opts.OmitToolResultName && e.Server != "" {
			m.Name = e.Server
		}
	case "assistant":
		if c := content(e, opts); c != nil {
			m.Content = c
		}
		for _, tc := range e.ToolCalls {
			args := string(tc.Args)
			if args == "" {
				args = "{}"
			}
			m.ToolCalls = append(m.ToolCalls, ToolCallOut{
				ID:       tc.ID,
				Type:     "function",
				Function: FunctionCall{Name: tc.Name, Arguments: args},
			})
		}
		if opts.SendReasoningOn() && e.Reasoning != "" {
			m.ReasoningContent = e.Reasoning
		}
	default: // user
		m.Content = content(e, opts)
	}
	return m
}

// content builds the content value: a plain string when there is no image on the wire, a
// [images..., text] array when there is. Deterministic, so both the live push and every
// rebuild produce identical bytes.
//
// Three rules that were missing:
//
//   - the entry's own text survives next to an attachment. A user message with an image still
//     says what was asked, and a tool result still carries its text; `Parts` used to replace
//     `Content`, so "你看到了啥?" simply never reached the model.
//   - there is never an empty text part. Strict parsers throw on it, and it carries no
//     information anyway.
//   - when images are switched off the bytes are not sent at all, but the model is told an image
//     is there — otherwise it answers a question about a picture it was never given, silently.
func content(e *ir.Entry, opts Options) any {
	var imgs []ContentPart
	var texts []string
	for _, pt := range e.Parts {
		switch {
		case pt.Type == ir.PartImage && opts.SendImagesOn():
			imgs = append(imgs, ContentPart{Type: PartTypeImage,
				ImageURL: &ImageURL{URL: imageURL(pt, opts)}})
		case pt.Type == ir.PartImage:
			texts = append(texts, imageNote(pt, opts.Blobs))
		default:
			if pt.Text != "" {
				texts = append(texts, pt.Text)
			}
		}
	}
	text := e.Content
	if extra := join(texts, "\n"); extra != "" && extra != text {
		if text != "" {
			text += "\n"
		}
		text += extra
	}
	if len(imgs) == 0 {
		if e.Role == "assistant" && text == "" {
			return nil // omit content for tool-call-only assistant messages
		}
		if text == "" {
			// Policy, not presence: Message's hand-written marshal and checkShape already keep the
			// key on the wire (the 2026-11-13 dogfood 400 was the engine's omitempty deleting it from
			// an interface holding ""; the probe measured the drop, the llama.cpp replay measured
			// that a present "" answers 200). Empty is still not sent as empty, because a model
			// handed a content-free tool result reads the silence as "no answer yet" and re-calls
			// the tool — and other gateways, unlike the one measured here, may truthiness-check the
			// field and read "" as the missing key all over again. So the emptiness is said in
			// words, as the synthesised result and the image note already are — which words depends
			// on who was empty, because each sentence says something true.
			if e.Role == "tool" {
				return "[empty result — the tool returned no output]"
			}
			return "[empty message]"
		}
		return text
	}
	parts := make([]ContentPart, 0, len(imgs)+1)
	parts = append(parts, imgs...)
	if text != "" {
		parts = append(parts, ContentPart{Type: PartTypeText, Text: text})
	}
	return parts
}

// imageNote is what an image looks like to a text-only model: one line naming it, no base64.
//
// The wording is the whole point. A note that only mentions that a picture was here gets answered
// with an invention — a chatty model would rather describe an image than admit it cannot see one,
// which is exactly what happened in a real session: it produced a confident four-point description
// of pixels that were never sent, while its own reasoning noticed the note and overruled it.
//
// The quoted KB is counted out of the base64, and a reference part does not carry it: the size is
// asked of the resolver and worked back through the standard encoding's exact length, so the note
// says the same number byte for byte whether the entry travels with bytes or with a reference —
// the KV prefix cannot tell the two shapes apart. A size nobody can answer (no resolver, a blob
// that is not there) is said as "?", never as a number that was never measured.
func imageNote(p ir.Part, blobs Blobs) string {
	name := p.Name
	if name == "" {
		name = "image"
	}
	mt := p.MimeType
	if mt == "" {
		mt = "unknown type"
	}
	return "[attached " + name + " (" + mt + ", ~" + kbWord(p, blobs) + " KB) is in this conversation " +
		"but its pixels were NOT sent to you: this session sends text only. You cannot see it, so do " +
		"not describe it and do not guess what it shows — say that you cannot see the image and ask " +
		"the user to describe it or to turn image sending on.]"
}

// kbWord is the "~N KB" number of the note: the base64's length run through the
// formula this file has always used, whether the part carries that base64 or names
// it in the blob table. Standard base64 of n bytes is exactly 4*((n+2)/3) long, so
// the reference path arrives at the identical digits — the length is what the old
// formula divided, and this hands it the same number.
func kbWord(p ir.Part, blobs Blobs) string {
	if sha, isRef := strings.CutPrefix(p.Data, ir.RefPrefix); isRef {
		if blobs == nil {
			return "?"
		}
		size, err := blobs.Size(sha)
		if err != nil {
			return "?"
		}
		b64Len := 4 * ((size + 2) / 3)
		return strconv.Itoa(b64Len * 3 / 4 / 1024)
	}
	return strconv.Itoa(len(p.Data) * 3 / 4 / 1024) // base64 → real bytes → KB
}

// imageURL is an image part's wire spelling while the payload is being assembled: the
// data URL its bytes always travelled as, or — for a reference part — the reference
// itself, kept verbatim as a placeholder. Assembling over references therefore reads
// no blob at all; the bytes are fetched at the one moment they are owed,
// by ExpandMessages, which swaps the placeholder for the identical data URL.
func imageURL(p ir.Part, opts Options) string {
	if _, isRef := strings.CutPrefix(p.Data, ir.RefPrefix); isRef {
		return p.Data
	}
	return DataURL(p)
}

// dataURL is the one formula an image's wire spelling takes.
func dataURL(mime, b64 string) string {
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + b64
}

// DataURL renders an image part as a data URL (one encoding, everywhere).
func DataURL(p ir.Part) string {
	return dataURL(p.MimeType, p.Data)
}

// ExpandMessages returns the wire-ready copy of msgs: every image URL that is still a
// blob reference (ir.RefPrefix) is replaced by the data URL its bytes spell — the very
// string DataURL produced when the bytes were carried in hand, byte for byte, so a
// session's KV prefix cannot tell a reference-built payload from a bytes-built one.
// Messages without references are returned as they stand, and nothing is copied until
// the first reference appears.
//
// A reference whose blob cannot be read is an error, never a silent drop: a payload
// that quietly lost a picture is a different conversation than the one the cached
// prefix was measured over, and sending one would spend the cache to ask about the
// wrong pixels.
func ExpandMessages(msgs []Message, res Blobs) ([]Message, error) {
	expanded := make([]Message, len(msgs))
	copy(expanded, msgs)
	for i := range expanded {
		parts, ok := expanded[i].Content.([]ContentPart)
		if !ok {
			continue // a plain string (or nothing) carries no image URL to expand
		}
		hasRef := false
		for _, p := range parts {
			if p.ImageURL != nil && strings.HasPrefix(p.ImageURL.URL, ir.RefPrefix) {
				hasRef = true
				break
			}
		}
		if !hasRef {
			continue
		}
		parts = append([]ContentPart(nil), parts...) // this message's parts are ours to rewrite
		for j := range parts {
			p := &parts[j]
			if p.ImageURL == nil {
				continue
			}
			sha, isRef := strings.CutPrefix(p.ImageURL.URL, ir.RefPrefix)
			if !isRef {
				continue
			}
			if res == nil {
				return nil, fmt.Errorf("asm: image reference %.20s… with no resolver to expand it", sha)
			}
			b64, mime, err := res.Get(sha)
			if err != nil {
				return nil, fmt.Errorf("asm: image blob %s cannot be sent: %w", sha, err)
			}
			p.ImageURL = &ImageURL{URL: dataURL(mime, b64)}
		}
		expanded[i].Content = parts
	}
	return expanded, nil
}

// repair makes the payload always sendable, deterministically:
//   - drop tool results whose tool call is not visible in this payload (orphan tool
//     messages are rejected by the API/template, spec §2.2)
//   - synthesise a tool result for an answered-then-dangling tool call that sits in the
//     middle of the payload, so the model never sees an unanswered call followed by more
//     messages
func repair(b *Built) int {
	calls := map[string]bool{}
	for _, e := range b.Entries {
		for _, tc := range e.ToolCalls {
			calls[tc.ID] = true
		}
	}
	answered := map[string]bool{}
	for i, e := range b.Entries {
		if e.Role == "tool" && e.ToolCallID != "" {
			answered[e.ToolCallID] = true
		}
		_ = i
	}
	synth := 0
	outEntries := make([]*ir.Entry, 0, len(b.Entries))
	outMsgs := make([]Message, 0, len(b.Messages))
	for i, e := range b.Entries {
		if e.Role == "tool" && !calls[e.ToolCallID] {
			continue // orphan
		}
		if unsendableAssistant(e) {
			continue // nothing a provider would accept (see unsendableAssistant)
		}
		outEntries = append(outEntries, e)
		outMsgs = append(outMsgs, b.Messages[i])
		if e.Role != "assistant" || len(e.ToolCalls) == 0 {
			continue
		}
		// Does anything follow this assistant message?
		last := i == len(b.Entries)-1
		for _, tc := range e.ToolCalls {
			if answered[tc.ID] {
				continue
			}
			if last {
				continue // trailing unanswered call (interrupted turn) is legal
			}
			// Insert a synthetic result right after the assistant message.
			syn := &ir.Entry{
				ID:         e.ID + "#" + tc.ID,
				Kind:       ir.KindTool,
				Role:       "tool",
				ToolCallID: tc.ID,
				Tool:       tc.Name,
				Server:     tc.Server,
				Content:    "[no result recorded — turn was interrupted]",
				IsError:    true,
			}
			outEntries = append(outEntries, syn)
			outMsgs = append(outMsgs, messageFor(syn, Options{}))
			synth++
		}
	}
	b.Entries, b.Messages = outEntries, outMsgs
	return synth
}

// unsendableAssistant reports an assistant message a provider will refuse outright: no content, no
// tool_calls and no image parts. An interrupted turn leaves exactly that -- reasoning only -- and
// reasoning is not message content, so sending it back is a guaranteed 400 that then poisons every
// later turn of the session. Dropping it here changes only what goes on the wire: the entry stays
// in the log and in the UI, where the partial reasoning is still worth reading. Same defence as the
// synthesised tool result for an unanswered call above, seen from the other side.
func unsendableAssistant(e *ir.Entry) bool {
	if e == nil || e.Role != "assistant" || len(e.ToolCalls) > 0 {
		return false
	}
	if strings.TrimSpace(e.Content) != "" {
		return false
	}
	for _, p := range e.Parts {
		if p.Type == ir.PartImage || strings.TrimSpace(p.Text) != "" {
			return false
		}
	}
	return true
}

// join is the string joiner of this file.
func join(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}

// Estimate is the bytes/4 fallback size for a message with no real server data (spec §2.4:
// only for the unsent tail). Attachments use fixed reference costs: image ~1000.
//
// structuredMax is the ceiling this payload's assembler will apply to a tool's structuredContent; 0
// asks for the default. It is a parameter instead of a constant because the estimate has to answer
// the same question the assembler answers — what can this entry cost on the wire — and that answer
// now depends on what the session asked to be shown.
func Estimate(e *ir.Entry, structuredMax int) int {
	if e == nil {
		return 0
	}
	n := utf8.RuneCountInString(e.Content) / 4
	if n == 0 && e.Content != "" {
		n = 1
	}
	for _, tc := range e.ToolCalls {
		n += (len(tc.Name) + len(tc.Args) + 8) / 4
	}
	for _, p := range e.Parts {
		if p.Type == ir.PartImage {
			n += 1000
		} else {
			n += len(p.Text) / 4
		}
	}
	// A tool's structuredContent is counted whether or not this session's mode sends it: the
	// estimate is a ceiling on what the entry can cost, and under-counting a 200 KB JSON is how a
	// request reaches the pre-send guard instead of the compaction trigger that would have made
	// room for it. Over-counting under "drop" only makes the unsent tail look bigger than it is.
	//
	// What it counts is the ceiling the assembler actually honours, not the stored document: the fold
	// cannot put more than the cap plus its note on the wire, so pricing ten megabytes here would not
	// be conservative, it would be false — a session holding one huge result would read as a
	// multi-million-token session, and after a restart (which forgets the real usage) its first send
	// would be refused by a guard costing a payload that is really the cap.
	if wire := structuredWireBytes(len(e.Structured), structuredMax); wire > 0 {
		n += wire / 4
	}
	if n == 0 {
		n = 1
	}
	return n
}
