package pricing

import (
	"math"
	"testing"

	"github.com/since2006/go-dock/host/internal/config"
)

func TestLookupLongestPrefix(t *testing.T) {
	tb := New(nil)
	cases := map[string]float64{
		"claude-opus-5-5":          4,
		"claude-opus-5":            5,
		"claude-opus-4-8[1m]":      5,
		"claude-opus-4-20250514":   15,
		"claude-sonnet-4-5-202509": 3,
		"claude-fable-5-1":         10,
	}
	for model, in := range cases {
		p, ok := tb.Lookup(model)
		if !ok || p.Input != in {
			t.Errorf("%s: got %+v ok=%v, want input %v", model, p, ok, in)
		}
	}
	if _, ok := tb.Lookup("<synthetic>"); ok {
		t.Error("unknown model matched")
	}
}

func TestCostAndOverride(t *testing.T) {
	tb := New(map[string]config.Price{"claude-opus-5-5": {Input: 1, Output: 1}})
	c, ok := tb.Cost("claude-opus-5-5", Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000})
	if !ok || math.Abs(c-2) > 1e-9 {
		t.Fatalf("override cost = %v", c)
	}
	c, _ = New(nil).Cost("claude-opus-5-5", Usage{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheWrite5m: 1e6, CacheWrite1h: 1e6})
	if math.Abs(c-(4+20+0.2+5+8)) > 1e-9 {
		t.Fatalf("builtin cost = %v", c)
	}
}
