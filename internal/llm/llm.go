// Package llm is the provider abstraction for the agent loop.
//
// Only one transport-level provider is implemented (OpenAI-compatible
// /chat/completions, which llama.cpp, vLLM, Ollama, LM Studio, OpenAI, OpenRouter and our
// own llmmock all speak), but its usage parsing covers the llama.cpp native counters too,
// because compaction depends on real KV usage rather than estimates (spec §2.4).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

// Tool is a tool definition sent to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Request is a chat completion request.
type Request struct {
	Model       string
	Messages    []asm.Message
	Tools       []Tool
	Stream      bool
	Temperature *float64
	MaxTokens   int
	Stop        []string
	// ReasoningEffort is sent as `reasoning_effort` when set (OpenAI-style servers).
	ReasoningEffort string
	// Extra is merged into the JSON body, so provider-specific knobs need no code change.
	Extra map[string]any
}

// ToolCallDelta is an incremental tool call fragment.
type ToolCallDelta struct {
	Index int
	ID    string
	Name  string
	Args  string // partial JSON arguments
}

// Event is one streaming event.
type Event struct {
	// Content / Reasoning are append-only text fragments.
	Content   string
	Reasoning string
	ToolCalls []ToolCallDelta
	// FinishReason is only set on the final event.
	FinishReason string
	Usage        *ir.Usage
	// Err terminates the stream with an error.
	Err error
	// Raw is the raw JSON of the chunk (debug / trace).
	Raw string
}

// Result is a non-streaming completion.
type Result struct {
	Content      string
	Reasoning    string
	ToolCalls    []ir.ToolCall
	FinishReason string
	Usage        *ir.Usage
}

// Provider streams or completes chat.
type Provider interface {
	// Name identifies the implementation (openai, ...).
	Name() string
	// Stream returns a channel closed after the terminal event. Cancelling ctx aborts the
	// HTTP request, which is the cancellation signal for the server as well.
	Stream(ctx context.Context, req Request) (<-chan Event, error)
	// Complete is the non-streaming call (used by the compaction summary, spec §3.5).
	Complete(ctx context.Context, req Request) (*Result, error)
	// ModelNCTX reports the model context window when the server can be asked (0 = unknown).
	NCTX(ctx context.Context) int
}

// Config configures a provider.
type Config struct {
	Kind    string `json:"kind"` // "openai" (default)
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
	// Headers are extra request headers (e.g. extra authorisation).
	Headers map[string]string `json:"headers"`
	// NCTX is the model context window; 0 = ask the server / unknown.
	NCTX int `json:"nCtx"`
	// CacheIncludedInPrompt says whether prompt_tokens already counts cached tokens.
	// Default true (OpenAI semantics). llama.cpp native timings are handled automatically.
	CacheIncludedInPrompt *bool `json:"cacheIncludedInPrompt"`
	// Path overrides the endpoint path (default /chat/completions).
	Path string `json:"path"`
	// Timeout for a whole non-streaming request.
	Timeout time.Duration `json:"-"`
	HTTP    *http.Client  `json:"-"`
}

// Same reports whether two configs describe the same endpoint. Config carries a map and an
// *http.Client, so == cannot be used on it; the fields that matter when deciding whether a new
// provider must be built are compared here (the http client and the timeout are not part of an
// endpoint identity).
func (c Config) Same(o Config) bool {
	if c.Kind != o.Kind || c.BaseURL != o.BaseURL || c.APIKey != o.APIKey || c.Model != o.Model ||
		c.Path != o.Path || c.NCTX != o.NCTX {
		return false
	}
	if (c.CacheIncludedInPrompt == nil) != (o.CacheIncludedInPrompt == nil) {
		return false
	}
	if c.CacheIncludedInPrompt != nil && *c.CacheIncludedInPrompt != *o.CacheIncludedInPrompt {
		return false
	}
	if len(c.Headers) != len(o.Headers) {
		return false
	}
	for k, v := range c.Headers {
		if ov, ok := o.Headers[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

// New builds a provider from cfg.
func New(cfg Config) (Provider, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: baseUrl is required")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Kind)) {
	case "", "openai", "openai-compatible", "llamacpp", "vllm", "ollama":
		return &openai{cfg: cfg}, nil
	default:
		return nil, errors.New("llm: unknown provider kind " + cfg.Kind)
	}
}
