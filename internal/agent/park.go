package agent

// park.go — a wait the loop does not hold open.
//
// A `wait` longer than parkAfter is handed over here instead of being slept (builtin.Parked says so,
// builtin.Input.MaxInLoopSleep is what makes it ask). What makes that possible is that nothing new has
// to be remembered: the tool call is in the log before the tool is called (runTools commits the
// assistant entry, then calls), and the moment the wait is due is recomputable from that entry's
// arguments and its own timestamp. A goroutine held for six hours buys nothing a timer does not, and
// unlike a timer it does not survive a restart.
//
// So while a session is parked this process holds: one timer, the messages that arrive meanwhile, and
// one line of explanation for a wake the daemon had to produce itself. None of it is the answer to
// anything — the answer is written once, by the run that wakes, which is the only reason there is one
// result per tool call rather than two competing ones.
//
// The invariant every branch here protects: while a call is unanswered, nothing is appended to the
// path. asm.repair synthesises "[no result recorded — turn was interrupted]" for an unanswered call
// that has something after it (only a trailing one is legal), so a message appended during a wait
// does not merely read out of order — it destroys the result before it exists. That is why parked
// input is held in memory, and why a restart loses it rather than the path gaining a hole.
//
// The predicate that decides it is answeringLocked, and it asks about the call rather than about the wait:
// an ordinary tool call leaves the same window open for the length of the call, with nothing parked and no
// timer anywhere, and the messages that arrive in it are held by this file all the same.
//
// The third window is not a call at all: it is a request that has left this process, whose answer is not on
// the path yet. Nothing committed points at it, so no entry can be walked back to find it — which is why
// awaitingReply is a field rather than another question asked of the path. It holds the same thing for the
// same reason: a message appended in that window travels in front of a reply that never saw it.

import (
	"context"
	"fmt"
	"time"

	"bareloop/internal/builtin"
	"bareloop/internal/ir"
)

const (
	// parkAfter is the longest a run will hold the loop inside one tool call. A wait past it is
	// handed over: parked, a five-minute wait costs a goroutine and a restart costs the session its
	// one unanswered call, so this number trades the common case's simplicity against the long
	// case's durability. It is deliberately not a session setting — one value that the tool's
	// handover threshold and this package's timer must agree on is not a preference.
	parkAfter = 5 * time.Minute

	// wakeLead is how early the timer fires, so a run is already being built while the last moments
	// of the wait are still being slept. The result's resumed_at stays exact, because it is the tool's
	// own clock after its own sleep rather than the timer's: waking early must not be able to make a
	// wait finish early.
	wakeLead = 20 * time.Second

	// rearmCap is how long the timer is ever allowed to sleep for at once. Go's timers count
	// monotonic time, which does not pass while the machine is suspended — a single timer set for
	// forty days on a laptop closed for thirty of them fires thirty days late. Rearming hourly and
	// comparing against the wall clock bounds that lateness to the cap, and a daemon that was simply
	// not running wakes on the first comparison after it starts.
	rearmCap = time.Hour
)

// parked is one wait the daemon is holding instead of the loop. It is never written anywhere: the log
// already says which call is unanswered and when it is due, which is what makes the whole thing
// re-derivable at open time.
type parked struct {
	until time.Time
	// timer is nil whenever a run owns the wait again — a timer that outlives its own handover is how
	// one wait could be woken twice.
	timer *time.Timer
	// early says somebody has asked for this wait to end before its moment, and cause names who. Both
	// are what tells the wake "answer now, in the past tense" from the scheduled one, where the tool
	// sleeps what is left of its own wait and answers for itself.
	early bool
	cause error
}

