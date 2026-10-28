package agent

// n_ctx is decided by an operator's rule, not by whichever number was read last. The rule is small
// enough to write as a table, and it is the part of the system that a silent change would most
// damage: a value larger than the server's means every request is refused by the provider, a value
// smaller than the server's wastes a window the user paid for.

import "testing"

func TestResolveNCTXFollowsTheOperatorsRule(t *testing.T) {
	cases := []struct {
		cfg, server, want int
		what              string
	}{
		{0, 40960, 40960, "config unset: the server decides"},
		{40960, 0, 40960, "server reports nothing: config stands"},
		{0, 0, 0, "neither: no window, the UI has to ask"},
		{16384, 40960, 16384, "config below the server: the deliberate under-spend stands"},
		{262144, 40960, 40960, "config above the server: it cannot be honoured"},
		{40960, 40960, 40960, "equal"},
		{-1, 40960, 40960, "nonsense config is not a window"},
		{40960, -1, 40960, "nonsense probe result is not a window"},
		{262144, 0, 262144, "a server that does not report (OpenAI) keeps config"},
	}
	for _, c := range cases {
		if got := resolveNCTX(c.cfg, c.server); got != c.want {
			t.Errorf("%s: resolveNCTX(%d, %d) = %d, want %d", c.what, c.cfg, c.server, got, c.want)
		}
	}
}

// TestNctxSourceExplainsTheClamp: the drift that cost a debugging session was a configured 262144
// against a 40960 server. The label has to say which number won and why.
func TestNctxSourceExplainsTheClamp(t *testing.T) {
	if got := nctxSource(262144, 40960); got != "server (config asked for more than the server has)" {
		t.Fatalf("source = %q", got)
	}
	if got := nctxSource(16384, 40960); got != "config (below the server window)" {
		t.Fatalf("source = %q", got)
	}
	if got := nctxSource(0, 40960); got != "server" {
		t.Fatalf("source = %q", got)
	}
	if got := nctxSource(40960, 0); got != "config" {
		t.Fatalf("source = %q", got)
	}
	if got := nctxSource(40960, 40960); got != "config (the server reports the same window)" {
		// Equal is not "config asked for more": the first live run of this feature printed exactly
		// that misleading label while config and server both said 40960.
		t.Fatalf("source for equal numbers = %q", got)
	}
	if got := nctxSource(0, 0); got != "unset" {
		t.Fatalf("both unknown must be reported as unset, got %q", got)
	}
}

func TestIsContextOverflowRecognisesTheRealAnswers(t *testing.T) {
	yes := []string{
		`llm: 400 Bad Request: {"error":{"code":400,"message":"request (185164 tokens) exceeds the available context size (40960 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":185164,"n_ctx":40960}}`,
		"request (184939 tokens) exceeds the available context size (40960 tokens)",
		`{"error":{"type":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens"}}`,
		"Context length exceeded: prompt is too long",
		"This model's maximum context length is 4096 tokens. However, you requested 9000 tokens",
		"Please reduce your prompt; or completion length.",
	}
	for _, s := range yes {
		if !isContextOverflow(s) {
			t.Errorf("must be recognised as an overflow: %s", s[:min(80, len(s))])
		}
	}
	no := []string{
		`llm: 400 Bad Request: {"error":{"code":400,"message":"Assistant message must contain either 'content' or 'tool_calls'!"}}`,
		"llm: 500 Internal Server Error",
		"Post \"http://192.168.11.113:8080/v1/chat/completions\": dial tcp: connection refused",
		"empty stream",
		"",
	}
	for _, s := range no {
		if isContextOverflow(s) {
			t.Errorf("not an overflow: %s", s)
		}
	}
}
