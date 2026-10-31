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
	"bareloop/internal/tlsconf"
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

// Pricing is billing metadata for one endpoint: what each kind of token costs, **all three in the same
// unit — per 1,000,000 tokens** — because that is how providers publish prices and how they are read
// off a bill. It lives with baseUrl/model/apiKey because it is a property of the endpoint: the same
// endpoint costs the same whatever the conversation does, and a second session on the same model
// should not need its own price tag (it inherits, and may override).
//
// There is deliberately no currency field. bareloop does not know which one you are billed in, and a
// symbol it invented would be a guess; enter the numbers in whatever unit your provider quotes and
// read the total as that.
//
// The three kinds match what a reply actually reports, so a price is only ever multiplied by a
// measured number (ir.Usage): Input is the prompt the server had to compute (prompt minus cached),
// CachedInput the prompt it served from cache, Output what it generated.
type Pricing struct {
	// No omitempty on purpose: a config file that says "pricing" should show all three numbers, the
	// zeroes included, because "I did not write that one" and "that one is free" must not look alike.
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cachedInput"`
	Output      float64 `json:"output"`
}

// Validate rejects prices that could not be paid. A nil Pricing is not a problem: it means the
// session says nothing about money, and the UI then shows tokens and no cost at all.
func (p *Pricing) Validate() error {
	if p == nil {
		return nil
	}
	if p.Input < 0 || p.CachedInput < 0 || p.Output < 0 {
		return errors.New("llm: pricing must not be negative")
	}
	return nil
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
	// Path overrides the endpoint path (default /chat/completions).
	Path string `json:"path"`
	// InsecureTLS skips certificate verification for this endpoint. It is a pointer so the three
	// answers stay separate over the API: absent means nobody configured it, false means "verify",
	// true means "do not verify". A plain bool would make absent and false the same value, and a
	// session that once asked for it could never be switched back. It is not "ignore the certificate but
	// still check the name": in crypto/tls the name and the chain are one verification, so skipping it
	// lets any host between here and there answer for that endpoint — not only the one you meant — which
	// is why the daemon says so out loud when it is on.
	InsecureTLS *bool `json:"insecureTLS,omitempty"`
	// CAFile is a PEM bundle this endpoint is verified against — and the only thing it is verified
	// against, because naming a file is a trust decision rather than an addition to one: the system
	// trust store is not consulted. It is read when the config is saved, not on the first request,
	// so a wrong path is reported while there is still a session open in front of whoever typed it.
	CAFile string `json:"caFile,omitempty"`
	// Pricing is what a token costs on this endpoint (per 1,000,000). It changes no request, which is
	// why it is a pointer to a struct of plain numbers: absent means "no idea, show no cost", and an
	// explicit {"input":0,"cachedInput":0,"output":0} is a real answer meaning "free".
	Pricing *Pricing `json:"pricing,omitempty"`
	// Timeout for a whole non-streaming request.
	Timeout time.Duration `json:"-"`
	HTTP    *http.Client  `json:"-"`
}

// Same reports whether two configs describe the same endpoint. Config carries a map and an
// *http.Client, so == cannot be used on it; the fields that matter when deciding whether a new
// provider must be built are compared here (the http client and the timeout are not part of an
// endpoint identity).
//
// Pricing is deliberately not compared, and adding it here would be a bug: a price tag changes no
// request, so it must never be a reason to rebuild a provider, reconnect, or warn that saving a
// setting costs the KV cache.
//
// The two TLS fields are compared, and they belong here: they decide whether a request is even sent
// to that host. Absent and false are treated as the same answer, though — neither asks anything of the
// transport — so a PATCH that only spells out "false" does not rebuild a provider and does not warn
// about a KV cache nothing changed.
func (c Config) Same(o Config) bool {
	if c.Kind != o.Kind || c.BaseURL != o.BaseURL || c.APIKey != o.APIKey || c.Model != o.Model ||
		c.Path != o.Path || c.NCTX != o.NCTX {
		return false
	}
	if a, b := c.TLS(), o.TLS(); a.Insecure() != b.Insecure() || c.CAFile != o.CAFile {
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

// TLS is this endpoint's certificate policy in the shape internal/tlsconf takes. A method, so that
// nothing asking "how is this endpoint verified" can read one of the two fields without the other.
func (c Config) TLS() tlsconf.Options {
	return tlsconf.Options{InsecureTLS: c.InsecureTLS, CAFile: c.CAFile}
}

// New builds a provider from cfg.
func New(cfg Config) (Provider, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("llm: baseUrl is required")
	}
	// The kind is settled before anything is read from disk: an unknown kind should fail without
	// having opened a caFile.
	switch strings.ToLower(strings.TrimSpace(cfg.Kind)) {
	case "", "openai", "openai-compatible", "llamacpp", "vllm", "ollama":
	default:
		return nil, errors.New("llm: unknown provider kind " + cfg.Kind)
	}
	// The TLS policy is resolved once, here, for two reasons: a caFile that cannot be read is a
	// failure of the config rather than of the first request, and every request this provider makes
	// then shares one transport — which is what keeps connections pooled now that the policy means
	// a transport of its own instead of the process-wide default.
	tr, err := cfg.TLS().Transport("llm")
	if err != nil {
		return nil, err
	}
	if cfg.HTTP != nil && cfg.HTTP.Transport != nil {
		// A caller-supplied transport wins, policy fields and all: what it does about certificates is
		// that caller's business, not a second policy to reconcile with the first.
		tr = cfg.HTTP.Transport
	}
	return &openai{cfg: cfg, tr: tr}, nil
}
