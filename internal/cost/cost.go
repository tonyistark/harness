// Package cost turns token usage into dollars using the signed catalog's
// prices. The vendor's own total_cost_usd is kept beside it so the two can be
// reconciled; both are estimates until matched against the bill.
package cost

import (
	"github.com/gpn/gpn/internal/driver"
	"github.com/gpn/gpn/internal/policy"
)

// Compute prices usage with the catalog entry. Cache reads are billed as
// input tokens at the catalog's input price; cache writes likewise (routes
// price these differently, which is one reason reconciliation exists).
func Compute(u driver.Usage, e policy.CatalogEntry) float64 {
	in := float64(u.InputTokens+u.CacheReadTokens+u.CacheCreationTokens) / 1_000_000 * e.PriceInPerMTok
	out := float64(u.OutputTokens) / 1_000_000 * e.PriceOutPerMTok
	return in + out
}
