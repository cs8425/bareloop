package agent

// What a summary is asked about, and how it is asked — the two questions a cache is built out of.
//
// The live measurement these tests are named for: a turn reused 171,147 of 184,383 prompt tokens, and
// the summary chain that ran moments later reused 0 on every one of its parts. Nothing about the
// conversation had changed. The chain had been cutting the region with a ruler that charged tokens the
// summariser was never given, asking each part in a request whose first byte was new to the provider,
// and leaving out of the summary the one field that switches the model's thinking on — which changes
// every byte of the prompt.
//
// The tests below are the three claims that fix it: a summary request begins with bytes the provider
// already has; a size the server reported outranks an estimate; and every call of a session is cut from
// one thinking level.

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/compact"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

// ─── summaryPayload: which bytes are worth their bytes ───────────────────────

// sentFixture is a payload of one system message and two entries: the smallest shape where the offset
// between a message index and an entry index is not zero, which is exactly where an off-by-one becomes
// a summary of the wrong conversation.
func sentFixture() ([]asm.Message, []string, []llm.Tool) {
	sent := []asm.Message{
		{Role: "system", Content: "You are a test assistant."},
		{Role: "user", Content: "the oldest question still on the path"},
		{Role: "assistant", Content: "the answer the loop wrote"},
	}
	return sent, []string{"system:aa", "e1", "e2"}, []llm.Tool{{Name: "fs_read"}}
}

// TestSummaryPayloadIsWarmWhileTheSentPayloadIsStillTheFront: two entries have been committed since the
// request was made, so what the provider holds is now the front of the path rather than all of it. What
// a part is cut from and what may be claimed about the cache are two questions — the first is answered
// by the payload assembled now, because that is the only one that describes the two entries nobody has
// sent yet, and the second is answered by the sent payload.
func TestSummaryPayloadIsWarmWhileTheSentPayloadIsStillTheFront(t *testing.T) {
	sent, sentIDs, tools := sentFixture()
	cur := append(append([]asm.Message{}, sent...),
		asm.Message{Role: "user", Content: "a question that arrived after it"},
		asm.Message{Role: "assistant", Content: "and its answer"})
	curIDs := append(append([]string{}, sentIDs...), "e3", "e4")

	refs := summaryPayload(sent, sentIDs, cur, curIDs, tools, 4)
	if !refs.warm {
		t.Fatal("a payload that is still the front of the path is exactly the prefix worth sending")
	}
	if len(refs.msgs) != len(cur) {
		t.Fatalf("a summary is cut from the payload that describes the whole path: %d messages, want %d — the two entries nobody has sent are still entries that have to be summarised",
			len(refs.msgs), len(cur))
	}
	// A round wants everything from the top through its cut, and nothing behind it. The offset
	// between a message index and an entry index is the whole danger: one system message in front
	// shifts every slice, and the summary lands over the wrong conversation.
	if pre, ok := refs.through(1); !ok || len(pre) != 2 || !reflect.DeepEqual(pre, cur[:2]) {
		t.Fatalf("a round cut after entry 0 = %v, want the system head and entry 0", pre)
	}
	if pre, ok := refs.through(2); !ok || !reflect.DeepEqual(pre, cur[:3]) {
		t.Fatalf("entry 1 sits at message 2 behind a system head, got %v (%v)", pre, ok)
	}
	// An entry past the sent prefix is cold, not unknowable: it is on the path, the payload assembled
	// now carries it, and flattening it into a transcript would lose every tool result whose answer
	// travels in structuredContent.
	if pre, ok := refs.through(3); !ok || !reflect.DeepEqual(pre, cur[:4]) {
		t.Fatalf("entry 2 arrived after the last request and still has to reach the summariser, got %v (%v)", pre, ok)
	}
	if pre, ok := refs.through(4); !ok || len(pre) != len(cur) {
		t.Fatalf("a round reaching the end of the path wants the whole payload, got %d messages (%v)", len(pre), ok)
	}
}

