package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"bareloop/internal/asm"
	"bareloop/internal/llm"
)

// Tests for deltaBatcher (delta.go): what a streamed reply costs the wire. The rule under test is
// the producer's — LLM fragments are gathered and merged before anything is stamped, so what
// leaves is ordinary delta events, fewer of them, each with its own stamp — and three properties
// have to hold at once: nothing is lost (the page appends the same characters), nothing is late
// twice over (the first token never waits, and a paused model still delivers what it already
// said), and nothing arrives after the frames that depend on it (every fragment is on the wire
// before the entry and the Done).

// deltaFixture is a session whose model is a script and whose wire is a notebook: every event is
// captured with the stamp it left the manager with. Unlike awaitingFixture's, the fanout here is
// the capturing one — these tests read the stream, not the log.
func deltaFixture(t *testing.T, win time.Duration, script ...turn) (*Session, *fakeProvider, func() []Event) {
	t.Helper()
	old := deltaWindow
	deltaWindow = win
	t.Cleanup(func() { deltaWindow = old })

	var (
		mu     sync.Mutex
		frames []Event
	)
	m := NewManager(t.TempDir(), false, Config{}, func(e Event) {
		mu.Lock()
		frames = append(frames, e)
		mu.Unlock()
	}, nil)
	cfg := Config{ID: "paced", BuiltinTools: []string{"wait", "get_datetime"},
		LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}}
	s, err := m.Create(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f := &fakeProvider{script: script}
	s.provider = f
	t.Cleanup(func() { _ = s.Close() })
	return s, f, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), frames...)
	}
}

// sayings is one reply's open content, in the order the wire got it. One generate call per test,
// so no stream id is needed to know whose deltas they are.
func sayings(frames []Event) string {
	var b strings.Builder
	for _, e := range frames {
		if e.Type == EvtDelta && e.Delta != nil && !e.Delta.Done {
			b.WriteString(e.Delta.Content)
		}
	}
	return b.String()
}

func TestAFastBurstLeavesAsFewDeltasAsTimeAllows(t *testing.T) {
	var evs []llm.Event
	for _, w := range []string{"the", " quick", " brown", " fox", " jumps"} {
		evs = append(evs, llm.Event{Content: w})
	}
	s, _, frames := deltaFixture(t, 50*time.Millisecond,
		func(context.Context, llm.Request, *gate) (<-chan llm.Event, error) {
			return stream(evs...), nil // every fragment lands within microseconds, then the stream ends
		})

	entry, _, _ := s.generate(context.Background(), &asm.Built{System: "test"}, nil, 1)
	if entry == nil {
		t.Fatal("generate returned no entry")
	}
	want := "the quick brown fox jumps"
	if entry.Content != want {
		t.Errorf("the committed reply is %q, want %q", entry.Content, want)
	}

	got := deltasOf(frames())
	// Five fragments, and the wire carried at most one leading frame plus one merged frame per
	// window the machine happened to open. Far below five is the point; say so loudly when it is
	// not, because a future wire change that quietly restores one-frame-per-token reads harmless.
	if len(got) >= len(evs) {
		t.Errorf("%d fragments left as %d delta events: nothing merged", len(evs), len(got))
	}
	if got[0].Content != "the" {
		t.Errorf("the first fragment did not go out first and alone: %q — the page saw the first token late", got[0].Content)
	}
	if got := sayings(frames()); got != want {
		t.Errorf("merged, the deltas say %q, want %q — batching may not lose or reorder characters", got, want)
	}
	// And each of them is an ordinary event: its own stamp, spent on the way out, the same one
	// every subscriber's frames and the heartbeat's arithmetic count.
	var prev int64 = -1
	for _, e := range frames() {
		if e.Type != EvtDelta || e.Delta == nil || e.Delta.Done {
			continue
		}
		if e.Seq <= prev {
			t.Fatalf("delta stamps do not increase: %d after %d", e.Seq, prev)
		}
		prev = e.Seq
	}
}

// deltasOf is the same list sayings folds, unbuilt, for tests that count frames.
func deltasOf(frames []Event) []Delta {
	var out []Delta
	for _, e := range frames {
		if e.Type == EvtDelta && e.Delta != nil && !e.Delta.Done {
			out = append(out, *e.Delta)
		}
	}
	return out
}

func TestASlowModelPaysNoWindowAtAll(t *testing.T) {
	const win = 30 * time.Millisecond
	words := []string{"one", " two", " three"}
	ch := make(chan llm.Event)
	s, _, frames := deltaFixture(t, win,
		func(context.Context, llm.Request, *gate) (<-chan llm.Event, error) {
			go func() {
				defer close(ch)
				for _, w := range words {
					time.Sleep(2 * win) // slower than the window: every fragment finds it closed
					ch <- llm.Event{Content: w}
				}
			}()
			return ch, nil
		})

	_, _, _ = s.generate(context.Background(), &asm.Built{System: "test"}, nil, 1)
	got := deltasOf(frames())
	if len(got) != len(words) {
		t.Fatalf("%d spaced fragments left %d deltas, want %d — a model slower than the window must not pay for batching:%s",
			len(words), len(got), len(words), describeFrames(frames()))
	}
	for i, d := range got {
		if d.Content != words[i] {
			t.Errorf("delta %d = %q, want %q", i, d.Content, words[i])
		}
	}
}

