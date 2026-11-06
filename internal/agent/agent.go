// Package agent holds the server-side agent loop.
//
// The design follows _notes/compact-threshold-mode.en.md:
//
//   - the path in storage is the truth; every turn re-reads it, so a message the user
//     sends while the loop runs becomes part of the next payload without extra machinery
//     (§0 "the path is the truth", §6 "new message during compaction → queued, then sent")
//   - exactly one assembler (package asm) renders payloads — main send, in-loop rebuild,
//     summary request, inspector, rebuild after reload (§5.4)
//   - compaction never runs while a generation is running (§1.1) and commits the entry and
//     the new path tip in one storage write (§3.8, §3.10)
//   - every tool the model may call comes from an MCP server, except the handful this daemon
//     answers itself (package builtin), which a session has to ask for by name
package agent

import (
	"encoding/json"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
	"bareloop/internal/mcp"
)

// Event types published to the UI.
const (
	EvtHello       = "hello"
	EvtEntry       = "entry"        // a committed entry (user, assistant, tool, compaction)
	EvtDelta       = "delta"        // streaming fragment of the running assistant message
	EvtToolCall    = "tool_call"    // a tool call is starting
	EvtToolResult  = "tool_result"  // a tool call finished
	EvtState       = "state"        // session status changed
	EvtPending     = "pending"      // pending (not yet sent) message ids
	EvtCompact     = "compaction"   // a compaction round finished or its progress
	EvtRequest     = "request"      // the server asks the user something (elicitation / gate)
	EvtRequestDone = "request_done" // the request was answered or cancelled
	EvtLog         = "log"          // transport / loop diagnostics
	EvtError       = "error"
	EvtInfo        = "info"
	EvtRemoved     = "removed" // an entry was deleted
	EvtTools       = "tools"   // the published tool set changed (a policy change landed)
)

// Event is one UI event.
type Event struct {
	Seq     int64     `json:"seq"`
	Type    string    `json:"type"`
	Session string    `json:"session"`
	Entry   *ir.Entry `json:"entry,omitempty"`
	// StreamID names the delta stream an assistant entry was streamed out on, and only an entry that
	// was streamed carries it: a tool result, a held message and a compaction arrive with it empty and
	// omit the key. It exists because a live buffer in the browser is keyed by nothing else, and the
	// alternative to naming it — "the newest buffer must be the one this reply belongs to" — is a guess
	// that leaves one reply drawn twice whenever a late Done revives a buffer the entry had already
	// taken over. With the name, the node a reader watched being written is upgraded into the committed
	// entry in place instead of being replaced beside it.
	StreamID string     `json:"streamId,omitempty"`
	Delta    *Delta     `json:"delta,omitempty"`
	State    *Status    `json:"state,omitempty"`
	Pending  []string   `json:"pending,omitempty"`
	Run      *RunInfo   `json:"run,omitempty"`
	Gate     *GateAsk   `json:"gate,omitempty"`
	Elicit   *ElicitAsk `json:"elicitation,omitempty"`
	Info     string     `json:"info,omitempty"`
	Level    string     `json:"level,omitempty"`
	Data     any        `json:"data,omitempty"`
	Ts       time.Time  `json:"ts"`
}

// Delta is a streaming fragment of the assistant message being generated.
type Delta struct {
	StreamID  string `json:"streamId"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	// ToolCall is set when a tool call fragment arrived (partial arguments).
	ToolCall *ToolCallProgress `json:"toolCall,omitempty"`
	// Done closes the stream buffer in the UI.
	Done bool `json:"done,omitempty"`
	// Error says this stream ends with nothing committed, and why. It rides on the Done rather than on an
	// error event beside it for two reasons: the Done is what every reader already treats as the end (the
	// hub forgets the stream's fragments on it), and a delta is the only frame that carries a stream id —
	// so it is the only one that can reach the one node that is wrong. A reply storage refused otherwise
	// looks, to the page that watched it being written, exactly like a reply that arrived: see
	// closeStreamErr.
	Error string `json:"error,omitempty"`
}

// ToolCallProgress tracks one streamed tool call.
type ToolCallProgress struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Args  string `json:"arguments,omitempty"`
	Index int    `json:"index"`
}

// RunInfo describes a tool run for the UI.
type RunInfo struct {
	ID     string `json:"id"`
	CallID string `json:"callId"`
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool"`
	Args   string `json:"arguments,omitempty"`
	Status string `json:"status"` // running|ok|error
	Result string `json:"result,omitempty"`
	// Structured is the tool's structuredContent, sent to the UI whatever the session's mode is:
	// "drop" hides it from the model, not from the operator reading the transcript. It arrives as a
	// JSON value, exactly as the server sent it — the same shape the entry carries, so the UI has one
	// renderer for both.
	Structured json.RawMessage `json:"structuredContent,omitempty"`
	Images     int             `json:"images,omitempty"`
	Ms         int64           `json:"ms,omitempty"`
	Error      string          `json:"error,omitempty"`
	EntryID    string          `json:"entryId,omitempty"`
	Rounds     int             `json:"rounds,omitempty"`
}

