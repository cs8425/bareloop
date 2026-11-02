package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// at is a fixed instant everybody in this file agrees on: 2026-11-01T14:25:00Z, which is a Sunday.
// Sunday is asserted rather than assumed, because "the day of the week" is half of what the tool is
// for and the other half is an offset a typo can move by a day on its own.
var at = time.Date(2026, 11, 1, 14, 25, 0, 0, time.UTC)

func fixedInput(tz string) Input {
	return Input{DefaultTimezone: tz, Now: func() time.Time { return at }}
}

func call(t *testing.T, name, args string, in Input) Result {
	t.Helper()
	res, err := tryCall(t, name, args, in)
	if err != nil {
		t.Fatalf("builtin %s handed the call back to the daemon (%v) instead of answering it", name, err)
	}
	return res
}

// tryCall is for the tests that are about a call not being answered here, which is the one thing a
// builtin can say with an error.
func tryCall(t *testing.T, name, args string, in Input) (Result, error) {
	t.Helper()
	tool, ok := Get(name)
	if !ok {
		t.Fatalf("builtin %q does not exist", name)
	}
	return tool.Run(context.Background(), in, json.RawMessage(args))
}

// TestSetIsSortedByName is the published order: the tool block is rendered in front of the history,
// so the name list is part of the KV prefix and must not depend on map iteration.
func TestSetIsSortedByName(t *testing.T) {
	if got := Names(); len(got) != 2 || got[0] != "get_datetime" || got[1] != "wait" {
		t.Fatalf("Names() = %v, want [get_datetime wait]", got)
	}
	tools := All()
	if len(tools) != 2 {
		t.Fatalf("All() returned %d tools, want 2", len(tools))
	}
	for i, want := range Names() {
		if tools[i].Name != want {
			t.Fatalf("All()[%d].Name = %q, want %q", i, tools[i].Name, want)
		}
		if tools[i].Description == "" || len(tools[i].InputSchema) == 0 {
			t.Fatalf("builtin %q is published without a description or a schema", want)
		}
		var schema map[string]any
		if err := json.Unmarshal(tools[i].InputSchema, &schema); err != nil {
			t.Fatalf("builtin %q has a schema that is not JSON: %v", want, err)
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("builtin %q does not close its argument object: %s", want, tools[i].InputSchema)
		}
	}
}

func TestGetDatetimeDefaultIsUTC(t *testing.T) {
	got := call(t, "get_datetime", `{}`, Input{Now: func() time.Time { return at }})
	if got.IsError {
		t.Fatalf("a call with no arguments and no config failed: %s", got.Text)
	}
	want := "2026-11-01 14:25:00 +00:00 (UTC) Sunday\n2026-11-01T14:25:00Z"
	if got.Text != want {
		t.Fatalf("no timezone configured:\n got %q\nwant %q", got.Text, want)
	}
}

// TestGetDatetimeSessionDefaultIsUsed is the config knob: the session's zone answers a call that
// names none, and the stamp stays RFC3339 — which is a wait argument, so the two tools line up.
func TestGetDatetimeSessionDefaultIsUsed(t *testing.T) {
	got := call(t, "get_datetime", "", fixedInput("Asia/Taipei"))
	if got.IsError {
		t.Fatalf("session default failed: %s", got.Text)
	}
	for _, want := range []string{"2026-11-01 22:25:00 +08:00", "Asia/Taipei", "Sunday", "2026-11-01T22:25:00+08:00"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("answer %q does not contain %q", got.Text, want)
		}
	}
}

// TestGetDatetimeArgumentBeatsDefault is the whole reason the argument exists: asking about the
// other end of a conversation.
func TestGetDatetimeArgumentBeatsDefault(t *testing.T) {
	got := call(t, "get_datetime", `{"timezone":"+05:30"}`, fixedInput("Asia/Taipei"))
	if got.IsError {
		t.Fatalf("timezone argument failed: %s", got.Text)
	}
	if !strings.Contains(got.Text, "2026-11-01 19:55:00 +05:30 (UTC+05:30) Sunday") {
		t.Fatalf("the argument did not win over the session default: %q", got.Text)
	}
}