// parkedNoteKey carries a daemon's note into the one call it is addressed to. It is a different context
// value from resumeCallKey because the two answer different questions: "what did this daemon witness
// around this call" (a string, read per call by registry.builtinInput) and "which call is this run here to
// answer" (an identity, checked per call by runTools). Only the second one carries an address, and
// runTools is the only code that turns one into the other — which is why a wait asked for later in the same
// run cannot be told it was due an hour ago. See resumeCall for why addressing is done by call rather
// than by scope.
type parkedNoteKey struct{}

func withDaemonNote(ctx context.Context, note string) context.Context {
	if note == "" {
		return ctx
	}
	return context.WithValue(ctx, parkedNoteKey{}, note)
}

func daemonNote(ctx context.Context) string {
	n, _ := ctx.Value(parkedNoteKey{}).(string)
	return n
}

// resumeCall is one call a wake run is here to answer, together with the two facts about it that the call
// itself cannot witness: when it was first asked, and what this daemon has to say about the gap between the
// promise and the answer.
//
// Both are addressed by call rather than scoped to the tail, because of what a run is: the tail ends, the
// answer is written, and the same context goes on taking the answer to the model — so anything scoped to
// "these calls" is really scoped to "the rest of this run", which is wider than either fact. A run that
// resumes a six hour wait and is then asked for a two minute one has to be able to say which of the two the
// fact is about: `ResumeNote` speaks of "this wait", and a re-counted deadline would turn a continued wait
// into a restarted one.
//
// why not "the first wait of the run" either: the call being resumed is not reliably the first tool of the
// tail (a model can ask for `get_datetime` next to `wait` in a single turn, and tailCalls skips whatever
// already has a result), and a wait has no way to witness any of that from inside its own call.
type resumeCall struct {
	callID string
	// askedAt is the timestamp of the entry carrying the call, which is the only answer to "six hours from
	// when" that survives a restart. It is what turns the re-run of a wait into a continuation of it.
	askedAt time.Time
	// note is empty for every wake this daemon did not have to answer on somebody else's behalf, which is
	// most of them: a wait that comes due on schedule owes nobody an explanation.
	note string
}

type resumeCallKey struct{}

func withResumeCall(ctx context.Context, c resumeCall) context.Context {
	if c.callID == "" {
		return ctx
	}
	return context.WithValue(ctx, resumeCallKey{}, c)
}

// resumeCallFor is the address check: the facts reach exactly one call, and every other call in the run is
// told there are none. An unnamed call matches nothing.
func resumeCallFor(ctx context.Context, callID string) (resumeCall, bool) {
	c, ok := ctx.Value(resumeCallKey{}).(resumeCall)
	if !ok || c.callID == "" || c.callID != callID {
		return resumeCall{}, false
	}
	return c, true
}

// askedAtKey is the resumed call's own half of that: the moment it was asked, handed to the one call it
// belongs to so the tool can compute the promise it is keeping rather than a new one.
type askedAtKey struct{}

func withAskedAt(ctx context.Context, at time.Time) context.Context {
	if at.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, askedAtKey{}, at)
}

// askedAt is zero for every call that was not resumed, which is how a tool is told "you are being asked
// now" without having to be told it in words.
func askedAt(ctx context.Context) time.Time {
	at, _ := ctx.Value(askedAtKey{}).(time.Time)
	return at
}

// endedCallKey marks the one call a wake is resuming that somebody ended before its moment, and with
// what cause. It is a context value rather than a cancelled run because of what has to happen next: an
// interrupt is followed by that person's message, and a run that has been cancelled cannot answer it.
// The wait gets the cancellation it deserves; the loop gets to keep working.
type endedCallKey struct{}

type endedCall struct {
	callID string
	cause  error
}

func withEndedCall(ctx context.Context, callID string, cause error) context.Context {
	return context.WithValue(ctx, endedCallKey{}, endedCall{callID: callID, cause: cause})
}

