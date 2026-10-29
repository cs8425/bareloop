// Package ir holds the conversation record model ("entries on a path").
//
// Design notes (see notes/compact-threshold-mode.en.md §0):
//   - the path (root -> tip) is the single source of truth for "what gets sent"
//   - a compaction is just another record on the path, not a field on the session
//   - entries are never removed by compaction; they simply stop appearing in the payload
package ir

import (
	"encoding/json"
	"time"
)

// Part types.
const (
	PartText  = "text"
	PartImage = "image"
)

// Part is one piece of a multi-part message (text or image attachment).
//
// Parts always travel in a fixed order ([images..., text]) and images always use the
// same encoding, so that "rebuild from storage" is byte-identical to "the live push"
// (spec §5.4).
type Part struct {
	Type string `json:"type"`
	// Text is used when Type == PartText.
	Text string `json:"text,omitempty"`
	// MimeType + Data (base64, no data-url prefix) are used when Type == PartImage.
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"arguments,omitempty"`
	Server string          `json:"server,omitempty"` // MCP server that owns the tool
}

// Usage is the real token accounting reported by the server for one assistant turn.
//
// There is one semantics, everywhere: PromptTokens is the whole prompt **including** the prefix the
// server served from its KV cache, CacheTokens is the part of it that was cached (so CacheTokens <=
// PromptTokens, never a quantity to add), and CompletionTokens is what it generated. llama.cpp answers
// in two places at once — `usage.prompt_tokens` already counts the cached prefix, while its `timings`
// split the same prompt into `prompt_n` (recomputed) and `cache_n` (reused) — so the two are read as
// one number, not summed. An earlier round carried a `cacheIncludedInPrompt` switch for a dialect
// difference that measurement says does not exist (three backends, one dialect: prompt includes
// cached); its default silently discarded `cached_tokens`, which is the very number a cache hit rate
// and a bill are made of, so it is gone.
//
// PromptMs / CompletionMs are that same reply's own prefill and decode measurements, which is what
// makes a tok/s figure mean something: `completion / completionMs` is this server's decode rate at
// this size, not a constant someone remembered. A request is one data point; nothing here is averaged.
//
// ContextUsed() is the one number shared by the UI progress bar and the compaction trigger check
// (spec §0 "contextUsed (real usage)").
type Usage struct {
	// PromptTokens is the whole prompt, cached tokens included.
	PromptTokens int `json:"prompt,omitempty"`
	// CacheTokens is the part of PromptTokens the server did not have to compute again.
	CacheTokens int `json:"cache,omitempty"`
	// CompletionTokens is the generated size of this turn.
	CompletionTokens int `json:"completion,omitempty"`

	// PromptMs / CompletionMs are the server's own prefill and decode timings in milliseconds.
	// DraftN / DraftAccepted come from speculative decoding (proposed / accepted). Zero means the
	// server said nothing — the UI drops the field instead of showing a zero.
	PromptMs      float64 `json:"promptMs,omitempty"`
	CompletionMs  float64 `json:"completionMs,omitempty"`
	DraftN        int     `json:"draftN,omitempty"`
	DraftAccepted int     `json:"draftAccepted,omitempty"`

	// HasPrompt / HasCompletion record "the server really told us", so that a missing
	// count is never confused with a zero count.
	HasPrompt     bool `json:"hasPrompt,omitempty"`
	HasCompletion bool `json:"hasCompletion,omitempty"`
}

// ContextUsed returns the real KV occupancy of the completed turn: the whole prompt (cached prefix
// included) plus what was generated. Cached tokens must not be added a second time.
func (u *Usage) ContextUsed() int {
	if u == nil || !u.HasPrompt {
		return 0
	}
	return u.PromptTokens + u.CompletionTokens
}

// Entry kinds.
const (
	KindUser       = "user"
	KindAssistant  = "assistant"
	KindTool       = "tool"
	KindCompaction = "compaction"
)

// CompactionMeta is the metadata of a compaction entry.
//
// The three boundary states of spec §2.5 must stay distinguishable:
//
//	BoundaryID == nil                 -> deliberate empty kept ("compact everything").
//	BoundaryID != nil, found on path  -> normal boundary, kept starts at that message.
//	BoundaryID != nil, not found      -> broken: fail open (send everything).
type CompactionMeta struct {
	BoundaryID  *string `json:"boundaryId"`
	Replaced    int     `json:"replaced"`
	IsSplitTurn bool    `json:"isSplitTurn,omitempty"`
	Model       string  `json:"model,omitempty"`
	UsageBefore int     `json:"usageBefore,omitempty"`
	SummarySize int     `json:"summarySize,omitempty"`
	SummaryMode string  `json:"summaryMode,omitempty"`
	// Broken is computed while resolving the path (never trusted from storage).
	Broken bool `json:"broken,omitempty"`
}

// Entry is one conversation record on the path.
type Entry struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	Kind     string `json:"kind"`
	// Role is the API role: user|assistant|tool. A compaction entry travels with role user.
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// Reasoning is the model's chain-of-thought channel (never sent back to the model).
	Reasoning string     `json:"reasoning,omitempty"`
	Parts     []Part     `json:"parts,omitempty"`
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`

	// Tool-result only fields.
	ToolCallID string `json:"toolCallId,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Server     string `json:"server,omitempty"`
	IsError    bool   `json:"isError,omitempty"`

	Interrupted bool   `json:"interrupted,omitempty"`
	Model       string `json:"model,omitempty"`
	Usage       *Usage `json:"usage,omitempty"`
	// Finish is the provider's finish reason ("stop", "tool_calls", "length", …).
	Finish string `json:"finishReason,omitempty"`
	// Ms is how long generating this entry / running this tool took.
	Ms int64 `json:"ms,omitempty"`

	Compaction *CompactionMeta `json:"compaction,omitempty"`

	// Label is a free-form UI annotation (e.g. "interrupted by user").
	Label string    `json:"label,omitempty"`
	Ts    time.Time `json:"ts"`
}

// IsCompaction reports whether the entry is a compaction record.
func (e *Entry) IsCompaction() bool { return e.Kind == KindCompaction }

// Path walks from id up to the root and returns the root-to-id sequence.
// A missing parent terminates the walk (the visible prefix is returned).
func Path(entries map[string]*Entry, id string) []*Entry {
	seen := map[string]bool{}
	var out []*Entry
	cur := id
	for cur != "" {
		if seen[cur] {
			break // corrupt cycle
		}
		seen[cur] = true
		e, ok := entries[cur]
		if !ok {
			break
		}
		out = append(out, e)
		cur = e.ParentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Effective is the path the model actually sees after the active compaction entry is
// applied (spec §3.2): "planning input == assembler input" holds structurally, because
// both call this one function.
//
// Rules (spec §2.5, §3.3):
//   - only the last compaction entry on the path is active
//   - boundary absent  -> kept = entries after the compaction entry
//   - boundary found   -> kept = boundary .. end, everything up to the entry replaced by it
//   - boundary missing -> fail open: everything except the active entry, Broken marked on meta
//
// A boundary id is resolved over the whole path: the commit appends the compaction entry at the
// tip, so the id it records points backwards at the first kept message (see the body for specs).
//
// The returned kept slice never contains the compaction entry itself; the caller splices
// the summary in front of it (with role user).
func Effective(entries map[string]*Entry, path []*Entry) ([]*Entry, *Entry) {
	idx := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].IsCompaction() {
			idx = i
			break
		}
	}
	if idx < 0 {
		return path, nil
	}
	cmp := path[idx]
	meta := cmp.Compaction
	if meta == nil {
		meta = &CompactionMeta{}
		cmp.Compaction = meta
	}
	meta.Broken = false
	// Earlier compaction entries are never sent: their content lives inside the summary that
	// replaced them (rolling summary, spec §3.6), and the boundary of the active one may even
	// point at one of them.
	withoutOlder := func(from int) []*Entry {
		kept := make([]*Entry, 0, len(path)-from)
		for _, e := range path[from:] {
			if e.IsCompaction() {
				continue
			}
			kept = append(kept, e)
		}
		return kept
	}
	if meta.BoundaryID == nil {
		// Deliberate empty kept: payload = [summary] + everything after the entry.
		kept := withoutOlder(idx + 1)
		return kept, cmp
	}
	bid := *meta.BoundaryID
	// The boundary is resolved against the whole path. The commit appends the compaction entry at
	// the tip (spec 3.8 step 1: parent = last entry of the pre-compaction path), so the id it
	// records necessarily points *backwards* at the first kept message. Spec 2.5: broken means "id
	// present but not found on the path", not "not found after the entry". An empty id matches no
	// entry and therefore lands in the broken branch below, as before.
	for i := range path {
		if path[i].ID == bid {
			// withoutOlder also drops compaction entries: the caller splices the summary in front,
			// so the active entry is never sent as an ordinary message (and older ones live inside
			// the rolling summary, spec 3.6).
			return withoutOlder(i), cmp
		}
	}
	// Not on the path at all (branch, that message deleted, imported legacy data): broken -> fail
	// open. Send everything except the active entry itself: spec 2.5 says "ignore the compaction
	// entry", and its text is exactly what the caller splices in front, so keeping it here would
	// send the summary twice. Older compaction entries stay -- with no summary spliced in there is
	// nothing else holding their content.
	meta.Broken = true
	all := make([]*Entry, 0, len(path)-1)
	for _, e := range path {
		if e != cmp {
			all = append(all, e)
		}
	}
	return all, cmp
}

// LastUsage returns the real usage of the last completed assistant turn on the path,
// or nil when no turn has run yet (spec: "unavailable -> do not check, do not guess").
//
// stopAtCompaction implements spec 1.5. An active compaction entry folded everything before it
// into a summary, so a usage recorded before it prices a payload that no longer exists: the walk
// therefore stops at the newest compaction entry and reports the number as unavailable for the
// one turn that follows a commit, instead of handing back a value that is too high (which made
// every compaction followed by exactly one redundant compaction, and let guardFit refuse a
// payload that had just shrunk). Callers pass false when the boundary is broken: fail-open sends
// everything (spec 2.5), so there the pre-entry usage really is still the price of the wire.
func LastUsage(path []*Entry, stopAtCompaction bool) *Usage {
	for i := len(path) - 1; i >= 0; i-- {
		e := path[i]
		if stopAtCompaction && e.IsCompaction() {
			return nil
		}
		if e.Kind == KindAssistant && e.Usage != nil && e.Usage.HasPrompt {
			return e.Usage
		}
	}
	return nil
}
