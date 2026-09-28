package ingest

import (
	"testing"

	ccusage "github.com/vector76/cc_usage_dashboard"
)

// Dated model ids (claude-haiku-4-5-20251001) must resolve to their undated
// price entry when no exact entry exists — transcripts report dated snapshot
// ids while prices.yaml lists family names, and an exact-only lookup left
// such events permanently unpriced (989 live haiku events).

func datedTestTable() PriceTable {
	return PriceTable{
		"claude-haiku-4-5": &ModelPrices{
			InputRate:         1.0,
			OutputRate:        5.0,
			CacheCreationRate: 1.25,
			CacheReadRate:     0.10,
		},
	}
}

func TestResolveCostDatedIDFallsBackToUndatedEntry(t *testing.T) {
	cost, source := ResolveCost(nil, "claude-haiku-4-5-20251001",
		1000, 1000, 0, 0, 0, datedTestTable())

	if cost == nil {
		t.Fatal("expected cost computed via undated fallback, got nil")
	}
	// 1000*1.0/1e6 + 1000*5.0/1e6 = 0.006
	if *cost < 0.005999 || *cost > 0.006001 {
		t.Errorf("cost = %v, want ~0.006", *cost)
	}
	if source != "computed" {
		t.Errorf("source = %q, want computed", source)
	}
}

func TestResolveCostExactDatedEntryWinsOverFallback(t *testing.T) {
	table := datedTestTable()
	table["claude-haiku-4-5-20251001"] = &ModelPrices{
		InputRate: 2.0, OutputRate: 10.0,
	}

	cost, source := ResolveCost(nil, "claude-haiku-4-5-20251001",
		1000, 1000, 0, 0, 0, table)

	if cost == nil || source != "computed" {
		t.Fatalf("cost=%v source=%q, want computed", cost, source)
	}
	// Exact entry rates: 1000*2.0/1e6 + 1000*10.0/1e6 = 0.012, not 0.006.
	if *cost < 0.011999 || *cost > 0.012001 {
		t.Errorf("cost = %v, want ~0.012 from the exact dated entry", *cost)
	}
}

// A dated id whose base is also absent is not a price-table match. It now
// lands on the ceiling like any other unrecognized model; what matters here is
// that it is *not* reported as "computed", which would claim a real rate lookup
// had happened.
func TestResolveCostDatedIDWithoutBaseEntryIsNotComputed(t *testing.T) {
	cost, source := ResolveCost(nil, "claude-mystery-9-20260101",
		1000, 1000, 0, 0, 0, datedTestTable())

	if source != "ceiling" {
		t.Errorf("cost=%v source=%q, want ceiling for an unknown base", cost, source)
	}
}

func TestResolveCostNonDateSuffixIsNotStripped(t *testing.T) {
	for _, model := range []string{
		"claude-haiku-4-5-preview",   // non-numeric suffix
		"claude-haiku-4-5-2025",      // too few digits
		"claude-haiku-4-5-202510012", // too many digits
		"-20251001",                  // empty base
	} {
		cost, source := ResolveCost(nil, model, 1000, 1000, 0, 0, 0, datedTestTable())
		// The point is that no undated match is fabricated: these must not be
		// priced at haiku's rates. Landing on the ceiling instead is correct.
		if source != "ceiling" {
			t.Errorf("model %q: cost=%v source=%q, want ceiling (no undated fallback)", model, cost, source)
		}
	}
}

