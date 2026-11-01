package ir

// Tests for the turn list. The property that matters is not the shape of one row but that every id
// the list offers is a place the page cutter can begin a page: a ruler that offers a jump into the
// middle of a turn would hand the browser a tool result whose call it never drew.

import (
	"strings"
	"testing"
)

func TestTurnsOfTheWholePath(t *testing.T) {
	path := msgs(9, 3) // nine turns of four entries
	l := TurnsOf(path, 0)
	if len(l.Turns) != 9 || l.Total != 9 {
		t.Fatalf("got %d rows of a total %d, want 9 of 9", len(l.Turns), l.Total)
	}
	if l.HasOlder || l.Clamped || l.Limit != MaxTurnRows {
		t.Errorf("got older=%v clamped=%v limit=%d, want a whole path with no ceiling forced on it",
			l.HasOlder, l.Clamped, l.Limit)
	}
	if !equal(turnIDs(l), []string{"0000", "0004", "0008", "0012", "0016", "0020", "0024", "0028", "0032"}) {
		t.Errorf("got %v, want the head of every turn in path order", turnIDs(l))
	}
}

func TestTurnsAreTakenFromTheNewestEnd(t *testing.T) {
	path := msgs(9, 3)
	for _, limit := range []int{1, 2, 5} {
		l := TurnsOf(path, limit)
		if len(l.Turns) != limit {
			t.Fatalf("limit %d: got %d rows, want %d", limit, len(l.Turns), limit)
		}
		if l.Total != 9 || !l.HasOlder {
			t.Errorf("limit %d: got total %d older=%v, want 9 and older turns still out there",
				limit, l.Total, l.HasOlder)
		}
		want := make([]string, 0, limit)
		for i := 9 - limit; i < 9; i++ {
			want = append(want, posID(i*4))
		}
		if !equal(turnIDs(l), want) {
			t.Errorf("limit %d: got %v, want the last %d turn heads in order", limit, turnIDs(l), limit)
		}
		if l.Limit != limit || l.Clamped {
			t.Errorf("limit %d: got limit %d clamped=%v, want the size asked for, unclamped",
				limit, l.Limit, l.Clamped)
		}
	}
}

func TestTurnListIsClampedAndSaysSo(t *testing.T) {
	path := msgs(MaxTurnRows+5, 0) // one entry per turn, so the fixture stays small
	l := TurnsOf(path, MaxTurnRows+1)
	if !l.Clamped || l.Limit != MaxTurnRows {
		t.Errorf("got clamped=%v limit=%d, want the ceiling %d forced on the request", l.Clamped, l.Limit, MaxTurnRows)
	}
	if len(l.Turns) != MaxTurnRows || !l.HasOlder || l.Total != MaxTurnRows+5 {
		t.Errorf("got %d rows of a total %d older=%v, want %d rows and the rest older",
			len(l.Turns), l.Total, l.HasOlder, MaxTurnRows)
	}
	// The newest turn is never one of the rows that gets dropped.
	if got := l.Turns[len(l.Turns)-1].ID; got != posID(MaxTurnRows+4) {
		t.Errorf("got last row %s, want the tip's turn %s", got, posID(MaxTurnRows+4))
	}
	// A request under the ceiling is not clamping, and neither is no request at all.
	if short := TurnsOf(path, 10); short.Clamped || len(short.Turns) != 10 {
		t.Errorf("limit 10: got clamped=%v %d rows, want 10 rows unclamped", short.Clamped, len(short.Turns))
	}
	if none := TurnsOf(path, 0); none.Clamped || len(none.Turns) != MaxTurnRows || !none.HasOlder {
		t.Errorf("no limit: got clamped=%v %d rows older=%v, want the ceiling %d of them and older ones",
			none.Clamped, len(none.Turns), none.HasOlder, MaxTurnRows)
	}
}

// TestCompactionStartsATurnOfItsOwn reads the page_test fixture as a ruler: a summary is a jump target
// of its own and says so by kind, because the transcript draws a banner there and not a message.
func TestCompactionStartsATurnOfItsOwn(t *testing.T) {
	path := []*Entry{
		entry("1", "", KindUser, "user", "q"),
		entry("2", "1", KindAssistant, "assistant", "a"),
		compaction("3", "2", "1"),
		entry("4", "3", KindUser, "user", "q2"),
	}
	l := TurnsOf(path, 0)
	if !equal(turnIDs(l), []string{"1", "3", "4"}) {
		t.Fatalf("got %v, want the question, the summary and the next question", turnIDs(l))
	}
	for i, k := range []string{KindUser, KindCompaction, KindUser} {
		if l.Turns[i].Kind != k {
			t.Errorf("row %d: got kind %q, want %q", i, l.Turns[i].Kind, k)
		}
	}
	if l.Turns[1].Snippet != "[Conversation summary]" {
		t.Errorf("got snippet %q, want the summary's own first line", l.Turns[1].Snippet)
	}
}

