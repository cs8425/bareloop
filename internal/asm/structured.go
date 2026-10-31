package asm

import (
	"fmt"
	"strings"

	"bareloop/internal/ir"
)

// StructuredMode decides what a tool result's structuredContent does on its way to the model.
//
// The JSON is stored once, in its own entry field (ir.Entry.Structured), and folded into the
// message here rather than when the tool answered. That is what lets a session change its mind
// about a server that turned out to repeat itself without hand-editing data/<id>/log.jsonl.
// The cost is known and accepted: the folded bytes are part of the message, so switching mode
// rewrites the payload from the first affected tool result onwards and the KV prefix is lost
// there — which is why the option is documented next to the setting, not buried in code.
type StructuredMode string

const (
	// StructuredAuto appends the JSON unless the text already contains it. This is what a server
	// that sends both a text block and structuredContent wants, and it is the default because it
	// is what bareloop did before the option existed.
	StructuredAuto StructuredMode = "auto"
	// StructuredFallback uses the JSON only when the result carries no text block: for a server
	// whose text is a lossy rendering of the same data.
	StructuredFallback StructuredMode = "fallback"
	// StructuredAlways appends it whatever the text says, duplicates included.
	StructuredAlways StructuredMode = "always"
	// StructuredDrop stores it and never sends it. It is still in the log and still in the UI, so
	// switching to drop costs no information — only the model stops seeing it.
	StructuredDrop StructuredMode = "drop"
)

// StructuredMarker labels the appended JSON, so the model can tell the server's prose from the
// server's data instead of guessing which half of the text it may quote as its own.
const StructuredMarker = "[structuredContent]"

// ParseStructuredMode maps a config string to a mode. "" means "not set" and yields the default.
//
// An unknown word is an error rather than a shrug: quietly reading a typo as "auto" turns the
// option into one that appears to do nothing, which is the worst possible outcome for a switch
// whose whole job is what costs tokens.
func ParseStructuredMode(s string) (StructuredMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return StructuredAuto, nil
	case "auto":
		return StructuredAuto, nil
	case "fallback":
		return StructuredFallback, nil
	case "always":
		return StructuredAlways, nil
	case "drop":
		return StructuredDrop, nil
	default:
		return "", fmt.Errorf("asm: unknown structuredContent mode %q: want auto, fallback, always or drop", s)
	}
}

// StructuredTargets says how structuredContent is sent: one session-wide default plus the servers
// that differ from it. The zero value means "auto everywhere", which keeps Options{} in a test
// meaning what it meant before this option existed.
type StructuredTargets struct {
	// Default is the session-wide mode ("" = auto).
	Default StructuredMode
	// ByServer is keyed by the MCP server's configured name, which is what an entry records in
	// ir.Entry.Server.
	ByServer map[string]StructuredMode
}

// For returns the mode of one server, falling back to the session default and then to auto.
// A nil target means "nothing was configured", which is auto everywhere — that is what the zero
// Options of every test in this package carries.
func (t *StructuredTargets) For(server string) StructuredMode {
	if t == nil {
		return StructuredAuto
	}
	if m, ok := t.ByServer[server]; ok && m != "" {
		return m
	}
	if t.Default != "" {
		return t.Default
	}
	return StructuredAuto
}

// foldStructured returns a copy of e whose text carries the structuredContent per mode. The entry
// itself is never modified: Built.Entries, the log behind it and the UI all keep the stored bytes,
// and a rebuild with a different mode produces the new bytes from the same entry.
func foldStructured(e *ir.Entry, mode StructuredMode) *ir.Entry {
	if e == nil || len(e.Structured) == 0 {
		return e
	}
	if mode == "" {
		mode = StructuredAuto // a caller that forgot to resolve still gets the documented default
	}
	raw := string(e.Structured)
	switch mode {
	case StructuredAlways:
		// no condition: the user asked for it every time, duplicates included
	case StructuredAuto:
		if strings.Contains(e.Content, raw) {
			return e // the server already put the JSON in its text block
		}
	case StructuredFallback:
		if strings.TrimSpace(e.Content) != "" {
			return e
		}
	default:
		return e // StructuredDrop
	}
	c := *e
	if e.Content != "" {
		c.Content = e.Content + "\n" + StructuredMarker + "\n" + raw
	} else {
		c.Content = StructuredMarker + "\n" + raw
	}
	return &c
}
