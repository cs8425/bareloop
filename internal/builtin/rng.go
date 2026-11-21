package builtin

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// rng is the first builtin whose subject is the model rather than the loop (the package header
// describes the others as tools that act on the moment a turn waits for and the clock a model may ask
// about). It belongs here for the same reason they do: nothing in the world is touched, no permission
// is needed, and the one thing it supplies — entropy the model cannot fake or predict — is something
// only the daemon has.
//
// why it exists at all: asked to name a random number, a model is not random. It reaches for 7 and 42,
// avoids repeating a digit it just used, keeps lengths near the middle, and produces two "random"
// values that are far more alike than two draws should be. Every one of those is a bias in the thing
// the call was supposed to remove, so the bias has to be bought from outside the model.
//
// The entropy source is deliberately the opposite of wait's. A random answer is written into the log,
// so nothing ever has to recompute it: it may as well be genuinely unpredictable (crypto/rand). A wait
// is the reverse — the moment it promised has to be recomputable by whoever answers it after a
// restart, which is why that one draws from the call's own id instead (see Deadline).

// rngAlphabet is the only character set a random string is drawn from. It is fixed rather than an
// argument for two reasons: a value that can never contain a comma or a newline is what lets the
// answer be a bare list, and "hex" as an argument is ambiguous (three characters, or sixteen?).
// Digits-only is `type:"int"` with a range; anything more exotic belongs to an MCP server, which is
// where format and the world live.
const rngAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

const (
	// rngMaxCount caps one call's draw. The cap is about bytes, not luck: the answer is going into
	// the next request, so an unbounded count is a way for one tool call to cost a kilobyte.
	rngMaxCount = 200
	// rngMaxLen caps one string's length, for the same reason and with the same number in mind:
	// random characters tokenize badly (no subword matches them), so length is the expensive axis.
	rngMaxLen = 64
	// rngFloatDecimals is fixed rather than an argument. Two decimals is what money looks like and
	// six is what a probability looks like, and a knob that applies to one type out of three is the
	// kind of schema line a caller gets wrong. Needing cents? Draw an int in cents.
	rngFloatDecimals = 3
	// rngIntRangeLimit is the honest boundary of "an integer a JSON number carries exactly". Beyond
	// 2^53 the value in the request is not the value the caller meant, so a draw is refused rather
	// than silently rounded — the same reason the tool exists.
	rngIntRangeLimit = float64(1 << 53)
	// rngIntDefaultHi is the top of an int draw whose caller named no max. 1000 rather than 100
	// (2026-11-21, the person who owns this): a bounded-by-hand call says what it means, and the bare
	// {"type":"int"} is the "any number at all" case, which a hundred is too small to answer.
	rngIntDefaultHi = 1000
)

// rngSource is where the bytes come from: a variable so a test can hand it a fixed stream, while
// production keeps the real one, because this answer's job is to be unpredictable.
var rngSource io.Reader = crand.Reader

func rngTool() Tool {
	return Tool{
		Name: "rng",
		Description: "Draw random values from the daemon rather than inventing them, because a model asked to be random is not. " +
			"type says what to draw: int, float or string. min and max are inclusive and apply to int and float " +
			"(defaults: int 1..1000, float 0..1); length applies to string only (1..64, default 8, characters from a-zA-Z0-9); " +
			"count draws that many in one call rather than making several ones (1..200). " +
			"int and float answers are separated by commas, string answers one per line.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","enum":["int","float","string"],"description":"What to draw. int and float read min/max; string reads length."},"min":{"type":"number","description":"Lowest value to draw, inclusive. Default 1 for int, 0 for float. Ignored by string."},"max":{"type":"number","description":"Highest value to draw, inclusive. Default 1000 for int, 1 for float. Ignored by string."},"length":{"type":"integer","description":"Characters per string, 1..64. Default 8. Ignored by int and float."},"count":{"type":"integer","description":"How many values this one call draws, 1..200. Default 1."}},"required":["type"],"additionalProperties":false}`),
		Run:         runRng,
	}
}

// rngArgs keeps every bound as a pointer, which is the only way to tell "the caller said 0" from "the
// caller said nothing" — and 0 is a value anybody drawing an int or a float will actually use.
type rngArgs struct {
	Type   *string  `json:"type"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Length *int     `json:"length"`
	Count  *int     `json:"count"`
}

