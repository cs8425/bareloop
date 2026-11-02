// Package builtin holds the tools bareloopd answers itself, with no MCP server in between.
//
// What makes a tool belong here is not that it is useful. Anything that touches the world belongs
// in an MCP server, because that is where the permission to touch it lives. These are the tools
// only this daemon can answer, because what they act on is the loop itself: the moment the next
// turn waits for, and the clock a model is allowed to ask about.
//
// Two rules follow from that, and they are enforced by the caller (internal/agent) rather than
// here, because they are about publishing and not about answering:
//
//   - Every one of them is switched off until a session names it in builtinTools. A handle on the
//     loop is not something a model gets by default — and a builtin added in a later version must
//     not appear in a session that never asked for it, which is what an on-list buys over an
//     off-list.
//   - A builtin is published under its own name or not at all. It is what the documentation quotes
//     and what a transcript already quotes, so unlike an MCP tool it is never renamed to make
//     room; if an MCP server already owns the name, the builtin stays unpublished and says so.
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Result is a builtin's answer: the text the model reads, and whether this counts as a failed
// call. Two fields and not an error, because a rejected argument is a result the model can read
// and fix — which is exactly what an MCP tool with isError does, so the loop records the two
// kinds of tool the same way.
type Result struct {
	Text    string
	IsError bool
}

// Input is what the session hands a builtin on every call.
type Input struct {
	// DefaultTimezone is this session's zone (Config.timezone) for a call that does not name one.
	// Empty means UTC: "no timezone configured" is not "the zone of the machine that answered",
	// because which machine answered is not information anybody asked for.
	DefaultTimezone string
	// Logf is the session log. A tool that waits says so before it waits, because a run that
	// produces nothing for five minutes is otherwise indistinguishable from a hung one — and the
	// line is written by the code that knows how long the wait was.
	Logf func(format string, args ...any)
	// Now is the clock, injectable so a test does not have to sleep for five minutes. Nothing in
	// here reads the wall clock directly.
	Now func() time.Time
}

func (in Input) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in Input) logf(format string, args ...any) {
	if in.Logf != nil {
		in.Logf(format, args...)
	}
}

// Tool is one builtin tool: the definition the model is shown, plus what to do when it is called.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// Run answers the call. It is handed the loop's context, so a tool that waits is interrupted
	// by Stop()/Interrupt() like any other tool call, and reports which of the two happened.
	Run func(ctx context.Context, in Input, args json.RawMessage) Result
}

// known is every builtin tool that exists, sorted by name. The order is not cosmetic: the tool
// block is rendered in front of the history, so the list of names is part of the KV prefix and
// has to come out the same on every restart.
var known = sortedByName([]Tool{waitTool(), datetimeTool()})

func sortedByName(tools []Tool) []Tool {
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// All returns every builtin tool that exists, whether or not this session switched it on: the
// tools drawer shows the whole set for exactly that reason (a switch nobody can find is a switch
// nobody uses).
func All() []Tool {
	out := make([]Tool, len(known))
	copy(out, known)
	return out
}

// Get looks one up by its published name.
func Get(name string) (Tool, bool) {
	for _, t := range known {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Names returns every builtin tool name, sorted — what an error message lists when a config asks
// for a builtin that does not exist, and what the drawer's "everything in this group" answer is
// made of.
func Names() []string {
	out := make([]string, 0, len(known))
	for _, t := range known {
		out = append(out, t.Name)
	}
	return out
}

// ErrText is a failure the model is meant to read and correct: it arrives as a tool result with
// isError set, which is how the loop already records a tool that answered "no".
func ErrText(text string) Result {
	return Result{Text: text, IsError: true}
}

// JSON marshals one of the fixed result shapes below. Marshal cannot fail on a struct of strings,
// but "cannot" is a claim about this code, so it is answered rather than assumed.
func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"status":"error","message":%q}`, err.Error())
	}
	return string(b)
}

// argText reads one optional string argument, empty when it was not sent. The pointer in the arg
// struct is what tells "absent" from "" — here both mean "not specified", and saying so in one
// place is what keeps the two tools from meaning the two words differently.
func argText(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// decodeArgs reads a call's arguments, accepting the empty body a model sends when every argument
// is optional. Anything that is not valid JSON is worth naming, because that is the call that
// produced it and the model is the one that gets to fix it.
func decodeArgs(raw json.RawMessage, v any) error {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("the arguments are not a JSON object (%v)", err)
	}
	return nil
}
