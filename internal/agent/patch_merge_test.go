package agent

import (
	"context"
	"testing"

	"bareloop/internal/llm"
)

// TestAnUnrelatedPatchKeepsEverySamplingField pins the one rule README.md states for the whole API —
// "the zero value means keep what is stored" — for the sampling family, field by field.
//
// maxTokens broke it in silence: an absent key decoded to 0 with no keep-old line beside MaxTurns, so
// the documented one-liner for setting a provider knob from a terminal
//
//	curl -X PATCH …/api/session/x -d '{"extra":{"top_k":20}}'
//
// also dropped whatever output cap that session had been given. Nothing was reported, because a body
// carrying no key is not an error; the next request merely stopped carrying the ceiling. Hence a test
// that names every field of the family: a sampling field added later should arrive here missing an
// assertion, which is cheaper than discovering it in a bill or in a model that stopped being capped.
// (compact is deliberately absent: that block replaces whole by design, documented separately.)
//
// The second half is the other half of the same field. A plain int could hold "4096" and "not 4096", so
// making silence mean unchanged would have cost the owner the only way back; as a pointer the three
// answers are three spellings, and the test walks all of them.
func TestAnUnrelatedPatchKeepsEverySamplingField(t *testing.T) {
	const base = "http://127.0.0.1:1/v1"
	m := NewManager(t.TempDir(), false, Config{LLM: llm.Config{BaseURL: base}}, nil, nil)
	ctx := context.Background()
	s, err := m.Create(ctx, Config{ID: "smerge", LLM: llm.Config{BaseURL: base}})
	if err != nil {
		t.Fatal(err)
	}
	temp, out := 0.25, 4096
	if err := s.UpdateConfig(ctx, Config{
		MaxTokens: &out, MaxTurns: 7, Temperature: &temp,
		ReasoningEffort: "low", Extra: map[string]any{"top_k": float64(5)},
	}); err != nil {
		t.Fatal(err)
	}
	// Exactly the shape a hand-written request has: the one field it means to change, and silence about
	// everything else.
	if err := s.UpdateConfig(ctx, Config{Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	got := s.Config()
	if got.Name != "renamed" {
		t.Fatalf("the patch did not even land its own field: Name = %q", got.Name)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 4096 {
		t.Fatalf("a patch that never mentioned maxTokens left it at %v: the body's silence was read as an answer", got.MaxTokens)
	}
	if got.MaxTurns != 7 {
		t.Fatalf("maxTurns reset to %d", got.MaxTurns)
	}
	if got.Temperature == nil || *got.Temperature != 0.25 {
		t.Fatalf("temperature reset to %+v", got.Temperature)
	}
	if got.ReasoningEffort != "low" {
		t.Fatalf("reasoningEffort reset to %q", got.ReasoningEffort)
	}
	if got.Extra["top_k"] != float64(5) {
		t.Fatalf("extra reset to %v", got.Extra)
	}

	// Saying 0 out loud is the second state, and it has to survive the round trip through the disk: it is
	// the owner asking for no ceiling of ours, which is why the key is written rather than left out.
	off := 0
	if err := s.UpdateConfig(ctx, Config{MaxTokens: &off}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().MaxTokens; got == nil || *got != 0 {
		t.Fatalf("after a patch carrying maxTokens 0 the config holds %v, want a stored 0 — that patch was an answer, not a shrug", got)
	}
	// And the third state, told apart from the second only in the config: a patch that carries no key keeps
	// the stored 0 rather than treating it as nothing to keep.
	if err := s.UpdateConfig(ctx, Config{Name: "renamed again"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Config().MaxTokens; got == nil || *got != 0 {
		t.Fatalf("an unrelated patch moved maxTokens off the stored 0 (now %v)", got)
	}
	if got := or0(s.Config().MaxTokens); got != 0 {
		t.Fatalf("a stored 0 has to reach the wire as no ceiling, got %d", got)
	}

	// And the fourth thing a body can hold is not an answer at all. A negative is not omitted by the encoder
	// the way 0 is, so accepting one is signing up every later request of this session to explain a
	// provider's arithmetic back to us. Both doors refuse it, and the refused one leaves nothing behind.
	bad := -1
	if err := s.UpdateConfig(ctx, Config{MaxTokens: &bad}); err == nil {
		t.Fatal("a PATCH carrying maxTokens -1 was accepted; that is \"max_tokens\": -1 on every request from here")
	}
	if got := s.Config().MaxTokens; got == nil || *got != 0 {
		t.Fatalf("the refused patch still moved the config (now %v)", got)
	}
	if _, err := m.Create(ctx, Config{ID: "snegcap", LLM: llm.Config{BaseURL: base}, MaxTokens: &bad}); err == nil {
		t.Fatal("Create accepted the same negative through the other door")
	}
}
