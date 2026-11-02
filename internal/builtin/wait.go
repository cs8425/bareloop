package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// maxWait caps what a wait may ask for. It is not a safety limit for the daemon — Stop() is that —
// it is a typo limit: "1000h" is a mistake somebody wants told rather than a promise this process
// undertakes to keep a timer for.
const maxWait = 100 * 365 * 24 * time.Hour

// tooLong is what an absurd wait says. It names the way out in the same breath, because the number
// that trips it is always a mistake and the caller's next move is the question.
const tooLong = " is more than a century away, which is a typo and not a schedule"

func waitTool() Tool {
	return Tool{
		Name: "wait",
		Description: "Pause the agent loop and resume it after the specified duration or at the " +
			"specified time. Nothing is sent to the model while it waits; the result of this call is " +
			"what the resumed turn sees.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"duration":{"type":"string","description":"Relative duration, e.g. 5m, 30s, 2h, 1h30m; a bare number is seconds"},"until":{"type":"string","description":"Absolute RFC3339 timestamp with timezone, e.g. 2026-11-01T22:25:00+08:00; one without an offset is read in this session's default timezone"}},"additionalProperties":false}`),
		Run:         runWait,
	}
}

// waitArgs is two ways of saying when, and nothing else: additionalProperties is false in the
// schema, so this code never has to wonder what an unexpected key meant.
type waitArgs struct {
	Duration *string `json:"duration"`
	Until    *string `json:"until"`
}

// waitResult is what this tool answers, and the pair scheduled_at/resumed_at is what tells the
// model whether it got the wait it asked for: interrupted with a scheduled_at still in the future
// is the record of somebody ending it early.
type waitResult struct {
	Status      string `json:"status"`
	ScheduledAt string `json:"scheduled_at"`
	ResumedAt   string `json:"resumed_at"`
	// Reason names who ended the wait, and it is only ever there when it was a person: a bare
	// "interrupted" cannot tell "somebody stopped this" from "this ended early and you should try
	// again", and the two call for opposite next moves. Stop() has nobody's message after it, so it
	// has nothing to name and leaves the field out.
	Reason string `json:"reason,omitempty"`
}

// Deadline is the one answer to "when is this wait due", and it is the same answer for whoever asks
// it: the call that is about to sleep here, and a daemon that has to answer the same call after a
// restart, cannot be allowed to disagree about which moment was promised. Every error it returns
// names the tool, because the caller shows the text to a model as it stands.
//
// why it is given the timestamp of the entry that carries the call rather than reading a clock: six
// hours means six hours from the ask. Computed from now, a wait would restart itself on every
// restart — which is the one thing a resumed wait must not do.
func Deadline(args json.RawMessage, calledAt time.Time, zone string) (time.Time, error) {
	var q waitArgs
	if err := decodeArgs(args, &q); err != nil {
		return time.Time{}, fmt.Errorf("wait: %v", err)
	}
	duration, until := argText(q.Duration), argText(q.Until)
	switch {
	case duration != "" && until != "":
		return time.Time{}, errors.New(`wait takes either duration or until, not both: duration is a length of time ("5m") and until is a moment ("2026-11-01T22:25:00+08:00").`)
	case duration == "" && until == "":
		return time.Time{}, errors.New(`wait needs to be told when: duration is a length of time ("5m", "30s", "2h") and until is a moment ("2026-11-01T22:25:00+08:00").`)
	}

	loc, err := LoadTimezone(zone)
	if err != nil {
		// The session's own default is what is broken (a hand-edited config.json is enough), so the
		// message names the setting instead of reading like a bad argument.
		return time.Time{}, fmt.Errorf("wait: %v (this session's timezone setting)", err)
	}

	moment, err := schedule(calledAt, duration, until, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("wait: %v", err)
	}
	if after := moment.Sub(calledAt); after > maxWait {
		return time.Time{}, fmt.Errorf("wait: %s%s", moment.Format(time.RFC3339), tooLong)
	}
	return moment, nil
}

func runWait(ctx context.Context, in Input, raw json.RawMessage) (Result, error) {
	// The moment is computed from when the call was asked, which for a resumed call is not now: see
	// Input.ResumeCalledAt. A wait that re-counted its own length on every wake would never come due.
	calledAt := in.now()
	if !in.ResumeCalledAt.IsZero() {
		calledAt = in.ResumeCalledAt
	}
	scheduled, err := Deadline(raw, calledAt, in.DefaultTimezone)
	if err != nil {
		return ErrText(err.Error()), nil
	}
	at := scheduled.Format(time.RFC3339)

	if remaining := scheduled.Sub(in.now()); remaining > 0 {
		// why this is asked before anything else: a call that has already been ended has to answer, not be
		// rescheduled. Without it, an interrupt that arrives while the daemon is waking this call up would
		// put the same wait back on the timer forever — and a Stop that wins the race against the sleep
		// deserves the answer it asked for rather than a wake-up nobody is waiting for.
		if ctx.Err() != nil {
			return interrupted(ctx, in, scheduled), nil
		}
		// why the limit is checked for being set at all: zero is what every caller that has never
		// heard of parking passes, and for those the behaviour here is exactly what it always was.
		if limit := in.MaxInLoopSleep; limit > 0 && remaining > limit {
			// Past what this run will hold the loop open for, the wait is handed to the daemon rather
			// than slept: the moment it is due can be recomputed from this very call (it is in the log
			// before the answer is), so a goroutine held for six hours buys nothing that a timer does
			// not — and it is the goroutine that a restart takes away.
			in.logf("wait: %s is %s from now, longer than this run will hold the loop for (%s); the loop is released and the daemon owns the wake-up",
				at, remaining.Truncate(time.Second), in.MaxInLoopSleep)
			return Result{}, &Parked{Until: scheduled}
		}
		// Say it before sleeping, not after: a run that produces nothing for five minutes is
		// otherwise indistinguishable from a hung one, and this is the line that tells the
		// difference. It goes out before the sleep because after the sleep it would be too late.
		in.logf("wait: the loop is parked until %s (%s from now); Stop interrupts it", at, remaining.Truncate(time.Second))
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			// why: a cancellation can win the race against a deadline that has already passed, and a
			// wait that did finish must not be reported as interrupted — on that word the model
			// would schedule the same wait all over again.
			if in.now().Before(scheduled) {
				return interrupted(ctx, in, scheduled), nil
			}
		}
	}

	return in.withNote(Result{Text: JSON(waitResult{
		Status:      "completed",
		ScheduledAt: at,
		ResumedAt:   in.now().Format(time.RFC3339),
	})}), nil
}

