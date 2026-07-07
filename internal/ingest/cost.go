// Package ingest provides transcript parsing and data ingestion.
package ingest

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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
//
// A missing file (empty path or os.IsNotExist) is treated as non-fatal: a
// warning is logged and an empty table is returned with a nil error. A
// malformed YAML file returns an empty table together with a wrapped error so
// the caller can surface or fail-fast as appropriate.
func LoadPriceTable(path string) (PriceTable, error) {
	if path == "" {
		slog.Debug("no price table path configured; cost computation disabled")
		return make(PriceTable), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("price table file not found; cost computation disabled", "path", path)
			return make(PriceTable), nil
		}
		return make(PriceTable), fmt.Errorf("read price table %q: %w", path, err)
	}

	return ParsePriceTable(data, path)
}

// ParsePriceTable decodes a YAML price table from raw bytes. source is used
// only to make parse errors locatable (a file path, or a label like
// "embedded default"). A malformed document yields an empty table and a
// wrapped error.
func ParsePriceTable(data []byte, source string) (PriceTable, error) {
	type priceConfig struct {
		Models map[string]struct {
			InputRatePerM         float64 `yaml:"input_rate_usd_per_m"`
			OutputRatePerM        float64 `yaml:"output_rate_usd_per_m"`
			CacheCreationRatePerM float64 `yaml:"cache_creation_rate_usd_per_m"`
			CacheReadRatePerM     float64 `yaml:"cache_read_rate_usd_per_m"`
		} `yaml:"models"`
	}

	var cfg priceConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return make(PriceTable), fmt.Errorf("parse price table %q: %w", source, err)
	}

	table := make(PriceTable, len(cfg.Models))
	for modelName, rates := range cfg.Models {
		table[modelName] = &ModelPrices{
			InputRate:         rates.InputRatePerM,
			OutputRate:        rates.OutputRatePerM,
			CacheCreationRate: rates.CacheCreationRatePerM,
			CacheReadRate:     rates.CacheReadRatePerM,
		}
	}

	slog.Debug("loaded price table", "source", source, "models", len(table))
	return table, nil
}

// ResolvePriceTable selects and loads a price table following a precedence
// chain designed so cost computation always has a usable table:
//
//  1. explicitPath — the config's pricing.table_path. When it is non-empty
//     and the file exists, it is loaded and wins. A malformed file at an
//     explicit path is fatal (returns the wrapped parse error) so a broken
//     override is surfaced rather than silently masked.
//
//     Precedence decision (3a): an explicit path pointing at a MISSING file
//     is NOT fatal and does NOT disable cost computation. A warning is logged
//     and resolution falls through to (2)/(3). Rationale: the whole point of
//     this feature is that a working price table is always available; a stale
//     or mistyped config path should degrade to the built-in default (with a
//     visible warning) instead of leaving costs uncomputed. This is a
//     deliberate change from the older LoadPriceTable behavior, where a
//     missing explicit path yielded an empty table.
//
//  2. searchDirs — the first prices.yaml found in these directories (e.g.
//     next to the executable, then the app's config/data dirs) is loaded.
//     This is the no-rebuild override hook. A malformed override here is also
//     fatal for the same surface-don't-mask reason.
//
//  3. embedded — the built-in default table, parsed from the embedded bytes.
//     This always succeeds for a well-formed embed and is the guaranteed
//     fallback.
//
// The returned string is a human-readable description of the chosen source
// ("embedded default" or a file path) for logging.
func ResolvePriceTable(explicitPath string, searchDirs []string, embedded []byte) (PriceTable, string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err == nil {
			pt, lerr := LoadPriceTable(explicitPath)
			return pt, explicitPath, lerr
		} else if !os.IsNotExist(err) {
			// A stat error other than "not found" (e.g. permission denied)
			// is a genuine problem with an explicitly configured path.
			return make(PriceTable), explicitPath, fmt.Errorf("stat price table %q: %w", explicitPath, err)
		}
		slog.Warn("configured price table not found; falling back to override search and embedded default", "path", explicitPath)
	}

	for _, dir := range searchDirs {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "prices.yaml")
		if _, err := os.Stat(candidate); err == nil {
			pt, lerr := LoadPriceTable(candidate)
			return pt, candidate, lerr
		}
	}

	pt, err := ParsePriceTable(embedded, "embedded default")
	return pt, "embedded default", err
}
