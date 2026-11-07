package xfer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bareloop/internal/ir"
)

// The webui tests replay hand-written fixtures written against the upstream schema —
// the same dispatch the route runs (Sniff, then the dialect's reader), so a fixture that
// drifts from what llama-ui actually writes fails where it matters.

func fixture(t *testing.T, name string) *Import {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	stream, kind, err := Sniff(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("fixture %s: sniff: %v", name, err)
	}
	if kind != KindWebUI {
		t.Fatalf("fixture %s: sniffed %q, want webui", name, kind)
	}
	imp, err := DecodeWebUI(stream)
	if err != nil {
		t.Fatalf("fixture %s: decode: %v", name, err)
	}
	return imp
}

func byIDOf(imp *Import) map[string]*ir.Entry {
	m := make(map[string]*ir.Entry, len(imp.Entries))
	for _, e := range imp.Entries {
		m[e.ID] = e
	}
	return m
}

func TestWebUIBasic(t *testing.T) {
	imp := fixture(t, "webui_basic.jsonl")
	if imp.Name != "Basic" {
		t.Fatalf("name %q", imp.Name)
	}
	if imp.System != "You are a careful assistant." {
		t.Fatalf("system root did not become System: %q", imp.System)
	}
	if imp.Tip != "m3" {
		t.Fatalf("tip %q, want m3 (currNode)", imp.Tip)
	}
	if len(imp.Entries) != 3 {
		t.Fatalf("%d entries, want 3 (the root is config, not a turn)", len(imp.Entries))
	}
	m := byIDOf(imp)
	if m["m1"].ParentID != "" {
		t.Fatalf("the message under a skipped root should be a root itself, parent %q", m["m1"].ParentID)
	}
	if m["m1"].Kind != ir.KindUser || m["m2"].Kind != ir.KindAssistant || m["m3"].Kind != ir.KindUser {
		t.Fatalf("roles did not map to kinds")
	}
	if m["m2"].Model != "gemma-test" {
		t.Fatalf("model lost")
	}
	want := time.UnixMilli(1762499930000).UTC()
	if !m["m3"].Ts.Equal(want) {
		t.Fatalf("timestamp %v, want %v (ms epoch)", m["m3"].Ts, want)
	}
}

func TestWebUITreeKeepsEveryBranch(t *testing.T) {
	imp := fixture(t, "webui_tree.jsonl")
	if len(imp.Entries) != 6 {
		t.Fatalf("%d entries, want 6 (three siblings plus the forked question)", len(imp.Entries))
	}
	m := byIDOf(imp)
	if imp.Tip != "u2" {
		t.Fatalf("tip %q, want u2 — the abandoned regenerations stay off the tip, not off the file", imp.Tip)
	}
	kids := map[string]int{}
	for _, e := range imp.Entries {
		kids[e.ParentID]++
	}
	if kids["u1"] != 3 {
		t.Fatalf("u1 has %d children, want the three regenerated siblings", kids["u1"])
	}
	if kids[""] != 2 {
		t.Fatalf("roots after the system bypass: %d, want u1 and u1b", kids[""])
	}
	// The tip's ancestry, walked.
	var chain []string
	for cur := m["u2"]; cur != nil; {
		chain = append(chain, cur.ID)
		next, ok := m[cur.ParentID]
		if !ok {
			break
		}
		cur = next
	}
	if strings.Join(chain, ">") != "u2>a1c>u1" {
		t.Fatalf("ancestry %v", chain)
	}
}

