package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
)

type openai struct{ cfg Config }

func (p *openai) Name() string { return "openai" }

func (p *openai) client(timeout time.Duration) *http.Client {
	c := &http.Client{}
	if p.cfg.HTTP != nil {
		c.Transport = p.cfg.HTTP.Transport
	}
	if timeout > 0 {
		c.Timeout = timeout
	}
	return c
}

func (p *openai) endpoint() string {
	base := strings.TrimRight(p.cfg.BaseURL, "/")
	if p.cfg.Path != "" {
		return base + "/" + strings.TrimLeft(p.cfg.Path, "/")
	}
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// propsURL strips the chat path so provider metadata endpoints can be probed.
func (p *openai) metaURL(path string) string {
	base := strings.TrimRight(p.cfg.BaseURL, "/")
	for _, suf := range []string{"/v1/chat/completions", "/chat/completions", "/v1", "/completion"} {
		if strings.HasSuffix(base, suf) {
			return strings.TrimSuffix(base, suf) + path
		}
	}
	return base + path
}

type toolDefJSON struct {
	Type     string      `json:"type"`
	Function functionDef `json:"function"`
}

type functionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model           string         `json:"model"`
	Messages        []asm.Message  `json:"messages"`
	Tools           []toolDefJSON  `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	Stop            []string       `json:"stop,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

func (p *openai) encodeBody(req Request, stream bool) ([]byte, error) {
	body := chatRequest{
		Model:           or(req.Model, p.cfg.Model),
		Messages:        req.Messages,
		Stream:          stream,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		Stop:            req.Stop,
		ReasoningEffort: req.ReasoningEffort,
	}
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, toolDefJSON{Type: "function", Function: functionDef{
			Name: t.Name, Description: t.Description, Parameters: params,
		}})
	}
	extra := req.Extra
	if len(extra) == 0 {
		return json.Marshal(body)
	}
	// Merge provider-specific fields. Deterministic: encoding/json sorts map keys, and the
	// whole session uses one config, so the rendered prefix stays byte-identical.
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		m[k] = v
	}
	return json.Marshal(m)
}

func (p *openai) newHTTP(ctx context.Context, body []byte) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		r.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	for k, v := range p.cfg.Headers {
		r.Header.Set(k, v)
	}
	return r, nil
}

// Stream implements Provider. Cancelling ctx aborts the HTTP request, which is also the
// cancellation signal for the server (used by Stop / forced interrupt).
func (p *openai) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	req.Stream = true
	body, err := p.encodeBody(req, true)
	if err != nil {
		return nil, err
	}
	httpReq, err := p.newHTTP(ctx, body)
	if err != nil {
		return nil, err
	}
	resp, err := p.client(0).Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("llm: %s: %s", resp.Status, drain(resp))
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		// Server answered with one JSON object although we asked to stream.
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		var cr completionResponse
		if err := json.Unmarshal(raw, &cr); err != nil {
			return nil, fmt.Errorf("llm: unexpected %s response: %.200s", ct, string(raw))
		}
		res := cr.toResult()
		out := make(chan Event, 8)
		if res.Content != "" || res.Reasoning != "" {
			out <- Event{Content: res.Content, Reasoning: res.Reasoning}
		}
		if len(res.ToolCalls) > 0 {
			var d []ToolCallDelta
			for i, tc := range res.ToolCalls {
				d = append(d, ToolCallDelta{Index: i, ID: tc.ID, Name: tc.Name, Args: string(tc.Args)})
			}
			out <- Event{ToolCalls: d}
		}
		fr := res.FinishReason
		if fr == "" {
			fr = "stop"
		}
		out <- Event{FinishReason: fr, Usage: res.Usage}
		close(out)
		return out, nil
	}
	out := make(chan Event, 64)
	go p.readStream(resp, out)
	return out, nil
}

// readStream emits content/reasoning/tool-call events, then exactly one terminal event
// (finish_reason + usage), then closes out.
func (p *openai) readStream(resp *http.Response, out chan<- Event) {
	defer close(out)
	defer resp.Body.Close()

	var finish string
	var usage *ir.Usage
	var failure error

	br := bufio.NewReaderSize(resp.Body, 1<<16)
	for failure == nil {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if strings.HasPrefix(trimmed, "data:") {
				ev, e, ok := parseChunk(trimmed)
				if e != nil {
					failure = e
				} else if ok {
					if ev.FinishReason != "" {
						finish = ev.FinishReason
					}
					if ev.Usage != nil {
						usage = ev.Usage
					}
					if ev.Content != "" || ev.Reasoning != "" || len(ev.ToolCalls) > 0 {
						out <- Event{Content: ev.Content, Reasoning: ev.Reasoning, ToolCalls: ev.ToolCalls}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				failure = err
			}
			break
		}
	}
	if failure != nil {
		out <- Event{Err: failure}
		return
	}
	out <- Event{FinishReason: finish, Usage: usage}
}

// Complete implements Provider (non-streaming, spec §3.5: one shot with the full summary).
func (p *openai) Complete(ctx context.Context, req Request) (*Result, error) {
	req.Stream = false
	body, err := p.encodeBody(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := p.newHTTP(ctx, body)
	if err != nil {
		return nil, err
	}
	resp, err := p.client(p.cfg.Timeout).Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("llm: %s: %.500s", resp.Status, string(raw))
	}
	var cr completionResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("llm: bad response: %v (%.200s)", err, string(raw))
	}
	return cr.toResult(), nil
}

