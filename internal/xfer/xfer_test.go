package xfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
)

func sampleTree() []*ir.Entry {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	b := "a1"
	return []*ir.Entry{
		{ID: "a1", Kind: ir.KindUser, Role: "user", Content: "remember the list", Ts: t0},
		{ID: "a2", ParentID: "a1", Kind: ir.KindAssistant, Role: "assistant", Content: "noted", Model: "gemma-test",
			Usage: &ir.Usage{PromptTokens: 10, CacheTokens: 4, CompletionTokens: 3, HasPrompt: true, HasCompletion: true}, Ts: t0},
		{ID: "a3", ParentID: "a2", Kind: ir.KindUser, Role: "user", Content: "say it again", Ts: t0},
		{ID: "a4", ParentID: "a3", Kind: ir.KindAssistant, Role: "assistant", Content: "the list, first take",
			ToolCalls: []ir.ToolCall{{ID: "c1", Name: "get_datetime", Args: json.RawMessage(`{}`)}}, Ts: t0},
		{ID: "a5", ParentID: "a3", Kind: ir.KindAssistant, Role: "assistant", Content: "the list, redone", Ts: t0},
		{ID: "a6", ParentID: "a4", Kind: ir.KindCompaction, Role: "user", Content: "[Conversation summary]",
			Compaction: &ir.CompactionMeta{BoundaryID: &b, Replaced: 1}, Ts: t0},
	}
}

func encode(t *testing.T, codec string, hdr Header, entries []*ir.Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, closer, err := CompressWriter(codec, &buf)
	if err != nil {
		t.Fatalf("compress writer %q: %v", codec, err)
	}
	if err := Encode(w, hdr, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if closer != nil {
		if err := closer.Close(); err != nil {
			t.Fatalf("close %s stream: %v", codec, err)
		}
	}
	return buf.Bytes()
}

// The round trip is the format's contract: everything the entry answered with comes back,
// ids unmoved, and every codec reads as well as the plaintext.
func TestNativeRoundTripEveryCodec(t *testing.T) {
	hdr := Header{Tip: "a5", Scope: ScopeFull, Session: Session{ID: "src", Name: "src name"}}
	for _, codec := range []string{"none", "gzip", "zstd", ""} {
		t.Run(codec, func(t *testing.T) {
			raw := encode(t, codec, hdr, sampleTree())
			if codec != "none" && codec != "" {
				if len(raw) < 40 || bytes.HasPrefix(raw, []byte(`{"harness"`)) {
					t.Fatalf("codec %q produced plaintext anyway", codec)
				}
			}
			plain, closer, err := Uncompress(bytes.NewReader(raw))
			if closer != nil {
				defer closer.Close()
			}
			if err != nil {
				t.Fatalf("uncompress: %v", err)
			}
			stream, kind, err := Sniff(plain)
			if err != nil {
				t.Fatalf("sniff: %v", err)
			}
			if kind != KindNative {
				t.Fatalf("sniffed %q, want native", kind)
			}
			imp, err := DecodeNative(stream)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if imp.Tip != "a5" || len(imp.Entries) != 6 {
				t.Fatalf("tip %q entries %d", imp.Tip, len(imp.Entries))
			}
			byID := map[string]*ir.Entry{}
			for _, e := range imp.Entries {
				byID[e.ID] = e
			}
			// The interesting fields ride along, not just the count.
			if u := byID["a2"].Usage; u == nil || u.PromptTokens != 10 || u.CacheTokens != 4 || !u.HasPrompt || !u.HasCompletion {
				t.Fatalf("usage did not survive: %+v", u)
			}
			tc := byID["a4"].ToolCalls
			if len(tc) != 1 || tc[0].ID != "c1" || tc[0].Name != "get_datetime" || string(tc[0].Args) != "{}" {
				t.Fatalf("tool call did not survive: %+v", tc)
			}
			c := byID["a6"].Compaction
			if c == nil || c.BoundaryID == nil || *c.BoundaryID != "a1" || c.Replaced != 1 {
				t.Fatalf("compaction meta did not survive: %+v", c)
			}
			if byID["a5"].ParentID != "a3" || byID["a6"].ParentID != "a4" {
				t.Fatalf("edges did not survive the sibling tree")
			}
			if imp.Name != "src name" {
				t.Fatalf("session name lost: %q", imp.Name)
			}
		})
	}
}

// An entry exported and re-exported must produce byte-identical lines: "re-export equals
// GET" is what makes jq against either file the same exercise, and the round trip below
// is where a formatter that reorders or respaces numbers would be caught.
func TestReEncodeIsByteIdentical(t *testing.T) {
	first := encode(t, "none", Header{Tip: "a1", Scope: ScopePath}, sampleTree()[:2])
	imp, err := DecodeNative(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	second := encode(t, "none", Header{Tip: imp.Tip, Scope: ScopePath}, imp.Entries)
	if !bytes.Equal(first, second) {
		t.Fatalf("re-export moved bytes:\nfirst  %s\nsecond %s", first, second)
	}
}

func TestScrubbedConfigKeepsSecretsHome(t *testing.T) {
	cfg := agent.Config{Name: "keep me"}
	cfg.LLM.APIKey = "sk-secret"
	cfg.LLM.Headers = map[string]string{"Authorization": "Bearer also-secret"}
	cfg.LLM.Model = "gemma-test"
	s := ScrubbedConfig(cfg)
	if s.LLM.APIKey != "" || s.LLM.Headers != nil {
		t.Fatalf("secrets survived the scrub: %q %v", s.LLM.APIKey, s.LLM.Headers)
	}
	if s.LLM.Model != "gemma-test" || s.Name != "keep me" {
		t.Fatalf("the scrub took things it should have left alone: %+v", s.LLM)
	}
	if cfg.LLM.APIKey != "sk-secret" {
		t.Fatalf("the scrub mutated the live config")
	}
}

// The rejection list: everything that would land a broken tree in the store is refused
// before the first row is written.
func TestDecodeRefusesBrokenFiles(t *testing.T) {
	header := `{"harness":"bareloop","format":"bareloop/1","tip":"x2","scope":"full","session":{}}` + "\n"
	cases := []struct {
		name, body, want string
	}{
		{"no entries", header, "no entries"},
		{"missing id", header + `{"kind":"user","role":"user"}` + "\n", "carries no id"},
		{"duplicate id", header + `{"id":"x1","kind":"user","role":"user"}` + "\n" + `{"id":"x1","kind":"user","role":"user"}` + "\n", "duplicate"},
		{"dangling parent", header + `{"id":"x1","parentId":"ghost","kind":"user","role":"user"}` + "\n", "never carries"},
		{"tip not present", header + `{"id":"x1","kind":"user","role":"user"}` + "\n", "not among the entries"},
		{"broken json", header + `{"id":"x1"` + "\n", ""}, // any parse error sentence will do
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeNative(bytes.NewReader([]byte(c.body)))
			if err == nil {
				t.Fatalf("want an error saying %q, got none", c.want)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not say %q", err, c.want)
			}
		})
	}
}

