package builtin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	// tzdata is imported for its side effect. Go looks at the platform's zoneinfo first and falls
	// back to this embedded copy, which is what makes an IANA name resolve on a machine that has
	// no zoneinfo directory at all.
	_ "time/tzdata"
)

// LoadTimezone resolves a timezone the way a person writes one: an IANA name ("Asia/Taipei"), a
// fixed offset in any of the shapes that get typed ("+08:00", "UTC+8", "GMT-5:30"), or one of the
// words that mean themselves.
//
// Empty is UTC. That is the rule the session config leans on too, and it is UTC rather than the
// machine's own zone because which machine answered the request is not information anybody asked
// for; a session that wants the host's zone writes "Local".
func LoadTimezone(name string) (*time.Location, error) {
	s := strings.TrimSpace(name)
	if s == "" {
		return time.UTC, nil
	}
	switch strings.ToLower(s) {
	case "utc", "gmt", "z", "zulu":
		return time.UTC, nil
	case "local":
		return time.Local, nil
	}
	if loc, ok := fixedOffset(s); ok {
		return loc, nil
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, fmt.Errorf(`unknown timezone %q: use an IANA name like Asia/Taipei, an offset like +08:00 or UTC-5, or "UTC" ("Local" is the machine's own zone)`, s)
	}
	return loc, nil
}

// offsetRE is the fixed-offset dialect: an optional UTC/GMT/UT prefix, a sign, one or two digits of
// hour, and an optional minute part written with or without the colon. It matches what a person
// types when they have an offset in mind ("+08", "+08:00", "+0800", "UTC+8", "GMT-5:30") and is not
// trying to be ISO 8601.
var offsetRE = regexp.MustCompile(`^(?:UTC|GMT|UT)?\s*([+-])(\d{1,2})(?::?(\d{2}))?$`)

// fixedOffset reads a zone defined only by its distance from UTC. The zone is named after the
// offset ("UTC+08:00") instead of being handed an abbreviation nobody agreed to, because that name
// shows up in the tool result and the offset is the only thing here that was actually stated.
func fixedOffset(s string) (*time.Location, bool) {
	m := offsetRE.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return nil, false
	}
	hour, err := strconv.Atoi(m[2])
	if err != nil {
		return nil, false
	}
	minutes := 0
	if m[3] != "" {
		if minutes, err = strconv.Atoi(m[3]); err != nil {
			return nil, false
		}
	}
	// Every real zone is within ±14 hours of UTC, so anything past that is a typo for something
	// else and is better sent to the message that lists the accepted forms.
	if hour > 14 || minutes > 59 {
		return nil, false
	}
	secs := hour*3600 + minutes*60
	if m[1] == "-" {
		secs = -secs
	}
	return time.FixedZone(fmt.Sprintf("UTC%s%02d:%02d", m[1], hour, minutes), secs), true
}

// ZoneLabel is how a zone gets named in a tool result: the zone, plus its abbreviation when that
// abbreviation says anything the name does not ("Asia/Taipei CST", but plain "UTC" rather than
// "UTC UTC").
func ZoneLabel(t time.Time) string {
	zone := t.Location().String()
	abbrev := t.Format("MST")
	offset := t.Format("-07:00")
	if abbrev == "" || abbrev == zone || abbrev == offset {
		return zone
	}
	return zone + " " + abbrev
}