func TestGetDatetimeBadZone(t *testing.T) {
	got := call(t, "get_datetime", `{"timezone":"Mars/Olympus_Mons"}`, Input{})
	if !got.IsError {
		t.Fatalf("an unknown zone answered: %q", got.Text)
	}
	for _, want := range []string{"unknown timezone", "Asia/Taipei", "Local"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("the error %q does not point at %q", got.Text, want)
		}
	}
}

// TestGetDatetimeBrokenDefaultNamesTheSetting is about who has to fix it: a session whose own
// timezone value is wrong cannot be told to retry with a different argument.
func TestGetDatetimeBrokenDefaultNamesTheSetting(t *testing.T) {
	got := call(t, "get_datetime", `{}`, fixedInput("Not A Zone"))
	if !got.IsError {
		t.Fatalf("a broken session default answered: %q", got.Text)
	}
	if !strings.Contains(got.Text, "timezone setting") {
		t.Fatalf("the error does not say which setting is broken: %q", got.Text)
	}
}

func TestGetDatetimeRejectsBadJSON(t *testing.T) {
	got := call(t, "get_datetime", `{"timezone":`, Input{})
	if !got.IsError || !strings.Contains(got.Text, "JSON object") {
		t.Fatalf("a malformed body was not reported as one: %q", got.Text)
	}
}

