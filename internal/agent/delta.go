package agent

import "time"

// deltaWindow is how long a streamed reply's fragments may be gathered before they go out.
// 50ms is twenty frames a second — the fastest refresh a page can show — and an order of
// magnitude below the rate at which a local model hands over tokens. A variable so tests can
// run this on a clock they can hold.
var deltaWindow = 50 * time.Millisecond

// deltaBatcher is generate()'s answer to one frame per token. Everything the provider streams
// is gathered here first (see the consuming loop in generate): the first fragment after an idle
// moment goes out at once, so the first token never waits, and fragments landing inside the
// window are merged — content glued to content, reasoning to reasoning, the argument fragments
// of one tool call to each other — and sent as one merged event of each kind when the window
// closes. A page receives fewer, longer deltas of exactly the shape it already appends.
//
// Merging happens before s.emit, which is what makes the rest of the system need to know
// nothing: every event that leaves is an ordinary one, gets its new stamp from the manager,
// occupies one frame per subscriber, and the heartbeat's arithmetic — frames the daemon says it
// wrote against frames the page counted — closes on the events themselves.
//
// The window is pinned to the last real send and never slides: a model producing tokens faster
// than the window would otherwise keep pushing the send in front of it forever.
//
// Not thread-safe — one batcher belongs to one generate() call, driven by that call's loop.
type deltaBatcher struct {
	win   time.Duration
	emit  func(Event)
	now   func() time.Time // the test's clock
	last  time.Time        // the last send that actually went out; zero means none yet
	group []Delta          // the merged groups, in the order they first arrived
	timer *time.Timer
}

func newDeltaBatcher(win time.Duration, emit func(Event)) *deltaBatcher {
	return &deltaBatcher{win: win, emit: emit, now: time.Now}
}

// frag takes one fragment in: alone on an idle wire, it is sent as it stands and the window
// starts from that send; inside a running window, it joins the group its kind owns and waits
// for the tick.
func (b *deltaBatcher) frag(d Delta) {
	if b.win <= 0 {
		b.emit(Event{Type: EvtDelta, Delta: &d}) // batching off: exactly the old one-frame-per-fragment wire
		return
	}
	now := b.now()
	if len(b.group) == 0 && now.Sub(b.last) >= b.win {
		// Leading edge. Nothing is waiting, so this fragment costs no delay — and it is the
		// send the window is now measured from.
		b.stop()
		b.last = now
		b.emit(Event{Type: EvtDelta, Delta: &d})
		return
	}
	b.add(d)
	if b.timer == nil {
		// The tick is armed once per window and never rearmed: a fragment that joins on the
		// way in must not buy the window more time.
		b.timer = time.NewTimer(b.win - now.Sub(b.last))
	}
}

// timerC is what the consuming loop selects on while fragments are waiting. It returns nil
// when none are — and a nil channel never fires, which is how "no window running" reads in a
// select without a second state to consult.
func (b *deltaBatcher) timerC() <-chan time.Time {
	if b.timer == nil {
		return nil
	}
	return b.timer.C
}

// flush sends everything gathered, as one event per group, and restarts the window from this
// send. The loop calls it on the tick, and once more when the stream ends — that last call is
// the one that keeps the contract the entry and the Done rely on: every fragment the reply
// said is on the wire before either of them is emitted.
func (b *deltaBatcher) flush() {
	b.stop()
	if len(b.group) == 0 {
		return
	}
	b.last = b.now()
	for i := range b.group {
		d := b.group[i]
		b.emit(Event{Type: EvtDelta, Delta: &d})
	}
	b.group = b.group[:0]
}

// stop arms nothing for later: the window is either running with something in it or over.
func (b *deltaBatcher) stop() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// add puts a fragment into the group at the tail, or opens the group it starts.
func (b *deltaBatcher) add(d Delta) {
	if n := len(b.group); n > 0 && sameGroup(&b.group[n-1], d) {
		b.group[n-1].glue(d)
		return
	}
	b.group = append(b.group, d)
}

// sameGroup is whether d reads as a continuation of head. Text and reasoning are separate
// buffers on a page but each is plain concatenation; a tool call's arguments are one byte
// stream cut arbitrarily — but only within one call, which is what the index says the piece
// belongs to.
func sameGroup(head *Delta, d Delta) bool {
	if (head.ToolCall == nil) != (d.ToolCall == nil) {
		return false
	}
	if head.ToolCall != nil {
		return head.ToolCall.Index == d.ToolCall.Index
	}
	return true
}

// glue appends d to a group of its own kind. The identity fields of a streamed call arrive on
// whichever fragment carried them, and generate sends the newest of each with every fragment;
// "newest non-empty" is the same rule it sends by, one step later.
func (head *Delta) glue(d Delta) {
	if d.ToolCall != nil {
		t := head.ToolCall
		t.Args += d.ToolCall.Args
		if d.ToolCall.ID != "" {
			t.ID = d.ToolCall.ID
		}
		if d.ToolCall.Name != "" {
			t.Name = d.ToolCall.Name
		}
		return
	}
	head.Content += d.Content
	head.Reasoning += d.Reasoning
}
