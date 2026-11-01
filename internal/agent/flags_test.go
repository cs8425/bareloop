package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"bareloop/internal/ir"
	"bareloop/internal/llm"
	"bareloop/internal/store"
)

// These are the tests of the two flags a session list is drawn with: that pinning a session costs its
// config.json and nothing else, that neither flag is ever both, and that a config written for some other
// reason cannot move either one.

// flagRow is one row of the list this manager answers with, read out of a fresh List every time: the flags
// are only worth what they are worth if a listing is what carries them to whoever draws the list.
func flagRow(t *testing.T, m *Manager, id string) Summary {
	t.Helper()
	for _, s := range m.List() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("%q is not in the list", id)
	return Summary{}
}

// TestPinningAClosedSessionCostsAFileWrite is the whole reason there is a POST /api/session/{id}/flags
// instead of two more fields on the config PATCH: that PATCH answers its {id} by opening the session, and
// opening one reads its whole log. Marking where a session sits in a list has to cost a config.json and
// must not reach for a transcript - and it has to be a fact of the session rather than of this process,
// which is what a second Manager over the same root is here to catch.
func TestPinningAClosedSessionCostsAFileWrite(t *testing.T) {
	// The fixture is a session this process writes and abandons: exactly a directory in the list that
	// nothing has opened.
	root := t.TempDir()
	l, err := store.Open(filepath.Join(root, "shut"), false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := &ir.Entry{Kind: ir.KindUser, Role: "user", Content: "one", Ts: time.Now().UTC()}
	if err := l.AppendEntry(e, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if _, err := l.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m := NewManager(root, false, Config{}, nil, nil)
	if err := m.SaveConfig(Config{ID: "shut", Name: "a session nobody opened"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	pin, no := true, false
	if err := m.SetFlags("shut", &pin, nil); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	row := flagRow(t, m, "shut")
	if !row.Pinned {
		t.Error("Pinned: false, and the flag was just set on this id")
	}
	if row.Archived {
		t.Error("Archived: true, and setting a pin is not a word about the archive")
	}
	if !row.Closed {
		t.Error("the row is no longer closed: marking a session opened it")
	}
	if _, open := m.Peek("shut"); open {
		t.Error("Peek finds the session open, so SetFlags went through Get to reach a config it could have read")
	}
	// lastTs rides along because the row is made from the index either way, and a pinned group still has to
	// be drawn in some order. A row stored in whole seconds is compared in whole seconds.
	if row.LastTs == nil {
		t.Fatal("no lastTs on the row of a session whose one entry has a timestamp")
	}
	if row.LastTs.Unix() != e.Ts.Unix() {
		t.Errorf("lastTs %s, want the entry's %s", row.LastTs.Format(time.RFC3339), e.Ts.Format(time.RFC3339))
	}
	// why a second Manager says so: a pin that only lived in this process's memory, or only survived while
	// the directory happened to be open, is a pin that disappears on a daemon restart.
	if again := flagRow(t, NewManager(root, false, Config{}, nil, nil), "shut"); !again.Pinned {
		t.Error("a fresh Manager over the same root does not see the pin, so it was never written out")
	}

	// Archiving a session that is pinned is a change of mind, not a contradiction to litigate: the flag that
	// was just asked for wins, because one row is drawn in one place and cannot also sit at the top of it.
	if err := m.SetFlags("shut", nil, &pin); err != nil {
		t.Fatalf("SetFlags archived: %v", err)
	}
	row = flagRow(t, m, "shut")
	if !row.Archived || row.Pinned {
		t.Errorf("Pinned %v with Archived %v, want the archive to have taken the row it was just asked to take", row.Pinned, row.Archived)
	}
	// Unarchiving is not an undo, and that is the price of the invariant rather than an oversight: two flags
	// that are never both true means the pin was spent getting the session archived.
	if err := m.SetFlags("shut", nil, &no); err != nil {
		t.Fatalf("SetFlags unarchive: %v", err)
	}
	row = flagRow(t, m, "shut")
	if row.Pinned || row.Archived {
		t.Errorf("Pinned %v with Archived %v after unarchiving, want an ordinary row", row.Pinned, row.Archived)
	}
	// why this is asked of the manager and not just of applyFlags: a caller with a stale list is the way an
	// id with nothing behind it shows up, and a pin applied to nothing has to be an error.
	if err := m.SetFlags("nowhere", &pin, nil); err == nil {
		t.Error("SetFlags answered without error for an id that has no config.json")
	}
}

// TestAConfigPatchCarriesTheFlagsAlong is why UpdateConfig copies the stored pair into the patch it is
// about to merge: the settings panel PATCHes a whole config, and a body that never mentioned pinning must
// not be able to undo one. It is the same invariant applyFlags guards, from the other side - these two move
// only where somebody moves them.
func TestAConfigPatchCarriesTheFlagsAlong(t *testing.T) {
	ctx := context.Background()
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(ctx, Config{ID: "keep", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pin := true
	if err := s.SetFlags(&pin, nil); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if err := s.UpdateConfig(ctx, Config{MaxTurns: 3}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if !s.Config().Pinned {
		t.Error("a config patch that never mentioned pinning left the session unpinned")
	}
	if row := flagRow(t, m, "keep"); !row.Pinned {
		t.Error("the row a listing answers with lost the pin, so the patch wrote it out of config.json")
	}
	// The pair is carried over rather than merged from the body, so a client that sends the two fields as
	// part of a whole config does not have to work out which of its two requests won: the flags route is the
	// only door, and this says so with a patch that tried.
	if err := s.UpdateConfig(ctx, Config{Pinned: false, Archived: true}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if got := s.Config(); !got.Pinned || got.Archived {
		t.Errorf("Pinned %v with Archived %v, want a config patch to say nothing at all about either", got.Pinned, got.Archived)
	}
}