func TestLoadTimezoneForms(t *testing.T) {
	cases := []struct {
		in     string
		want   string // what a time in the zone prints as its offset
		bad    bool
		nameAs string // the zone name is expected to contain this
	}{
		{in: "", want: "+00:00", nameAs: "UTC"},
		{in: "utc", want: "+00:00", nameAs: "UTC"},
		{in: "Zulu", want: "+00:00", nameAs: "UTC"},
		{in: "Asia/Taipei", want: "+08:00", nameAs: "Asia/Taipei"},
		{in: "America/New_York", want: "-05:00", nameAs: "America/New_York"},
		{in: "+08:00", want: "+08:00", nameAs: "UTC+08:00"},
		{in: "+0800", want: "+08:00", nameAs: "UTC+08:00"},
		{in: "+8", want: "+08:00", nameAs: "UTC+08:00"},
		{in: "UTC+8", want: "+08:00", nameAs: "UTC+08:00"},
		{in: "UTC-05:30", want: "-05:30", nameAs: "UTC-05:30"},
		{in: "GMT-5", want: "-05:00", nameAs: "UTC-05:00"},
		{in: "Mars/Olympus_Mons", bad: true},
		{in: "+25:00", bad: true},
		{in: "Asia/Nowhere", bad: true},
	}
	for _, c := range cases {
		loc, err := LoadTimezone(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("LoadTimezone(%q) accepted a zone that does not exist", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("LoadTimezone(%q): %v", c.in, err)
			continue
		}
		if got := at.In(loc).Format("-07:00"); got != c.want {
			t.Errorf("LoadTimezone(%q): offset %s, want %s", c.in, got, c.want)
		}
		if !strings.Contains(loc.String(), c.nameAs) {
			t.Errorf("LoadTimezone(%q): zone %q, expected it to contain %q", c.in, loc.String(), c.nameAs)
		}
	}
}

// TestLocalIsTheHostZone pins the one word that means something other than UTC: a session that
// wants the machine's zone has to be able to ask for it.
func TestLocalIsTheHostZone(t *testing.T) {
	loc, err := LoadTimezone("local")
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	if loc != time.Local {
		t.Fatalf(`LoadTimezone("local") = %v, want time.Local`, loc)
	}
}

func TestParseDurationForms(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "5m", want: 5 * time.Minute},
		{in: "30s", want: 30 * time.Second},
		{in: "2h", want: 2 * time.Hour},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "1.5h", want: 90 * time.Minute},
		{in: "500ms", want: 500 * time.Millisecond},
		{in: "90", want: 90 * time.Second},
		{in: "0", want: 0},
		{in: "0.5", want: 500 * time.Millisecond},
		{in: "-5m", bad: true},
		{in: "-5", bad: true},
		{in: "", bad: true},
		{in: "banana", bad: true},
		{in: "nan", bad: true},
		{in: "inf", bad: true},
		{in: "99999999999999999999", bad: true},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseDuration(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestScheduleRelativeUsesSessionZone is why scheduled_at is worth asserting on its own: a wait of
// five minutes has to be reported in the same "now" get_datetime just printed, and that is only
// true if the session's zone is the one printed.
func TestScheduleRelativeUsesSessionZone(t *testing.T) {
	loc, err := LoadTimezone("Asia/Taipei")
	if err != nil {
		t.Fatalf("LoadTimezone: %v", err)
	}
	got, err := schedule(at, "5m", "", loc)
	if err != nil {
		t.Fatalf(`schedule("5m"): %v`, err)
	}
	if want := "2026-11-01T22:30:00+08:00"; got.Format(time.RFC3339) != want {
		t.Fatalf(`schedule("5m") = %s, want %s`, got.Format(time.RFC3339), want)
	}
}

// TestScheduleAbsoluteKeepsItsOwnZone is the other half: what you asked for is what gets printed,
// offset and all.
func TestScheduleAbsoluteKeepsItsOwnZone(t *testing.T) {
	loc, err := LoadTimezone("")
	if err != nil {
		t.Fatalf("LoadTimezone: %v", err)
	}
	got, err := schedule(at, "", "2026-11-01T22:25:00+08:00", loc)
	if err != nil {
		t.Fatalf(`schedule(until): %v`, err)
	}
	if want := "2026-11-01T22:25:00+08:00"; got.Format(time.RFC3339) != want {
		t.Fatalf("schedule(until) = %s, want %s", got.Format(time.RFC3339), want)
	}
}

// TestScheduleWithoutOffsetReadsItInTheSessionZone is the deliberate softening of "RFC3339 with
// timezone": the call is ambiguous, the answer is not, because scheduled_at names the zone.
func TestScheduleWithoutOffsetReadsItInTheSessionZone(t *testing.T) {
	loc, err := LoadTimezone("+05:00")
	if err != nil {
		t.Fatalf("LoadTimezone: %v", err)
	}
	got, err := schedule(at, "", "2026-11-01T22:25:00", loc)
	if err != nil {
		t.Fatalf(`schedule(until without offset): %v`, err)
	}
	if want := "2026-11-01T22:25:00+05:00"; got.Format(time.RFC3339) != want {
		t.Fatalf("schedule(until without offset) = %s, want %s", got.Format(time.RFC3339), want)
	}
	if _, err := schedule(at, "", "sometime after lunch", loc); err == nil {
		t.Fatal("a moment written in prose was accepted")
	}
}

// TestWaitPastDeadlineReturnsNow is what a model learns from a stale `until`: the timer already
// went off, so the answer is complete, not an error to argue with.
func TestWaitPastDeadlineReturnsNow(t *testing.T) {
	start := time.Now()
	got := call(t, "wait", `{"until":"2020-01-01T00:00:00Z"}`, Input{})
	if got.IsError {
		t.Fatalf("a wait whose deadline passed failed: %s", got.Text)
	}
	if spent := time.Since(start); spent > 50*time.Millisecond {
		t.Fatalf("a wait whose deadline passed slept %v", spent)
	}
	var res waitResult
	if err := json.Unmarshal([]byte(got.Text), &res); err != nil {
		t.Fatalf("the result is not the JSON the tool promises: %v (%s)", err, got.Text)
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed (%s)", res.Status, got.Text)
	}
	if res.ScheduledAt != "2020-01-01T00:00:00Z" {
		t.Fatalf("scheduled_at = %q, want the moment that was asked for", res.ScheduledAt)
	}
	if _, err := time.Parse(time.RFC3339, res.ResumedAt); err != nil {
		t.Fatalf("resumed_at %q is not RFC3339: %v", res.ResumedAt, err)
	}
}

func TestWaitRefusesToGuessBetweenTwoArguments(t *testing.T) {
	got := call(t, "wait", `{"duration":"5m","until":"2030-01-01T00:00:00Z"}`, Input{})
	if !got.IsError || !strings.Contains(got.Text, "not both") {
		t.Fatalf("duration and until together were not refused: %q", got.Text)
	}
	if got := call(t, "wait", `{}`, Input{}); !got.IsError || !strings.Contains(got.Text, "told when") {
		t.Fatalf("a wait with no schedule at all was not refused: %q", got.Text)
	}
	if got := call(t, "wait", `{"duration":"  "}`, Input{}); !got.IsError {
		t.Fatalf("a wait of nothing at all was not refused: %q", got.Text)
	}
}

func TestWaitSleepsAndResumes(t *testing.T) {
	start := time.Now()
	got := call(t, "wait", `{"duration":"40ms"}`, Input{})
	spent := time.Since(start)
	if got.IsError {
		t.Fatalf("a short wait failed: %s", got.Text)
	}
	if spent < 30*time.Millisecond {
		t.Fatalf("a 40ms wait came back after %v", spent)
	}
	if !strings.Contains(got.Text, `"status":"completed"`) {
		t.Fatalf("the short wait did not complete: %q", got.Text)
	}
}

// clockAt is the frozen clock the parking tests need: with Now fixed at `at`, a six-hour wait is
// decided exactly the way a six-millisecond one is, and whether the tool slept is found out with
// the one clock the tool was not handed a fake of — time.Since.
func clockAt() Input {
	return Input{Now: func() time.Time { return at }}
}

// TestALongWaitIsHandedOverNotSlept is the whole point of MaxInLoopSleep: past the limit the tool
// says "not here" and gives the daemon the moment, without the call having been answered.
func TestALongWaitIsHandedOverNotSlept(t *testing.T) {
	in := clockAt()
	in.MaxInLoopSleep = 5 * time.Minute
	due := at.Add(6 * time.Hour)

	var lines []string
	in.Logf = func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}

	start := time.Now()
	// Both ways of asking for six hours, because a relative wait is the one whose moment has to be
	// computed before anybody can be told when to come back.
	for _, args := range []string{`{"until":"` + due.Format(time.RFC3339) + `"}`, `{"duration":"6h"}`} {
		res, err := tryCall(t, "wait", args, in)
		if spent := time.Since(start); spent > time.Second {
			t.Fatalf("%s parked and still cost %v of real time", args, spent)
		}
		var parked *Parked
		if !errors.As(err, &parked) {
			t.Fatalf("%s returned err=%v, want a *builtin.Parked", args, err)
		}
		if !parked.Until.Equal(due) {
			t.Fatalf("%s parked until %s, want %s", args, parked.Until.Format(time.RFC3339), due.Format(time.RFC3339))
		}
		if res != (Result{}) {
			t.Fatalf("%s parked and still brought an answer: %+v", args, res)
		}
	}

	if len(lines) != 2 {
		t.Fatalf("parking wrote %d log lines, want one per call: %q", len(lines), lines)
	}
	// The line a person debugging this has to read has to say the loop is gone and who is left
	// holding the wake-up — "parked until" alone would not tell the two parks apart.
	for _, want := range []string{"released", "daemon owns the wake-up", "5m0s"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("the park log line %q does not contain %q", lines[0], want)
		}
	}
}