// endedCallCause is the answer for one call: "you were ended, by this". Every other call in the run —
// including the wait's own re-run after an ordinary scheduled wake — gets nothing.
func endedCallCause(ctx context.Context, callID string) (error, bool) {
	e, ok := ctx.Value(endedCallKey{}).(endedCall)
	if !ok || e.callID == "" || e.callID != callID {
		return nil, false
	}
	return e.cause, true
}

// isParked reports that a tool call is handed over to the daemon and still unanswered. The session
// keeps reporting phase "running" while that is true — which is what keeps a parked session's Enter key
// meaning "queue" (D84) and keeps a second run from starting next to an armed wake — so this, not the
// phase, is the question that distinguishes asleep from awake.
//
// It is deliberately true for the microseconds a run is still winding down after handing the wait over:
// what makes the ordering safe is the unanswered call, and that outlives the run.
func (s *Session) isParked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.park != nil
}

// isAnswering reports that the path is owed an answer — to a tool call (the wait is on a timer, or a run is
// between committing the call and writing its result) or to a request that is still on the wire. Anything that
// would put an entry between a call and its result has to ask this rather than isParked, because the wake is
// exactly the moment a park stops being visible and the hole does not.
func (s *Session) isAnswering() bool { return s.answering() != "" }

// awaitingReplyOwed is what answering reports for the one debt that has no entry to name: the request we
// have sent and whose reply is not committed yet. It is a sentence rather than an id because the only thing
// read out of it is a log line, and inventing an id for an entry that does not exist yet would be worse than
// admitting the answer has no address yet.
const awaitingReplyOwed = "the request in flight"

// answering names the call the path is owed an answer for, and "" when it is owed none. Two things read it:
// a message that would otherwise fall into the hole (Send), and a compaction that would otherwise cut the
// path at the call (CompactNow).
func (s *Session) answering() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answeringLocked()
}

// answeringLocked is that question with the lock held. Three shapes owe an answer, and the third is the one
// this file used to miss:
//
//   - s.held: a wait this daemon owns, asleep on a timer or handed over by the process that died;
//   - a run is in flight and one of the calls of the last entry that asked for tools has no result yet —
//     the tip of the path is that entry, or one of its results with more still owed. The call is
//     committed and its result is not, which is the same hole with no wait in it: measured, a message
//     appended in that window leaves the model reading its tool answer after the person's new message,
//     and it answered by inventing the output. Nothing in this project reorders a path, so the order
//     written in that window is the order every later turn sends.
//   - s.awaitingReply: a payload has gone out and its reply is not on the path. The hole is the mirror
//     image of the second shape — there is nothing unanswered to walk back to, and what a message
//     appended now overtakes is a reply that has not been written. Worse, when that turn asks for no
//     tools the reply is the last entry of all, so nothing is left at the tip saying an answer is owed:
//     needsRun reads the tip, finds an assistant message, and the message nobody has ever asked the
//     model about stays unasked about — in this run, and in every later one, since Resume refuses an
//     assistant tip too.
//
// why the run is part of the second shape and the shape alone is not enough: an unanswered call at the tip
// is also what an abort or a restart leaves behind, and then nothing in this process intends to answer it.
// Holding a message for a promise nobody is going to keep parks it in memory until the next restart, and
// the person pressing send is the only answer such a call is ever going to get. The same reason puts
// awaitingReply after the phase test rather than before it: the flag is a fact about a request, and the
// phase is the promise that somebody here is on their way to its answer.
func (s *Session) answeringLocked() string {
	if s.held != "" {
		return s.held
	}
	// why the phase rather than s.cancel: a run owns the phase from the moment it is started until the
	// moment it stops, and this is the only question worth asking — is somebody in this process on their
	// way to that answer.
	if s.phase != phaseRunning {
		return ""
	}
	// The request in flight owes the most and is the hardest one to see, because the path says nothing
	// about it: the payload is gone, the reply is not here yet, and every message sent meanwhile would
	// be committed in front of an answer that cannot include it any more.
	if s.awaitingReply {
		return awaitingReplyOwed
	}
	// why this walks back over the results instead of only reading the tip: a turn is answered one call at a
	// time, so between one result and the next the calls after it are still owed an answer, and a message
	// that lands there splits one of them from its result just as thoroughly. Counting the results is enough
	// rather than matching ids, because runTools works through a turn's calls in order, which makes the
	// results already on the path a prefix of the calls.
	answered := 0
	for id := s.st.Tip; id != ""; {
		e := s.st.Entries[id]
		if e == nil {
			return ""
		}
		if e.Kind == ir.KindTool {
			answered++
			id = e.ParentID
			continue
		}
		if e.Kind != ir.KindAssistant || len(e.ToolCalls) == 0 {
			return ""
		}
		if answered >= len(e.ToolCalls) {
			return ""
		}
		return e.ToolCalls[answered].ID
	}
	return ""
}

