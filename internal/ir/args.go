package ir

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
)

// CanonicalArgs rewrites tool-call arguments whose JSON object carries duplicate member
// names, so a reply like
//
//	{"path": "a.md", "show_line": false, "mode": "line", "show_line": false}
//
// — a shape local call heads really do emit when repeating optional schema fields (measured
// on a qwen3.8 session via llama.cpp, 2026-11-13; the fields are this village's own
// fs_read_file's) — stores instead of poisoning the turn. The poison mechanism is asymmetric:
// every consumer that ever reads the arguments collapses duplicates last-wins (JS
// JSON.parse, Python json.load, Go unmarshal into a map — the tool ran with the last value,
// the UI drew the last value), while storing them raw does not — ir.ToolCall.Args re-marshals
// through the strict engine, which refuses to re-emit raw input with duplicate members, and
// the assistant entry is refused whole ("commit assistant entry: … duplicate object member
// name"), losing the reply. One reader dissenting from a verdict every other reader already
// reached is worth conforming to the verdict, not to the letter: last-wins, at ingest, in
// this one place, so nothing downstream has to know duplicates ever existed.
//
// The rewrite keeps the first occurrence's position and the last occurrence's value (what
// every ordered parser displays), leaves every member value's bytes untouched (numbers keep
// the literal the model wrote), and recurses into nested objects and arrays. Arguments that
// are already strict — the overwhelming majority — return the input slice unchanged and
// false, so this cannot move the KV prefix of a healthy session by a byte. Arguments that
// are not parseable at all also return unchanged: a half object is completeArgs' business
// (internal/agent/loop.go), and this function does not change what it cannot improve.
func CanonicalArgs(raw []byte) (out []byte, deduped bool) {
	if strictArgs(raw) {
		return raw, false
	}
	rewritten, ok := rewriteObject(raw)
	if !ok {
		return raw, false
	}
	return rewritten, true
}

// strictArgs is the same test the commit will run — through the same engine, explicitly:
// marshalling the raw bytes through v2 validates RFC 8259 form including unique member
// names, and refuses everything the record would refuse (measured: "cannot marshal from Go
// jsontext.Value: duplicate object member name" at commit). It must be jsonv2 and not the
// v1 API: plain encoding/json does not know jsontext.Value and base64-encodes its bytes by
// reflection — a check that passes everything, which is precisely the silent direction this
// helper must never fail in (the first draft failed exactly that way, and the dedupe tests
// caught it).
func strictArgs(raw []byte) bool {
	_, err := jsonv2.Marshal(jsontext.Value(raw))
	return err == nil
}

// rewriteObject parses raw as one JSON object, keeping members in first-seen order with
// last-seen values, canonicalising every value on the way through; it fails (returning ok
// false) for anything that is not exactly one parseable object.
func rewriteObject(raw []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, false
	}
	type member struct {
		key string
		val json.RawMessage
	}
	var (
		ms    []member
		where = map[string]int{}
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		val, _ = canonicalValue(val) // nested duplicates ride the same rule; keep the bytes otherwise
		if at, seen := where[key]; seen {
			ms[at].val = val // last value wins; first position holds
			continue
		}
		where[key] = len(ms)
		ms = append(ms, member{key, val})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF { // nothing may follow the object
		return nil, false
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(m.key)
		if err != nil {
			return nil, false
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(m.val)
	}
	buf.WriteByte('}')
	out := buf.Bytes()
	if !strictArgs(out) { // belt: the rewrite must satisfy the very test it was written to pass
		return nil, false
	}
	return out, true
}

// canonicalValue rewrites one member value: objects rewrite as objects; arrays are walked
// element-wise, because a call may carry a list of objects and a duplicate inside the third
// element is the same disease; anything else is bytes kept verbatim.
func canonicalValue(v []byte) (json.RawMessage, bool) {
	t := bytes.TrimLeft(v, " \t\r\n")
	if len(t) == 0 {
		return v, false
	}
	switch t[0] {
	case '{':
		if strictArgs(v) {
			return v, false
		}
		out, ok := rewriteObject(v)
		if !ok {
			return v, false
		}
		return out, true
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(v, &elems); err != nil {
			return v, false
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		changed := false
		for i, el := range elems {
			if i > 0 {
				buf.WriteByte(',')
			}
			el, ch := canonicalValue(el)
			changed = changed || ch
			buf.Write(el)
		}
		buf.WriteByte(']')
		if !changed {
			return v, false // the array was clean element-wise: keep the model's bytes whole
		}
		return buf.Bytes(), true
	default:
		return v, false
	}
}
