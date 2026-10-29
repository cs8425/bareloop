package agent

import (
	"testing"

	"bareloop/internal/llm"
)

// A price tag belongs to the endpoint, so a new session inherits the daemon's — and "the session says
// otherwise" includes "this one is free", which is the reason the config carries a pointer to a struct
// instead of a struct of numbers where zero and unset are the same thing.
func TestPricingInheritsFromTheDaemon(t *testing.T) {
	price := &llm.Pricing{Input: 0.15, CachedInput: 0.0375, Output: 1.2}
	m := NewManager(t.TempDir(), false, Config{LLM: llm.Config{Pricing: price}}, nil, nil)

	if got := m.MergeDefaults(Config{ID: "s"}); got.LLM.Pricing != price {
		t.Fatalf("a session that says nothing about money must inherit the daemon's, got %+v", got.LLM.Pricing)
	}

	free := &llm.Pricing{}
	got := m.MergeDefaults(Config{ID: "s", LLM: llm.Config{Pricing: free}})
	if got.LLM.Pricing != free {
		t.Fatalf("an explicit price of zero is an answer and must win over the default, got %+v", got.LLM.Pricing)
	}
}
