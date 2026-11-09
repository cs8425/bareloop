package ir

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// The spine mirror (r101 B0) promises that PathMeta, EffectiveMeta, PageOfSpine and
// TurnsSpine answer a resident spine of Metas with exactly the rules Path, Effective,
// PageOf and TurnsOf answer a resident set of Entries. Every test here lays both readings
// of one tree side by side and fails where the answers differ — the drift the split could
// silently introduce is exactly what this file is.

func metaMirror(e *Entry) *Meta {
	m := &Meta{ID: e.ID, ParentID: e.ParentID, Kind: e.Kind, Ts: e.Ts}
	if e.Compaction != nil {
		c := *e.Compaction
		if e.Compaction.BoundaryID != nil {
			b := *e.Compaction.BoundaryID
			c.BoundaryID = &b
		}
		if e.Compaction.Summary != nil {
			c.Summary = append([]SummaryAttempt(nil), e.Compaction.Summary...)
		}
		m.Compaction = &c
	}
	return m
}

func mirrorMaps(es []*Entry) (map[string]*Entry, map[string]*Meta) {
	eMap := make(map[string]*Entry, len(es))
	mMap := make(map[string]*Meta, len(es))
	for _, e := range es {
		eMap[e.ID] = e
		mMap[e.ID] = metaMirror(e)
	}
	return eMap, mMap
}

