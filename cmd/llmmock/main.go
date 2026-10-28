// Command llmmock is a fake OpenAI-compatible chat completion server, used to exercise bareloopd
// without a real model: streaming, usage reporting (three dialects), reasoning content, tool
// calls and n_ctx probing.
//
//	llmmock -addr 127.0.0.1:8901 -nctx 4096
//
// Point a session at http://127.0.0.1:8901/v1 (any model name works).
//
// Behaviour worth knowing when testing:
//
//   - a prompt containing "tool" makes the model ask for the first available tool;
//   - a prompt containing "slow" streams slowly enough to test the stop button;
//   - a prompt containing "big" produces a long reply, useful for compaction testing;
//   - the compaction summary call (its instruction is in the last user message) gets a short
//     summary back, so compaction can be tested end to end.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var requests atomic.Int64

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type fnCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function fnCall `json:"function"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream"`
	Tools     []toolDef `json:"tools"`
	MaxTokens int       `json:"max_tokens"`
}

// text renders message content, which may be a string or a content-part array.
func (m message) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []contentPart
	if json.Unmarshal(m.Content, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "image_url":
				sb.WriteString("[image]")
			default:
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return string(m.Content)
}

func (r chatRequest) lastUser() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].text()
		}
	}
	return ""
}

func (r chatRequest) promptBytes() int {
	n := 0
	for _, m := range r.Messages {
		n += len(m.text()) + 8
	}
	return n
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8901", "listen address")
	nctx := flag.Int("nctx", 4096, "context window reported by /props")
	model := flag.String("model", "mock-model", "model name to report")
	latency := flag.Duration("latency", 12*time.Millisecond, "delay between streamed chunks")
	usageKind := flag.String("usage", "openai", "usage dialect: openai|llamacpp|ollama|none")
	flag.Parse()

	log.Printf("llmmock on http://%s/v1 (n_ctx=%d model=%s usage=%s)", *addr, *nctx, *model, *usageKind)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		handle(w, r, req, *model, *usageKind, *latency)
	})
	mux.HandleFunc("/v1/props", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"n_ctx": *nctx, "n_predict": 512, "model": *model})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"id": *model, "object": "model", "context_length": *nctx}},
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	srv := &http.Server{Addr: *addr, Handler: logReq(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		log.Printf("#%d %s %s", n, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// reply is the decided completion.
type reply struct {
	reasoning string
	content   string
	toolCalls []toolCall
}

func decide(req chatRequest) reply {
	last := req.lastUser()
	lower := strings.ToLower(last)

	// The summary call: the compaction instruction ends the request. Answer shortly so the
	// acceptance check (spec §3.4) passes.
	if strings.Contains(lower, "summarize the conversation") || strings.Contains(lower, "context summary") {
		return reply{content: "SUMMARY · The conversation was with a mock model. The user asked several questions, the assistant answered and called mock tools. Open decisions: none. Constraints seen: mock environment only. Next step: continue from the most recent messages.\n"}
	}
	if len(req.Tools) > 0 && strings.Contains(lower, "tool") {
		name := req.Tools[0].Function.Name
		args, _ := json.Marshal(map[string]any{"text": "hello from the mock model"})
		return reply{
			reasoning: "The user asked for a tool, so I will call the first one available.\n",
			toolCalls: []toolCall{{
				ID:       fmt.Sprintf("call_%08x", rand.Int31()),
				Type:     "function",
				Function: fnCall{Name: name, Arguments: string(args)},
			}},
		}
	}
	if strings.Contains(lower, "big") {
		var sb strings.Builder
		sb.WriteString("## A long answer\n\n")
		for i := 0; i < 140; i++ {
			fmt.Fprintf(&sb, "%d. The quick brown fox jumps over the lazy dog and keeps generating tokens so the context window fills up predictably.\n", i)
		}
		return reply{content: sb.String()}
	}

	user := last
	if len(user) > 300 {
		user = user[:300] + "…"
	}
	return reply{
		reasoning: fmt.Sprintf("The user said %q. I will answer with markdown so the renderer is exercised.\n", user),
		content: fmt.Sprintf(
			"You said: **%s**\n\nHere is a list:\n\n* one\n* two\n  * nested\n\n```go\nfmt.Println(\"request %d\")\n```\n\n| key | value |\n| --- | --- |\n| requests | %d |\n\n> mock model, nothing was thought here\n",
			user, requests.Load(), requests.Load()),
	}
}

// usageFor returns the usage object and/or the timings object for the chosen dialect.
func usageFor(kind string, promptBytes, completionLen int) (map[string]any, map[string]any) {
	prompt := promptBytes/4 + 1
	completion := completionLen/4 + 1
	cached := prompt * 2 / 3
	switch kind {
	case "llamacpp":
		// llama.cpp native: prompt_n EXCLUDES cache_n.
		return nil, map[string]any{"prompt_n": prompt, "cache_n": cached, "predicted_n": completion}
	case "ollama":
		return map[string]any{"prompt_eval_count": prompt, "eval_count": completion}, nil
	case "none":
		return nil, nil
	default:
		return map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
			"prompt_tokens_details": map[string]any{
				"cached_tokens": cached,
			},
		}, nil
	}
}

func finishReason(rep reply) string {
	if len(rep.toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func handle(w http.ResponseWriter, r *http.Request, req chatRequest, model, usageKind string, latency time.Duration) {
	if req.Model != "" {
		model = req.Model
	}
	rep := decide(req)

	completion := rep.content
	if len(rep.toolCalls) > 0 {
		completion = rep.toolCalls[0].Function.Arguments
	}
	usage, timings := usageFor(usageKind, req.promptBytes(), len(completion))

	if strings.Contains(strings.ToLower(req.lastUser()), "slow") {
		latency = 400 * time.Millisecond
	}

	id := fmt.Sprintf("chatcmpl-mock-%d", requests.Load())

	if !req.Stream {
		msg := map[string]any{
			"role":              "assistant",
			"content":           rep.content,
			"reasoning_content": rep.reasoning,
		}
		if len(rep.toolCalls) > 0 {
			msg["tool_calls"] = rep.toolCalls
		}
		body := map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finishReason(rep)}},
		}
		if usage != nil {
			body["usage"] = usage
		}
		if timings != nil {
			body["timings"] = timings
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	chunk := func(obj map[string]any) bool {
		b, err := json.Marshal(obj)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	meta := func() map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model}
	}

	for _, tok := range splitTokens(rep.reasoning) {
		body := meta()
		body["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": tok}}}
		if !chunk(body) || interrupted(r, latency) {
			return
		}
	}
	for _, tok := range splitTokens(rep.content) {
		body := meta()
		body["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": tok}}}
		if !chunk(body) || interrupted(r, latency) {
			return
		}
	}
	for i, tc := range rep.toolCalls {
		body := meta()
		body["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
			"index":    i,
			"id":       tc.ID,
			"type":     "function",
			"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
		}}}}}
		if !chunk(body) || interrupted(r, latency) {
			return
		}
	}

	final := meta()
	final["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason(rep)}}
	if usage != nil {
		final["usage"] = usage
	}
	if timings != nil {
		final["timings"] = timings
	}
	if !chunk(final) {
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// splitTokens cuts text into small pieces at spaces and newlines, keeping the separators, so
// the UI sees many small deltas.
func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// interrupted waits for the chunk delay, returning true when the client went away (which is how
// an abort reaches the mock: closing the HTTP connection).
func interrupted(r *http.Request, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		return true
	case <-timer.C:
		return false
	}
}
