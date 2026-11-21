// Package ingest provides transcript parsing and data ingestion.
package ingest

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// PriceTable holds model pricing information.
type PriceTable map[string]*ModelPrices

// ModelPrices holds pricing for a single model.
type ModelPrices struct {
	InputRate  float64
	OutputRate float64
	// CacheCreationRate prices 5-minute-TTL cache writes (1.25x input);
	// CacheCreation1hRate prices 1-hour-TTL writes (2x input). Claude Code
	// writes almost exclusively with the 1-hour TTL.
	CacheCreationRate   float64
	CacheCreation1hRate float64
	CacheReadRate       float64
	// LongContext, when set, replaces every rate above for a request whose
	// prompt (input + cache writes + cache reads) is over ThresholdTokens.
	// Claude Haiku 5.5 is priced this way; nil means one flat tier.
	LongContext *LongContextPrices
}

// LongContextPrices is the higher tier a request pays once its prompt is
// over ThresholdTokens. Prices.LongContext is always nil.
type LongContextPrices struct {
	ThresholdTokens int
	Prices          ModelPrices
}

// ResolveCost computes the cost of a usage event based on token counts and pricing.
// Returns the cost in USD and the source of the cost (reported, computed, or empty if unknown).
//
// cacheCreationTokens is the total of all cache writes, matching the usage
// block's cache_creation_input_tokens; cacheCreation1hTokens is the 1-hour-TTL
// subset of it (cache_creation.ephemeral_1h_input_tokens). The remainder is
// priced as 5-minute writes, so a caller that doesn't know the split passes 0
// and gets the old all-5-minute pricing.
//
// Model lookup tries an exact price-table match first; failing that, a dated
// snapshot id like "claude-haiku-4-5-20251001" falls back to its undated
// family entry ("claude-haiku-4-5"), or to its "-0" alias when that is how
// the family is keyed ("claude-sonnet-4-20250514" -> "claude-sonnet-4-0").
// Transcripts report dated ids while the price table lists family names, and
// an exact-only lookup left such events permanently unpriced. An explicit
// dated entry always wins, so a snapshot
// can still be priced differently from its family if that ever happens.
// A Bedrock or Vertex id that matches none of those is retried in its native
// form ("us.anthropic.claude-sonnet-4-5-20250929-v1:0" ->
// "claude-sonnet-4-5-20250929").
func ResolveCost(
	reportedCost *float64,
	model string,
	inputTokens, outputTokens, cacheCreationTokens, cacheCreation1hTokens, cacheReadTokens int,
	priceTable PriceTable,
) (*float64, string) {
	// If cost was reported, use it
	if reportedCost != nil && *reportedCost > 0 {
		return reportedCost, "reported"
	}

	// Try to compute from price table
	if model != "" && priceTable != nil {
		if prices := findPrices(priceTable, model); prices != nil {
			cost := computeCost(inputTokens, outputTokens, cacheCreationTokens, cacheCreation1hTokens, cacheReadTokens, prices)
			return &cost, "computed"
		}

		// Unrecognized model: price it at the table's ceiling rather than
		// leaving it NULL. A NULL cost drops the event out of every dollar
		// total and out of the dashboard's stacked bars, so a newly released
		// model silently vanishes until someone edits prices.yaml — which is
		// exactly what happened when claude-opus-5 arrived. Overstating is the
		// safe direction for a quota signal, and the dashboard renders
		// ceiling-priced events in the gray "other" family so the number is
		// visibly a guess rather than a measurement.
		if ceiling := CeilingPrices(priceTable); ceiling != nil {
			cost := computeCost(inputTokens, outputTokens, cacheCreationTokens, cacheCreation1hTokens, cacheReadTokens, ceiling)
			return &cost, "ceiling"
		}
	}

	// No model name, or no price table to draw a ceiling from.
	return nil, ""
}

// findPrices is the lookup ResolveCost prices with: the id as given (exact,
// dated snapshot, "-0" alias), then its native form if it is a Bedrock or
// Vertex id. Returns nil when the table has no rates for model.
func findPrices(t PriceTable, model string) *ModelPrices {
	if prices := lookupPrices(t, model); prices != nil {
		return prices
	}
	if native := nativeModelID(model); native != model {
		return lookupPrices(t, native)
	}
	return nil
}

// HasPrice reports whether ResolveCost would price model from its own rates
// rather than the table's ceiling.
func HasPrice(t PriceTable, model string) bool {
	return model != "" && t != nil && findPrices(t, model) != nil
}