// TestSummaryPayloadDropsTheSentPayloadTheMomentThePathMoves: a KV cache is keyed on bytes, so a
// payload that differs anywhere is worth nothing — and sending it anyway is worse than slow, because the
// summary would then be about a conversation that no longer exists. A rewritten entry, an entry edited
// where it stands, a rollback, an edited system prompt and a payload nobody identified are the ways.
func TestSummaryPayloadDropsTheSentPayloadTheMomentThePathMoves(t *testing.T) {
	sent, sentIDs, tools := sentFixture()
	cur := append(append([]asm.Message{}, sent...), asm.Message{Role: "user", Content: "newest"})
	curIDs := append(append([]string{}, sentIDs...), "e3")

	// A rewritten entry: the third message of the path is a different entry now.
	rewritten := []string{"system:aa", "e1", "zzz"}
	if r := summaryPayload(sent, rewritten, cur, curIDs, tools, 3); r.warm {
		t.Fatal("an entry that was rewritten is not a prefix, however many messages it shares")
	}
	// The path got shorter than what was sent (a rollback): the tail of the cache is gone.
	if r := summaryPayload(sent, sentIDs, cur[:2], sentIDs[:2], tools, 1); r.warm {
		t.Fatal("a payload longer than the path cannot be a prefix of it")
	}
	// An entry rewritten where it stands: the ids still match and the bytes are a different request.
	// This is what /entry/patch does, which is why ids alone cannot carry the claim.
	patched := append([]asm.Message{}, cur...)
	patched[1] = asm.Message{Role: "user", Content: "the oldest question, rewritten after the fact"}
	if r := summaryPayload(sent, sentIDs, patched, curIDs, tools, 3); r.warm {
		t.Fatal("an entry edited in place keeps its id, and the provider has never seen these bytes")
	}
	// Editing the system prompt changes every byte of the prompt while adding no entry at all.
	if r := summaryPayload(cur, curIDs, cur, []string{"system:bb", "e1", "e2", "e3"}, tools, 3); r.warm {
		t.Fatal("a different system prompt is a different prompt: nothing ahead of the first message survives it")
	}
	// Nothing named the messages, so nothing can be trusted about them.
	if r := summaryPayload(sent, []string{"", "", ""}, cur, []string{"", "", "", ""}, tools, 3); r.warm {
		t.Fatal("empty ids are a prefix of anything, and that is the one answer that must not be yes")
	}
	if r := summaryPayload(sent, nil, cur, []string{"system:aa", "e1", "e2", "e3"}, tools, 3); r.warm {
		t.Fatal("no ids at all is the same case with fewer bytes")
	}

	// In every one of those cases the payload assembled now is used, and used whole: the summary still
	// has to cover everything on the path.
	r := summaryPayload(sent, rewritten, cur, curIDs, tools, 3)
	if len(r.msgs) != len(cur) {
		t.Fatalf("the honest payload is the one assembled now: %d messages, want %d", len(r.msgs), len(cur))
	}
	if got, ok := r.through(3); !ok || len(got) != len(cur) {
		t.Fatalf("a rebuilt payload covers the whole path, got %d messages (%v)", len(got), ok)
	}
}

// TestSummaryPayloadWithoutASystemPrompt: the offset is where a region index becomes a message index,
// and with no system head there is nothing to skip.
func TestSummaryPayloadWithoutASystemPrompt(t *testing.T) {
	cur := []asm.Message{{Role: "user", Content: "one"}, {Role: "assistant", Content: "two"}}
	r := summaryPayload(nil, nil, cur, []string{"e1", "e2"}, nil, 2)
	if got, ok := r.through(1); !ok || len(got) != 1 || got[0].Content != "one" {
		t.Fatalf("entry 0 is message 0 when nothing precedes it: %+v (%v)", got, ok)
	}
	if got, ok := r.through(2); !ok || len(got) != 2 {
		t.Fatalf("a round cut at the end of the path wants both messages, got %d (%v)", len(got), ok)
	}
}

// TestSummaryPayloadRefusesToSliceAPayloadItCannotRead: ids that do not describe the messages are worse
// than no ids, because a boundary drawn through them cuts the wrong conversation. Everything still gets
// summarised — the caller falls back to a flat transcript and says so — because a compaction that
// refuses to run is the worse failure.
func TestSummaryPayloadRefusesToSliceAPayloadItCannotRead(t *testing.T) {
	cur := []asm.Message{{Role: "user", Content: "one"}}
	r := summaryPayload(nil, nil, cur, []string{"e1", "e2", "e3"}, nil, 3)
	if r.msgs != nil {
		t.Fatal("three entries cannot be cut out of one message")
	}
	if _, ok := r.through(1); ok {
		t.Fatal("and nothing can be sliced out of it either")
	}
}

// ─── requestIdentity: one thinking level per session ─────────────────────────

func identityFixture() Config {
	temp := 0.7
	return Config{
		ID:              "identity",
		LLM:             llm.Config{Model: "a-reasoning-model", NCTX: 4096},
		Temperature:     &temp,
		MaxTokens:       2048,
		ReasoningEffort: "high",
		Extra:           map[string]any{"enable_thinking": true, "chat_template_kwargs": map[string]any{"thinking": true}},
	}
}

