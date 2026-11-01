package agent

import (
	"path/filepath"
	"testing"
	"time"

	"bareloop/internal/ir"
	"bareloop/internal/store"
)

// TestAClosedSessionStillHasItsRow is what the session list promised: a directory on disk that nothing has
// opened is listed with its name and its numbers, and those numbers come from the index file rather than
// from a transcript nobody asked to read. The fixture is built by the store and then abandoned, which is
// exactly the state of a session this process has never opened.
func TestAClosedSessionStillHasItsRow(t *testing.T) {
	root := t.TempDir()
	l, err := store.Open(filepath.Join(root, "listed"), false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e := &ir.Entry{Kind: ir.KindUser, Role: "user", Content: "one", Ts: time.Now().UTC()}
	if err := l.AppendEntry(e, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	// The read is what writes an index: a session that was never read has no numbers to be listed with,
	// which is the other half of this and is tested in the store.
	if _, err := l.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m := NewManager(root, false, Config{}, nil, nil)
	if err := m.SaveConfig(Config{ID: "listed", Name: "a session nothing opened"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	var got *Summary
	for _, s := range m.List() {
		if s.ID == "listed" {
			row := s
			got = &row
		}
	}
	if got == nil {
		t.Fatal("the directory is not in the list")
	}
	if !got.Closed {
		t.Error("listing the session opened it, and the list is not supposed to")
	}
	if got.Name != "a session nothing opened" {
		t.Errorf("Name %q, want the one config.json holds", got.Name)
	}
	if got.Path != 1 || got.Log != 1 {
		t.Errorf("Path %d and Log %d, want the one entry and the one record the log holds", got.Path, got.Log)
	}
	if got.Tip != e.ID {
		t.Errorf("Tip %q, want %q", got.Tip, e.ID)
	}
	// Bytes is the figure the sidebar shows even when it has nothing else to show, because a stat asks
	// nothing of the log.
	if got.Bytes == 0 {
		t.Error("Bytes: 0, and the log is a file on disk")
	}
	if got.Skipped != 0 {
		t.Errorf("Skipped: %d, want 0", got.Skipped)
	}
}

// TestAListingDoesNotOpenWhatItLists is the same claim from the other side: after a listing, the manager
// still holds no sessions, which is what lets a daemon with a hundred session directories start without
// reading a hundred logs.
func TestAListingDoesNotOpenWhatItLists(t *testing.T) {
	root := t.TempDir()
	l, err := store.Open(filepath.Join(root, "unopened"), false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.AppendEntry(&ir.Entry{Kind: ir.KindUser, Role: "user", Content: "one", Ts: time.Now().UTC()}, true); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if _, err := l.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m := NewManager(root, false, Config{}, nil, nil)
	if len(m.List()) == 0 {
		t.Fatal("nothing in the list")
	}
	if _, open := m.Peek("unopened"); open {
		t.Error("the listing opened the session it was only asked to name")
	}
}
