// Package xfer is the exchange format for transcripts: bareloop's own line format, plus
// readers for foreign dialects.
//
// The native format is deliberately not a new format. Line 1 is a header — who wrote the
// file, which tip the story stands on, whether the body is the whole tree or one path, and
// the session config minus its secrets. Every line after is one ir.Entry marshalled
// exactly as the transcript endpoint answers it: an exported file and a paged GET speak the
// same JSON, so anything that reads one reads the other, and jq works both. Entry ids are
// kept as the source carries them — an import restores a history, and only a fork reissues
// ids (a merge that renamed its rows could not be recognised as the same conversations).
//
// Compression lives outside the format: the file either starts with a gzip or zstd magic
// or it does not, and readers sniff rather than being told. The zstd here is the ordinary
// frame format, not the store's zdict-compressed payloads — an exported transcript is
// plaintext JSON and owes nothing to any dictionary.
package xfer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"bareloop/internal/agent"
	"bareloop/internal/ir"
)

const (
	Harness = "bareloop"
	// FormatV1 is the only format this build writes, and the only body format it reads
	// natively. A header naming a newer format is an error, not a shrug: silently
	// importing entries a future build added fields to would rewrite a history through a
	// parser that cannot have seen them coming.
	FormatV1 = "bareloop/1"

	ScopeFull = "full"
	ScopePath = "path"
	// ScopeCtx marks the window export: only the entries the next request would carry
	// (the active summary plus everything from its boundary on), so a session that
	// compacted a thousand entries away can be resumed elsewhere without them.
	ScopeCtx = "ctx"
)

// The import gates. Exports are unbounded by design — one's own transcript streamed out is
// never the dangerous direction — but imports read untrusted bytes, so the compressed body
// is capped and the decompressed stream is capped again, separately: a 50 MB zstd bomb
// unfolds to gigabytes if nobody stands in the doorway. Both sides are streams end to end
// (reader → decompressor → parser → one store transaction), so the caps bound what is ever
// buffered. Vars, not consts, because dialing them down in a test is the only honest way
// to test a 512 MB doorframe.
var (
	MaxCompressedBody int64 = 50 << 20
	MaxDecompressed   int64 = 512 << 20
)

// ErrTooLarge names the limit a refused import hit; the route turns it into a 413.
var ErrTooLarge = errors.New("xfer: stream exceeds the import limit")

// Header is line 1 of a native export.
type Header struct {
	Harness string  `json:"harness"`
	Format  string  `json:"format"`
	Tip     string  `json:"tip"`
	Scope   string  `json:"scope"`
	Session Session `json:"session"`
}

// Session is the config side of an export. The id is provenance, not a command: importing
// never reuses it (the caller picks the new session's id, as at create).
type Session struct {
	ID     string        `json:"id,omitempty"`
	Name   string        `json:"name,omitempty"`
	Config *agent.Config `json:"config,omitempty"`
}

// Encode writes the header and the entries, one JSON object per line. Entries arrive in
// parent-before-child order (ir.TreeOrder or one path); the writer trusts that order and
// the importer re-verifies what matters.
func Encode(w io.Writer, hdr Header, entries []*ir.Entry) error {
	enc := json.NewEncoder(w)
	hdr.Harness, hdr.Format = Harness, FormatV1
	if err := enc.Encode(hdr); err != nil {
		return err
	}
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// ScrubbedConfig is the config as it goes into a file: the API key is stripped because an
// export is an artifact that outlives the session and travels, and the request headers go
// with it because an Authorization header is an API key wearing a hat. What is left is
// everything needed to resume the conversation — prompts, models, tools, policies — and
// the operator's own key stays on the machine that holds it.
func ScrubbedConfig(cfg agent.Config) *agent.Config {
	c := cfg
	c.LLM.APIKey = ""
	c.LLM.Headers = nil
	return &c
}

// Codecs an export may carry. "" and "none" are the honest plaintext; zstd is the
// default because these files are one-time-writers' compression gold (JSONL deflates by
// an order of magnitude) and every reader this build ships sniffs all three.
const (
	CodecNone = "none"
	CodecGzip = "gzip"
	CodecZstd = "zstd"
)

// CompressWriter wraps w in the named codec's stream writer and hands back the closer
// that flushes it — closing is the writer's part of a complete file, and a caller who
// forgets it delivers a truncated one, hence the pairing rather than a bare writer.
func CompressWriter(codec string, w io.Writer) (io.Writer, io.Closer, error) {
	switch codec {
	case CodecNone, "":
		return w, nil, nil
	case CodecGzip, "gz":
		z := gzip.NewWriter(w)
		return z, z, nil
	case CodecZstd, "zst":
		z, err := zstd.NewWriter(w)
		if err != nil {
			return nil, nil, err
		}
		return z, z, nil
	default:
		return nil, nil, fmt.Errorf("xfer: unknown codec %q (none, gzip, zstd)", codec)
	}
}

// Uncompress sniffs the magic bytes and returns a reader over the plaintext, capped at
// MaxDecompressed, plus a closer to run when done (nil for plaintext — nothing was opened).
// No magic means plaintext, which still gets the cap: the cap is about what an import may
// cost, not about who claimed to be compressed.
func Uncompress(r io.Reader) (io.Reader, io.Closer, error) {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		z, err := gzip.NewReader(br)
		if err != nil {
			return nil, nil, fmt.Errorf("xfer: gzip header: %w", err)
		}
		return &capReader{r: z, left: MaxDecompressed}, z, nil
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		z, err := zstd.NewReader(br)
		if err != nil {
			return nil, nil, fmt.Errorf("xfer: zstd header: %w", err)
		}
		// The zstd decoder returns its buffer to a pool on a Close that cannot fail;
		// the closer contract here wants an error to hand back, and hands back nil.
		return &capReader{r: z, left: MaxDecompressed}, closer{z}, nil
	default:
		return &capReader{r: br, left: MaxDecompressed}, nil, nil
	}
}

