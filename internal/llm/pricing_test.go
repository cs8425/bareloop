package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// Pricing is the one piece of endpoint config that must change nothing about a request, so the two
// properties worth locking down are: it survives a round trip through config.json exactly, and it
// never makes two otherwise identical endpoints look different.

func TestPricingRoundTrip(t *testing.T) {
	var c Config
	body := `{"baseUrl":"http://x/v1","pricing":{"input":0.15,"cachedInput":0.0375,"output":1.2}}`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Pricing == nil {
		t.Fatal("pricing vanished")
	}
	if want := (Pricing{Input: 0.15, CachedInput: 0.0375, Output: 1.2}); *c.Pricing != want {
		t.Fatalf("got %+v, want %+v", *c.Pricing, want)
	}

	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// All three numbers are written even when they are the interesting ones, so a person reading
	// config.json is never left guessing what an omitted price meant.
	for _, want := range []string{`"input":0.15`, `"cachedInput":0.0375`, `"output":1.2`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s missing from %s", want, out)
		}
	}
}

func TestPricingAbsentIsNotZero(t *testing.T) {
	out, err := json.Marshal(Config{BaseURL: "http://x/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "pricing") {
		t.Fatalf("a session that never talked about money must not grow a pricing block: %s", out)
	}

	// "free" is an answer, not an absence: an explicit block of zeroes stays a block on the way back.
	var free Config
	if err := json.Unmarshal([]byte(`{"pricing":{}}`), &free); err != nil {
		t.Fatal(err)
	}
	if free.Pricing == nil {
		t.Fatal(`"pricing":{} must decode to a pricing block of zeroes, not to nothing`)
	}
}

func TestPricingValidate(t *testing.T) {
	var none *Pricing
	if err := none.Validate(); err != nil {
		t.Fatalf("no pricing is not an error: %v", err)
	}
	if err := (&Pricing{}).Validate(); err != nil {
		t.Fatalf("free is a valid answer: %v", err)
	}
	for _, p := range []Pricing{{Input: -1}, {CachedInput: -0.5}, {Output: -0.001}} {
		if err := p.Validate(); err == nil {
			t.Fatalf("a negative price was accepted: %+v", p)
		}
	}
}

func TestPricingIsNotPartOfEndpointIdentity(t *testing.T) {
	a := Config{BaseURL: "http://x/v1", Model: "m"}
	b := a
	b.Pricing = &Pricing{Input: 3, CachedInput: 1, Output: 12}
	if !a.Same(b) {
		t.Fatal("a price tag changes no request, so it must not be a reason to rebuild a provider")
	}
	// The fields that do change a request still do.
	c := a
	c.BaseURL = "http://y/v1"
	if a.Same(c) {
		t.Fatal("a different endpoint must not read as the same")
	}
}