// TestTheLimitLeavesAShorterWaitAlone is the boundary, read the honest way: exactly at the limit is
// not past it, so the wait is still slept and still answered in place — the same single result a
// caller has always gotten, with no handover to make.
func TestTheLimitLeavesAShorterWaitAlone(t *testing.T) {
	in := clockAt()
	in.MaxInLoopSleep = 5 * time.Minute
	args := `{"until":"` + at.Add(5*time.Minute).Format(time.RFC3339) + `"}`

	ctx, cancel := context.WithCancel(context.Background())
	tool, _ := Get("wait")
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res, err := tool.Run(ctx, in, json.RawMessage(args))
	if err != nil {
		t.Fatalf("a wait at the limit was handed over instead of slept: %v", err)
	}
	if !strings.Contains(res.Text, `"status":"interrupted"`) {
		t.Fatalf("a slept wait did not report the interruption: %q", res.Text)
	}
	cancel()
}

// TestAResumedWaitIsContinuedRatherThanRestarted is the regression for the bug that made every resumed
// wait unanswerable. The moment a wait is due is computed from the moment it was asked; on a resumed call
// "when it was asked" is a fact about an entry in the log, and a process that re-counts the length from its
// own clock instead finds a wait that is due six hours from now, hands itself over, is woken, and finds
// itself six hours from then. The shape is TestALongWaitIsHandedOverNotSlept's — the same frozen clock and a
// wait longer than the sleep cap — with one fact added: when the call was asked. That addition is what turns
// a handover into an answer.
func TestAResumedWaitIsContinuedRatherThanRestarted(t *testing.T) {
	in := clockAt()
	in.MaxInLoopSleep = time.Minute
	// Asked an hour before it was due: six hours from the ask is in the past, so there is nothing left to
	// wait for and the call answers for itself.
	in.ResumeCalledAt = at.Add(-7 * time.Hour)

	got := call(t, "wait", `{"duration":"6h"}`, in)
	if got.IsError {
		t.Fatalf("a wait that had already come due failed: %s", got.Text)
	}
	var res waitResult
	if err := json.Unmarshal([]byte(got.Text), &res); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, got.Text)
	}
	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed: %s", res.Status, got.Text)
	}
	if want := at.Add(-time.Hour).Format(time.RFC3339); res.ScheduledAt != want {
		t.Errorf("scheduled_at = %q, want the ask plus six hours (%s): counted from this wake it is a promise nobody made",
			res.ScheduledAt, want)
	}
	if res.ResumedAt != at.Format(time.RFC3339) {
		t.Errorf("resumed_at = %q, want the clock it answered on (%s)", res.ResumedAt, at.Format(time.RFC3339))
	}
}

