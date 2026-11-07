package xfer

// The webui dialect: the JSONL that llama.cpp's web UI exports (harness "llama.app").
// Reading it is a mapping exercise, not a conversion project — its tree of messages
// (parent/children/currNode) is the same shape bareloop stores, tool calls and results
// have their places, reasoning and image attachments have their places. What does not
// survive the trip is the UI's accounting (its timings split the same prompt the way its
// server measured it, and bareloop's Usage has one semantics for that, so no numbers are
// invented) and its compaction dialect (the `kept` integer means "steps kept after the
// summary" and is nobody else's BoundaryID): an imported conversation grows bareloop's own
// compaction entry the first time its compactor fires, which beats forging a summary
// boundary from a number we cannot verify. The prose of the summary itself, if any, rides
// along as a plain user message — the history keeps its words and loses no claims.
//
// What the adapter refuses rather than guesses: unknown roles (a message with no bareloop
// kind would poison the path the model sees). Parents that point at rows this adapter
// skipped are bypassed — a grandchild keeps the closest kept ancestor.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bareloop/internal/ir"
)

// webRecord covers both line shapes: line 1 carries the session fields flat
// (type:"session"), every later line nests a message ({type:"message", message:{...}}).
// Unknown record types are skipped, which is also what the upstream importer does with
// them — forward compatibility by agreement, not by accident.
type webRecord struct {
	Harness  string      `json:"harness"`
	Type     string      `json:"type"`
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	CurrNode string      `json:"currNode"`
	Message  *webMessage `json:"message"`
}

type webMessage struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"` // "root" | "message"
	Role             string          `json:"role"` // user | assistant | tool
	Content          string          `json:"content"`
	Parent           string          `json:"parent"`
	Timestamp        int64           `json:"timestamp"` // milliseconds
	ReasoningContent string          `json:"reasoningContent"`
	Model            string          `json:"model"`
	ToolCalls        json.RawMessage `json:"toolCalls"` // array, or its JSON string
	ToolCallID       string          `json:"toolCallId"`
	Extra            []webExtra      `json:"extra"`
}

type webExtra struct {
	Base64URL string `json:"base64Url"`
	Name      string `json:"name"`
	Type      string `json:"type"` // "IMAGE", ...
}

// DecodeWebUI reads a llama-ui export into the same Import shape the native reader
// produces. Config is always nil; System carries the conversation's system root, which
// the route seeds into the new session's system prompt.
func DecodeWebUI(r io.Reader) (*Import, error) {
	dec := json.NewDecoder(r)
	imp := &Import{}
	byID := map[string]*ir.Entry{}
	var order []*ir.Entry
	var lastMessage string
	skipped := map[string]string{} // dropped row id → its parent, for the bypass walk
	var currNode string

	for n := 1; ; n++ {
		var rec webRecord
		err := dec.Decode(&rec)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("xfer: webui line %d: %w", n, err)
		}
		switch rec.Type {
		case "session":
			if currNode != "" || imp.Name != "" || len(order) > 0 {
				// Upstream concatenates whole conversations into one file; reading only
				// the first segments and quietly keeping the last would be a lie, and so
				// would merging them. One file, one session, until merge semantics exist.
				return nil, errors.New("xfer: webui file carries a second session header; import one conversation at a time")
			}
			imp.Name = rec.Name
			currNode = rec.CurrNode
			continue
		case "message":
			// handled below
		default:
			continue // a record type this build has no place for
		}
		m := rec.Message
		if m == nil || m.ID == "" {
			continue
		}
		// The system root is the conversation's prompt, not a turn: it seeds config,
		// it never joins the path. (The root row's role is "system"; naming it is what
		// type:"root" is for.)
		if m.Type == "root" {
			if imp.System == "" {
				imp.System = m.Content
			}
			skipped[m.ID] = m.Parent
			continue
		}
		// The inner message type is not the gate — real files say "text" (and older
		// ones say nothing); the role below is what decides, because a role with no
		// bareloop kind is the only shape that cannot be placed.
		e, ok := webEntry(m, n)
		if !ok {
			skipped[m.ID] = m.Parent
			continue
		}
		if _, dup := byID[e.ID]; dup {
			return nil, fmt.Errorf("xfer: webui line %d: duplicate message id %q", n, e.ID)
		}
		byID[e.ID] = e
		order = append(order, e)
		lastMessage = e.ID
	}
	if len(order) == 0 {
		return nil, errors.New("xfer: webui file carries no importable messages")
	}
	// Parents that were skipped point at dropped rows; the nearest kept ancestor takes
	// over the edge. (In practice this is one hop — the system root — but the walk makes
	// a chain of skips a reshape instead of a breakage.)
	for _, e := range order {
		for hop := 0; e.ParentID != ""; hop++ {
			if _, kept := byID[e.ParentID]; kept {
				break
			}
			next, was := skipped[e.ParentID]
			if !was || hop > len(order) {
				return nil, fmt.Errorf("xfer: webui message %q names parent %q the file never carries", e.ID, e.ParentID)
			}
			e.ParentID = next
		}
	}
	tip := currNode
	if _, ok := byID[tip]; !ok {
		tip = lastMessage // a file without a usable currNode stands on its newest row
	}
	imp.Tip = tip
	imp.Entries = order
	return imp, nil
}