// Status is the session status line.
type Status struct {
	Session string `json:"session"`
	Phase   string `json:"phase"` // idle|running|compacting
	Name    string `json:"name,omitempty"`
	Model   string `json:"model,omitempty"`
	NCTX    int    `json:"nCtx"`
	// NCTXSource says where that number came from ("config", "server", "unset", ...) and NCTXConfig
	// is the ceiling this session was configured with. The gap between the two is the drift an
	// operator has to see: a configured window larger than the server's is silently clamped, and a
	// session with no number at all can neither compact nor check that a request fits.
	NCTXSource   string `json:"nCtxSource,omitempty"`
	NCTXConfig   int    `json:"nCtxConfig,omitempty"`
	ContextUsed  int    `json:"contextUsed"`
	ContextKnown bool   `json:"contextKnown"`
	ThresholdPct int    `json:"thresholdPct"`
	UsedPct      int    `json:"usedPct"`
	Tip          string `json:"tip,omitempty"`
	Entries      int    `json:"entries"`
	Summary      bool   `json:"summary,omitempty"`
	Broken       bool   `json:"boundaryBroken,omitempty"`
	Pending      int    `json:"pending,omitempty"`
	// WakeAt is the moment a scheduled wait is due (UTC, RFC3339), and Queued counts what the user
	// sent while waiting for it. They are the only sign that a "running" session has no run in it:
	// a parked session says "running" on purpose (the composer's Enter must keep meaning "queue",
	// and an unanswered call is not a safe place for a compaction), so the two extra fields rather
	// than a fourth phase.
	WakeAt string `json:"wakeAt,omitempty"`
	Queued int    `json:"queued,omitempty"`
	Turn   int    `json:"turn,omitempty"`
	// Tools is what a request would carry right now; ToolsEnabled is what the session's policy
	// says it will carry once any run in flight has finished. The two differ only while a tool
	// change is waiting (§tools), and ToolsTotal is what the servers offer in total — the gap
	// between that and ToolsEnabled is what this session switched off.
	Tools        int            `json:"tools"`
	ToolsEnabled int            `json:"toolsEnabled"`
	ToolsTotal   int            `json:"toolsTotal"`
	ToolsPending bool           `json:"toolsPending,omitempty"`
	Servers      []ServerStatus `json:"servers,omitempty"`
	AutoCompact  bool           `json:"autoCompact"`
	InLoop       bool           `json:"inLoop"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// ServerStatus reports one MCP server.
type ServerStatus struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Connected bool   `json:"connected"`
	// Disabled means this session switched the whole server off: not connected, and deliberately
	// not an error. Enabled/Hidden split the tools it offers between the ones a request carries
	// and the ones this session switched off individually.
	Disabled bool   `json:"disabled,omitempty"`
	Enabled  int    `json:"enabled"`
	Hidden   int    `json:"hidden"`
	Era      string `json:"era,omitempty"`
	Version  string `json:"protocolVersion,omitempty"`
	Impl     string `json:"impl,omitempty"`
	Tools    int    `json:"tools"`
	Error    string `json:"error,omitempty"`
	Info     string `json:"instructions,omitempty"`
}

// ToolRow is one tool in the tools drawer. The drawer shows every tool the session's servers
// offer, published or not: a switched-off tool is still in the transcript, and seeing what it is
// worth is the only way to decide whether it earns its place in the prompt.
type ToolRow struct {
	// Name is the model-visible name — the tool's own name, "<prefix>_<tool>", or "<server>_<tool>"
	// when two tools want the same name; RawName is always the tool's own name from tools/list, which
	// is what a policy entry is matched against. RenamedFrom is set when a collision means Name is not
	// the name the policy asked for.
	Name        string `json:"name"`
	RawName     string `json:"rawName"`
	RenamedFrom string `json:"renamedFrom,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Schema is the tool's input schema as the model sees it. The drawer renders it as the parameter
	// table: switching a tool off should not require reading the server's source to find out what the
	// tool wants or even what its name means.
	Schema   json.RawMessage `json:"schema,omitempty"`
	Enabled  bool            `json:"enabled"`
	ReadOnly bool            `json:"readOnly,omitempty"`
	// EstTokens is a bytes/4 estimate of what the definition costs per request, not a measurement.
	EstTokens int    `json:"estTokens"`
	Uses      int    `json:"uses"`
	LastUsed  string `json:"lastUsed,omitempty"`
}

