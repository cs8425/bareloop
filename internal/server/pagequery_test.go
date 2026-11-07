package server

// Tests for the layer that turns a URL into ir.PageOptions. The paging itself is tested in ir; what can
// go wrong here, and only here, is a name the URL speaks that this server never reads. That bug is quiet:
// the dropped parameter leaves a request for the newest page, and a client that scrolls forward and gets
// the newest page again looks exactly like a client that works.

import (
	"net/http/httptest"
	"testing"

	"bareloop/internal/ir"
)

func TestPageQueryReadsEveryWindowName(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/session/s?limit=20&after=0000003", nil)
	o, err := pageQuery(r)
	if err != nil {
		t.Fatalf("pageQuery: %v", err)
	}
	if o.Limit != 20 || o.After != "0000003" {
		t.Errorf("got limit %d after %q, want 20 and 0000003", o.Limit, o.After)
	}
	if o.Before != "" || o.Anchor != "" {
		t.Errorf("got %+v, want nothing else set", o)
	}

	// An empty field is no cut — the rule `before` has always followed, and the reason a client may
	// serialise the whole window on every request: an empty field means "no id here", not "an id that
	// is not on the path", which would otherwise be a 400 on every request a jump page makes.
	for _, q := range []string{"?after=", "?after=&limit=20", "?before=&after="} {
		o, err := pageQuery(httptest.NewRequest("GET", "/api/session/s"+q, nil))
		if err != nil {
			t.Errorf("pageQuery(%q): %v", q, err)
			continue
		}
		if o.After != "" || o.Before != "" {
			t.Errorf("pageQuery(%q): got before %q after %q, want both absent", q, o.Before, o.After)
		}
	}

	// Which combinations mean anything is decided in ir.PageOf, where one rule has one home, so this
	// layer's job ends at handing both ids over. Handing over only one of them would turn a refused
	// request into an answered one — a page, where the answer is that the question is two pages.
	o, err = pageQuery(httptest.NewRequest("GET", "/api/session/s?after=0000003&before=0000001", nil))
	if err != nil || o.After == "" || o.Before == "" {
		t.Errorf("got %+v (%v), want both ids handed to ir", o, err)
	}
}

func TestPageQueryDefaultIsAPage(t *testing.T) {
	// The endpoint's contract: a GET that names no window buys a page, never the transcript.
	// (One session's unparameterised read measured 73 MB; this test is why it cannot happen again.)
	o, err := pageQuery(httptest.NewRequest("GET", "/api/session/s", nil))
	if err != nil || o.Limit != ir.DefaultPageEntries {
		t.Errorf("got %+v (%v), want the default page of %d", o, err, ir.DefaultPageEntries)
	}
	// A bare place name gets the default page too: "everything older than here" is an unbounded
	// slice wearing a cursor, and no URL shape buys one any more.
	for _, q := range []string{"?before=0000003", "?after=0000003"} {
		o, err := pageQuery(httptest.NewRequest("GET", "/api/session/s"+q, nil))
		if err != nil || o.Limit != ir.DefaultPageEntries {
			t.Errorf("pageQuery(%q): got limit %d (%v), want the default page", q, o.Limit, err)
		}
	}
	// An explicit limit still outranks the default.
	if o, err := pageQuery(httptest.NewRequest("GET", "/api/session/s?limit=7", nil)); err != nil || o.Limit != 7 {
		t.Errorf("got limit %d (%v), want 7", o.Limit, err)
	}
}

func TestTurnsRefusesEveryPlaceName(t *testing.T) {
	// A turn list that skipped a stretch of the path without saying so is worse than one that refuses,
	// and `after` names a stretch as precisely as the other two ids do.
	for _, q := range []string{"before=0000003", "after=0000003", "anchor=0000003"} {
		if n, err := turnLimit(httptest.NewRequest("GET", "/api/session/s/turns?"+q, nil)); err == nil {
			t.Errorf("turns with %s: want a refusal, got limit %d", q, n)
		}
	}
	// Empty is a client that always serialises every field, not a request for a place.
	n, err := turnLimit(httptest.NewRequest("GET", "/api/session/s/turns?after=&limit=7", nil))
	if err != nil || n != 7 {
		t.Errorf("turns with an empty after: got limit %d (%v), want 7", n, err)
	}
}