// interrupted is the answer for a wait that will not be slept: what was promised, when the call gave
// up, and who ended it. It is one function rather than two copies because the two moments that produce
// it — already cancelled on arrival, and cancelled during the sleep — must not be able to disagree
// about what the words are.
func interrupted(ctx context.Context, in Input, scheduled time.Time) Result {
	return in.withNote(ErrText(JSON(waitResult{
		Status:      "interrupted",
		ScheduledAt: scheduled.Format(time.RFC3339),
		ResumedAt:   in.now().Format(time.RFC3339),
		Reason:      interruptReason(ctx),
	})))
}

// interruptReason reads who ended the wait from the cause the run's context was cancelled with, and
// returns the sentinel's own words so the two cannot drift apart. Only a person is named: an
// uncaused cancellation (Stop(), a run that ended, this process going away) is still honestly
// reported as "interrupted", and a cause this code does not know is reported as no cause rather than
// guessed at.
func interruptReason(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), ErrUserInterrupt) {
		return ErrUserInterrupt.Error()
	}
	return ""
}

// schedule is the one place that decides when a wait ends, so the two ways of asking cannot drift
// apart over what "now" and "which zone" mean.
func schedule(now time.Time, duration, until string, loc *time.Location) (time.Time, error) {
	if duration != "" {
		d, err := ParseDuration(duration)
		if err != nil {
			return time.Time{}, err
		}
		// A relative wait is reported in the session's own zone, so scheduled_at is the same "now"
		// get_datetime just reported rather than a third one nobody asked for.
		return now.Add(d).In(loc), nil
	}
	return parseMoment(until, loc)
}

// tooLongSeconds bounds the float path of ParseDuration before it becomes a Duration: converting a
// float too large for a Duration is not a thing with a defined answer, and "defined answer" is what
// a bound is for.
const tooLongSeconds = float64(maxWait) / float64(time.Second)

// ParseDuration reads the duration a wait is given. The rules are time.ParseDuration's — "5m",
// "30s", "2h", "1h30m" — plus one: a bare number is seconds.
//
// why the one extra rule: a bare number is the thing time.ParseDuration refuses and the thing a
// caller types without thinking ("90"), and reading it as 90 minutes would be guessing at somebody
// else's unit in the one direction that costs hours.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		// why !(f >= 0): "nan" and "inf" are both numbers to strconv.ParseFloat, and a NaN is not
		// less than zero — so one test covers NaN, negatives and +Inf together.
		if !(f >= 0) {
			return 0, fmt.Errorf("%q is not a duration: a wait does not go backwards, and it is not infinite", s)
		}
		if f > tooLongSeconds {
			return 0, errors.New(s + " seconds" + tooLong)
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration: try 30s, 5m, 2h or 1h30m (a bare number means seconds)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("a wait cannot go backwards (%s)", s)
	}
	return d, nil
}

// momentLayouts are the shapes a timestamp arrives in once a model is writing one: RFC3339, which
// the schema asks for (Go's parser takes a fractional second on this path whether or not the layout
// spells it out), and the three that dropped the offset. They are tried in order, so the strictest
// reading is the one that wins.
var momentLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseMoment reads an absolute moment. The tool description asks for an offset, and a timestamp
// without one is still read — in defaultLoc, which is the zone this session has already said it
// works in — because scheduled_at comes back naming the zone it was read in, and that is what
// turns an ambiguous call into an unambiguous result.
func parseMoment(s string, defaultLoc *time.Location) (time.Time, error) {
	for _, layout := range momentLayouts {
		// why two parsers and not one: RFC3339 carries its own offset, so the moment is already
		// decided by the text. The layouts below carry none, and time.Parse would read "22:25" as
		// 22:25 UTC — the same digits in a different moment. ParseInLocation is what makes
		// "in this session's timezone" mean the wall clock the caller was looking at.
		var (
			t   time.Time
			err error
		)
		if layout == time.RFC3339 {
			t, err = time.Parse(layout, s)
		} else {
			t, err = time.ParseInLocation(layout, s, defaultLoc)
		}
		if err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a moment that can be read: try an RFC3339 timestamp with a timezone, like 2026-11-01T22:25:00+08:00", s)
}