// A newer format is a refusal, not a shrug.
func TestNewerFormatIsRefused(t *testing.T) {
	body := `{"harness":"bareloop","format":"bareloop/2","tip":"x1","scope":"full","session":{}}` + "\n" +
		`{"id":"x1","kind":"user","role":"user"}` + "\n"
	_, err := DecodeNative(bytes.NewReader([]byte(body)))
	if err == nil || !strings.Contains(err.Error(), "bareloop/2") {
		t.Fatalf("want the format refusal, got %v", err)
	}
}

// The header lands wherever the tip says; without one the last line stands.
func TestTipDefaultsToLastEntry(t *testing.T) {
	body := `{"harness":"bareloop","format":"bareloop/1","scope":"path","session":{}}` + "\n" +
		`{"id":"x1","kind":"user","role":"user"}` + "\n" + `{"id":"x2","parentId":"x1","kind":"user","role":"user"}` + "\n"
	imp, err := DecodeNative(bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if imp.Tip != "x2" {
		t.Fatalf("tip %q, want x2", imp.Tip)
	}
}

// The decompression gate says so out loud instead of truncating into a parse error.
func TestDecompressedCapRefusesLouderThanTruncation(t *testing.T) {
	old := MaxDecompressed
	MaxDecompressed = 48
	defer func() { MaxDecompressed = old }()

	big := bytes.Repeat([]byte("filler-line-abcdefgh\n"), 40)
	for _, codec := range []string{"none", "gzip", "zstd"} {
		var buf bytes.Buffer
		w, closer, err := CompressWriter(codec, &buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(big); err != nil {
			t.Fatal(err)
		}
		if closer != nil {
			closer.Close()
		}
		plain, c, err := Uncompress(bytes.NewReader(buf.Bytes()))
		if c != nil {
			defer c.Close()
		}
		if err != nil {
			t.Fatalf("uncompress %s: %v", codec, err)
		}
		_, err = io.ReadAll(plain)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("codec %s: got %v, want ErrTooLarge", codec, err)
		}
	}
}

// Sniff decides by content: the webui header is the other dialect, and anything else is
// named as unreadable rather than half-read.
func TestSniffNamesStrangers(t *testing.T) {
	_, kind, err := Sniff(bytes.NewReader([]byte(`{"type":"session","harness":"llama.app","currNode":"m1"}` + "\n")))
	if err != nil || kind != KindWebUI {
		t.Fatalf("webui header sniffed as %q (%v)", kind, err)
	}
	_, _, err = Sniff(bytes.NewReader([]byte("not json at all\n")))
	if err == nil {
		t.Fatal("junk should not sniff as anything readable")
	}
	_, _, err = Sniff(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("an empty file is not a transcript")
	}
}

// webArgs unwraps exactly the layers upstream carries and nothing else.
func TestWebArgs(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"{}"`, `{}`},
		{`"{\"a\": 1}"`, `{"a": 1}`},
		{`{"b":[1,2]}`, `{"b":[1,2]}`}, // already-unwrapped dialect passes through
		{`"plain prose"`, `{}`},        // not JSON under the string: {} and let the tool complain
		{`{"broken`, `{}`},             // invalid on both layers
	}
	for _, c := range cases {
		got := string(webArgs(json.RawMessage(c.in)))
		if got != c.want {
			t.Fatalf("webArgs(%s) = %s, want %s", c.in, got, c.want)
		}
		if !json.Valid([]byte(got)) {
			t.Fatalf("webArgs(%s) produced invalid JSON: %s", c.in, got)
		}
	}
}