// TestEveryTurnRowIsALegalPageBoundary is the seam between the two: centring a page on an id the list
// offered must put that entry first, and never cut it away.
func TestEveryTurnRowIsALegalPageBoundary(t *testing.T) {
	paths := [][]*Entry{
		msgs(6, 3),
		msgs(6, 0),
		append(msgs(2, 1), compaction("99", posID(3), posID(0))),
		msgs(1, 4)[1:], // a path that opens mid-turn, with no user message on it at all
	}
	for _, path := range paths {
		for _, tr := range TurnsOf(path, 0).Turns {
			p, err := PageOf(path, PageOptions{Limit: 1, Anchor: tr.ID})
			if err != nil {
				t.Fatalf("anchor %q: %v", tr.ID, err)
			}
			if len(p.Entries) == 0 {
				t.Errorf("anchor %q: the page came back empty", tr.ID)
				continue
			}
			if p.Entries[0].ID != tr.ID {
				t.Errorf("anchor %q: page starts at %v, want the row itself first", tr.ID, idsOf(p.Entries))
			}
		}
	}
}

func TestSnippetIsTheFirstLine(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"plain", "hello", "hello"},
		{"the line after", "first\nsecond", "first"},
		{"blank lines first", "\n\n  hi there\n", "hi there"},
		{"a windows pair", "one\r\ntwo", "one"},
		{"spaces off the end", "one   \ntwo", "one"},
		{"nothing at all", "", ""},
	}
	for _, c := range cases {
		l := TurnsOf([]*Entry{entry("1", "", KindUser, "user", c.content)}, 0)
		if got := l.Turns[0].Snippet; got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSnippetCutsByRunes is why the budget counts runes: a Chinese transcript cut at a byte boundary
// arrives as half a character.
func TestSnippetCutsByRunes(t *testing.T) {
	long := strings.Repeat("字", MaxTurnSnippet+30)
	l := TurnsOf([]*Entry{entry("1", "", KindUser, "user", long)}, 0)
	if want := strings.Repeat("字", MaxTurnSnippet) + "…"; l.Turns[0].Snippet != want {
		t.Errorf("got %d runes, want %d of them and an ellipsis", len([]rune(l.Turns[0].Snippet)), MaxTurnSnippet)
	}
	// Exactly at the budget is not a cut, and says nothing about one.
	exact := strings.Repeat("字", MaxTurnSnippet)
	if got := TurnsOf([]*Entry{entry("1", "", KindUser, "user", exact)}, 0).Turns[0].Snippet; got != exact {
		t.Errorf("got %q, want the line untouched", got)
	}
	// Bytes are never fewer than runes, so this one is over budget by exactly one and gets cut.
	ascii := strings.Repeat("a", MaxTurnSnippet+1)
	if got := TurnsOf([]*Entry{entry("1", "", KindUser, "user", ascii)}, 0).Turns[0].Snippet; got != ascii[:MaxTurnSnippet]+"…" {
		t.Errorf("got %q, want %d of them and an ellipsis", got, MaxTurnSnippet)
	}
}

func TestSnippetOfAnAttachment(t *testing.T) {
	named := &Entry{ID: "1", Kind: KindUser, Role: "user",
		Parts: []Part{{Type: PartImage, MimeType: "image/png", Name: "cat.png", Data: "AAA"}}}
	if got := snippetOf(named); got != "cat.png" {
		t.Errorf("got %q, want the name the user attached it under", got)
	}
	unnamed := &Entry{ID: "1", Kind: KindUser, Role: "user",
		Parts: []Part{{Type: PartImage, MimeType: "image/png", Data: "AAA"}}}
	if got := snippetOf(unnamed); got != PartImage {
		t.Errorf("got %q, want %q: there is an attachment, and a blank row would deny it", got, PartImage)
	}
	// A multi-part tool result keeps its text in the parts and not in content.
	multi := &Entry{ID: "1", Kind: KindTool, Role: "tool",
		Parts: []Part{{Type: PartImage, Data: "AAA"}, {Type: PartText, Text: "from a part"}}}
	if got := snippetOf(multi); got != "from a part" {
		t.Errorf("got %q, want the text part", got)
	}
	if got := snippetOf(&Entry{ID: "1", Kind: KindUser, Role: "user"}); got != "" {
		t.Errorf("got %q, want nothing to say", got)
	}
}

func TestTurnsOfAnEmptyPath(t *testing.T) {
	l := TurnsOf(nil, 10)
	if len(l.Turns) != 0 || l.Total != 0 || l.HasOlder {
		t.Errorf("got %d rows of a total %d older=%v, want no turns at all", len(l.Turns), l.Total, l.HasOlder)
	}
	if l.Turns == nil {
		t.Error("got a nil list, want an empty one so the response says [] and not null")
	}
}

// TestTurnsOfAPathWithNoUserMessage reads the page_test rule as a ruler: position 0 is a turn
// whatever it holds, so a path that opens mid-turn still has one place to jump to.
func TestTurnsOfAPathWithNoUserMessage(t *testing.T) {
	l := TurnsOf(msgs(1, 4)[1:], 0)
	if len(l.Turns) != 1 || l.Turns[0].ID != posID(1) {
		t.Fatalf("got %v, want one turn at %s", turnIDs(l), posID(1))
	}
}

// turnIDs lists the ids a turn list came back with, so a failure says which rows it meant.
func turnIDs(l *TurnList) []string {
	out := make([]string, 0, len(l.Turns))
	for _, tr := range l.Turns {
		out = append(out, tr.ID)
	}
	return out
}
