package tui

import (
	"math"
	"testing"

	"github.com/nihaal-prasad/oline/internal/event"
)

// TestCostCachedBilling checks that prompt tokens served from the cache are
// billed at the cache-read rate, not the full input rate.
func TestCostCachedBilling(t *testing.T) {
	m := testModel(t)
	const id = "accounts/fireworks/models/deepseek-v4p1-flash" // input 0.30, output 1.2, cacheRead 0.006

	m.apply(event.Usage{
		Model:              id,
		PromptTokens:       1_000_000,
		CachedPromptTokens: 800_000,
		CompletionTokens:   500_000,
	})

	// 200k uncached @0.30/M + 800k cached @0.006/M + 500k out @1.2/M
	want := 0.2*0.30 + 0.8*0.006 + 0.5*1.2
	if math.Abs(m.totalCost-want) > 1e-9 {
		t.Errorf("totalCost = %v, want %v (cached tokens must use the cache-read rate)", m.totalCost, want)
	}
}

// TestCostNoCacheData falls back to the input rate when the provider reports no
// cached tokens.
func TestCostNoCacheData(t *testing.T) {
	m := testModel(t)
	const id = "accounts/fireworks/models/deepseek-v4p1-flash"
	m.apply(event.Usage{Model: id, PromptTokens: 1_000_000, CompletionTokens: 0})
	want := 1.0 * 0.30
	if math.Abs(m.totalCost-want) > 1e-9 {
		t.Errorf("totalCost = %v, want %v", m.totalCost, want)
	}
}

// TestCostUnknownModel costs nothing (rather than panicking) for an unknown id.
func TestCostUnknownModel(t *testing.T) {
	m := testModel(t)
	m.apply(event.Usage{Model: "nope/unknown", PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	if m.totalCost != 0 {
		t.Errorf("totalCost = %v, want 0 for an unknown model", m.totalCost)
	}
}
