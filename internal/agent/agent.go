// Package agent holds the server-side agent loop.
//
// The design follows notes/compact-threshold-mode.en.md:
//
//   - the path in storage is the truth; every turn re-reads it, so a message the user
//     sends while the loop runs becomes part of the next payload without extra machinery
//     (§0 "the path is the truth", §6 "new message during compaction → queued, then sent")
//   - exactly one assembler (package asm) renders payloads — main send, in-loop rebuild,
//     summary request, inspector, rebuild after reload (§5.4)
//   - compaction never runs while a generation is running (§1.1) and commits the entry and
//     the new path tip in one storage write (§3.8, §3.10)
//   - there are no built-in tools: every tool comes from an MCP server (§"all tools via MCP")
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
)

// Event is one UI event.
type Event struct {
	Seq     int64        `json:"seq"`
	Type    string       `json:"type"`
	Session string       `json:"session"`
	Entry   *ir.Entry    `json:"entry,omitempty"`
	Delta   *Delta       `json:"delta,omitempty"`
	State   *Status      `json:"state,omitempty"`
	Pending []string     `json:"pending,omitempty"`
	Run     *RunInfo     `json:"run,omitempty"`
	Gate    *GateAsk     `json:"gate,omitempty"`
	Elicit  *ElicitAsk   `json:"elicitation,omitempty"`
	Info    string       `json:"info,omitempty"`
	Level   string       `json:"level,omitempty"`
	Data    any          `json:"data,omitempty"`
	Ts      time.Time    `json:"ts"`
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
	ID       string `json:"id"`
	CallID   string `json:"callId"`
	Server   string `json:"server,omitempty"`
	Tool     string `json:"tool"`
	Args     string `json:"arguments,omitempty"`
	Status   string `json:"status"` // running|ok|error
	Result   string `json:"result,omitempty"`
	Images   int    `json:"images,omitempty"`
	Ms       int64  `json:"ms,omitempty"`
	Error    string `json:"error,omitempty"`
	EntryID  string `json:"entryId,omitempty"`
	Rounds   int    `json:"rounds,omitempty"`
}

// Status is the session status line.
type Status struct {
	Session    string `json:"session"`
	Phase      string `json:"phase"` // idle|running|compacting
	Name       string `json:"name,omitempty"`
	Model      string `json:"model,omitempty"`
	NCTX       int    `json:"nCtx"`
	ContextUsed int   `json:"contextUsed"`
	ContextKnown bool `json:"contextKnown"`
	ThresholdPct int  `json:"thresholdPct"`
	UsedPct    int    `json:"usedPct"`
	Tip        string `json:"tip,omitempty"`
	Entries    int    `json:"entries"`
	Summary    bool   `json:"summary,omitempty"`
	Broken     bool   `json:"boundaryBroken,omitempty"`
	Pending    int    `json:"pending,omitempty"`
	Turn       int    `json:"turn,omitempty"`
	Tools      int    `json:"tools"`
	Servers    []ServerStatus `json:"servers,omitempty"`
	AutoCompact bool  `json:"autoCompact"`
	InLoop     bool   `json:"inLoop"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ServerStatus reports one MCP server.
type ServerStatus struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Connected bool   `json:"connected"`
	Era       string `json:"era,omitempty"`
	Version   string `json:"protocolVersion,omitempty"`
	Impl      string `json:"impl,omitempty"`
	Tools     int    `json:"tools"`
	Error     string `json:"error,omitempty"`
	Info      string `json:"instructions,omitempty"`
}

// GateAsk is the "needs user confirmation" card for an oversized summary (§3.4).
type GateAsk struct {
	ID            string `json:"id"`
	SummarySize   int    `json:"summarySize"`
	SummaryCap    int    `json:"summaryCap"`
	Attempts      int    `json:"attempts"`
	Preview       string `json:"preview,omitempty"`
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
	ID       string          `json:"id"`
	Server   string          `json:"server"`
	Tool     string          `json:"tool,omitempty"`
	Message  string          `json:"message"`
	Mode     string          `json:"mode,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
	Round    int             `json:"round,omitempty"`
}

// Config is one session's configuration (per-session MCP set, system prompt, context
// parameters — the multi-session requirement).
type Config struct {
	ID           string          `json:"id"`
	Name         string          `json:"name,omitempty"`
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	LLM          llm.Config      `json:"llm"`
	MCP          []mcp.Config    `json:"mcp,omitempty"`
	Compact      compact.Params  `json:"compact"`
	ASM          asm.Options     `json:"asm,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	MaxTokens    int             `json:"maxTokens,omitempty"`
	// MaxTurns caps one agentic run (0 = no cap).
	MaxTurns int `json:"maxTurns,omitempty"`
	// Extra is merged into every LLM request body (provider-specific kwargs).
	Extra map[string]any `json:"extra,omitempty"`
	// ReasoningEffort is forwarded to the provider when set.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// NotifyTools asks MCP servers for list_changed subscriptions.
	NotifyTools bool `json:"notifyTools,omitempty"`
}

