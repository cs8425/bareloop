package asm

// A tool's structuredContent is the one field in a payload whose size is chosen by somebody else:
// an MCP server that answers a file read with 10 MB of JSON decides what fraction of the model
// window this session has left. The fold in structured.go is where that field becomes message text,
// so it is also the only place a ceiling on it can go — one function, every mode, and nothing about
// the log changes: the entry keeps the whole document, which is what makes the ceiling reversible
// and (phase two) readable again.
//
// Two properties this file exists to keep:
//
//   - The cut is a prefix. A prefix of the same JSON is byte-identical across every rebuild of this
//     session, so a capped result costs the KV cache nothing after its first turn — which is the
//     reason for cutting the tail off rather than keeping head and tail the way compact.ClipEntry
//     does for a message that has to be summarised.
//   - The model is told. A truncation the model cannot see gets answered with an invention, so the
//     note below says what was shown, how much there was, how much is missing, and that the
//     fragment will not parse. Same lesson imageNote learned the hard way.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultStructuredMaxBytes is the byte ceiling on what a tool's structuredContent puts in front of
// the model when a session does not name a number of its own.
//
// 16 384 bytes: sixteen kibibytes, a power of two on the same scale a context window is quoted in,
// and four thousand tokens on the one ruler this package estimates with (four bytes per token). A
// document past that is already beyond what a model reasons over reliably — it reads a beginning and
// guesses at the rest — and a run whose servers answer that way a handful of times has spent a
// mid-sized window on raw data before anybody's question was answered.
const DefaultStructuredMaxBytes = 16384

// ValidateStructuredMax is the one door a configured ceiling goes through, so Create and
// UpdateConfig cannot disagree about what is storable. Zero is not a size but "unset": the default.
//
// There is deliberately no floor. A ceiling is a request to send less, a small one is a legitimate
// answer about a chatty server, and a number someone typed too small is visible the moment a tool
// result comes back thin — one PATCH to undo. Only the value that would mean the opposite of what it
// says is refused: a negative is what gets written for "no limit", and here it would quietly mean
// the default instead.
func ValidateStructuredMax(n int) error {
	if n < 0 {
		return fmt.Errorf("asm: structuredMaxBytes counts bytes and is never negative: got %d; 0 means the default %d, any larger number is the ceiling", n, DefaultStructuredMaxBytes)
	}
	return nil
}

// structuredField is StructuredMarker with its brackets trimmed. The marker is what introduces the
// document on the wire and the note is what apologises for the rest of it, so the two have to name the
// same thing — and since the note opens a bracket pair of its own, interpolating the marker into those
// brackets is how a model ends up reading "[[structuredContent] truncated:" where one word was meant.
var structuredField = strings.Trim(StructuredMarker, "[]")

// truncationNote is what a cut-off structuredContent says about itself, in the same brackets as the
// marker that introduced it, so the model reads the two as one statement about the same block. The
// sentence is quoted in README and pinned word for word by TestNoteReadsAsTheDocumentedSentence: every
// other test here compares the wire against this function, so rewording it keeps them green.
func truncationNote(kept, total int) string {
	return fmt.Sprintf("\n["+structuredField+" truncated: the first %d bytes of a %d-byte document are shown above, and it is missing its last %d bytes. What is above is cut off mid-document and will not parse; answer only from what is shown, and say what you cannot see.]",
		kept, total, total-kept)
}

// structuredNoteDigits is the widest any of the note's byte counts can be written: an int peaks at
// 9223372036854775807, which is nineteen characters.
const structuredNoteDigits = 19

// structuredNoteBytes is the most that note can ever add. It cannot be measured from one call, because
// the note carries three counts — shown, total, missing — and they are never widest together: measuring
// with (1<<62, 1<<62) reports the missing part as "0", one character where nineteen can go. So the
// allowance is the note's own wording plus the most each of its three counts can grow to (a 19-digit head
// of a 20-digit document is a reachable, 19-digit remainder). Guessing this number, or measuring it from
// the wrong example, is how an estimate ends up under-counting the very thing it bounds, and
// TestTruncationNoteStaysInsideItsCeiling fails if the wording ever outgrows it.
var structuredNoteBytes = len(truncationNote(0, 0)) + 3*(structuredNoteDigits-1)

// structuredFoldBytes is the label foldStructured writes between the prose and the JSON. The
// estimate counts it too: a bound that forgets the two newlines and nineteen characters of the
// marker is a bound that is quietly under the truth.
var structuredFoldBytes = len("\n" + StructuredMarker + "\n")

// structuredWireBytes is what a tool result's structuredContent can cost on the wire under a
// ceiling: the label, the document when it fits or its head when it does not, and the note when it
// was cut. Estimate uses it, and that is the whole reason the ceiling has to be a number a caller
// can pass — an estimate that still counted a capped 10 MB document at 10 MB would refuse to send a
// session whose payload is now sixteen thousand bytes, which is the opposite failure and just as
// fatal.
func structuredWireBytes(n, maxBytes int) int {
	if n <= 0 {
		return 0
	}
	if maxBytes <= 0 {
		maxBytes = DefaultStructuredMaxBytes
	}
	if n <= maxBytes {
		return n + structuredFoldBytes
	}
	return maxBytes + structuredFoldBytes + structuredNoteBytes
}

// headWithin returns the first n bytes of s, backed off to a boundary where no UTF-8 rune is split.
//
// Cutting at an arbitrary byte can leave half a multi-byte character at the end of the fragment:
// the model then reads mojibake where a value used to be, and a strict provider may reject the
// request over invalid UTF-8 — a far worse outcome than four fewer bytes of JSON.
func headWithin(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateStructured returns the text a tool's raw structuredContent is shown as under a ceiling
// (maxBytes <= 0 means the default). Which mode put the JSON on the wire is decided in
// foldStructured; the ceiling is the same question under every one of them.
func truncateStructured(raw string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultStructuredMaxBytes
	}
	if len(raw) <= maxBytes {
		return raw // fits: no cut, no note, and the payload is exactly what it was before this file
	}
	head := headWithin(raw, maxBytes)
	return head + truncationNote(len(head), len(raw))
}