// closer adapts the zstd decoder's Close-without-error to io.Closer.
type closer struct{ d interface{ Close() } }

func (c closer) Close() error { c.d.Close(); return nil }

// capReader passes MaxDecompressed bytes and calls the limit real after that.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// A caller reading past the cap is about to see EOF-shaped garbage anyway;
		// saying what happened beats letting a truncated JSON line say it sideways.
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// Kinds Sniff answers.
const (
	KindNative = "native"
	KindWebUI  = "webui"
)

// maxHeaderLine bounds line 1: every real header is a few kilobytes; a line bigger than
// this is not a header at all, and reading it whole is what a hostile file is for.
const maxHeaderLine = 1 << 20

// Sniff answers who wrote this stream by reading its first line, and hands back a reader
// starting at that same line. The probe is a json.Unmarshal, not a substring: key order
// across writers is a coin toss and spaces are free.
func Sniff(r io.Reader) (io.Reader, string, error) {
	br := bufio.NewReaderSize(r, 4096)
	line, err := br.ReadSlice('\n')
	if err != nil && err != io.EOF {
		// A read that failed for a reason (over the body cap first among them) is the
		// verdict itself — dressing it up as "unrecognized file" would send a 413-sized
		// problem out wearing a 400's face.
		return nil, "", err
	}
	if len(line) == 0 {
		return nil, "", errors.New("xfer: empty file")
	}
	if len(line) >= maxHeaderLine {
		return nil, "", errors.New("xfer: first line is not a header (too large)")
	}
	var probe struct {
		Harness string `json:"harness"`
		Type    string `json:"type"`
	}
	// A first line that is not json at all falls through to the verdict below, which is
	// where the sentence naming what this build can read lives.
	_ = json.Unmarshal(line, &probe)
	switch {
	case probe.Harness == Harness:
		return io.MultiReader(strings.NewReader(string(line)), br), KindNative, nil
	case probe.Type == "session":
		return io.MultiReader(strings.NewReader(string(line)), br), KindWebUI, nil
	default:
		return nil, "", fmt.Errorf("xfer: unrecognized transcript file: the first line names neither %q nor a webui session header", Harness)
	}
}

// Import is what a parsed stream hands the route: entries in parent-before-child order,
// the tip to stand on, and the config side. Config is nil unless the file carried one;
// foreign dialects fill System instead of a Config when all they have is a root prompt.
type Import struct {
	Name    string
	System  string
	Config  *agent.Config
	Tip     string
	Entries []*ir.Entry
}

// DecodeNative reads a native stream. Sniff has already vouched for line 1; one decoder
// then walks header and entries in order, which is also the file's writing order.
func DecodeNative(r io.Reader) (*Import, error) {
	dec := json.NewDecoder(r)
	var hdr Header
	if err := dec.Decode(&hdr); err != nil {
		return nil, fmt.Errorf("xfer: header: %w", err)
	}
	if hdr.Harness != Harness {
		return nil, fmt.Errorf("xfer: not a %s export (harness %q)", Harness, hdr.Harness)
	}
	if hdr.Format != FormatV1 {
		return nil, fmt.Errorf("xfer: format %q is not %s", hdr.Format, FormatV1)
	}
	entries, err := decodeEntries(dec)
	if err != nil {
		return nil, err
	}
	tip := hdr.Tip
	if tip == "" {
		tip = entries[len(entries)-1].ID
	}
	if _, ok := find(entries, tip); !ok {
		return nil, fmt.Errorf("xfer: header tip %q is not among the entries", tip)
	}
	return &Import{Name: hdr.Session.Name, Config: hdr.Session.Config, Tip: tip, Entries: entries}, nil
}

// decodeEntries reads entry objects to EOF, refusing what an import must not carry:
// a missing id, a duplicate id (two entries claiming one row), a parent the file never
// carries (a path nobody can walk), an empty body.
func decodeEntries(dec *json.Decoder) ([]*ir.Entry, error) {
	var entries []*ir.Entry
	seen := map[string]bool{}
	for {
		var e ir.Entry
		err := dec.Decode(&e)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("xfer: entry %d: %w", len(entries)+2, err)
		}
		if e.ID == "" {
			return nil, fmt.Errorf("xfer: entry %d carries no id", len(entries)+2)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("xfer: duplicate entry id %q", e.ID)
		}
		seen[e.ID] = true
		entries = append(entries, &e)
	}
	if len(entries) == 0 {
		return nil, errors.New("xfer: no entries after the header")
	}
	for _, e := range entries {
		if e.ParentID != "" && !seen[e.ParentID] {
			return nil, fmt.Errorf("xfer: entry %q names parent %q that the file never carries", e.ID, e.ParentID)
		}
	}
	return entries, nil
}

func find(entries []*ir.Entry, id string) (*ir.Entry, bool) {
	for _, e := range entries {
		if e.ID == id {
			return e, true
		}
	}
	return nil, false
}
