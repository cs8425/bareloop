package agent

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"bareloop/internal/asm"
	"bareloop/internal/llm"
	"bareloop/internal/store"
)

// This file is the byte side of the size table: what an entry costs on the wire (refSizes feeding
// asm's Wire column) and what the request around the entries costs (envelopeBytes). Neither number
// can be counted from tokens, which is the lesson of a 413: a server with a body ceiling refuses the
// request before it has read a token of it, so every prompt count and every token estimate in the
// house is silent about it. Bytes are the only witness, and the two numbers here are the honest
// version of them — one from the store's own size column, the other from the encoder that writes the
// body.

// refSizes is the size table's view of the blob table: a sha in, its raw byte count out.
//
// The answer comes from the column the write path fills (store.Log.BlobSize), so no image bytes are
// ever moved to learn how big they are, and it is cached for the life of the opened session because
// a blob is named by the bytes it holds: its size is a fact about the universe rather than a fact
// about a row, so there is nothing to invalidate. Without the cache a window holding twenty images
// asks twenty primary-key reads every time anything builds a payload — inspect, preview, compact —
// for a number that cannot change.
//
// A sha the store has never heard of is not an error and not zero bytes pretended to be free: it is
// a payload that cannot be sent, and ExpandMessages is the code that says so when the bytes are owed.
// The size table prices it at nothing and says no more.
type refSizes struct {
	log *store.Log

	mu    sync.Mutex
	sizes map[string]int // nil until the first reference is asked about
}

func newRefSizes(l *store.Log) *refSizes { return &refSizes{log: l} }

// Size implements asm.RefSizes.
func (r *refSizes) Size(sha string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.sizes[sha]; ok {
		return n, n > 0
	}
	n, found, err := r.log.BlobSize(sha)
	if err != nil || !found {
		// Not cached: a blob can arrive later (an import lands bytes for a sha that was only ever
		// referenced here), and a failed read says nothing about the next one.
		return 0, false
	}
	if r.sizes == nil {
		r.sizes = map[string]int{}
	}
	r.sizes[sha] = n
	return n, true
}

// envelopeCache holds one session's measurement of the part of a request that is nobody's message.
type envelopeCache struct {
	mu sync.Mutex
	fp string // what the number below was measured for
	n  int    // 0 means "never measured"; no request body is 0 bytes, so it doubles as unset
}

// envelopeBytes is what one of this session's requests costs before a single conversation message is
// put in it: the model name, the sampling fields, the whole tool array with its schemas, the system
// prompt, and one empty user turn.
//
// It is measured by the transport (llm.BodySizer) rather than counted here on purpose. The number it
// joins is asm.Sizes.WireTotal — message payloads summed by length, with no JSON punctuation in it at
// all — so this half has to be exact where the messages are counted approximately, or the sum is two
// estimates arguing. asm's column cannot include the punctuation, because an entry's payload is the
// same text whether it rides alone or beside forty others; the envelope can be exact, because there
// is exactly one of it and the code that writes it is right there.
//
// Cached on the fingerprint of the only things that move it. A conversation growing by a hundred
// turns does not touch this number; enabling a tool, editing a schema or changing the thinking level
// does. A provider that cannot measure itself — every test double — gives 0, and callers must read 0
// as "no idea", never as "free".
//
// system is the head that will actually be sent — the assembler's `built.System`, not the config
// field it started from — because this number joins the sums of a payload that carries the former.
// Passing the config's own prompt would measure a body nobody sends whenever an asm block is in
// front of it.
func (s *Session) envelopeBytes(system string, tools []llm.Tool) int {
	sizer, ok := s.provider.(llm.BodySizer)
	if !ok {
		return 0
	}
	cfg := s.Config()
	req := requestIdentity(cfg)
	fp := envelopeFingerprint(req, system, tools)

	c := s.env
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n > 0 && c.fp == fp {
		return c.n
	}
	req.Tools = tools
	req.Stream = true // the conversation sends streams, and stream_options is on the body too
	req.Messages = []asm.Message{{Role: "system", Content: system}}
	n, err := sizer.EnvelopeBytes(req)
	if err != nil || n <= 0 {
		return 0
	}
	c.fp, c.n = fp, n
	return n
}

// envelopeFingerprint is a cheap identity for the envelope: the system prompt in full, the identity
// fields read off requestIdentity's own result, and per tool its name plus the lengths of its
// description and schema (a schema edit changes the wire without changing a name).
//
// It is not a hash of a serialization: measuring the body is the thing this cache exists to avoid,
// and serializing one to fingerprint it would pay for the meal to save the napkin. The cost of the
// shortcut is honest and small: if requestIdentity grows a field that changes the body, this list has
// to grow with it, and forgetting costs a stale byte estimate — never a wrong request.
func envelopeFingerprint(req llm.Request, system string, tools []llm.Tool) string {
	temp := "-"
	if req.Temperature != nil {
		temp = strconv.FormatFloat(*req.Temperature, 'g', -1, 64)
	}
	var b strings.Builder
	b.WriteString(system)
	fmt.Fprintf(&b, "\n%s|%s|t=%s|m=%d|x=%v", req.Model, req.ReasoningEffort, temp, req.MaxTokens, req.Extra)
	for _, t := range tools {
		fmt.Fprintf(&b, "\n%s:%d:%d", t.Name, len(t.Description), len(t.Parameters))
	}
	return b.String()
}
