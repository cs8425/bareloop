package builtin

import (
	"context"
	"encoding/json"
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
	got := tool.Run(ctx, Input{}, json.RawMessage(`{"duration":"30s"}`))
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
	_ = tool.Run(context.Background(), in, json.RawMessage(`{"duration":"15ms"}`))
	if len(lines) == 0 {
		t.Fatal("a wait slept without saying it was parking")
	}
	if !strings.Contains(lines[0], "parked until") || !strings.Contains(lines[0], "Stop") {
		t.Fatalf("the log line is %q: it has to name the deadline and the way out", lines[0])
	}
}