// webEntry maps one message row; ok=false is the skip verdict (roles with no kind).
func webEntry(m *webMessage, line int) (*ir.Entry, bool) {
	e := &ir.Entry{
		ID:         m.ID,
		ParentID:   m.Parent,
		Content:    m.Content,
		Reasoning:  m.ReasoningContent,
		ToolCallID: m.ToolCallID,
		Model:      m.Model,
	}
	if m.Timestamp > 0 {
		e.Ts = time.UnixMilli(m.Timestamp).UTC()
	}
	switch m.Role {
	case "user":
		e.Kind, e.Role = ir.KindUser, "user"
	case "assistant":
		e.Kind, e.Role = ir.KindAssistant, "assistant"
	case "tool":
		e.Kind, e.Role = ir.KindTool, "tool"
	default:
		return nil, false
	}
	if len(m.ToolCalls) > 0 {
		calls, err := webToolCalls(m.ToolCalls, line)
		if err == nil {
			e.ToolCalls = calls
		}
		// A call list that will not parse loses its structure, not its row: the content
		// is the body of the message, and an unreadable call shape beside it is decoration.
	}
	for _, x := range m.Extra {
		if x.Type != "IMAGE" {
			continue // attachments without a bareloop part (pdf/text/audio/video) stay out of v1
		}
		mime, data, ok := splitDataURL(x.Base64URL)
		if !ok {
			continue
		}
		e.Parts = append(e.Parts, ir.Part{Type: ir.PartImage, MimeType: mime, Data: data, Name: x.Name})
	}
	return e, true
}

// webToolCalls unwraps the OpenAI nesting (and the upstream habit of carrying toolCalls
// as a JSON string): every element {id, type:"function", function:{name, arguments}},
// where arguments is itself a JSON string.
func webToolCalls(raw json.RawMessage, line int) ([]ir.ToolCall, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("xfer: webui line %d: toolCalls string: %w", line, err)
		}
		raw = json.RawMessage(s)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil // "" and null mean "no calls", which is not a parse failure
	}
	if raw[0] != '[' {
		return nil, fmt.Errorf("xfer: webui line %d: toolCalls is neither a JSON string nor an array", line)
	}
	var nested []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, fmt.Errorf("xfer: webui line %d: toolCalls: %w", line, err)
	}
	out := make([]ir.ToolCall, 0, len(nested))
	for i, c := range nested {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("imported-%d-%d", line, i) // tool rows pair on this; none is worse than a stray
		}
		out = append(out, ir.ToolCall{ID: id, Name: c.Function.Name, Args: webArgs(c.Function.Arguments)})
	}
	return out, nil
}

// webArgs settles the double-encoding question: arguments arrives as a JSON string
// wrapping the real JSON ("{\"a\":1}"); a dialect that already unwrapped it once (raw
// object) is kept; anything that is not valid JSON on either layer becomes {} — an empty
// argument object the tool can fail loudly over, which beats poisoning the log.
func webArgs(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		return json.RawMessage("{}")
	}
	if json.Valid(raw) {
		return raw
	}
	return json.RawMessage("{}")
}

// splitDataURL takes "data:<mime>;base64,<b64>" apart; bareloop stores the mime and the
// base64 in separate fields, with no prefix on either.
func splitDataURL(u string) (mime, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	semi := strings.Index(rest, ";")
	comma := strings.Index(rest, ",")
	if semi < 0 || comma < 0 || comma < semi {
		return "", "", false
	}
	if !strings.EqualFold(rest[semi+1:comma], "base64") {
		return "", "", false // data URLs that are not base64 have no home here
	}
	return rest[:semi], rest[comma+1:], true
}