// runRng answers here rather than handing anything to the daemon: a draw costs microseconds, so there
// is no wait worth parking, and the error return is always nil (see Input.MaxInLoopSleep for the tool
// that does have something to hand over).
//
// A bound that belongs to another type is ignored rather than refused: additionalProperties:false
// cannot catch it (the key is known), and spending a turn to tell the model that `length` meant
// nothing to its `int` is a poor use of a tool whose whole point is to save detours.
func runRng(_ context.Context, _ Input, raw json.RawMessage) (Result, error) {
	var q rngArgs
	if err := decodeArgs(raw, &q); err != nil {
		return ErrText("rng: " + err.Error()), nil
	}
	kind := strings.ToLower(argText(q.Type))
	switch kind {
	case "int", "float", "string":
	case "":
		return ErrText(`rng needs a type: "int", "float" or "string"`), nil
	default:
		return ErrText(`rng: unknown type "` + kind + `" (int, float, string)`), nil
	}
	count, err := rngCount(q.Count)
	if err != nil {
		return ErrText("rng: " + err.Error()), nil
	}
	switch kind {
	case "string":
		return rngStrings(count, q.Length)
	case "float":
		return rngFloats(count, q.Min, q.Max)
	default:
		return rngInts(count, q.Min, q.Max)
	}
}

// rngCount reads the one argument every type shares.
func rngCount(p *int) (int, error) {
	if p == nil {
		return 1, nil
	}
	if *p < 1 || *p > rngMaxCount {
		return 0, fmt.Errorf("count %d is outside 1..%d (the answer goes into the next request, so a long draw costs bytes)", *p, rngMaxCount)
	}
	return *p, nil
}

// rngIntBounds resolves the two number bounds. Each one is defaulted on its own, because "any int from
// 50 upward" is a request about the floor and not about the ceiling, and losing the other half of the
// question to a required pair would only push the caller to draw twice. The unbounded default is 1..1000:
// a hundred is what "pick a number" feels like, but it is also a range a model can exhaust in three calls
// and start looking rehearsed.
func rngIntBounds(min, max *float64) (lo, hi int64, err error) {
	lo, hi = 1, rngIntDefaultHi
	if min != nil {
		if lo, err = rngWhole(*min, "min"); err != nil {
			return 0, 0, err
		}
	}
	if max != nil {
		if hi, err = rngWhole(*max, "max"); err != nil {
			return 0, 0, err
		}
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("max %d is below min %d", hi, lo)
	}
	return lo, hi, nil
}

// rngWhole takes an integer bound only where it really is one, and only where a JSON number still
// carries it exactly.
func rngWhole(f float64, what string) (int64, error) {
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("%s %s is not a whole number (int draws whole values; use type float for a fraction)", what, strconv.FormatFloat(f, 'g', -1, 64))
	}
	if f < -rngIntRangeLimit || f > rngIntRangeLimit {
		return 0, fmt.Errorf("%s %s is past what a JSON number carries exactly (2^53)", what, strconv.FormatFloat(f, 'g', -1, 64))
	}
	return int64(f), nil
}

func rngInts(count int, min, max *float64) (Result, error) {
	lo, hi, err := rngIntBounds(min, max)
	if err != nil {
		return ErrText("rng: " + err.Error()), nil
	}
	span := uint64(hi-lo) + 1 // inclusive on both ends, so the count of values is width+1
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		v, err := rngUint(span)
		if err != nil {
			return ErrText("rng: " + err.Error()), nil
		}
		out = append(out, strconv.FormatInt(lo+int64(v), 10))
	}
	return Result{Text: strings.Join(out, ", ")}, nil
}

