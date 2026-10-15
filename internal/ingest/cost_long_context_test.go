package ingest

import (
	"math"
	"path/filepath"
	"testing"
)

// Haiku 5.5 is priced by prompt length: a request whose prompt (input +
// cache writes + cache reads) is over 100,000 tokens pays every rate from a
// higher tier, output included. These tests pin that rule.

func haiku55Table() PriceTable {
	return PriceTable{
		"claude-haiku-5-5": &ModelPrices{
			InputRate:           0.10,
			OutputRate:          0.50,
			CacheCreationRate:   0.125,
			CacheCreation1hRate: 0.20,
			CacheReadRate:       0.01,
			LongContext: &LongContextPrices{
				ThresholdTokens: 100_000,
				Prices: ModelPrices{
					InputRate:           0.50,
					OutputRate:          2.50,
					CacheCreationRate:   0.625,
					CacheCreation1hRate: 1.00,
					CacheReadRate:       0.05,
				},
			},
		},
	}
}

func assertCost(t *testing.T, got *float64, source string, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("cost is nil (source %q), want %.6f", source, want)
	}
	if source != "computed" {
		t.Errorf("source: want computed, got %q", source)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Errorf("cost: want %.9f, got %.9f", want, *got)
	}
}

func TestLongContextBelowThresholdUsesBaseRates(t *testing.T) {
	// Prompt = 10k input + 40k cache write (all 1h) + 50k cache read = 100,000:
	// exactly at the threshold, which is not "over" it.
	cost, source := ResolveCost(nil, "claude-haiku-5-5", 10_000, 2_000, 40_000, 40_000, 50_000, haiku55Table())
	want := 10_000*0.10/1e6 + 2_000*0.50/1e6 + 40_000*0.20/1e6 + 50_000*0.01/1e6
	assertCost(t, cost, source, want)
}

func TestLongContextOverThresholdUsesLongRatesForEveryKind(t *testing.T) {
	// One token past the threshold, reached mostly through a cache hit: the
	// whole request, output and the cache hit included, pays the long tier.
	cost, source := ResolveCost(nil, "claude-haiku-5-5", 10_001, 2_000, 40_000, 30_000, 50_000, haiku55Table())
	want := 10_001*0.50/1e6 + 2_000*2.50/1e6 + 10_000*0.625/1e6 + 30_000*1.00/1e6 + 50_000*0.05/1e6
	assertCost(t, cost, source, want)
}

func TestLongContextAppliesToDatedSnapshot(t *testing.T) {
	cost, source := ResolveCost(nil, "claude-haiku-5-5-20261001", 200_000, 0, 0, 0, 0, haiku55Table())
	assertCost(t, cost, source, 200_000*0.50/1e6)
}

func TestModelWithoutLongContextIgnoresPromptLength(t *testing.T) {
	table := PriceTable{"claude-opus-5-5": &ModelPrices{InputRate: 4.0, OutputRate: 20.0}}
	cost, source := ResolveCost(nil, "claude-opus-5-5", 900_000, 0, 0, 0, 0, table)
	assertCost(t, cost, source, 900_000*4.0/1e6)
}

func TestCeilingConsidersLongContextRates(t *testing.T) {
	// A table whose only long-tier rate tops every base rate: the ceiling
	// must not understate it.
	table := PriceTable{
		"m": &ModelPrices{
			InputRate: 1, OutputRate: 5,
			LongContext: &LongContextPrices{ThresholdTokens: 10, Prices: ModelPrices{InputRate: 3, OutputRate: 9}},
		},
	}
	c := CeilingPrices(table)
	if c.InputRate != 3 || c.OutputRate != 9 {
		t.Errorf("ceiling: want input 3 / output 9, got %v / %v", c.InputRate, c.OutputRate)
	}
	if c.LongContext != nil {
		t.Errorf("ceiling should be a flat rate set, got a long-context tier")
	}
}

func TestParsePriceTableLongContext(t *testing.T) {
	yamlDoc := []byte(`
models:
  claude-haiku-5-5:
    input_rate_usd_per_m: 0.10
    output_rate_usd_per_m: 0.50
    cache_creation_rate_usd_per_m: 0.125
    cache_creation_1h_rate_usd_per_m: 0.20
    cache_read_rate_usd_per_m: 0.01
    long_context:
      threshold_tokens: 100000
      input_rate_usd_per_m: 0.50
      output_rate_usd_per_m: 2.50
      cache_creation_rate_usd_per_m: 0.625
      cache_read_rate_usd_per_m: 0.05
  claude-sonnet-5-5:
    input_rate_usd_per_m: 2.00
    output_rate_usd_per_m: 10.00
`)
	pt, err := ParsePriceTable(yamlDoc, "test")
	if err != nil {
		t.Fatal(err)
	}
	lc := pt["claude-haiku-5-5"].LongContext
	if lc == nil {
		t.Fatal("long_context tier not loaded")
	}
	want := LongContextPrices{
		ThresholdTokens: 100_000,
		// The 1h key is omitted, so it defaults to 2x the tier's input rate.
		Prices: ModelPrices{InputRate: 0.50, OutputRate: 2.50, CacheCreationRate: 0.625, CacheCreation1hRate: 1.00, CacheReadRate: 0.05},
	}
	if *lc != want {
		t.Errorf("long_context: want %+v, got %+v", want, *lc)
	}
	if pt["claude-sonnet-5-5"].LongContext != nil {
		t.Error("a model without long_context must have a nil tier")
	}
}

func TestParsePriceTableRejectsLongContextWithoutThreshold(t *testing.T) {
	yamlDoc := []byte(`
models:
  m:
    input_rate_usd_per_m: 1
    long_context:
      input_rate_usd_per_m: 2
`)
	if _, err := ParsePriceTable(yamlDoc, "test"); err == nil {
		t.Error("want an error for a long_context tier with no threshold_tokens")
	}
}

func TestShippedTableHaiku55(t *testing.T) {
	pt, err := LoadPriceTable(filepath.Join("..", "..", "prices.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h := pt["claude-haiku-5-5"]
	if h == nil {
		t.Fatal("claude-haiku-5-5 missing from prices.yaml")
	}
	base := ModelPrices{InputRate: 0.10, OutputRate: 0.50, CacheCreationRate: 0.125, CacheCreation1hRate: 0.20, CacheReadRate: 0.01}
	long := LongContextPrices{
		ThresholdTokens: 100_000,
		Prices:          ModelPrices{InputRate: 0.50, OutputRate: 2.50, CacheCreationRate: 0.625, CacheCreation1hRate: 1.00, CacheReadRate: 0.05},
	}
	if h.LongContext == nil || *h.LongContext != long {
		t.Errorf("long tier: want %+v, got %+v", long, h.LongContext)
	}
	flat := *h
	flat.LongContext = nil
	if flat != base {
		t.Errorf("base tier: want %+v, got %+v", base, flat)
	}
}
