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

// ContentPart is an element of a multi-part content array.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
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
}

// SendReasoningOn is the effective value of SendReasoning: on unless explicitly disabled.
func (o Options) SendReasoningOn() bool { return o.SendReasoning == nil || *o.SendReasoning }

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
		m.Content = content(e)
		m.ToolCallID = e.ToolCallID
		if !opts.OmitToolResultName && e.Server != "" {
			m.Name = e.Server
		}
	case "assistant":
		if c := content(e); c != nil {
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
		m.Content = content(e)
	}
	return m
}

// content builds the content value: a plain string when there are no attachments, a
// [images..., text] array when there are. Deterministic, so both the live push and every
// rebuild produce identical bytes.
func content(e *ir.Entry) any {
	var imgs []ContentPart
	var text string
	if len(e.Parts) > 0 {
		var texts []string
		for _, pt := range e.Parts {
			switch pt.Type {
			case ir.PartImage:
				imgs = append(imgs, ContentPart{Type: PartTypeImage,
					ImageURL: &ImageURL{URL: DataURL(pt)}})
			default:
				texts = append(texts, pt.Text)
			}
		}
		text = join(texts, "\n")
	} else {
		text = e.Content
	}
	if len(imgs) == 0 {
		if e.Role == "assistant" && text == "" {
			return nil // omit content for tool-call-only assistant messages
		}
		return text
	}
	parts := make([]ContentPart, 0, len(imgs)+1)
	parts = append(parts, imgs...)
	parts = append(parts, ContentPart{Type: PartTypeText, Text: text})
	return parts
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
	if n == 0 {
		n = 1
	}
	return n
}