// TestDeadlineIsTheSameAnswerTheCallGives is the invariant the recovery path stands on: the moment
// this function reports for a call is the moment the call itself slept to. A daemon that finds the
// call in a log after a restart re-arms the wait that was asked for, not a new one it invented.
func TestDeadlineIsTheSameAnswerTheCallGives(t *testing.T) {
	for _, args := range []string{`{"duration":"1ms"}`, `{"until":"2020-01-01T00:00:00Z"}`} {
		got, err := Deadline(json.RawMessage(args), at, "Asia/Taipei")
		if err != nil {
			t.Fatalf("Deadline(%s): %v", args, err)
		}
		res := call(t, "wait", args, clockAt())
		var asked waitResult
		if err := json.Unmarshal([]byte(res.Text), &asked); err != nil {
			t.Fatalf("%s did not answer with the JSON the tool promises: %v (%s)", args, err, res.Text)
		}
		slept, err := time.Parse(time.RFC3339, asked.ScheduledAt)
		if err != nil {
			t.Fatalf("scheduled_at %q is not RFC3339: %v", asked.ScheduledAt, err)
		}
		// Asked at the resolution the answer is able to carry: scheduled_at is written with time.RFC3339,
		// which spells whole seconds, so the answer of a one-millisecond wait cannot say it was due a
		// millisecond past 22:25:00 and the two would differ by that much as text forever. Nothing computes
		// from the sentence — a daemon re-derives the moment from the call's arguments and the entry's own
		// timestamp, at full precision — so what is lost is in the words, not in the promise.
		if !got.Truncate(time.Second).Equal(slept) {
			t.Fatalf("%s: Deadline says %s and the call slept to %s — two answers to one promise",
				args, got.Format(time.RFC3339), slept.Format(time.RFC3339))
		}
	}
}

// TestDeadlineRefusesWhatIsNotASchedule is for a log somebody edited by hand: the daemon reading it
// must not be told a promise it cannot keep, and the answer it gets has to name its tool because a
// person is the one who will read the line.
func TestDeadlineRefusesWhatIsNotASchedule(t *testing.T) {
	for _, args := range []string{
		`{}`,
		`{"duration":"5m","until":"2030-01-01T00:00:00Z"}`,
		`{"duration":"1000000h"}`,
		`{"until":"sometime after lunch"}`,
		`{"duration":`,
	} {
		_, err := Deadline(json.RawMessage(args), at, "")
		if err == nil {
			t.Errorf("Deadline(%s) answered a call the tool would have refused", args)
			continue
		}
		if !strings.HasPrefix(err.Error(), "wait") {
			t.Errorf("Deadline(%s) = %q: an error a model or a log reads has to name its tool", args, err)
		}
	}
}