// MergePriceTables layers tables per model: each model takes its rates from
// the first table that lists it, so earlier tables win and later ones only
// add models. Nil tables are skipped. The result is a new map; the inputs
// are never modified, since the tailer and server hold them as shared
// snapshots.
func MergePriceTables(tables ...PriceTable) PriceTable {
	merged := make(PriceTable)
	for _, t := range tables {
		for model, prices := range t {
			if _, ok := merged[model]; !ok && prices != nil {
				merged[model] = prices
			}
		}
	}
	return merged
}

// CeilingPrices returns the per-rate maximum across every entry in the table:
// the most expensive each token kind can possibly be. Returns nil for an empty
// or nil table, where there is no ceiling to speak of.
//
// The maximum is taken per rate rather than by picking the single priciest
// model's row. In the shipped table the two coincide — claude-opus-4-1 / 4-0
// hold every maximum at 15/75/18.75/1.50 — but a table where one model has the
// highest output rate and another the highest cache-read rate would let the
// row-picking version understate. Cache reads dominate real usage by two orders
// of magnitude (394M cache-read against 1.9M output tokens in a recent week),
// so that rate in particular must not be sampled from whichever model happened
// to win on output.
func CeilingPrices(t PriceTable) *ModelPrices {
	var ceiling ModelPrices
	found := false
	for _, p := range t {
		if p == nil {
			continue
		}
		found = true
		raiseCeiling(&ceiling, p)
		if p.LongContext != nil {
			raiseCeiling(&ceiling, &p.LongContext.Prices)
		}
	}
	if !found {
		return nil
	}
	return &ceiling
}

// raiseCeiling lifts each of ceiling's rates to p's where p's is higher.
func raiseCeiling(ceiling, p *ModelPrices) {
	ceiling.InputRate = max(ceiling.InputRate, p.InputRate)
	ceiling.OutputRate = max(ceiling.OutputRate, p.OutputRate)
	ceiling.CacheCreationRate = max(ceiling.CacheCreationRate, p.CacheCreationRate)
	ceiling.CacheCreation1hRate = max(ceiling.CacheCreation1hRate, p.CacheCreation1hRate)
	ceiling.CacheReadRate = max(ceiling.CacheReadRate, p.CacheReadRate)
}

// datedModelID matches a model id ending in an 8-digit date suffix
// (e.g. "claude-haiku-4-5-20251001") and captures the undated base name.
var datedModelID = regexp.MustCompile(`^(.+)-\d{8}$`)

// undatedModelName strips a trailing -YYYYMMDD snapshot suffix from a model
// id, returning "" when the id has no such suffix.
func undatedModelName(model string) string {
	m := datedModelID.FindStringSubmatch(model)
	if m == nil {
		return ""
	}
	return m[1]
}

// lookupPrices finds model's row: an exact match, else its undated family
// entry, else that family's "-0" alias. Sonnet 4 and Opus 4 are listed under
// their aliases (claude-sonnet-4-0), which the date strip alone never reaches.
func lookupPrices(t PriceTable, model string) *ModelPrices {
	if p := t[model]; p != nil {
		return p
	}
	if base := undatedModelName(model); base != "" {
		if p := t[base]; p != nil {
			return p
		}
		return t[base+"-0"]
	}
	return nil
}

// bedrockModelID matches a Bedrock model id: an optional region prefix, the
// "anthropic." vendor prefix and an optional "-vN:M" version suffix, as in
// "us.anthropic.claude-sonnet-4-5-20250929-v1:0". vertexModelID matches
// Vertex's "@" date separator, as in "claude-opus-4-5@20251101".
var (
	bedrockModelID = regexp.MustCompile(`^(?:[a-z]+\.)?anthropic\.(claude-.+?)(?:-v\d+(?::\d+)?)?$`)
	vertexModelID  = regexp.MustCompile(`^(claude-.+)@(\d{8})$`)
)

// nativeModelID rewrites a Bedrock or Vertex model id into the Anthropic API
// form the price table uses, returning model unchanged for any other id.
func nativeModelID(model string) string {
	if m := bedrockModelID.FindStringSubmatch(model); m != nil {
		model = m[1]
	}
	if m := vertexModelID.FindStringSubmatch(model); m != nil {
		model = m[1] + "-" + m[2]
	}
	return model
}