// parkWaitLocked takes a wait over from the loop: the timer is armed, whatever is already held is kept
// (a message that arrived in the moment between the call and the handover is still owed its place), and
// the run that asked is then allowed to end. Caller holds mu.
func (s *Session) parkWaitLocked(name, callID string, until time.Time) {
	s.held = callID
	if s.park != nil {
		s.stopTimerLocked()
	}
	s.park = &parked{until: until}
	s.armLocked(s.park, time.Now())
	s.logf("%s handed to the daemon: scheduled for %s; the run ends here and another one resumes it",
		name, until.UTC().Format(time.RFC3339))
	s.emit(Event{Type: EvtInfo, Info: fmt.Sprintf("%s scheduled for %s: the loop is released",
		name, until.UTC().Format(time.RFC3339))})
}

// armLocked sets (or resets) the timer for the wake: wakeLead before the moment the wait is due, and
// never further ahead than rearmCap, so the wall clock is consulted at least that often.
func (s *Session) armLocked(p *parked, now time.Time) {
	d := p.until.Sub(now) - wakeLead
	if d > rearmCap {
		d = rearmCap
	}
	if d < 0 {
		d = 0
	}
	p.timer = time.AfterFunc(d, s.parkTick)
}

// parkTick is the timer's only action: decide, from the wall clock rather than from how long this
// process has been alive, whether the wake has arrived.
func (s *Session) parkTick() { s.parkTickAt(time.Now()) }

// parkTickAt is parkTick with the clock handed in, because "is it time yet" is the one decision here a
// test has to ask twice — once a day early, once late — without waiting for either.
func (s *Session) parkTickAt(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.park
	if p == nil {
		return
	}
	p.timer = nil
	if s.cancel != nil {
		// A run owns this session, and two runs at once is not a thing that exists. This cannot be the
		// wait's own run — it is the one that just handed the wait over, and its timer cannot come due
		// that fast — so if it ever happens the honest answer is to ask again after one wake lead, the
		// shortest slice around a moment this package treats as real, and to say so.
		s.logf("wake: a run still owns the session; asking again in %s", wakeLead)
		p.timer = time.AfterFunc(wakeLead, s.parkTick)
		return
	}
	if !p.early && p.until.Sub(now) > wakeLead {
		// The machine was suspended, or this process was not running, or this is simply the hourly
		// rearm. Nobody needs to know which: the next interval gets its own timer.
		s.armLocked(p, now)
		return
	}
	s.wakeLocked()
}

// wake is one reason for stopping a wait before the daemon was going to.
//
// It is empty for the scheduled wake, and that is deliberate: the wait is meant to answer for itself.
// The run re-runs the call, the tool sleeps whatever the last few seconds are, and "completed" comes
// from the code that actually slept — which is the only reason the answer's timestamp is a measurement
// rather than a promise.
type wake struct {
	// callID is a wait that was ended early and has to say so. Naming it marks only that call: the
	// rest of the tail, including any other tool the model asked for in the same turn, runs for real.
	callID string
	// cause is who ended it. Only builtin.ErrUserInterrupt is named to the model; nil means nobody in
	// particular (Stop, or a wait that was simply in the way), which the tool reports as a bare
	// "interrupted" — no reason, because there is no message of anybody's coming after it.
	cause error
}