func rngFloats(count int, min, max *float64) (Result, error) {
	lo, hi := 0.0, 1.0
	if min != nil {
		lo = *min
	}
	if max != nil {
		hi = *max
	}
	if hi < lo {
		return ErrText(fmt.Sprintf("rng: max %s is below min %s",
			strconv.FormatFloat(hi, 'g', -1, 64), strconv.FormatFloat(lo, 'g', -1, 64))), nil
	}
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		f, err := rngUniform()
		if err != nil {
			return ErrText("rng: " + err.Error()), nil
		}
		v := lo + f*(hi-lo)
		// The rounding is a presentation choice with a hard edge: a draw that lands within half a
		// thousandth of the top still prints as hi, which is inside a range that includes hi anyway.
		v = math.Round(v*math.Pow10(rngFloatDecimals)) / math.Pow10(rngFloatDecimals)
		out = append(out, strconv.FormatFloat(v, 'f', rngFloatDecimals, 64))
	}
	return Result{Text: strings.Join(out, ", ")}, nil
}

func rngStrings(count int, length *int) (Result, error) {
	n := 8
	if length != nil {
		n = *length
		if n < 1 || n > rngMaxLen {
			return ErrText(fmt.Sprintf("rng: length %d is outside 1..%d", n, rngMaxLen)), nil
		}
	}
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var b strings.Builder
		b.Grow(n)
		for j := 0; j < n; j++ {
			k, err := rngUint(uint64(len(rngAlphabet)))
			if err != nil {
				return ErrText("rng: " + err.Error()), nil
			}
			b.WriteByte(rngAlphabet[k])
		}
		out = append(out, b.String())
	}
	// One per line, not comma-separated: these are values somebody copies out one at a time, and a
	// line break is the cheapest thing that makes "where does this one end" unaskable.
	return Result{Text: strings.Join(out, "\n")}, nil
}

// rngUint draws a uniform value in [0, n) from the entropy pool, rejecting what does not land inside
// the range. Expected iterations: at most two.
func rngUint(n uint64) (uint64, error) {
	var buf [8]byte
	for {
		if _, err := io.ReadFull(rngSource, buf[:]); err != nil {
			return 0, err
		}
		if v, ok := rngScale(binary.LittleEndian.Uint64(buf[:]), n); ok {
			return v, nil
		}
	}
}

// rngScale maps one uniform 64-bit value into [0, n): keep it when its low bits land inside the range,
// throw it away and take another when they do not.
//
// The rejection is the whole point of doing it this way. Reading a byte and taking it modulo 62 is
// cheaper, and it is biased: 62 does not divide 256, so the first eight characters get five byte values
// to the last fifty-four's four — a quarter more often, which is an embarrassing thing to ship in the
// one tool whose job is to be free of bias. Rejecting the overage instead costs one extra read every
// couple of draws and stays uniform for any n, not only for the alphabet sizes that happen to divide
// nicely. It is shared rather than duplicated because wait draws from a seeded stream instead of the
// pool, and the two of them must not be able to disagree about what "uniform" means.
func rngScale(v, n uint64) (uint64, bool) {
	if n <= 1 {
		return 0, true
	}
	mask := uint64(1)<<bits.Len64(n-1) - 1
	if x := v & mask; x < n {
		return x, true
	}
	return 0, false
}

// rngUniform builds a float in [0,1) out of 53 random bits rather than dividing a 64-bit draw,
// because 2^53 is exactly the point where a float64 still has room for every bit it is given.
func rngUniform() (float64, error) {
	v, err := rngUint(1 << 53)
	if err != nil {
		return 0, err
	}
	return float64(v) / float64(1<<53), nil
}
