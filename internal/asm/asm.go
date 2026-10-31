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
	// Content is a string, a []ContentPart, or omitted (assistant with only tool_calls).
	Content any `json:"content,omitempty"`
	// ReasoningContent is only sent when Options.SendReasoning is on; the same switch is
	// used by every caller, so the prefix stays identical either way (spec §5.4 #3).
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallOut `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	Name             string        `json:"name,omitempty"`
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
}

// structuredFor is the mode of the server that produced an entry ("" = the session default).
func (o Options) structuredFor(server string) StructuredMode { return o.Structured.For(server) }

// SendReasoningOn is the effective value of SendReasoning: on unless explicitly disabled.
func (o Options) SendReasoningOn() bool { return o.SendReasoning == nil || *o.SendReasoning }

// SendImagesOn is the effective value of SendImages: on unless explicitly disabled.
func (o Options) SendImagesOn() bool { return o.SendImages == nil || *o.SendImages }

// Built is the assembled payload plus the entries it came from.
type Built struct {
	// System is the system head (may be "").
	System string
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
	b := &Built{System: p.System}
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
		m.Content = content(foldStructured(e, opts.structuredFor(e.Server)), opts.SendImagesOn())
		m.ToolCallID = e.ToolCallID
		if !opts.OmitToolResultName && e.Server != "" {
			m.Name = e.Server
		}
	case "assistant":
		if c := content(e, opts.SendImagesOn()); c != nil {
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
		m.Content = content(e, opts.SendImagesOn())
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
func content(e *ir.Entry, sendImages bool) any {
	var imgs []ContentPart
	var texts []string
	for _, pt := range e.Parts {
		switch {
		case pt.Type == ir.PartImage && sendImages:
			imgs = append(imgs, ContentPart{Type: PartTypeImage,
				ImageURL: &ImageURL{URL: DataURL(pt)}})
		case pt.Type == ir.PartImage:
			texts = append(texts, imageNote(pt))
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
func imageNote(p ir.Part) string {
	name := p.Name
	if name == "" {
		name = "image"
	}
	mt := p.MimeType
	if mt == "" {
		mt = "unknown type"
	}
	kb := len(p.Data) * 3 / 4 / 1024 // base64 → real bytes → KB
	return "[attached " + name + " (" + mt + ", ~" + strconv.Itoa(kb) + " KB) is in this conversation " +
		"but its pixels were NOT sent to you: this session sends text only. You cannot see it, so do " +
		"not describe it and do not guess what it shows — say that you cannot see the image and ask " +
		"the user to describe it or to turn image sending on.]"
}

// DataURL renders an image part as a data URL (one encoding, everywhere).
func DataURL(p ir.Part) string {
	mt := p.MimeType
	if mt == "" {
		mt = "application/octet-stream"
	}
	return "data:" + mt + ";base64," + p.Data
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

// Bytes returns the serialised size of the payload (inspector helper column, never a
// primary token estimate — spec §2.4).
func (b *Built) Bytes() int {
	raw, err := json.Marshal(b.All())
	if err != nil {
		return 0
	}
	return len(raw)
}

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
func Estimate(e *ir.Entry) int {
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
	if len(e.Structured) > 0 {
		n += len(e.Structured) / 4
	}
	if n == 0 {
		n = 1
	}
	return n
}
