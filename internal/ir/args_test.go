package ir

import "testing"

// The 2026-11-13 qwen3.8 incident is the origin case: a repeated optional field made the
// whole assistant entry refuse to commit ("duplicate object member name" on the strict
// re-marshal), while every reader of those arguments — the tool run, the UI — had already
// collapsed it last-wins. Storage was the lone dissenting reader; CanonicalArgs conforms
// ingest to the verdict, and these tests pin the verdict down.

func TestCanonicalArgsDedupesLastWins(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "the incident",
			in:   `{"path": "p.md", "offset": 2648, "length": 28, "show_line": false, "mode": "line", "show_line": false}`,
			want: `{"path":"p.md","offset":2648,"length":28,"show_line":false,"mode":"line"}`,
		},
		{
			name: "last value wins, first position holds",
			in:   `{"a":1,"b":2,"a":3}`,
			want: `{"a":3,"b":2}`,
		},
		{
			name: "nested objects and array elements ride the same rule",
			in:   `{"a":{"x":1,"x":2},"b":[{"y":1,"y":2},{"z":1}]}`,
			want: `{"a":{"x":2},"b":[{"y":2},{"z":1}]}`,
		},
		{
			name: "values are kept as written — literal survives",
			in:   `{"n":1.50,"big":12345678901234567890,"s":"a{b}c","t":true,"u":null,"dup":1,"dup":2}`,
			want: `{"n":1.50,"big":12345678901234567890,"s":"a{b}c","t":true,"u":null,"dup":2}`,
		},
		{
			name: "a string that looks like duplicates is not duplicates",
			in:   `{"a":"{\"x\":1,\"x\":2}","b":1,"b":2}`,
			want: `{"a":"{\"x\":1,\"x\":2}","b":2}`,
		},
		{
			name: "a unicode key survives the collapse unescaped",
			in:   `{"kéy":1,"kéy":2}`,
			want: `{"kéy":2}`,
		},
	} {
		out, deduped := CanonicalArgs([]byte(tc.in))
		if !deduped {
			t.Errorf("%s: CanonicalArgs(%s) reported no rewrite", tc.name, tc.in)
			continue
		}
		if got := string(out); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
		if _, again := CanonicalArgs(out); again {
			t.Errorf("%s: rewrite is not idempotent: %s", tc.name, out)
		}
	}
}

func TestCanonicalArgsKeepsHealthyBytesUnmoved(t *testing.T) {
	// A healthy session's arguments must leave this function byte for byte, or the fix would
	// pay for the rare slip by moving the KV prefix of every call forever.
	for _, raw := range []string{
		`{}`,
		`{"path":"a.md","offset":2648,"length":28,"show_line":false,"mode":"line"}`,
		`{"a":{"x":[1,2,{"y":"z"}]},"b":1e2,"c":1.50,"d":null}`,
		`{"a":"{\"x\":1,\"x\":2}"}`,
		`{"a":[{"y":1},{"y":2}]}`,
	} {
		out, deduped := CanonicalArgs([]byte(raw))
		if deduped || string(out) != raw {
			t.Errorf("CanonicalArgs(%s) = (%s, %v), want unchanged", raw, out, deduped)
		}
	}
}

func TestCanonicalArgsLeavesWhatItCannotImprove(t *testing.T) {
	// Unparseable arguments are completeArgs' territory (internal/agent/loop.go), and a
	// non-object top level is no duplicate's home. Either way: the input, untouched.
	for _, raw := range []string{
		`{"a":1`,
		`{"a":1,"a":2} trailing`,
		`[{"x":1,"x":2}]`,
		`null`,
		`"str"`,
		``,
	} {
		out, deduped := CanonicalArgs([]byte(raw))
		if deduped || string(out) != raw {
			t.Errorf("CanonicalArgs(%s) = (%s, %v), want untouched", raw, out, deduped)
		}
	}
}
