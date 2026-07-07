package ingest

import "testing"

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
		1000, 1000, 0, 0, datedTestTable())

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
		1000, 1000, 0, 0, table)

	if cost == nil || source != "computed" {
		t.Fatalf("cost=%v source=%q, want computed", cost, source)
	}
	// Exact entry rates: 1000*2.0/1e6 + 1000*10.0/1e6 = 0.012, not 0.006.
	if *cost < 0.011999 || *cost > 0.012001 {
		t.Errorf("cost = %v, want ~0.012 from the exact dated entry", *cost)
	}
}

func TestResolveCostDatedIDWithoutBaseEntryStaysUnknown(t *testing.T) {
	cost, source := ResolveCost(nil, "claude-mystery-9-20260101",
		1000, 1000, 0, 0, datedTestTable())

	if cost != nil || source != "" {
		t.Errorf("cost=%v source=%q, want nil/empty for unknown base", cost, source)
	}
}

func TestResolveCostNonDateSuffixIsNotStripped(t *testing.T) {
	for _, model := range []string{
		"claude-haiku-4-5-preview",   // non-numeric suffix
		"claude-haiku-4-5-2025",      // too few digits
		"claude-haiku-4-5-202510012", // too many digits
		"-20251001",                  // empty base
	} {
		cost, source := ResolveCost(nil, model, 1000, 1000, 0, 0, datedTestTable())
		if cost != nil || source != "" {
			t.Errorf("model %q: cost=%v source=%q, want nil/empty (no fallback)", model, cost, source)
		}
	}
}