// TestResumeNoteSaysWhatWasMissed pins the words the daemon puts under a wait it had to answer for:
// what was missed and why, in the voice of a wait rather than of a log line. It deliberately does
// not repeat the moment the wait resumed — resumed_at is that timestamp, and a second one printed a
// microsecond later is a contradiction waiting for a reader.
func TestResumeNoteSaysWhatWasMissed(t *testing.T) {
	got := ResumeNote("the daemon restarted", at.Add(-3*time.Hour-4*time.Minute), at)
	for _, want := range []string{
		"the daemon restarted",
		"was due at 2026-11-01T11:21:00Z",
		"3h4m0s before it could be answered",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the note %q does not contain %q", got, want)
		}
	}
	if n := strings.Count(got, "2026-11-01"); n != 1 {
		t.Fatalf("the note prints a day %d times, and one of them is a second answer to when: %q", n, got)
	}
	// A clock that went backwards while the process was gone is a fact about the clock, not a
	// duration to print in a sentence about the past.
	if back := ResumeNote("the clock jumped", at.Add(time.Hour), at); !strings.Contains(back, "0s before") {
		t.Fatalf("a wait that came back before it was due says: %q", back)
	}
}

// TestDaemonNoteGoesAfterTheJSON is what keeps the two accounts separable: a reader that parses the
// tool's answer must not have the daemon's sentence glued onto it, and a reader that wants the
// restart must not have to know the JSON to find it.
func TestDaemonNoteGoesAfterTheJSON(t *testing.T) {
	scheduled := at.Add(-time.Hour)
	in := clockAt()
	in.DaemonNote = ResumeNote("the daemon restarted", scheduled, at)

	got := call(t, "wait", `{"until":"`+at.Add(-time.Hour).Format(time.RFC3339)+`"}`, in)
	if got.IsError {
		t.Fatalf("a resumed wait failed: %s", got.Text)
	}
	head, tail, ok := strings.Cut(got.Text, "\n\n")
	if !ok {
		t.Fatalf("the note is not separated from the answer: %q", got.Text)
	}
	var res waitResult
	if err := json.Unmarshal([]byte(head), &res); err != nil {
		t.Fatalf("the answer alone is not the JSON the tool promises: %v (%s)", err, head)
	}
	if res.Status != "completed" {
		t.Fatalf("a resumed wait is %q, want completed: %s", res.Status, got.Text)
	}
	if res.ResumedAt != at.Format(time.RFC3339) {
		t.Fatalf("resumed_at = %q, want the clock the daemon answered on", res.ResumedAt)
	}
	if tail != in.DaemonNote {
		t.Fatalf("the note came out as %q, want %q", tail, in.DaemonNote)
	}
	// And a tool that was not told anything still answers as it always did.
	if plain := call(t, "wait", `{"until":"`+at.Add(-time.Hour).Format(time.RFC3339)+`"}`, clockAt()); plain.Text != head {
		t.Fatalf("an unannotated answer changed shape: %q", plain.Text)
	}
}

