package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func datetimeTool() Tool {
	return Tool{
		Name: "get_datetime",
		Description: "Get the current date and time, with the day of the week. " +
			"Without a timezone argument it answers in this session's default timezone (UTC when none is configured). " +
			"The answer always names the timezone it is in and ends with an RFC3339 stamp, which a wait call can take as-is.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"timezone":{"type":"string","description":"Which zone to answer in: an IANA name (Asia/Taipei), an offset (+08:00, UTC-5), UTC, or Local. Default: this session's timezone, UTC when unset."}},"additionalProperties":false}`),
		Run:         runDatetime,
	}
}

// datetimeArgs is the whole argument set: one optional zone, because the interesting question this
// tool answers is never "what is the epoch" but "what time is it where the work is happening".
type datetimeArgs struct {
	Timezone *string `json:"timezone"`
}

// runDatetime has nothing to hand over to the daemon: an answer about the clock is always cheap
// enough to give here, so every return below is an answer and the error is always nil.
func runDatetime(_ context.Context, in Input, raw json.RawMessage) (Result, error) {
	var q datetimeArgs
	if err := decodeArgs(raw, &q); err != nil {
		return ErrText("get_datetime: " + err.Error()), nil
	}
	// A named zone wins over the session default: that is the whole point of the argument, and it
	// is what lets one call ask about the other end of a conversation.
	name := argText(q.Timezone)
	if name == "" {
		name = in.DefaultTimezone
	}
	loc, err := LoadTimezone(name)
	if err != nil {
		if argText(q.Timezone) == "" {
			// The broken value is the session's own, which is a different file to go and fix, so
			// the message says which value was read rather than blaming the caller's argument.
			return ErrText("get_datetime: " + err.Error() + ` (this session's timezone setting)`), nil
		}
		return ErrText("get_datetime: " + err.Error()), nil
	}
	return Result{Text: FormatTime(in.now().In(loc))}, nil
}

// FormatTime is the two lines get_datetime answers with: the human one (date, clock, offset, zone,
// day of the week) and the machine one (RFC3339), which is a `wait` argument waiting to be pasted —
// the two tools are meant to read as one conversation with a clock.
//
// The zone is named whatever the caller asked for, including when nobody asked. An unlabelled
// timestamp is a claim about a zone nobody made, and "which zone is that in" is the one question a
// time tool's answer has to make unaskable.
func FormatTime(t time.Time) string {
	return fmt.Sprintf("%s %s (%s) %s\n%s",
		t.Format("2006-01-02 15:04:05"), t.Format("-07:00"), ZoneLabel(t), t.Format("Monday"),
		t.Format(time.RFC3339))
}
