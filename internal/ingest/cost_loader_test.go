package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPriceTableFromFile(t *testing.T) {
	// Create a temporary YAML file with valid pricing data
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "prices.yaml")

	yamlContent := `models:
  claude-3-5-sonnet-20241022:
    input_rate_usd_per_m: 3.00
    output_rate_usd_per_m: 15.00
    cache_creation_rate_usd_per_m: 3.75
    cache_read_rate_usd_per_m: 0.30

  claude-3-opus-20250219:
    input_rate_usd_per_m: 15.00
    output_rate_usd_per_m: 75.00
    cache_creation_rate_usd_per_m: 18.75
    cache_read_rate_usd_per_m: 1.50
`

	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test YAML file: %v", err)
	}

	pt := LoadPriceTable(configPath)

	// Verify sonnet pricing
	sonnet, ok := pt["claude-3-5-sonnet-20241022"]
	if !ok {
		t.Fatal("sonnet model not found in price table")
	}
	if sonnet.InputRate != 3.00 {
		t.Errorf("expected sonnet input rate 3.00, got %f", sonnet.InputRate)
	}
	if sonnet.OutputRate != 15.00 {
		t.Errorf("expected sonnet output rate 15.00, got %f", sonnet.OutputRate)
	}
	if sonnet.CacheCreationRate != 3.75 {
		t.Errorf("expected sonnet cache creation rate 3.75, got %f", sonnet.CacheCreationRate)
	}
	if sonnet.CacheReadRate != 0.30 {
		t.Errorf("expected sonnet cache read rate 0.30, got %f", sonnet.CacheReadRate)
	}

	// Verify opus pricing
	opus, ok := pt["claude-3-opus-20250219"]
	if !ok {
		t.Fatal("opus model not found in price table")
	}
	if opus.InputRate != 15.00 {
		t.Errorf("expected opus input rate 15.00, got %f", opus.InputRate)
	}
	if opus.OutputRate != 75.00 {
		t.Errorf("expected opus output rate 75.00, got %f", opus.OutputRate)
	}
}

func TestLoadPriceTableMissingFile(t *testing.T) {
	pt := LoadPriceTable("/nonexistent/path/prices.yaml")
	// Should return empty table without error (non-fatal)
	if pt == nil {
		t.Fatal("expected empty price table, got nil")
	}
	if len(pt) > 0 {
		t.Errorf("expected empty price table, got %d entries", len(pt))
	}
}

func TestLoadPriceTableMalformedYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bad.yaml")

	yamlContent := `models:
  claude-3-5-sonnet-20241022:
    input_rate_usd_per_m: "not a number"
`

	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test YAML file: %v", err)
	}

	// This should handle gracefully and return a clear error or empty table
	pt := LoadPriceTable(configPath)
	if pt == nil {
		t.Error("expected a price table (possibly empty), got nil")
	}
}

func TestResolveCostWithLoadedPriceTable(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "prices.yaml")

	yamlContent := `models:
  claude-3-5-sonnet-20241022:
    input_rate_usd_per_m: 3.00
    output_rate_usd_per_m: 15.00
    cache_creation_rate_usd_per_m: 3.75
    cache_read_rate_usd_per_m: 0.30
`

	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test YAML file: %v", err)
	}

	pt := LoadPriceTable(configPath)

	// Test with known model
	cost, source := ResolveCost(nil, "claude-3-5-sonnet-20241022", 1000000, 1000000, 0, 0, pt)

	if source != "computed" {
		t.Errorf("expected cost_source 'computed', got '%s'", source)
	}

	if cost == nil {
		t.Fatal("expected computed cost, got nil")
	}

	// 1M input tokens at $3/M = $3, 1M output at $15/M = $15, total $18
	expectedCost := 3.0 + 15.0
	if *cost != expectedCost {
		t.Errorf("expected cost %.2f, got %.2f", expectedCost, *cost)
	}
}
