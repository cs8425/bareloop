package agent

import (
	"context"
	"testing"
	"time"

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

// TestRejectedPatchReleasesTheSession is a deadlock regression test. A patch UpdateConfig refused used
// to return from inside the section that merges with s.mu held, which did more than answer the request:
// it left the mutex held for good, so every later request touching that session — and GET /api/sessions,
// which walks all of them — hung until the process was killed. The watchdog is what turns that into a
// named failure instead of a test binary that times out ten minutes later with no explanation.
func TestRejectedPatchReleasesTheSession(t *testing.T) {
	m := NewManager(t.TempDir(), false, Config{}, nil, nil)
	s, err := m.Create(context.Background(), Config{ID: "reject", LLM: llm.Config{BaseURL: "http://127.0.0.1:1/v1"}})
	if err != nil {
		t.Fatal(err)
	}

	// A negative price is refused...
	if err := s.UpdateConfig(context.Background(), Config{LLM: llm.Config{Pricing: &llm.Pricing{Output: -1}}}); err == nil {
		t.Fatal("a negative price must be refused")
	}
	// ...and the session has to be usable straight afterwards.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if st := s.Status(); st == nil || st.Session != "reject" {
			t.Error("status stopped describing this session")
		}
		if err := s.UpdateConfig(context.Background(), Config{LLM: llm.Config{Pricing: &llm.Pricing{Input: 1}}}); err != nil {
			t.Errorf("a valid patch after a refused one must work: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a refused patch left the session locked: every request for it now hangs")
	}

	if got := s.Config().LLM.Pricing; got == nil || got.Input != 1 {
		t.Fatalf("the patch that was accepted after the refused one did not land: %+v", got)
	}
}