// askWakeLocked ends the wait before its moment and makes sure the wake still happens. Caller holds mu
// and has checked there is a park. A nil cause means nobody in particular asked, which is what a Stop
// reports: the wait ends and says "interrupted" without naming anybody, because nothing of anybody's is
// coming after it to be read.
func (s *Session) askWakeLocked(cause error) {
	p := s.park
	p.early, p.cause = true, cause
	if s.cancel == nil {
		s.wakeLocked()
		return
	}
	// why not the wake itself: the run that is winding down owns the session for a moment longer, and the
	// moment it clears s.cancel is the first moment a second run may exist. Asking again after one wake
	// lead leaves an interrupt that lands inside the handover seconds late; skipping it would leave that
	// interrupt postponed until the wait was due anyway, which for a six hour wait is the difference
	// between an interrupt and a shrug.
	s.logf("wake asked for while a run is still ending; asking again in %s", wakeLead)
	s.stopTimerLocked()
	p.timer = time.AfterFunc(wakeLead, s.parkTick)
}

// wakeLocked ends a park and hands the wait back to a run, which produces the call's only result. It is
// the single unpark path — expiry, Stop(), Interrupt() and the restart scan all come through here, so
// there is exactly one place that decides what waking costs.
func (s *Session) wakeLocked() {
	if s.park == nil || s.cancel != nil {
		return
	}
	s.stopTimerLocked()
	// The tail is read here, under the lock, because it is the answer to "what is this wake for" — and
	// if the path moved while the wait was being held (an entry edited, a tip rewound) there is nothing
	// to answer and this wake must invent nothing.
	tail := parkedTailOf(ir.Path(s.st.Entries, s.st.Tip))
	if len(tail) == 0 {
		// why the lock is put down here: abandoning the park means putting what was held onto the path,
		// and appending takes this same lock. The defer picks it back up, because every caller of this
		// function holds it.
		s.mu.Unlock()
		defer s.mu.Lock()
		s.dropPark("the path no longer ends on the call that was waiting")
		return
	}
	// What was held stays held, and so does s.held: this run is the one that owes that call its answer,
	// and until it writes one a message must not join the path. Every unanswered call leaves that hole
	// open (a message sent during a slow MCP call lands in front of its result); a wake leaves it open for
	// a wake lead as well, and closing it here is what makes the answer the only thing held messages follow.
	var w wake
	if s.park.early {
		w = wake{callID: s.held, cause: s.park.cause}
	}
	// The two facts the resumed call cannot witness, addressed to it: the moment it was asked (which is what
	// makes re-running it a continuation of the same wait rather than a new one) and no note, because a wake
	// this daemon scheduled for itself has nothing to apologise for.
	rc := &resumeCall{callID: s.held, askedAt: tail[0].Ts}
	s.logf("wake: %s is no longer being waited on; the run that answers it starts now", s.held)
	// A parked session reports "running" while having no run, and that is precisely the state a wake
	// takes over — so the phase is set back to idle here, for one reason only: so that the guard in
	// beginRunLocked ("one run at a time") means the same thing for every run that ever starts. Nothing
	// can see the two instructions happen: mu is held across both, and beginRunLocked cannot fail to
	// start a run once the phase says idle.
	s.park = nil
	s.phase = phaseIdle
	s.beginRunLocked(context.Background(), runStart{tail: tail, w: w, resume: rc})
}

// holdMessage takes a message that must not reach the path right now off the sender's hands, taking the
// lock itself. Send() uses this; Interrupt() uses holdMessageLocked, because queueing and waking have to
// be one decision there — a wake that starts in between would leave the message on a path whose tool
// call is still unanswered.
func (s *Session) holdMessage(msg Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holdMessageLocked(msg)
}