// computeCost computes the cost from tokens and pricing.
func computeCost(inputTokens, outputTokens, cacheCreationTokens, cacheCreation1hTokens, cacheReadTokens int, prices *ModelPrices) float64 {
	const millionTokens = 1_000_000.0

	// A prompt over the long-context threshold pays the higher tier on every
	// token kind, even when most of the prompt is a cache hit.
	if lc := prices.LongContext; lc != nil &&
		inputTokens+cacheCreationTokens+cacheReadTokens > lc.ThresholdTokens {
		prices = &lc.Prices
	}

	cost := 0.0
	if inputTokens > 0 {
		cost += float64(inputTokens) * prices.InputRate / millionTokens
	}
	if outputTokens > 0 {
		cost += float64(outputTokens) * prices.OutputRate / millionTokens
	}
	// The 1h count is a subset of the total. Clamp the 5m remainder at zero
	// so a malformed split can't subtract dollars.
	if cache5m := cacheCreationTokens - cacheCreation1hTokens; cache5m > 0 {
		cost += float64(cache5m) * prices.CacheCreationRate / millionTokens
	}
	if cacheCreation1hTokens > 0 {
		cost += float64(cacheCreation1hTokens) * prices.CacheCreation1hRate / millionTokens
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

// rateConfig is one tier of rates as prices.yaml spells it.
type rateConfig struct {
	InputRatePerM           float64 `yaml:"input_rate_usd_per_m"`
	OutputRatePerM          float64 `yaml:"output_rate_usd_per_m"`
	CacheCreationRatePerM   float64 `yaml:"cache_creation_rate_usd_per_m"`
	CacheCreation1hRatePerM float64 `yaml:"cache_creation_1h_rate_usd_per_m"`
	CacheReadRatePerM       float64 `yaml:"cache_read_rate_usd_per_m"`
}

func (r rateConfig) toPrices() *ModelPrices {
	// An override file written before the 1h key existed would otherwise
	// price every 1-hour cache write at $0. Anthropic's published rule is
	// 2x base input, so that is the default.
	rate1h := r.CacheCreation1hRatePerM
	if rate1h == 0 {
		rate1h = 2 * r.InputRatePerM
	}
	return &ModelPrices{
		InputRate:           r.InputRatePerM,
		OutputRate:          r.OutputRatePerM,
		CacheCreationRate:   r.CacheCreationRatePerM,
		CacheCreation1hRate: rate1h,
		CacheReadRate:       r.CacheReadRatePerM,
	}
}

// ParsePriceTable decodes a YAML price table from raw bytes. source is used
// only to make parse errors locatable (a file path, or a label like
// "embedded default"). A malformed document yields an empty table and a
// wrapped error.
func ParsePriceTable(data []byte, source string) (PriceTable, error) {
	type priceConfig struct {
		Models map[string]struct {
			rateConfig  `yaml:",inline"`
			LongContext *struct {
				ThresholdTokens int `yaml:"threshold_tokens"`
				rateConfig      `yaml:",inline"`
			} `yaml:"long_context"`
		} `yaml:"models"`
	}

	var cfg priceConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return make(PriceTable), fmt.Errorf("parse price table %q: %w", source, err)
	}

	table := make(PriceTable, len(cfg.Models))
	for modelName, rates := range cfg.Models {
		prices := rates.toPrices()
		if lc := rates.LongContext; lc != nil {
			// A tier with no threshold would apply to every request and
			// silently reprice the model, so reject it outright.
			if lc.ThresholdTokens <= 0 {
				return make(PriceTable), fmt.Errorf("parse price table %q: model %q: long_context needs a positive threshold_tokens", source, modelName)
			}
			prices.LongContext = &LongContextPrices{ThresholdTokens: lc.ThresholdTokens, Prices: *lc.toPrices()}
		}
		table[modelName] = prices
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
// An override from (1) or (2) is layered over the embedded table per model
// (MergePriceTables): it replaces the rates of the models it lists, and every
// model it leaves out keeps its built-in rates rather than dropping to the
// ceiling. So an override file can hold just the one or two models it means
// to change.
//
// The returned string is a human-readable description of the chosen source
// ("embedded default" or a file path) for logging.
func ResolvePriceTable(explicitPath string, searchDirs []string, embedded []byte) (PriceTable, string, error) {
	override, source, err := resolveOverride(explicitPath, searchDirs)
	if err != nil {
		return make(PriceTable), source, err
	}
	pt, err := ParsePriceTable(embedded, "embedded default")
	if err != nil {
		return pt, "embedded default", err
	}
	if override == nil {
		return pt, "embedded default", nil
	}
	return MergePriceTables(override, pt), source, nil
}

// resolveOverride loads the override half of ResolvePriceTable: the explicit
// path if it exists, else the first prices.yaml in searchDirs. A nil table
// with a nil error means there is no override.
func resolveOverride(explicitPath string, searchDirs []string) (PriceTable, string, error) {
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
	return nil, "", nil
}