// TestAPausedModelStillSaidWhatItHad is the timer's reason to exist: with the batch driven by the
// consuming loop, silence on the stream could otherwise hold the last fragments inside the
// window until the next one arrives — which is exactly when a model has run out of things to
// stream and a page is waiting to read what it already watched being written.
func TestAPausedModelStillSaidWhatItHad(t *testing.T) {
	const win = 30 * time.Millisecond
	ch := make(chan llm.Event, 8)
	s, _, frames := deltaFixture(t, win,
		func(context.Context, llm.Request, *gate) (<-chan llm.Event, error) {
			return ch, nil // the test decides when the stream ends: close(ch) below
		})

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.generate(context.Background(), &asm.Built{System: "test"}, nil, 1)
	}()

	ch <- llm.Event{Content: "alpha"}
	ch <- llm.Event{Content: " beta"}
	// No third fragment, and the channel stays open: only the timer can move " beta" now.
	// (And the channel not being closed is what makes this the timer and not the final flush.)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sayings(frames()) == "alpha beta" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := sayings(frames()); got != "alpha beta" {
		t.Errorf("the second fragment sat in the window while the model paused: %q", got)
	}

	close(ch) // let generate drain and return
	<-done
}

func TestAMergedDeltaGluesWhatThePageWouldGlue(t *testing.T) {
	t0 := time.Now()
	clock := t0
	var out []Event
	b := newDeltaBatcher(50*time.Millisecond, func(e Event) { out = append(out, e) })
	b.now = func() time.Time { return clock }

	b.frag(Delta{Content: "he"}) // leading edge: goes now
	clock = clock.Add(10 * time.Millisecond)
	b.frag(Delta{Content: "llo"})
	b.frag(Delta{Reasoning: "why"}) // a text group glues both page-side buffers
	b.frag(Delta{ToolCall: &ToolCallProgress{ID: "c1", Name: "fs_list", Args: `{"a`, Index: 0}})
	b.frag(Delta{ToolCall: &ToolCallProgress{Args: `"b":1}`, Index: 0}}) // same call: glued
	b.frag(Delta{ToolCall: &ToolCallProgress{ID: "c2", Name: "fs_read", Args: `{}`, Index: 1}})
	b.frag(Delta{Content: "!"}) // back to text: a new group, not a rejoin of the first
	b.flush()

	if len(out) != 5 {
		t.Fatalf("%d events, want 5 (he | llo+why | call0 | call1 | !): %+v", len(out), events(out))
	}
	if out[0].Delta.Content != "he" {
		t.Errorf("leading frame is not the leading fragment: %+v", out[0].Delta)
	}
	if g := out[1].Delta; g.Content != "llo" || g.Reasoning != "why" {
		t.Errorf("the text group glued to %q/%q, want llo/why", g.Content, g.Reasoning)
	}
	if c0 := out[2].Delta.ToolCall; c0 == nil || c0.Args != `{"a"b":1}` || c0.ID != "c1" || c0.Name != "fs_list" || c0.Index != 0 {
		t.Errorf("call 0 merged wrong: %+v", out[2].Delta.ToolCall)
	}
	if c1 := out[3].Delta.ToolCall; c1 == nil || c1.Index != 1 || c1.ID != "c2" {
		t.Errorf("a second call opened no group of its own: %+v", out[3].Delta.ToolCall)
	}
	if out[4].Delta.Content != "!" {
		t.Errorf("text after a tool call rejoined the wrong group: %+v", out[4].Delta)
	}

	// An empty flush is silent and costs no window.
	n := len(out)
	b.flush()
	if len(out) != n {
		t.Error("an empty flush emitted something")
	}

	// The window is pinned to the last real send — the flush above was one, at 10ms. A fragment
	// inside the window waits; one after the window has closed leads again, alone.
	b.frag(Delta{Content: "x"})
	if len(out) != n {
		t.Error("a fragment inside the window went out early")
	}
	b.flush() // as the tick would
	if len(out) != n+1 || out[n].Delta.Content != "x" {
		t.Fatalf("the tick did not deliver what the window held: %+v", events(out[n:]))
	}
	clock = t0.Add(60 * time.Millisecond) // 50ms on from the flush at 10ms: the window is over
	b.frag(Delta{Content: "y"})
	if len(out) != n+2 || out[n+1].Delta.Content != "y" {
		t.Errorf("a fragment after the window does not lead: %d events out", len(out)-n)
	}

	// The off switch: a zero window is the pre-batching wire, fragment for fragment.
	var plain []Event
	z := newDeltaBatcher(0, func(e Event) { plain = append(plain, e) })
	z.frag(Delta{Content: "a"})
	z.frag(Delta{Content: "b"})
	if len(plain) != 2 || plain[0].Delta.Content != "a" || plain[1].Delta.Content != "b" {
		t.Errorf("batching off did not stay one event per fragment: %d events", len(plain))
	}
}

// events is the %v a table of merged deltas wants: the content, not eight fields of zero values.
func events(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if e.Delta != nil {
			out = append(out, "delta:"+e.Delta.Content)
			continue
		}
		out = append(out, string(e.Type))
	}
	return out
}
