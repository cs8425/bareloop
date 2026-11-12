package compact

// Shared fixtures for the compact tests: entries of a chosen size, a payload head and tool array,
// and the readers that turn a SummaryCall back into something a failure can point at.

import (
	"strconv"
	"strings"

	"bareloop/internal/asm"
	"bareloop/internal/ir"
	"bareloop/internal/llm"
)

func txt(id, role string, chars int) *ir.Entry {
	return &ir.Entry{ID: id, Kind: role, Role: role, Content: strings.Repeat("abcd ", chars/5)}
}

func ids(es []*ir.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func region(n int) []*ir.Entry {
	out := make([]*ir.Entry, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, txt(string(rune('a'+i)), role, 500))
	}
	return out
}

func chainParams() Params {
	return Params{ThresholdPct: 70, SummaryCapPct: 20, Temperature: 0.2, Marker: DefaultMarker}
}

func testHead() []asm.Message {
	return []asm.Message{{Role: "system", Content: "You are a test assistant. Be brief."}}
}

func testTools() []llm.Tool {
	return []llm.Tool{
		{Name: "fs_read", Description: "read a file"},
		{Name: "fs_grep", Description: "search files"},
	}
}

func textOf(m asm.Message) string {
	s, _ := m.Content.(string)
	return s
}

func names(ts []llm.Tool) string {
	out := make([]string, 0, len(ts))
	for _, tl := range ts {
		out = append(out, tl.Name)
	}
	return strings.Join(out, ",")
}

// dump is for the one failure worth reading: which message of the call is which.
func dump(msgs []asm.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		b.WriteString("\n  " + strconv.Itoa(i) + " " + m.Role + " " + clip(textOf(m), 90))
	}
	return b.String()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
