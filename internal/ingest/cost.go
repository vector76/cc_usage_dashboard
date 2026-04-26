// Package ingest provides transcript parsing and data ingestion.
package ingest

import (
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
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

// LoadPriceTable loads the price table from a YAML file.
// Returns an empty table if the file cannot be loaded (non-fatal).
func LoadPriceTable(path string) PriceTable {
	if path == "" {
		slog.Debug("no price table path configured")
		return make(PriceTable)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("failed to read price table file", "path", path, "err", err)
		return make(PriceTable)
	}

	// Parse YAML into a structured format
	type priceConfig struct {
		Models map[string]struct {
			InputRatePerM          float64 `yaml:"input_rate_usd_per_m"`
			OutputRatePerM         float64 `yaml:"output_rate_usd_per_m"`
			CacheCreationRatePerM  float64 `yaml:"cache_creation_rate_usd_per_m"`
			CacheReadRatePerM      float64 `yaml:"cache_read_rate_usd_per_m"`
		} `yaml:"models"`
	}

	var cfg priceConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		slog.Warn("failed to parse price table YAML", "path", path, "err", err)
		return make(PriceTable)
	}

	table := make(PriceTable)
	for modelName, rates := range cfg.Models {
		table[modelName] = &ModelPrices{
			InputRate:         rates.InputRatePerM,
			OutputRate:        rates.OutputRatePerM,
			CacheCreationRate: rates.CacheCreationRatePerM,
			CacheReadRate:     rates.CacheReadRatePerM,
		}
	}

	slog.Debug("loaded price table", "path", path, "models", len(table))
	return table
}
