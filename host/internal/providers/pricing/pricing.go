// Package pricing estimates API-equivalent USD cost from token counts.
package pricing

import (
	"strings"

	"github.com/since2006/go-dock/host/internal/config"
)

// builtin holds Anthropic first-party list prices in USD per million
// tokens, keyed by model ID prefix (longest match wins). Cache reads vary by
// model; cache writes are 1.25x input (5m) and 2x input (1h).
var builtin = map[string]config.Price{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite5m: 12.5, CacheWrite1h: 20},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite5m: 12.5, CacheWrite1h: 20},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1, CacheWrite5m: 12.5, CacheWrite1h: 20},
	"claude-mythos-5":   {Input: 10, Output: 50, CacheRead: 1, CacheWrite5m: 12.5, CacheWrite1h: 20},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite5m: 5, CacheWrite1h: 8},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10},
	"claude-opus-4-5":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite5m: 6.25, CacheWrite1h: 10},
	"claude-opus-4":     {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite5m: 18.75, CacheWrite1h: 30},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite5m: 2.5, CacheWrite1h: 4},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite5m: 3.75, CacheWrite1h: 6},
	"claude-3-7-sonnet": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite5m: 3.75, CacheWrite1h: 6},
	"claude-3-5-sonnet": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite5m: 3.75, CacheWrite1h: 6},
	// Haiku 5.5 rate for prompts up to 100K tokens.
	"claude-haiku-5-5": {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite5m: 0.125, CacheWrite1h: 0.2},
	"claude-haiku-4-5": {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite5m: 1.25, CacheWrite1h: 2},
	"claude-3-5-haiku": {Input: 0.8, Output: 4, CacheRead: 0.08, CacheWrite5m: 1, CacheWrite1h: 1.6},
	"claude-3-haiku":   {Input: 0.25, Output: 1.25, CacheRead: 0.03, CacheWrite5m: 0.3, CacheWrite1h: 0.5},
}

// Table looks up prices, with user overrides taking precedence.
type Table struct {
	overrides map[string]config.Price
}

func New(overrides map[string]config.Price) *Table { return &Table{overrides: overrides} }

// Lookup returns the price for model, matching the longest known prefix.
func (t *Table) Lookup(model string) (config.Price, bool) {
	model = strings.ToLower(model)
	if p, ok := longest(t.overrides, model); ok {
		return p, true
	}
	return longest(builtin, model)
}

func longest(m map[string]config.Price, model string) (config.Price, bool) {
	best, found := "", false
	var price config.Price
	for prefix, p := range m {
		if strings.HasPrefix(model, strings.ToLower(prefix)) && len(prefix) > len(best) {
			best, price, found = prefix, p, true
		}
	}
	return price, found
}

// Usage is one response's token counts.
type Usage struct {
	Input, Output, CacheRead, CacheWrite5m, CacheWrite1h int64
}

// Cost returns the USD cost of u under model's price.
func (t *Table) Cost(model string, u Usage) (float64, bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	const m = 1e6
	return float64(u.Input)*p.Input/m +
		float64(u.Output)*p.Output/m +
		float64(u.CacheRead)*p.CacheRead/m +
		float64(u.CacheWrite5m)*p.CacheWrite5m/m +
		float64(u.CacheWrite1h)*p.CacheWrite1h/m, true
}