// holdMessageLocked holds a message that must not reach the path right now, and reports whether it did.
// False is the caller's signal to append the message like any other message. There are two reasons it must
// not, and they are different reasons:
//
// Holding behind an owed answer is not a courtesy: appended now, the message would sit between the call and
// its result, and the assembler answers that shape by inventing a failure for a tool that is merely still
// waiting. The question is answeringLocked rather than s.park, because of the wake: that run answers the
// call and then asks the model, and the wait it was built from is over by then — while the hole a message
// could fall into is not. And rather than s.held, because of the call a run is in the middle of: the same
// hole, with nothing parked and nothing holding.
//
// Holding behind a running manual summary is the other shape of the same problem, and it is about the
// boundary rather than a hole: the summary is about to write an entry whose plan was made from a path that
// does not contain this message, so appending now puts the message *under* a summary that cannot know it
// exists. It costs the same to wait, and endManualCompaction writes it the moment the boundary is on disk
// (or the moment the summary is abandoned, which must not strand a message either).
func (s *Session) holdMessageLocked(msg Message) bool {
	// Held is not the same as owned, and this line is the difference: the message is now in memory with no
	// request that saw it and nothing writing it out, so whoever ends the thing that is holding it has to be
	// the one that asks after it. A run's epilogue and endManualCompaction are the two that can; each clears
	// the promise as it takes the queue over.
	s.acceptedWhileBusy = true
	if call := s.answeringLocked(); call != "" {
		s.queue = append(s.queue, msg)
		s.logf("message held in memory instead of appended: it joins the path after the answer to %s, and a restart drops it",
			call)
		return true
	}
	if s.manualSummaryLocked() {
		s.queue = append(s.queue, msg)
		s.logf("message held in memory instead of appended: the summary running now plans a boundary it is not part of, and a restart drops it")
		return true
	}
	return false
}

// flushHeld releases what this session was holding off the path and puts it on the path, which are the
// only two things worth doing with it together: a queue that is released must not be kept, and a queue
// that is kept must not be half-appended. It reports how many messages reached the path, which is the
// difference between "nothing was waiting" and "a person asked something that nobody has answered yet".
//
// The whole drain happens in one critical section, and that is the ordering rather than a detail. These
// messages arrived before whatever a concurrent Send is asking for, and appendEntry takes this same lock
// once per entry, so putting the lock down between two of them let a message that arrived later reach the
// path first - a conversation rewritten by nothing but timing. So the queue is emptied and written under
// s.mu: a Send that arrives meanwhile is waiting for this same lock to ask whether anything is owed, and by
// the time it gets it the queue is gone, so its message lands after every message in the drain. A message
// that arrived before the drain is already in the queue and is what this function is writing.
//
// The events go out after the lock is put down, which is not a preference: emitState reads the status and
// emitPending the pending list, and both take this lock.
func (s *Session) flushHeld() int {
	s.mu.Lock()
	s.held = ""
	queue := s.queue
	s.queue = nil
	written := make([]*ir.Entry, 0, len(queue))
	var failed []string
	for _, m := range queue {
		e, err := s.appendLocked(s.userEntry(m))
		if err != nil {
			// The message is dropped rather than held again: a write that failed is a storage failure,
			// and keeping it for a quieter moment would only make it vanish unnoticed. The rest of the
			// queue is still written, because one bad entry is not a reason to lose the others.
			failed = append(failed, err.Error())
			continue
		}
		written = append(written, e)
	}
	s.mu.Unlock()

	for _, reason := range failed {
		s.logf("held message could not join the path: %s", reason)
		s.emit(Event{Type: EvtError, Info: "held message: " + reason})
	}
	for _, e := range written {
		s.emit(Event{Type: EvtEntry, Entry: e})
	}
	if len(written) > 0 {
		s.logf("%d held message(s) joined the path after what they were waiting behind", len(written))
		s.emitPending()
		s.emitState()
	}
	return len(written)
}