// ToolsServer is one MCP server with its tools, as the drawer shows them.
type ToolsServer struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Connected bool   `json:"connected"`
	Disabled  bool   `json:"disabled,omitempty"`
	// Builtin marks the group this daemon answers itself: no transport, no prefix to set, and every
	// one of its tools off until the session names it. The drawer needs the flag rather than a name
	// test because the row it renders changes (no prefix box, a different explanation), not because
	// it has to route anything — routing is carried by toolRef.Builtin.
	Builtin bool   `json:"builtin,omitempty"`
	Error   string `json:"error,omitempty"`
	Impl    string `json:"impl,omitempty"`
	// Prefix is the model-visible prefix in force; empty means no namespace at all, which is the
	// default. ConfiguredPrefix is what config.json says (toolPrefix), so a difference between the two
	// can only be whitespace.
	Prefix           string    `json:"prefix"`
	ConfiguredPrefix string    `json:"configuredPrefix,omitempty"`
	Known            int       `json:"known"`
	Enabled          int       `json:"enabled"`
	Note             string    `json:"note,omitempty"`
	Tools            []ToolRow `json:"tools"`
}

// ToolsView is the tools drawer: what the model may call right now, roughly what each tool costs,
// and whether a change is still waiting for the end of the run.
type ToolsView struct {
	Session        string        `json:"session"`
	Servers        []ToolsServer `json:"servers"`
	Visible        int           `json:"visible"`
	Total          int           `json:"total"`
	PendingChanges bool          `json:"pendingChanges"`
	// AppliesTo is when a change takes effect. "next-run" is the only mode there is: the tool
	// block is rendered in front of the history, so replacing it mid-run would invalidate the KV
	// prefix of everything already generated — and a request that is already on the wire cannot
	// change at all.
	AppliesTo   string    `json:"appliesTo"`
	ContextUsed int       `json:"contextUsed"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// GateAsk is the "needs user confirmation" card for an oversized summary (§3.4).
type GateAsk struct {
	ID          string `json:"id"`
	SummarySize int    `json:"summarySize"`
	SummaryCap  int    `json:"summaryCap"`
	Attempts    int    `json:"attempts"`
	Preview     string `json:"preview,omitempty"`
}

// GateAnswer is the user's answer to GateAsk: accept|retry|skip.
type GateAnswer string

const (
	GateAccept GateAnswer = "accept"
	GateRetry  GateAnswer = "retry"
	GateSkip   GateAnswer = "skip"
)

// ElicitAsk is a server input request forwarded to the UI.
type ElicitAsk struct {
	ID      string          `json:"id"`
	Server  string          `json:"server"`
	Tool    string          `json:"tool,omitempty"`
	Message string          `json:"message"`
	Mode    string          `json:"mode,omitempty"`
	Schema  json.RawMessage `json:"schema,omitempty"`
	Round   int             `json:"round,omitempty"`
}

// Config is one session's configuration (per-session MCP set, system prompt, context
// parameters — the multi-session requirement).
type Config struct {
	ID           string       `json:"id"`
	Name         string       `json:"name,omitempty"`
	SystemPrompt string       `json:"systemPrompt,omitempty"`
	LLM          llm.Config   `json:"llm"`
	MCP          []mcp.Config `json:"mcp,omitempty"`
	// Proxy is this session's answer to where an endpoint's traffic goes: socks5://, socks5h://, http:// or
	// https://, with user:password when the proxy asks for one, or the literal "direct" to ignore HTTP_PROXY
	// and connect to endpoints themselves. It is the fallback rather than the override: an endpoint that names
	// a proxy of its own (llm.proxy, mcp[].proxy) is dialled through that one, an endpoint that names none is
	// dialled through this, and one where neither is set keeps whatever the environment says. Nothing of it is
	// stored into the endpoints — the fallback is applied where dialling is set up (llmDial, mcpDial) — so
	// data/<id>/config.json and GET /config keep showing which field a proxy was actually written in, and a
	// settings page cannot PATCH an inherited value into an endpoint that never asked for one.
	//
	// Two of them, nested, is not implemented: which one would wrap the other is an unanswered question, so
	// the rule stays one answer per endpoint and the nearest one wins.
	//
	// It cannot be sent back to empty over the API, because empty means unchanged here just as it does for
	// timezone below; send "direct" for the environment to stop being in charge.
	Proxy       string         `json:"proxy,omitempty"`
	Compact     compact.Params `json:"compact"`
	ASM         asm.Options    `json:"asm,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	MaxTokens   int            `json:"maxTokens,omitempty"`
	// MaxTurns caps one agentic run (0 = no cap).
	MaxTurns int `json:"maxTurns,omitempty"`
	// Extra is merged into every LLM request body (provider-specific kwargs).
	Extra map[string]any `json:"extra,omitempty"`
	// ReasoningEffort is forwarded to the provider when set.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// NotifyTools asks MCP servers for list_changed subscriptions.
	NotifyTools bool `json:"notifyTools,omitempty"`

	// BuiltinTools lists the tools this daemon answers itself, by name: "wait", "get_datetime".
	// It is an on-list and not an off-list, and that is the whole policy: these are handles on the
	// loop rather than capabilities, so none of them is on until a session asks for it by name — and a
	// builtin added in a later version cannot appear in a session that never asked, which an
	// off-list would have done. A name that does not exist is refused by the API; a hand-written
	// config.json gets a warning in the log and is otherwise ignored.
	BuiltinTools []string `json:"builtinTools,omitempty"`
	// Timezone is the zone the time tools answer in when a call does not name one: an IANA name
	// ("Asia/Taipei"), a fixed offset ("+08:00", "UTC+8") or "Local" for the machine's own. Empty is
	// UTC, deliberately — which machine answered a request is not information anybody asked for. It
	// cannot be sent back to empty over the API (empty means unchanged); send "UTC" for that.
	Timezone string `json:"timezone,omitempty"`

	// StructuredContent is this session's default for what a tool's structuredContent does on the way
	// to the model: "auto" (default: append it unless the text already contains it), "fallback" (only
	// when the result has no text), "always" (append, duplicates included) or "drop" (never send it).
	// A server may override it with mcp[].structuredContent. Switching it rewrites the payload from
	// the first tool result of that server onwards, so the KV prefix is lost there — the log itself is
	// never rewritten, which is the point: it is how a bad choice gets undone without hand-editing
	// data/<id>/log.jsonl.
	StructuredContent string `json:"structuredContent,omitempty"`

	// StructuredMaxBytes is how many bytes of a tool's structuredContent this session puts in front of
	// the model before the rest is cut off, with a note naming the size that was left out. 0 is not a
	// size but "unset", and unset means asm.DefaultStructuredMaxBytes: 16 384 bytes, which reads as
	// four thousand tokens on the same 4-bytes-per-token ruler this project estimates with. Raise it to
	// give a chatty server more of its own JSON; it is one number for the session rather than one per
	// server, because a token budget is easier to reason about than a budget per server.
	//
	// Only the wire is cut. data/<id>/log.jsonl and the UI keep the whole document, so this can be
	// raised or lowered later and the next request is simply assembled differently — the same reason
	// switching the mode above never needs a hand-edit of the log. What it cannot do is go back to the
	// factory default over the API, because 0 already means "unchanged" in a PATCH body; set the
	// number you want instead.
	StructuredMaxBytes int `json:"structuredMaxBytes,omitempty"`

	// Pinned and Archived are where this session sits in a list, and the only two fields here that say
	// nothing about what gets sent to the model: pinned keeps it above the rest, archived takes it out of
	// the way. They live in config.json beside Name because they are a fact about the session and not a
	// fact about the browser looking at it - a second client, or the same person on another machine, sees
	// the same list. Two of them and not one state word because "not pinned" and "archived" are
	// different answers to different questions.
	//
	// They are mutually exclusive (Manager.SetFlags clears the other half), and nothing here is ever
	// merged from a PATCH body: UpdateConfig carries the stored pair over, because a whole-config PATCH
	// that never mentioned them must not be able to unpin a session by accident.
	Pinned   bool `json:"pinned,omitempty"`
	Archived bool `json:"archived,omitempty"`
}