// TestSummaryRequestKeepsTheThinkingLevelOfTheTurn is the field that cost the eight-minute summaries.
// A server told to reason renders a different template around the same messages, so a summary asked
// without the loop's reasoning_effort is not the same prompt — and "not the same prompt" is the whole
// conversation over again.
func TestSummaryRequestKeepsTheThinkingLevelOfTheTurn(t *testing.T) {
	cfg := identityFixture()
	call := compact.SummaryCall{
		Messages:    []asm.Message{{Role: "user", Content: "summarise"}},
		Tools:       []llm.Tool{{Name: "fs_read"}},
		MaxTokens:   777,
		Temperature: 0.2,
	}
	got := summaryRequest(cfg, call)
	wants := requestIdentity(cfg) // exactly what the loop sends

	if got.ReasoningEffort != wants.ReasoningEffort || got.ReasoningEffort != "high" {
		t.Fatalf("the summary asked with reasoning_effort %q while the turn asked with %q: a thinking level nobody chose costs a full prefill of every message the provider already held",
			got.ReasoningEffort, wants.ReasoningEffort)
	}
	if !reflect.DeepEqual(got.Extra, wants.Extra) {
		t.Fatalf("extra = %v, want the session's own %v: that is where a thinking switch is spelled for the servers that take it that way", got.Extra, wants.Extra)
	}
	if got.Model != wants.Model || got.Model != "a-reasoning-model" {
		t.Fatalf("model = %q, want the session's model", got.Model)
	}
	if wants.MaxTokens != cfg.MaxTokens {
		t.Fatal("the loop's own request has to carry the session's output cap, or identity means nothing")
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Fatal("the summariser is asked to be more deterministic than the conversation it reads (§3.5)")
	}
	if got.MaxTokens != 777 {
		t.Fatalf("max_tokens = %d, want the summariser's own budget", got.MaxTokens)
	}
	if got.Stream {
		t.Fatal("a summary is one complete call, not a stream (§3.5)")
	}
	if len(got.Tools) != 1 {
		t.Fatal("the tools of the turn are part of the prompt the summary wants to reuse")
	}
}

// ─── the replay, end to end ───────────────────────────────────────────────────

// chainProvider records every summary call and answers it. The usage is part of the fixture rather than
// decoration: the planning numbers and the log lines are built on the numbers the server reports, so a
// provider that reported nothing would let a regression in either pass.
type chainProvider struct {
	mu   sync.Mutex
	asks []llm.Request
}

func (p *chainProvider) Name() string { return "chain" }

// Stream is not what these tests drive: the chain is asked through Complete. An answer here would only
// hide a turn nobody scripted.
func (p *chainProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, error) {
	return nil, errChainNoTurn
}

func (p *chainProvider) Complete(_ context.Context, req llm.Request) (*llm.Result, error) {
	p.mu.Lock()
	n := len(p.asks)
	p.asks = append(p.asks, req)
	p.mu.Unlock()
	// Counted from the request itself, so that the numbers beside the log line cannot be satisfied by a
	// fixture that made them up.
	prompt := promptFor(req)
	cache := prompt - 120
	if cache < 0 {
		cache = 0
	}
	return &llm.Result{
		Content:      "SUMMARY " + strconv.Itoa(n+1),
		FinishReason: "stop",
		Usage:        &ir.Usage{PromptTokens: prompt, CacheTokens: cache, CompletionTokens: 40, HasPrompt: true},
	}, nil
}

// promptFor is the fake's four-bytes-to-a-token count of a request, tools included. The tests are about
// a byte budget, and an invented constant would let a part vanish from its own request without a single
// number in the fixture changing.
func promptFor(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		n += len(msgText(m)) + len(m.Role)
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description)
	}
	if n < 4 {
		n = 4
	}
	return n / 4
}

func (p *chainProvider) NCTX(context.Context) int { return 0 }

func (p *chainProvider) all() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request{}, p.asks...)
}

func (p *chainProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.asks)
}

// chainFixture is a session with a system prompt, a tool the loop can be shown, a small window and a
// thinking level — every field these tests are about, and nothing else.
func chainFixture(t *testing.T, id string, nctx int) (*Session, *chainProvider) {
	t.Helper()
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(context.Background(), Config{
		ID:           id,
		SystemPrompt: "You are a test assistant. Be brief.",
		LLM:          llm.Config{BaseURL: "http://127.0.0.1:1/v1", Model: "a-reasoning-model", NCTX: nctx},
		// One builtin is enough to put a tool in the array, which is the whole point: an assertion
		// about "the same tools" proves nothing while both sides are empty.
		BuiltinTools:    []string{"get_datetime"},
		ReasoningEffort: "high",
		Extra:           map[string]any{"enable_thinking": true},
	})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	cp := &chainProvider{}
	s.provider = cp
	t.Cleanup(func() { _ = s.Close() })
	return s, cp
}