// NCTX probes provider metadata endpoints for the context window. Best effort.
func (p *openai) NCTX(ctx context.Context) int {
	for _, path := range []string{"/props", "/model", "/models"} {
		ctx2, cancel := context.WithTimeout(ctx, 4*time.Second)
		req, err := http.NewRequestWithContext(ctx2, http.MethodGet, p.metaURL(path), nil)
		if err != nil {
			cancel()
			continue
		}
		if p.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}
		for k, v := range p.cfg.Headers {
			req.Header.Set(k, v)
		}
		resp, err := p.client(4 * time.Second).Do(req)
		if err != nil {
			cancel()
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if err != nil || resp.StatusCode/100 != 2 {
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		keys := []string{"n_ctx", "nCtx", "total_nctx", "max_context", "context_length", "max_model_len"}
		if n, ok := findNumber(v, keys, 0); ok && n >= 1024 {
			return n
		}
	}
	return 0
}

func findNumber(v any, keys []string, depth int) (int, bool) {
	if depth > 8 {
		return 0, false
	}
	switch t := v.(type) {
	case map[string]any:
		for _, k := range keys {
			if cand, ok := t[k]; ok {
				if n, ok2 := asInt(cand); ok2 && n > 0 {
					return n, true
				}
			}
		}
		for _, child := range t {
			if n, ok := findNumber(child, keys, depth+1); ok {
				return n, true
			}
		}
	case []any:
		for _, child := range t {
			if n, ok := findNumber(child, keys, depth+1); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

func drain(resp *http.Response) string {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "(empty body)"
	}
	if len(s) > 800 {
		return s[:800] + "…"
	}
	return s
}

// ---- response shapes ----

type completionResponse struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []choiceMessage `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
	// llama.cpp native fields.
	Timings json.RawMessage `json:"timings"`
	Content string          `json:"content"`
	Error   *apiError       `json:"error"`
}

type choiceMessage struct {
	Index        int             `json:"index"`
	Message      msgPayload      `json:"message"`
	Delta        msgPayload      `json:"delta"`
	FinishReason string          `json:"finish_reason"`
	Usage        json.RawMessage `json:"usage"`
}

type msgPayload struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
	ReasoningDetails []struct {
		Text string `json:"text"`
		Type string `json:"type"`
	} `json:"reasoning_details"`
	ToolCalls []toolCallJSON `json:"tool_calls"`
}

type toolCallJSON struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

func (e *apiError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("llm api error: %s (%s)", e.Message, e.Type)
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

func (m msgPayload) text() string { return rawText(m.Content) }
func (m msgPayload) reasoning() string {
	s := rawText(m.ReasoningContent)
	if s == "" {
		s = rawText(m.Reasoning)
	}
	if s == "" {
		for _, d := range m.ReasoningDetails {
			if d.Text != "" {
				return d.Text
			}
		}
	}
	return s
}

func (cr completionResponse) toResult() *Result {
	res := &Result{Usage: parseUsage(cr.Usage, cr.Timings)}
	if len(cr.Choices) == 0 {
		res.Content = cr.Content
		if cr.Error != nil {
			res.FinishReason = "error"
		}
		return res
	}
	ch := cr.Choices[0]
	res.Content = ch.Message.text()
	res.Reasoning = ch.Message.reasoning()
	res.FinishReason = ch.FinishReason
	for _, tc := range ch.Message.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, ir.ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(orEmptyObj(tc.Function.Arguments)),
		})
	}
	if res.Usage == nil && len(ch.Usage) > 0 {
		// cr.Timings may still be there even though the top level had no usage object, and counts from
		// `usage` are never spoiled by adding the timings of the same reply a second time.
		res.Usage = parseUsage(ch.Usage, cr.Timings)
	}
	return res
}

// parseUsage reads the usage dialects we care about into one semantics (see ir.Usage): PromptTokens
// is the whole prompt, cached prefix included. OpenAI-style `usage` reports it that way directly.
// llama.cpp reports the same reply twice — `usage.prompt_tokens` already includes the cached prefix,
// while `timings` splits it into `prompt_n` (recomputed) and `cache_n` (reused) — so the two are read
// as one number and never summed; `timings` assembles the counts only when `usage` did not speak
// (native /completion). `timings` is always mined for the milliseconds, because those are what turn a
// token count into a rate this server actually produced, and one request is one data point.
func parseUsage(raw json.RawMessage, timings json.RawMessage) *ir.Usage {
	var u, t map[string]json.RawMessage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &u)
	}
	if len(timings) > 0 {
		_ = json.Unmarshal(timings, &t)
	}
	got := &ir.Usage{}
	if t != nil {
		if f, ok := getFloat(t, "prompt_ms"); ok {
			got.PromptMs = f
		}
		if f, ok := getFloat(t, "predicted_ms"); ok {
			got.CompletionMs = f
		}
		if n, ok := getInt(t, "draft_n"); ok {
			got.DraftN = n
		}
		if n, ok := getInt(t, "draft_n_accepted"); ok {
			got.DraftAccepted = n
		}
		if len(u) == 0 {
			if n, ok := getInt(t, "prompt_n"); ok {
				got.PromptTokens, got.HasPrompt = n, true
				if c, ok := getInt(t, "cache_n"); ok {
					got.CacheTokens, got.PromptTokens = c, n+c
				}
			}
			if n, ok := getInt(t, "predicted_n"); ok {
				got.CompletionTokens, got.HasCompletion = n, true
			}
			if !got.HasPrompt && !got.HasCompletion {
				return nil
			}
			return got
		}
	}
	if len(u) == 0 {
		return nil
	}
	if n, ok := getInt(u, "prompt_eval_count"); ok {
		got.PromptTokens, got.HasPrompt = n, true
		if n, ok := getInt(u, "eval_count"); ok {
			got.CompletionTokens, got.HasCompletion = n, true
		}
		if !got.HasCompletion {
			return nil
		}
		if n, ok := getInt(u, "prompt_cache_hit_count"); ok {
			got.CacheTokens = n
		}
		return got
	}
	if n, ok := getInt(u, "prompt_tokens"); ok {
		got.PromptTokens, got.HasPrompt = n, true
	}
	if n, ok := getInt(u, "completion_tokens"); ok {
		got.CompletionTokens, got.HasCompletion = n, true
	}
	// The reused prefix is a *part* of prompt_tokens: OpenAI names it prompt_tokens_details.cached_tokens,
	// llama.cpp /v1 the same, some proxies flatten it to cache_tokens. It is stored as reported and adds
	// nothing to ContextUsed(). An earlier round threw it away unless a config switch said the endpoint
	// excluded it — which is how a session lost the only number a cache hit rate is made of.
	if n, ok := getInt(u, "prompt_tokens_details", "cached_tokens"); ok {
		got.CacheTokens = n
	} else if n, ok := getInt(u, "cache_tokens"); ok {
		got.CacheTokens = n
	}
	if !got.HasPrompt && !got.HasCompletion {
		return nil
	}
	return got
}

func getInt(m map[string]json.RawMessage, path ...string) (int, bool) {
	cur := m
	for i, k := range path {
		raw, ok := cur[k]
		if !ok {
			return 0, false
		}
		if i == len(path)-1 {
			var f float64
			if err := json.Unmarshal(raw, &f); err != nil {
				return 0, false
			}
			return int(f), true
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(raw, &next); err != nil {
			return 0, false
		}
		cur = next
	}
	return 0, false
}

// getFloat reads a number that keeps its fraction. Milliseconds are worth their decimal (a warm
// prefill is 0.13 s, and rounding 127.5 to 127 is a lie we do not need to tell).
func getFloat(m map[string]json.RawMessage, key string) (float64, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}

// parseChunk turns one SSE `data:` line into an Event.
func parseChunk(line string) (Event, error, bool) {
	if !strings.HasPrefix(line, "data:") {
		return Event{}, nil, false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return Event{}, nil, false
	}
	var ch chunk
	if err := json.Unmarshal([]byte(data), &ch); err != nil {
		return Event{}, nil, false
	}
	if ch.Error != nil {
		return Event{}, ch.Error, false
	}
	ev := Event{}
	if len(ch.Choices) > 0 {
		c := ch.Choices[0]
		ev.Content = c.Delta.text()
		ev.Reasoning = c.Delta.reasoning()
		for _, tc := range c.Delta.ToolCalls {
			ev.ToolCalls = append(ev.ToolCalls, ToolCallDelta{
				Index: tc.Index, ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments,
			})
		}
		if c.FinishReason != "" {
			ev.FinishReason = c.FinishReason
		}
	}
	if len(ch.Usage) > 0 || len(ch.Timings) > 0 {
		// llama.cpp puts both in the last chunk when stream_options.include_usage is set, which is what
		// we ask for (see streamOptions); a usage-free chunk with timings alone still becomes a Usage so
		// the milliseconds are not thrown away.
		ev.Usage = parseUsage(ch.Usage, ch.Timings)
	}
	if ev.Content == "" && ev.Reasoning == "" && len(ev.ToolCalls) == 0 &&
		ev.FinishReason == "" && ev.Usage == nil {
		return ev, nil, false
	}
	return ev, nil, true
}

type chunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Choices []choiceMessage `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
	// Timings is llama.cpp's own measurement of the reply, carried by the last streamed chunk next
	// to usage: prompt_n/cache_n/prompt_ms/predicted_ms plus draft_n/draft_n_accepted when speculative
	// decoding is on. OpenAI servers leave it empty.
	Timings json.RawMessage `json:"timings"`
	Error   *apiError       `json:"error"`
}

func or(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func orEmptyObj(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
