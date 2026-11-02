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
	"errors"
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
	// MaxInLoopSleep is the longest a tool may keep the loop waiting inside one call. Past that
	// length the wait is handed back instead of slept (see Parked): a wait that resumes in six
	// hours is not worth a goroutine, and a goroutine does not survive a restart while the tool call
	// already in the log does. Zero means "sleep as long as you were asked to", which is what every
	// caller of this package meant before there was a choice — including every test here, and any
	// session that has no reason to hold a wait short.
	MaxInLoopSleep time.Duration
	// DaemonNote is an aside about this call that only the daemon can witness — "the daemon
	// restarted between this call and its answer". A tool that can be interrupted like that puts it
	// after its own answer and never inside it: the JSON stays the tool's account of what it did,
	// the note stays the daemon's account of what happened around it. It is addressed to the one call
	// being resumed, not to the rest of the run, and of the tools here only a wait has anything to say
	// with it — get_datetime has no past to explain.
	DaemonNote string
	// ResumeCalledAt is the moment this call was first asked, for the one case where a process is answering
	// a call that somebody else left unanswered: a resumed wait. Zero means "the call is being asked now",
	// which is every other call there is.
	//
	// why it exists: a wait is a promise about a moment, not a length to be re-counted. Computed from now,
	// the call a wake re-runs would find itself six hours from now, hand itself over again, and be woken to
	// find itself six hours from then — a wait that restarts itself forever and never comes due.
	ResumeCalledAt time.Time
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

// withNote is how an answer leaves a tool: what it decided to say, and the daemon's aside after it
// when there is one. Both a completed wait and an interrupted one carry it, because the fact the
// note states is about the call rather than about which answer the call got.
func (in Input) withNote(r Result) Result {
	if in.DaemonNote == "" {
		return r
	}
	if r.Text == "" {
		return Result{Text: in.DaemonNote, IsError: r.IsError}
	}
	return Result{Text: r.Text + "\n\n" + in.DaemonNote, IsError: r.IsError}
}

// Tool is one builtin tool: the definition the model is shown, plus what to do when it is called.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// Run answers the call. It is handed the loop's context, so a tool that waits is interrupted
	// by Stop()/Interrupt() like any other tool call, and reports which of the two happened.
	//
	// The error is not a failed call: a failed call is a Result with IsError set, which is text the
	// model is meant to read and fix. The error is something only the daemon can act on, and today
	// there is exactly one such thing (Parked). It is a second return value rather than a field of
	// Result so that "answered" and "did not answer, and here is why" cannot be mixed by forgetting
	// a line — and so the compiler asks every caller to look.
	Run func(ctx context.Context, in Input, args json.RawMessage) (Result, error)
}

// Parked is a builtin handing its work to the daemon instead of doing it here: the moment has been
// decided, this loop is not the thing that will wake up, and no result is written yet — the result
// belongs to the turn that wakes. It comes back as the error of a call, which is the one channel a
// caller cannot ignore by accident, since an ignored Result would leave the tool call silently
// unanswered.
type Parked struct {
	// Until is the moment the wait is due, not a length: arming from a moment means a daemon that
	// wakes up an hour late resumes an hour late rather than waiting an hour more.
	Until time.Time
}

// Error describes the handover in the words the model would need if it ever saw it. That is not a
// dead case: a caller that treats every error as a failed call turns this line into a tool result,
// and even then the model should learn that the wait is scheduled rather than lost.
func (p *Parked) Error() string {
	return fmt.Sprintf("this wait is scheduled for %s and the loop is not held open for it",
		p.Until.Format(time.RFC3339))
}

// ErrUserInterrupt is how the session tells a waiting tool that a person ended the wait rather than
// the clock or Stop(): cancel the run's context with it as the cause (context.WithCancelCause) and
// wait says so in its result. The word matters because of what comes next — an interrupt appends
// the person's message right after that result, and a model that reads "interrupted" with no
// reason schedules the same wait again instead of reading the message.
var ErrUserInterrupt = errors.New("user interrupt")

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

// ResumeNote is the daemon's aside about a wait it had to answer on someone else's behalf. The
// wording lives here because it is the wait's own schedule being described — one voice for both
// halves — while the caller supplies only the fact only it can witness ("the daemon restarted").
// reason has to read like the start of a sentence, because that is what it is spliced into, and the
// moment it resumed is deliberately not repeated: resumed_at in the JSON is that timestamp and this
// function would print a second one a microsecond apart from it.
func ResumeNote(reason string, scheduled, resumed time.Time) string {
	late := resumed.Sub(scheduled)
	// why the guard: a clock that went backwards during a restart (an NTP correction is enough) must
	// not produce "-4m12s before it could be answered", which is a sentence about the future.
	if late < 0 {
		late = 0
	}
	return fmt.Sprintf("[%s: this wait was due at %s, %s before it could be answered]",
		reason, scheduled.Format(time.RFC3339), late.Truncate(time.Second))
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
