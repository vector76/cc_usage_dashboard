// Package ingest provides transcript parsing and data ingestion.
package ingest

import (
	"log/slog"
)

// PriceTable holds model pricing information.
type PriceTable map[string]*ModelPrices

// ModelPrices holds pricing for a single model.
type ModelPrices struct {
	InputRate            float64
	OutputRate           float64
	CacheCreationRate    float64
	CacheReadRate        float64
}

// ResolveCost computes the cost of a usage event based on token counts and pricing.
// Returns the cost in USD and the source of the cost (reported, computed, or empty if unknown).
func ResolveCost(
	reportedCost *float64,
	model string,
	inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int,
	priceTable PriceTable,
) (*float64, string) {
	// If cost was reported, use it
	if reportedCost != nil && *reportedCost > 0 {
		return reportedCost, "reported"
	}

	// Try to compute from price table
	if model != "" && priceTable != nil {
		prices, ok := priceTable[model]
		if ok && prices != nil {
			cost := computeCost(inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, prices)
			return &cost, "computed"
		}
	}

	// Unknown model or no price table
	return nil, ""
}

// computeCost computes the cost from tokens and pricing.
func computeCost(inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int, prices *ModelPrices) float64 {
	const millionTokens = 1_000_000.0

	cost := 0.0
	if inputTokens > 0 {
		cost += float64(inputTokens) * prices.InputRate / millionTokens
	}
	if outputTokens > 0 {
		cost += float64(outputTokens) * prices.OutputRate / millionTokens
	}
	if cacheCreationTokens > 0 {
		cost += float64(cacheCreationTokens) * prices.CacheCreationRate / millionTokens
	}
	if cacheReadTokens > 0 {
		cost += float64(cacheReadTokens) * prices.CacheReadRate / millionTokens
	}

	return cost
}

// LoadPriceTable loads the price table from config.
// Returns nil if the table cannot be loaded (non-fatal).
func LoadPriceTable(path string) PriceTable {
	// Placeholder: will be implemented with YAML loading
	// For now, return nil and let the system handle unknown costs
	slog.Debug("price table loading not yet implemented", "path", path)
	return nil
}