func TestWebUIToolsUnwrapAndPair(t *testing.T) {
	imp := fixture(t, "webui_tools.jsonl")
	m := byIDOf(imp)
	if imp.Tip != "a3" {
		t.Fatalf("tip %q, want a3 (currNode need not be the last line)", imp.Tip)
	}
	// The string-encoded call list (upstream's serialization) and the nested OpenAI shape.
	calls := m["a1"].ToolCalls
	if len(calls) != 1 || calls[0].ID != "callZ" || calls[0].Name != "get_datetime" || string(calls[0].Args) != "{}" {
		t.Fatalf("string-form toolCalls did not unwrap: %+v", calls)
	}
	t1 := m["t1"]
	if t1.Kind != ir.KindTool || t1.ToolCallID != "callZ" {
		t.Fatalf("tool row did not pair: kind %q call id %q", t1.Kind, t1.ToolCallID)
	}
	if calls := m["a2"].ToolCalls; len(calls) != 1 || calls[0].ID != "callY" {
		t.Fatalf("array-form toolCalls did not read: %+v", calls)
	}
	// The denial is prose in this dialect and stays prose in ours — no invented flags.
	if m["t2"].Content != "Tool execution was denied by the user." || m["t2"].IsError {
		t.Fatalf("denial row changed: %+v", m["t2"])
	}
	// A call with no id gets one, so a later tool row can still pair against it.
	last := m["a3"].ToolCalls
	if len(last) != 2 {
		t.Fatalf("a3 calls %d, want 2", len(last))
	}
	if last[0].ID != "callW" {
		t.Fatalf("named call id lost: %q", last[0].ID)
	}
	if !strings.HasPrefix(last[1].ID, "imported-") || last[1].Name != "get_datetime" {
		t.Fatalf("the id-less call did not get a fresh one: %+v", last[1])
	}
}

func TestWebUIImageBecomesAPart(t *testing.T) {
	imp := fixture(t, "webui_image.jsonl")
	m := byIDOf(imp)
	i0 := m["i0"]
	if len(i0.Parts) != 1 {
		t.Fatalf("%d parts, want the image alone (unknown extras stay out of v1): %+v", len(i0.Parts), i0.Parts)
	}
	p := i0.Parts[0]
	if p.Type != ir.PartImage || p.MimeType != "image/png" || p.Name != "px.png" {
		t.Fatalf("part shape: %+v", p)
	}
	if !strings.HasPrefix(p.Data, "iVBORw0KGgo") || strings.HasPrefix(p.Data, "data:") {
		t.Fatalf("data should be bare base64: %.20s", p.Data)
	}
}

// A row skipped mid-tree must not orphan its children: the edge climbs to the nearest
// kept ancestor.
func TestWebUIUnknownRoleIsBypassedNotBroken(t *testing.T) {
	file := `{"type":"session","harness":"llama.app","id":"c","name":"x"}` + "\n" +
		`{"type":"message","message":{"id":"u1","type":"message","role":"user","content":"a","timestamp":1}}` + "\n" +
		`{"type":"message","message":{"id":"x1","parent":"u1","type":"message","role":"banana","content":"no kind for this","timestamp":2}}` + "\n" +
		`{"type":"message","message":{"id":"u2","parent":"x1","type":"message","role":"user","content":"c","timestamp":3}}` + "\n"
	stream, kind, err := Sniff(strings.NewReader(file))
	if err != nil || kind != KindWebUI {
		t.Fatalf("sniff: %v", err)
	}
	imp, err := DecodeWebUI(stream)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := byIDOf(imp)
	if len(imp.Entries) != 2 {
		t.Fatalf("%d entries, want 2 (the banana row is skipped)", len(imp.Entries))
	}
	if m["u2"].ParentID != "u1" {
		t.Fatalf("orphaned grandchild: parent %q, want u1 (bypassed through the skip)", m["u2"].ParentID)
	}
	if imp.Tip != "u2" {
		t.Fatalf("tip %q, want the last message (no currNode in this file)", imp.Tip)
	}
}

func TestWebUIEmptyFileRefused(t *testing.T) {
	file := `{"type":"session","harness":"llama.app","id":"c","name":"empty"}` + "\n" +
		`{"type":"message","message":{"id":"r","type":"root","role":"system","content":"only a prompt"}}` + "\n"
	if _, err := DecodeWebUI(strings.NewReader(file)); err == nil {
		t.Fatal("a file whose every row was skipped is not an import")
	}
}