// Every dated snapshot id Claude Code emits must price against the embedded
// table at its family's real rate. Sonnet 4 and Opus 4 are keyed by their
// "-0" aliases (claude-sonnet-4-0), so stripping the date alone
// (claude-sonnet-4) missed and those events fell to the ceiling.
func TestResolveCostEmbeddedTablePricesDatedIDs(t *testing.T) {
	pt, err := ParsePriceTable(ccusage.DefaultPriceTableYAML, "embedded default")
	if err != nil {
		t.Fatalf("ParsePriceTable(embedded): %v", err)
	}
	cases := []struct {
		model string
		want  float64 // 1M input tokens at the family's input rate
	}{
		{"claude-sonnet-4-20250514", 3.00},
		{"claude-opus-4-20250514", 15.00},
		{"claude-opus-4-1-20250805", 15.00},
		{"claude-sonnet-4-5-20250929", 3.00},
		{"claude-haiku-4-5-20251001", 1.00},
		{"claude-opus-4-5-20251101", 5.00},
	}
	for _, tc := range cases {
		cost, source := ResolveCost(nil, tc.model, 1_000_000, 0, 0, 0, 0, pt)
		if source != "computed" || cost == nil {
			t.Errorf("%s: cost=%v source=%q, want computed", tc.model, cost, source)
			continue
		}
		if *cost < tc.want-1e-9 || *cost > tc.want+1e-9 {
			t.Errorf("%s: cost=%v, want %v", tc.model, *cost, tc.want)
		}
	}
}

// The "-0" alias fallback applies only after the undated base misses, so a
// family that has its own undated row is never re-routed to a "-0" row.
func TestResolveCostAliasFallbackDoesNotShadowBaseEntry(t *testing.T) {
	table := PriceTable{
		"claude-opus-4":   &ModelPrices{InputRate: 1.0},
		"claude-opus-4-0": &ModelPrices{InputRate: 2.0},
	}
	cost, source := ResolveCost(nil, "claude-opus-4-20250514", 1_000_000, 0, 0, 0, 0, table)
	if source != "computed" || cost == nil || *cost != 1.0 {
		t.Errorf("cost=%v source=%q, want 1.0 computed from the undated base", cost, source)
	}
}

// Bedrock and Vertex spell model ids with a region/vendor prefix, a "-vN:M"
// version suffix, or an "@" date separator. Those forms must resolve to the
// same row as the native id instead of falling to the ceiling.
func TestResolveCostCloudProviderIDs(t *testing.T) {
	pt, err := ParsePriceTable(ccusage.DefaultPriceTableYAML, "embedded default")
	if err != nil {
		t.Fatalf("ParsePriceTable(embedded): %v", err)
	}
	cases := []struct {
		model string
		want  float64 // 1M input tokens at the family's input rate
	}{
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", 3.00},
		{"global.anthropic.claude-sonnet-4-5-20250929-v1:0", 3.00},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", 3.00},
		{"anthropic.claude-haiku-4-5-20251001-v1:0", 1.00},
		{"anthropic.claude-opus-5", 5.00},
		{"us.anthropic.claude-sonnet-4-20250514-v1:0", 3.00},
		{"claude-opus-4-5@20251101", 5.00},
		{"claude-sonnet-4@20250514", 3.00},
		{"claude-opus-5-5", 4.00},
	}
	for _, tc := range cases {
		cost, source := ResolveCost(nil, tc.model, 1_000_000, 0, 0, 0, 0, pt)
		if source != "computed" || cost == nil {
			t.Errorf("%s: cost=%v source=%q, want computed", tc.model, cost, source)
			continue
		}
		if *cost < tc.want-1e-9 || *cost > tc.want+1e-9 {
			t.Errorf("%s: cost=%v, want %v", tc.model, *cost, tc.want)
		}
	}
}

// Only the documented provider spellings are rewritten: a look-alike that is
// not one of them stays unrecognized.
func TestResolveCostProviderNormalizationIsNarrow(t *testing.T) {
	for _, model := range []string{
		"anthropic-claude-haiku-4-5", // dash, not the "anthropic." prefix
		"claude-haiku-4-5@preview",   // "@" not followed by a date
		"claude-haiku-4-5-v1:0",      // version suffix without the Bedrock prefix
		"us.other.claude-haiku-4-5",  // another vendor
	} {
		cost, source := ResolveCost(nil, model, 1000, 1000, 0, 0, 0, datedTestTable())
		if source != "ceiling" {
			t.Errorf("model %q: cost=%v source=%q, want ceiling", model, cost, source)
		}
	}
}