// dropPark abandons a park that no longer describes the path. Anything already held is appended rather
// than discarded: by then it is owed to the conversation, and there is no longer a call for it to be out
// of order in front of.
func (s *Session) dropPark(reason string) {
	s.mu.Lock()
	if s.park == nil {
		s.mu.Unlock()
		return
	}
	s.stopTimerLocked()
	s.park = nil
	// why the phase is put back here: it said running only because the wait was being held, and this is
	// the one path that ends a park without starting a run to say so later. A phase nobody is left to
	// reset is how a session ends up refusing its own next message — but a run that is still winding down
	// owns that answer, so it is not touched while one exists.
	if s.cancel == nil {
		s.phase = phaseIdle
	}
	s.mu.Unlock()

	s.logf("wait handed over: %s", reason)
	s.emit(Event{Type: EvtInfo, Info: "wait handed over: " + reason})
	s.flushHeld()
	s.emitState()
}

// stopTimerLocked takes the timer away from a park that is done being a park. Caller holds mu.
func (s *Session) stopTimerLocked() {
	if s.park != nil && s.park.timer != nil {
		s.park.timer.Stop()
		s.park.timer = nil
	}
}

// parkedTailOf finds the tail a wake owes the loop: the assistant entry that asked for the wait, plus
// whatever came after it. The tail rather than the one call, because a model can ask for `wait` next to
// another tool in a single turn, and replaying the turn is how the calls it never got to make are still
// made. Re-running `wait` itself is safe by construction: the second call recomputes the same moment
// from the same arguments and sleeps what is left, so the answer the model reads comes from the code
// that actually waited — once. See tailCalls, which is what keeps an already-answered call from running
// a second time.
func parkedTailOf(path []*ir.Entry) []*ir.Entry {
	if len(path) == 0 {
		return nil
	}
	i := len(path) - 1
	for i >= 0 && path[i].Kind != ir.KindAssistant {
		i--
	}
	if i < 0 {
		return nil
	}
	if len(path[i].ToolCalls) == 0 {
		// A compaction boundary, or an entry edited until it has no calls: the park's tail is gone,
		// and "nothing to replay" is the answer that lets the caller abandon the park.
		return nil
	}
	return path[i:]
}

// tailCalls is every call the tail is still owed, in the order the model asked for them. Anything in
// the tail that already has a result is skipped: a second result for one call is what the provider
// refuses, and it would be the daemon's invention rather than the model's.
func tailCalls(tail []*ir.Entry) []ir.ToolCall {
	answered := map[string]bool{}
	for _, e := range tail {
		if e.Kind == ir.KindTool {
			answered[e.ToolCallID] = true
		}
	}
	var out []ir.ToolCall
	for _, e := range tail {
		if e.Kind != ir.KindAssistant {
			continue
		}
		for _, c := range e.ToolCalls {
			if !answered[c.ID] {
				out = append(out, c)
			}
		}
	}
	return out
}