func fillChainPath(t *testing.T, s *Session, n, chars int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := &ir.Entry{Kind: ir.KindUser, Role: "user", Content: strings.Repeat("abcd ", chars/5), Ts: time.Now().UTC()}
		if i%2 == 1 {
			e.Kind, e.Role = ir.KindAssistant, "assistant"
		}
		if _, err := s.appendEntry(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// wantPartOnePrefix is gone with the chain; replay_test.go's TestReplayRoundOneIsABytePrefixOfTheSent
// payload asserts the same contract on the round that replaced part 1.

// isOneOf reports whether the message is byte-for-byte one of the payload's own.
func isOneOf(sent []asm.Message, m asm.Message) bool {
	for _, s := range sent {
		if reflect.DeepEqual(s, m) {
			return true
		}
	}
	return false
}

func msgText(m asm.Message) string {
	s, _ := m.Content.(string)
	return s
}

func dumpForTest(msgs []asm.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString("\n  " + strconv.Itoa(i) + " " + m.Role + " " + clipForTest(msgText(m), 60))
	}
	return b.String()
}

func clipForTest(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

type chainError struct{}

func (e *chainError) Error() string { return "chain provider: no scripted turn, only summaries" }

var errChainNoTurn = &chainError{}

// ─── the two shapes a fixture kept missing ──────────────────────────────────
//
// Every test above handed the session a payload that described the whole path, which is the one case
// where counting messages against entries happens to give the right answer. Out of a running loop that
// never holds: the payload still in the provider's cache is a prefix of the path, so it is shorter than
// it, and the two counts part company. These two tests are the live failure and the claim it traded on.

// TestAPrefixShorterThanThePathStillHasASystemHead is the live bug in one fixture. The offset between an
// entry index and a message index was worked out as len(ids) - entries: three messages over a
// three-entry path is zero, read as "this payload has no system head", which shifted every slice of
// every part one message to the left. Part 1 arrived as the system prompt alone; the server counted 655
// prompt tokens for a part the ruler had priced at 3,840; the line beside it said "reused 529 (prefix
// of the last request)"; and the summary was committed over a real user turn.
func TestAPrefixShorterThanThePathStillHasASystemHead(t *testing.T) {
	built := &asm.Built{
		System: "You are a test assistant.",
		Entries: []*ir.Entry{
			{ID: "e1", Kind: ir.KindUser, Role: "user", Content: "ten kilobytes of notes the user just pasted"},
			{ID: "e2", Kind: ir.KindAssistant, Role: "assistant", Content: "one line"},
			{ID: "e3", Kind: ir.KindUser, Role: "user", Content: "and a second question"},
		},
		Messages: []asm.Message{
			{Role: "user", Content: "ten kilobytes of notes the user just pasted"},
			{Role: "assistant", Content: "one line"},
			{Role: "user", Content: "and a second question"},
		},
	}
	cur, curIDs := built.All(), asm.EntryIDs(built)
	// The loop has only ever sent the first two entries of a three-entry path: that is what a cache is.
	sent, sentIDs := cur[:3], curIDs[:3]

	refs := summaryPayload(sent, sentIDs, cur, curIDs, nil, len(built.Entries))
	if !refs.warm {
		t.Fatal("the sent payload is the front of the path, so it is worth its bytes")
	}
	pre, ok := refs.through(1)
	if !ok || len(pre) != 2 || !strings.Contains(msgText(pre[len(pre)-1]), "ten kilobytes") {
		t.Fatalf("a round cut after entry 0 was handed %d message(s) and the entry it was told to read is not in it:%s",
			len(pre), dumpForTest(pre))
	}
	// A payload shorter than the path still counts messages from its own front: the offset comes from
	// the system head, not from len(sent) against the path. Work it out the second way and every slice
	// shifts one message to the left — the summary lands over the wrong conversation, which is the bug
	// this fixture keeps dead.
	if pre, ok := refs.through(3); !ok || len(pre) != len(cur) || pre[len(pre)-1].Content != "and a second question" {
		t.Fatalf("the entry nobody has sent yet still has to be reachable behind the head, got %v (%v)", pre, ok)
	}
}