func entryIDs(es []*Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func metaIDs(ms []*Meta) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// spineChain is one twelve-entry line with a compaction in the middle of it: users,
// replies, a tool result, a summary whose boundary resolves onto the path. Enough shape
// that turn starts, page cuts and boundaries all mean something.
//
// Role walks in step with Kind (a compaction travels with role user) because that is the
// writer's invariant: IsTurnStart reads a role off an entry while IsTurnStartM reads a
// kind off the spine, and a fixture that let the two disagree would be testing drift
// that no append could ever produce.
func spineChain() []*Entry {
	kinds := []string{KindUser, KindAssistant, KindTool, KindAssistant, KindUser, KindCompaction,
		KindUser, KindAssistant, KindUser, KindAssistant, KindUser, KindAssistant}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var out []*Entry
	var parent string
	for i, k := range kinds {
		id := fmt.Sprintf("%07d", i+1)
		role := "user"
		switch k {
		case KindAssistant:
			role = "assistant"
		case KindTool:
			role = "tool"
		}
		e := &Entry{ID: id, ParentID: parent, Kind: k, Role: role,
			Content: "first line of " + id + "\nsecond line of " + id,
			Ts:      base.Add(time.Duration(i) * time.Minute)}
		if k == KindCompaction {
			b := "0000005"
			e.Compaction = &CompactionMeta{BoundaryID: &b, Replaced: 5, SummarySize: 40}
		}
		out = append(out, e)
		parent = id
	}
	return out
}

func TestPathMetaWalksWhatPathWalks(t *testing.T) {
	chain := spineChain()
	eMap, mMap := mirrorMaps(chain)
	for _, id := range []string{chain[len(chain)-1].ID, "0000003", "0000001", "0000099", ""} {
		want, got := entryIDs(Path(eMap, id)), metaIDs(PathMeta(mMap, id))
		if !reflect.DeepEqual(want, got) {
			t.Errorf("Path(%q) = %v, PathMeta = %v", id, want, got)
		}
	}

	// A cycle is corruption, and both sides survive it the same way: each entry visited
	// once, the walk stops where it first doubled.
	cyc := map[string]*Entry{
		"a": {ID: "a", ParentID: "b", Kind: KindUser},
		"b": {ID: "b", ParentID: "a", Kind: KindUser},
	}
	cycM := map[string]*Meta{"a": metaMirror(cyc["a"]), "b": metaMirror(cyc["b"])}
	if want, got := entryIDs(Path(cyc, "a")), metaIDs(PathMeta(cycM, "a")); !reflect.DeepEqual(want, got) {
		t.Errorf("cycle: Path = %v, PathMeta = %v", want, got)
	}
}

func TestEffectiveMetaParityWithEffective(t *testing.T) {
	cmp := func(id, parent string, boundary *string) *Entry {
		return &Entry{ID: id, ParentID: parent, Kind: KindCompaction, Role: "user", Content: "sum",
			Ts: time.Now().UTC(), Compaction: &CompactionMeta{BoundaryID: boundary}}
	}
	ptr := func(s string) *string { return &s }

	// Each scenario is one root-to-tip chain handed to both readings; ids are positional
	// so a boundary string can name an entry the scenario just built.
	scenarios := []struct {
		name string
		path []*Entry
	}{
		{name: "no compaction", path: []*Entry{
			{ID: "u1", Kind: KindUser, Role: "user"},
			{ID: "a1", ParentID: "u1", Kind: KindAssistant, Role: "assistant"},
		}},
		{name: "boundary resolves", path: []*Entry{
			{ID: "u1", Kind: KindUser, Role: "user"},
			{ID: "a1", ParentID: "u1", Kind: KindAssistant, Role: "assistant"},
			{ID: "u2", ParentID: "a1", Kind: KindUser, Role: "user"},
			cmp("c1", "u2", ptr("a1")),
		}},
		{name: "deliberate empty kept", path: []*Entry{
			{ID: "u1", Kind: KindUser, Role: "user"},
			cmp("c1", "u1", nil),
			{ID: "a1", ParentID: "c1", Kind: KindAssistant, Role: "assistant"},
		}},
		{name: "boundary gone: fail open", path: []*Entry{
			{ID: "u1", Kind: KindUser, Role: "user"},
			{ID: "a1", ParentID: "u1", Kind: KindAssistant, Role: "assistant"},
			cmp("c1", "a1", ptr("vanished")),
		}},
		{name: "the later summary is the active one", path: []*Entry{
			cmp("c1", "", nil),
			{ID: "u1", ParentID: "c1", Kind: KindUser, Role: "user"},
			cmp("c2", "u1", ptr("u1")),
		}},
		{name: "a summary with no record yet", path: []*Entry{
			{ID: "u1", Kind: KindUser, Role: "user"},
			{ID: "c1", ParentID: "u1", Kind: KindCompaction, Role: "user", Content: "sum"},
			{ID: "a1", ParentID: "c1", Kind: KindAssistant, Role: "assistant"},
		}},
	}
	for _, sc := range scenarios {
		eMap, mMap := mirrorMaps(sc.path)
		keptE, cmpE := Effective(eMap, Path(eMap, sc.path[len(sc.path)-1].ID))
		keptM, cmpM := EffectiveMeta(mMap, PathMeta(mMap, sc.path[len(sc.path)-1].ID))
		if want, got := entryIDs(keptE), metaIDs(keptM); !reflect.DeepEqual(want, got) {
			t.Errorf("%s: kept %v vs %v", sc.name, want, got)
		}
		if (cmpE == nil) != (cmpM == nil) {
			t.Fatalf("%s: active summary %v vs %v", sc.name, cmpE, cmpM)
		}
		if cmpE != nil {
			if cmpE.ID != cmpM.ID {
				t.Errorf("%s: active summary %s vs %s", sc.name, cmpE.ID, cmpM.ID)
			}
			if cmpE.Compaction.Broken != cmpM.Compaction.Broken {
				t.Errorf("%s: broken %v vs %v", sc.name, cmpE.Compaction.Broken, cmpM.Compaction.Broken)
			}
		}
	}
}

func TestPageOfSpineParityWithPageOf(t *testing.T) {
	chain := spineChain()
	eMap, mMap := mirrorMaps(chain)
	fullE := Path(eMap, "0000012")
	fullM := PathMeta(mMap, "0000012")

	cases := []struct {
		o    PageOptions
		want string // "" for legal, the word a refusal must contain
	}{
		{PageOptions{}, ""},
		{PageOptions{Limit: 1}, ""},
		{PageOptions{Limit: 3}, ""},
		{PageOptions{Limit: 5, Before: "0000007"}, ""},
		{PageOptions{Before: "0000005", Limit: 2}, ""},
		{PageOptions{Limit: 2, After: "0000005"}, ""},
		{PageOptions{Limit: 6, Anchor: "0000006"}, ""},
		{PageOptions{Limit: 1, Anchor: "0000002"}, ""},
		{PageOptions{Before: "0000004"}, ""},
		{PageOptions{Limit: MaxPageEntries + 1}, ""},
		{PageOptions{Anchor: "0000002"}, "size"},
		{PageOptions{Before: "0000002", Anchor: "0000003"}, "combines"},
		{PageOptions{Before: "0000002", After: "0000003"}, "combines"},
		{PageOptions{Limit: 2, Before: "0000099"}, "unknown id"},
		{PageOptions{Limit: 2, After: "0000099"}, "unknown id"},
		{PageOptions{Limit: 2, Anchor: "0000099"}, "unknown id"},
	}
	for _, c := range cases {
		pe, errE := PageOf(fullE, c.o)
		pM, errM := PageOfSpine(fullM, c.o)
		if (errE == nil) != (errM == nil) {
			t.Fatalf("%+v: PageOf err %v, PageOfSpine err %v", c.o, errE, errM)
		}
		if errE != nil {
			if errM != nil && (errE.Error() != errM.Error()) {
				t.Errorf("%+v: refusals disagree: %q vs %q", c.o, errE, errM)
			}
			if c.want == "" {
				t.Errorf("%+v: unexpectedly refused: %v", c.o, errE)
			}
			continue
		}
		if c.want != "" {
			t.Errorf("%+v: should have been refused (%q)", c.o, c.want)
		}
		if want, got := entryIDs(pe.Entries), pM.IDs; !reflect.DeepEqual(want, got) {
			t.Errorf("%+v: ids %v vs %v", c.o, want, got)
		}
		if pe.Total != pM.Total || pe.HasOlder != pM.HasOlder || pe.HasNewer != pM.HasNewer ||
			pe.Limit != pM.Limit || pe.Clamped != pM.Clamped || pe.Truncated != pM.Truncated {
			t.Errorf("%+v: flags disagree: %+v vs %+v", c.o, pe, pM)
		}
	}
}

func TestTurnsSpineParityWithTurnsOf(t *testing.T) {
	chain := spineChain()
	byID := make(map[string]*Entry, len(chain))
	for _, e := range chain {
		byID[e.ID] = e
	}
	eMap, mMap := mirrorMaps(chain)
	fullE, fullM := Path(eMap, "0000012"), PathMeta(mMap, "0000012")

	contents := func(ids []string) (map[string]*Entry, error) {
		out := make(map[string]*Entry, len(ids))
		for _, id := range ids {
			if e := byID[id]; e != nil {
				out[id] = e
			}
		}
		return out, nil
	}

	// The turn list's rows carry the entry's id, kind, timestamp and first line — and the
	// spine path holds exactly those, so the two answers compare struct for struct.
	for _, limit := range []int{0, 2, 5, MaxTurnRows + 1} {
		want, err1 := TurnsOf(fullE, limit), error(nil)
		got, err2 := TurnsSpine(fullM, limit, contents)
		if err1 != nil || err2 != nil {
			t.Fatalf("limit %d: %v / %v", limit, err1, err2)
		}
		if !reflect.DeepEqual(want.Turns, got.Turns) {
			t.Errorf("limit %d: rows disagree:\n%v\n%v", limit, want.Turns, got.Turns)
		}
		if want.Total != got.Total || want.HasOlder != got.HasOlder ||
			want.Limit != got.Limit || want.Clamped != got.Clamped {
			t.Errorf("limit %d: flags disagree: %+v vs %+v", limit, want, got)
		}
	}

	// An entry deleted between the spine walk and the lookup keeps its row — the row is
	// the jump target, and the jump target is still true — and loses only its snippet.
	holes := func(ids []string) (map[string]*Entry, error) {
		out, err := contents(ids)
		if err != nil {
			return nil, err
		}
		delete(out, "0000005") // the turn the summary's boundary names
		return out, nil
	}
	got, err := TurnsSpine(fullM, 0, holes)
	if err != nil {
		t.Fatalf("with a hole: %v", err)
	}
	var holeRow *Turn
	for i := range got.Turns {
		if got.Turns[i].ID == "0000005" {
			holeRow = &got.Turns[i]
		}
	}
	if holeRow == nil {
		t.Fatal("the deleted turn lost its row instead of its snippet")
	}
	if holeRow.Snippet != "" || holeRow.Kind != KindUser || holeRow.Ts.IsZero() {
		t.Errorf("the hole is not a bare true row: %+v", *holeRow)
	}

	if _, err := TurnsSpine(fullM, 0, func([]string) (map[string]*Entry, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Error("a failing lookup must end the ruler, not blank it open")
	}

	// Both sides answer the empty path with no rows and no error.
	empty, err := TurnsSpine(nil, 0, contents)
	if err != nil || len(empty.Turns) != 0 || empty.Total != 0 {
		t.Errorf("empty path: %+v, %v", empty, err)
	}
}