// recoverPark is the open-time scan: the process that died may have been holding a wait, and this
// session's log is the only record of what it was holding. It runs after the replay and after the tool
// policy is installed, because "is this an unanswered builtin call" needs both.
//
// It acts on one shape and nothing else: the path ends on an assistant entry with an unanswered call,
// that call is the last of them, and it resolves to one of the daemon's own scheduling tools — today
// `wait`, and only `wait`. Everything else is left exactly as the log has it, because the one thing
// worse than a wait that is not resumed is a tool that is run twice: an MCP call has a side effect,
// and nothing here can tell whether it already happened.
func (s *Session) recoverPark() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.park != nil || s.cancel != nil {
		return
	}
	path := ir.Path(s.st.Entries, s.st.Tip)
	tail := parkedTailOf(path)
	// why the tail has to reach the end of the path: once anything has joined after the call — a message
	// sent by a daemon that did not know how to hold one, an entry edited in by hand — writing the
	// result now would put it after that, which is the ordering this whole file exists to prevent. The
	// assembler already refuses to invent a result for a trailing call, so leaving it is not a new hole.
	if len(tail) == 0 || tail[len(tail)-1].ID != path[len(path)-1].ID {
		return
	}
	calls := tailCalls(tail)
	if len(calls) == 0 {
		return
	}
	c := calls[len(calls)-1]
	ref, ok := s.reg.resolve(c.Name)
	if !ok || !ref.Builtin {
		// An unresolvable name is most often a tool the session switched off, and an MCP tool is most
		// often something that touched the world. Neither is this scan's business: asm.repair already
		// refuses to invent a result for a call at the end of the path, so this stays the legal state
		// it is — an unanswered call at the tip — and a person decides what it was worth.
		s.logf("path ends on an unanswered tool call %q (%s): not a wait this daemon owns, so it is left as it is",
			c.Name, c.ID)
		return
	}
	if ref.Tool != toolWait {
		s.logf("path ends on an unanswered builtin call %q: it schedules nothing, so it is left as it is", ref.Tool)
		return
	}
	// From here on this scan has decided that something will answer this call, which is also the moment a
	// message stops being allowed on the path: every branch below starts a run that owes the call an
	// answer, and a run releases the hold when it writes one.
	s.held = c.ID
	// The deadline is asked of the same code the call asked it of, from the timestamp of the entry that
	// carries the call: "six hours" means six hours from the ask, whatever process is doing the waking.
	until, err := builtin.Deadline(c.Args, tail[0].Ts, s.cfg.Timezone)
	if err != nil {
		// The call was never going to wait anything, because its arguments cannot be read. Re-running it
		// is not a guess in that case: it produces the same error the call would have produced, and the
		// session goes back to being an ordinary run.
		s.logf("unanswered wait %s cannot be scheduled (%v): letting the next run answer it", c.ID, err)
		s.beginRunLocked(context.Background(), runStart{tail: tail})
		return
	}
	if now := time.Now(); now.Before(until) {
		s.park = &parked{until: until}
		s.armLocked(s.park, now)
		// why the phase says running here: this session is holding a wait, which is what every other park in
		// this file reports, and the process that died believed it was running — nothing about the restart
		// made that untrue. Two real things read the phase rather than the hold: the composer offers "send"
		// instead of "queue" on a session it believes is idle (D84), and a tool array change is applied on the
		// spot instead of waiting for the wake that replays this call (session.go SetToolPolicy reads
		// `phase != phaseIdle`, and the wake publishes the array it starts with). A compaction is not a third
		// one: that is refused by the hold, not by the phase (compact.go asks isAnswering).
		s.phase = phaseRunning
		s.logf("path ends on a wait the previous process was holding: %s due at %s, this daemon owns the wake-up",
			c.Name, until.UTC().Format(time.RFC3339))
		return
	}
	// Due already, or due days ago. It is answered now, late, by a run that starts immediately — and the
	// note says what was missed rather than leaving the model a completed wait with no mention of the
	// gap between the promise and the answer.
	note := builtin.ResumeNote("the daemon restarted", until, time.Now())
	s.logf("a wait due at %s came due while this daemon was not running: answering it now", until.UTC().Format(time.RFC3339))
	s.beginRunLocked(context.Background(), runStart{
		tail:   tail,
		resume: &resumeCall{callID: c.ID, askedAt: tail[0].Ts, note: note},
	})
}

// toolWait is the one builtin whose unanswered call this package recognises by name. It is a
// comparison rather than a registry lookup because the question is not "is this published" but "does
// this tool's answer arrive later than its call" — and of the tools the daemon answers itself, only a
// wait can hand its own deadline to somebody else.
const toolWait = "wait"
