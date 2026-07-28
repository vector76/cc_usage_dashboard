package ingest

import "testing"

// An unrecognized model is priced at the table's rate ceiling rather than left
// unpriced. Leaving it NULL dropped the events out of every dollar total and
// out of the dashboard's stacked bars entirely, so a newly released model was
// invisible until someone noticed and edited prices.yaml. Overstating its cost
// is the safe direction: the quota signal errs toward "less headroom than you
// think", and the "other" family renders the guess in gray so it is visibly a
// guess.

// ceilingTestTable spreads the four maxima across different models on purpose,
// so a per-rate ceiling is distinguishable from "copy the priciest model's row".
func ceilingTestTable() PriceTable {
	return PriceTable{
		"claude-sonnet-9": &ModelPrices{
			InputRate:         3.0,
			OutputRate:        15.0,
			CacheCreationRate: 3.75,
			CacheReadRate:     0.30,
		},
		"claude-opus-9": &ModelPrices{
			InputRate:         15.0, // highest input
			OutputRate:        75.0, // highest output
			CacheCreationRate: 1.00,
			CacheReadRate:     0.05,
		},
		"claude-cachey-9": &ModelPrices{
			InputRate:         1.0,
			OutputRate:        2.0,
			CacheCreationRate: 18.75, // highest cache write
			CacheReadRate:     1.50,  // highest cache read
		},
	}
}

func TestCeilingPricesTakesPerRateMaximum(t *testing.T) {
	got := CeilingPrices(ceilingTestTable())
	if got == nil {
		t.Fatal("expected ceiling prices, got nil")
	}
	want := ModelPrices{
		InputRate:         15.0,
		OutputRate:        75.0,
		CacheCreationRate: 18.75,
		CacheReadRate:     1.50,
	}
	if *got != want {
		t.Errorf("ceiling = %+v, want %+v", *got, want)
	}
}

func TestCeilingPricesEmptyTable(t *testing.T) {
	if got := CeilingPrices(PriceTable{}); got != nil {
		t.Errorf("expected nil ceiling for an empty table, got %+v", got)
	}
	if got := CeilingPrices(nil); got != nil {
		t.Errorf("expected nil ceiling for a nil table, got %+v", got)
	}
}

func TestResolveCostUnknownModelUsesCeiling(t *testing.T) {
	cost, source := ResolveCost(nil, "claude-brandnew-6",
		1000, 1000, 1000, 1000, ceilingTestTable())

	if cost == nil {
		t.Fatal("expected an unknown model to be priced at the ceiling, got nil")
	}
	if source != "ceiling" {
		t.Errorf("source = %q, want ceiling", source)
	}
	// (15 + 75 + 18.75 + 1.50) = 110.25 per million, 1000 tokens of each
	want := 1000 * 110.25 / 1e6 // 0.11025
	if *cost < want-1e-9 || *cost > want+1e-9 {
		t.Errorf("cost = %v, want %v (per-rate ceiling)", *cost, want)
	}
}

// The ceiling must never displace a real answer.
func TestResolveCostCeilingIsLastResort(t *testing.T) {
	table := ceilingTestTable()
	table["claude-haiku-4-5"] = &ModelPrices{InputRate: 1.0, OutputRate: 5.0}

	t.Run("exact entry wins", func(t *testing.T) {
		cost, source := ResolveCost(nil, "claude-haiku-4-5", 1000, 1000, 0, 0, table)
		if source != "computed" {
			t.Fatalf("source = %q, want computed", source)
		}
		if *cost < 0.005999 || *cost > 0.006001 {
			t.Errorf("cost = %v, want ~0.006 from the exact entry", *cost)
		}
	})

	t.Run("undated fallback wins", func(t *testing.T) {
		cost, source := ResolveCost(nil, "claude-haiku-4-5-20251001", 1000, 1000, 0, 0, table)
		if source != "computed" {
			t.Fatalf("source = %q, want computed", source)
		}
		if *cost < 0.005999 || *cost > 0.006001 {
			t.Errorf("cost = %v, want ~0.006 from the undated fallback", *cost)
		}
	})

	t.Run("reported wins", func(t *testing.T) {
		reported := 0.05
		cost, source := ResolveCost(&reported, "claude-brandnew-6", 1000, 1000, 0, 0, table)
		if source != "reported" || *cost != 0.05 {
			t.Errorf("cost=%v source=%q, want the reported 0.05", cost, source)
		}
	})
}

// Without a table there is no ceiling to fall back to, so the event stays
// unpriced exactly as before. Same for a model we cannot name at all: an empty
// model is not an unrecognized model, it is a record we cannot reason about,
// and inventing the priciest possible cost for it would be noise.
func TestResolveCostNoCeilingAvailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		table PriceTable
	}{
		{"nil table", "claude-brandnew-6", nil},
		{"empty table", "claude-brandnew-6", PriceTable{}},
		{"empty model", "", ceilingTestTable()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost, source := ResolveCost(nil, tc.model, 1000, 1000, 0, 0, tc.table)
			if cost != nil || source != "" {
				t.Errorf("cost=%v source=%q, want nil/empty", cost, source)
			}
		})
	}
}
