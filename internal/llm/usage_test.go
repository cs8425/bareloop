package llm

import (
	"encoding/json"
	"testing"

	"bareloop/internal/ir"
)

// The job of parseUsage is to make several servers agree on one meaning (ir.Usage): PromptTokens is
// the whole prompt, cached prefix included, CacheTokens is the part of it that was reused, and the
// millisecond figures are this reply's own measurements. Every fixture below is the shape a real
// backend sends — llama.cpp /v1 with stream_options.include_usage answers with `usage` and `timings`
// for the same reply, and reading those two as a sum is the bug these tests exist to keep out.

func fixture(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if !json.Valid([]byte(s)) {
		panic("test fixture is not valid json: " + s)
	}
	return json.RawMessage(s)
}

func TestParseUsageOneSemantics(t *testing.T) {
	cases := []struct {
		name    string
		usage   string
		timings string
		want    *ir.Usage // nil means "this reply says nothing usable"
	}{
		{
			name:  "openai dialect keeps the cached tokens it reports",
			usage: `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":900}}`,
			// A previous round dropped cached_tokens here unless a config switch said the endpoint
			// excluded them from prompt_tokens — which left a session unable to compute a hit rate.
			want: &ir.Usage{
				PromptTokens: 1000, CacheTokens: 900, CompletionTokens: 50,
				HasPrompt: true, HasCompletion: true,
			},
		},
		{
			name:    "llama.cpp sends usage and timings for one reply and they are not summed",
			usage:   `{"prompt_tokens":835,"completion_tokens":2,"total_tokens":837,"prompt_tokens_details":{"cached_tokens":830}}`,
			timings: `{"prompt_n":5,"cache_n":830,"prompt_ms":145.2,"predicted_n":2,"predicted_ms":14.5,"predicted_per_second":137.9,"draft_n":33,"draft_n_accepted":30}`,
			// 835 = prompt_n 5 + cache_n 830: the counts come from usage, timings only brings the ms.
			want: &ir.Usage{
				PromptTokens: 835, CacheTokens: 830, CompletionTokens: 2,
				PromptMs: 145.2, CompletionMs: 14.5, DraftN: 33, DraftAccepted: 30,
				HasPrompt: true, HasCompletion: true,
			},
		},
		{
			name:    "timings alone assemble the whole prompt",
			timings: `{"prompt_n":395,"cache_n":292,"prompt_ms":603.2,"predicted_n":12,"predicted_ms":88.4}`,
			want: &ir.Usage{
				PromptTokens: 687, CacheTokens: 292, CompletionTokens: 12,
				PromptMs: 603.2, CompletionMs: 88.4,
				HasPrompt: true, HasCompletion: true,
			},
		},
		{
			name:  "ollama native keeps its cache hit count",
			usage: `{"prompt_eval_count":410,"eval_count":33,"prompt_cache_hit_count":300}`,
			want: &ir.Usage{
				PromptTokens: 410, CacheTokens: 300, CompletionTokens: 33,
				HasPrompt: true, HasCompletion: true,
			},
		},
		{
			name:  "ollama half a report is no report",
			usage: `{"prompt_eval_count":410}`,
			want:  nil,
		},
		{
			name:  "a total without parts is no report",
			usage: `{"total_tokens":9}`,
			want:  nil,
		},
		{
			name:    "an empty pair of objects is no report",
			usage:   `{}`,
			timings: `{}`,
			want:    nil,
		},
		{
			name:    "garbage timings are ignored, usage still counts",
			usage:   `{"prompt_tokens":12,"completion_tokens":3}`,
			timings: `"not an object"`,
			want: &ir.Usage{
				PromptTokens: 12, CompletionTokens: 3,
				HasPrompt: true, HasCompletion: true,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseUsage(fixture(c.usage), fixture(c.timings))
			if c.want == nil {
				if got != nil {
					t.Fatalf("must report nothing, got %+v", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("usage was reported, parseUsage returned nil")
			}
			if *got != *c.want {
				t.Fatalf("got %+v, want %+v", *got, *c.want)
			}
			if used := got.ContextUsed(); used != c.want.PromptTokens+c.want.CompletionTokens {
				t.Fatalf("ContextUsed must be prompt + completion, got %d", used)
			}
		})
	}
}

// TestStreamedFinalChunkKeepsItsTimings guards the one place the numbers used to be thrown away:
// the streaming path parsed usage with no timings at all, so a whole run produced no milliseconds.
func TestStreamedFinalChunkKeepsItsTimings(t *testing.T) {
	// Verbatim shape of the last `data:` line of a llama.cpp stream (usage plus timings together).
	line := `data: {"choices":[],"created":1,"id":"a","model":"gemma","object":"chat.completion.chunk",` +
		`"system_fingerprint":"fp","usage":{"completion_tokens":1,"prompt_tokens":18,"total_tokens":19,` +
		`"prompt_tokens_details":{"cached_tokens":14}},` +
		`"timings":{"cache_n":14,"prompt_n":4,"prompt_ms":740.1,"predicted_n":1,"predicted_ms":41.0}}`
	ev, err, ok := parseChunk(line)
	if err != nil || !ok {
		t.Fatalf("the final chunk must produce an event (ok=%v err=%v)", ok, err)
	}
	if ev.Usage == nil {
		t.Fatal("final chunk usage lost")
	}
	want := ir.Usage{
		PromptTokens: 18, CacheTokens: 14, CompletionTokens: 1,
		PromptMs: 740.1, CompletionMs: 41.0,
		HasPrompt: true, HasCompletion: true,
	}
	if *ev.Usage != want {
		t.Fatalf("got %+v, want %+v", *ev.Usage, want)
	}
}

func TestParseChunkContentOnlyHasNoUsage(t *testing.T) {
	ev, err, ok := parseChunk(`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	if err != nil || !ok {
		t.Fatalf("a content chunk must produce an event (ok=%v err=%v)", ok, err)
	}
	if ev.Content != "hi" || ev.Usage != nil {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if _, _, ok := parseChunk(`data: [DONE]`); ok {
		t.Fatal("[DONE] must not produce an event")
	}
}

// TestToResultKeepsTimings covers the non-streaming plumbing: the answer's timings have to reach the
// usage even when the counts arrive in the choice rather than at the top level.
func TestToResultKeepsTimings(t *testing.T) {
	usage := `{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":80}}`
	timings := `{"prompt_n":20,"cache_n":80,"prompt_ms":250.5,"predicted_n":7,"predicted_ms":70.25}`

	res := completionResponse{Usage: fixture(usage), Timings: fixture(timings)}.toResult()
	if res.Usage == nil || res.Usage.PromptMs != 250.5 || res.Usage.CompletionMs != 70.25 {
		t.Fatalf("top level timings lost: %+v", res.Usage)
	}
	if res.Usage.PromptTokens != 100 || res.Usage.CacheTokens != 80 {
		t.Fatalf("timings must not be added to a prompt that already includes them: %+v", *res.Usage)
	}

	res = completionResponse{
		Choices: []choiceMessage{{Index: 0, Message: msgPayload{Content: fixture(`"ok"`)}, Usage: fixture(usage)}},
		// Timings that carry no counts of their own, so the usage sitting on the choice is the only
		// place the token numbers exist — and the milliseconds still have to survive the trip.
		Timings: fixture(`{"predicted_ms":70.25}`),
	}.toResult()
	if res.Usage == nil || res.Usage.PromptTokens != 100 || res.Usage.CacheTokens != 80 ||
		res.Usage.CompletionMs != 70.25 {
		t.Fatalf("choice level usage with top level timings: %+v", res.Usage)
	}
}