// TestInterruptedNamesWhoEndedIt is the difference between waiting again and reading the message:
// a person who interrupts gets their message appended right after this result, and only the word
// "user" tells the model to look at it. A cancellation nobody attributed stays plainly interrupted,
// because Stop() has no message after it and inventing a reason would be a claim about a person.
func TestInterruptedNamesWhoEndedIt(t *testing.T) {
	tool, _ := Get("wait")

	byPerson, cancelPerson := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancelPerson(ErrUserInterrupt)
	}()
	got, err := tool.Run(byPerson, Input{}, json.RawMessage(`{"duration":"30s"}`))
	if err != nil {
		t.Fatalf("an interrupted wait handed over instead of answering: %v", err)
	}
	var res waitResult
	if jsonErr := json.Unmarshal([]byte(got.Text), &res); jsonErr != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", jsonErr, got.Text)
	}
	if res.Reason != "user interrupt" {
		t.Fatalf("a wait ended by a person says reason=%q, want %q", res.Reason, "user interrupt")
	}
	cancelPerson(nil)

	byStop, cancelStop := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancelStop()
	}()
	gotStop, err := tool.Run(byStop, Input{}, json.RawMessage(`{"duration":"30s"}`))
	if err != nil {
		t.Fatalf("a stopped wait handed over instead of answering: %v", err)
	}
	if !strings.Contains(gotStop.Text, `"status":"interrupted"`) {
		t.Fatalf("a stopped wait is not interrupted: %q", gotStop.Text)
	}
	if strings.Contains(gotStop.Text, `"reason"`) {
		t.Fatalf("a stop nobody attributed still named a reason: %q", gotStop.Text)
	}
	cancelStop()
}

// TestACallThatArrivedEndedIsNotRescheduled is the rule the daemon's interrupt rests on: a wait handed a
// context that is already ended has to answer instead of handing itself over again, because every wake
// would park it again and the wait would then outlive the person who ended it. It is the check in front
// of the sleep limit that makes that true, which is why the order of the two is asserted here rather
// than left to reading.
func TestACallThatArrivedEndedIsNotRescheduled(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrUserInterrupt)

	tool, _ := Get("wait")
	got, err := tool.Run(ctx, Input{MaxInLoopSleep: time.Minute}, json.RawMessage(`{"duration":"6h"}`))
	if err != nil {
		var parked *Parked
		if errors.As(err, &parked) {
			t.Fatalf("an ended wait handed itself over to %s: it will be woken for nobody", parked.Until.Format(time.RFC3339))
		}
		t.Fatalf("an ended wait failed instead of answering: %v", err)
	}
	if !strings.Contains(got.Text, `"status":"interrupted"`) {
		t.Fatalf("an ended wait is not interrupted: %q", got.Text)
	}
	if !strings.Contains(got.Text, `"reason":"user interrupt"`) {
		t.Fatalf("an ended wait did not say who ended it: %q", got.Text)
	}
}

// TestWaitIsInterruptedByCancel is the escape hatch the session has: Stop() ends the wait, and the
// model is told which of the two happened rather than being left to infer it from a gap.
func TestWaitIsInterruptedByCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool, ok := Get("wait")
	if !ok {
		t.Fatal("wait does not exist")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	got, _ := tool.Run(ctx, Input{}, json.RawMessage(`{"duration":"30s"}`))
	if spent := time.Since(start); spent > 5*time.Second {
		t.Fatalf("the cancellation did not end the wait (took %v)", spent)
	}
	if !got.IsError || !strings.Contains(got.Text, `"status":"interrupted"`) {
		t.Fatalf("an interrupted wait did not say so: %q", got.Text)
	}
	cancel()
}

func TestWaitRefusesACentury(t *testing.T) {
	for _, args := range []string{`{"duration":"1000000h"}`, `{"until":"2200-01-01T00:00:00Z"}`} {
		got := call(t, "wait", args, Input{})
		if !got.IsError || !strings.Contains(got.Text, "typo") {
			t.Fatalf("%s was accepted instead of called a typo: %q", args, got.Text)
		}
	}
}

// TestWaitNamesTheParkBeforeItSleeps is the line that tells a five-minute silence apart from a
// hung run, so it has to be written before the sleep and not after it.
func TestWaitNamesTheParkBeforeItSleeps(t *testing.T) {
	var lines []string
	tool, _ := Get("wait")
	in := Input{Logf: func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}}
	_, _ = tool.Run(context.Background(), in, json.RawMessage(`{"duration":"15ms"}`))
	if len(lines) == 0 {
		t.Fatal("a wait slept without saying it was parking")
	}
	if !strings.Contains(lines[0], "parked until") || !strings.Contains(lines[0], "Stop") {
		t.Fatalf("the log line is %q: it has to name the deadline and the way out", lines[0])
	}
}
