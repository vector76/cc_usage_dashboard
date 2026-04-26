package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadPriceTableExampleFile loads the checked-in example config and
// verifies the documented rates round-trip from YAML to PriceTable.
func TestLoadPriceTableExampleFile(t *testing.T) {
	// internal/ingest -> repo root -> config/prices.example.yaml
	examplePath := filepath.Join("..", "..", "config", "prices.example.yaml")

	pt, err := LoadPriceTable(examplePath)
	if err != nil {
		t.Fatalf("LoadPriceTable(%q) returned error: %v", examplePath, err)
	}

	cases := []struct {
		model         string
		input         float64
		output        float64
		cacheCreation float64
		cacheRead     float64
	}{
		// Spot check against Anthropic's published Messages API rates.
		// Cache creation uses the 5-minute multiplier (1.25x input).
		{"claude-opus-4-7", 5.00, 25.00, 6.25, 0.50},     // repriced from $15/$75
		{"claude-sonnet-4-6", 3.00, 15.00, 3.75, 0.30},
		{"claude-haiku-4-5", 1.00, 5.00, 1.25, 0.10},
		{"claude-opus-4-1", 15.00, 75.00, 18.75, 1.50},   // pre-repricing tier
		{"claude-3-5-sonnet-20241022", 3.00, 15.00, 3.75, 0.30},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			mp, ok := pt[tc.model]
			if !ok {
				t.Fatalf("model %q missing from loaded table", tc.model)
			}
			if mp.InputRate != tc.input {
				t.Errorf("InputRate: want %.4f, got %.4f", tc.input, mp.InputRate)
			}
			if mp.OutputRate != tc.output {
				t.Errorf("OutputRate: want %.4f, got %.4f", tc.output, mp.OutputRate)
			}
			if mp.CacheCreationRate != tc.cacheCreation {
				t.Errorf("CacheCreationRate: want %.4f, got %.4f", tc.cacheCreation, mp.CacheCreationRate)
			}
			if mp.CacheReadRate != tc.cacheRead {
				t.Errorf("CacheReadRate: want %.4f, got %.4f", tc.cacheRead, mp.CacheReadRate)
			}
		})
	}
}

// TestLoadPriceTableMissingFile confirms the missing-file path returns an
// empty (non-nil) table without an error so trayapp can keep running with
// cost computation disabled.
func TestLoadPriceTableMissingFile(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"nonexistent file", filepath.Join(t.TempDir(), "does-not-exist.yaml")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pt, err := LoadPriceTable(tc.path)
			if err != nil {
				t.Fatalf("expected nil error for missing file, got %v", err)
			}
			if pt == nil {
				t.Fatal("expected non-nil empty table, got nil")
			}
			if len(pt) != 0 {
				t.Errorf("expected empty table, got %d entries", len(pt))
			}
		})
	}
}

// TestLoadPriceTableMalformedYAML confirms that a syntactically broken YAML
// file produces a clear, wrapped error referencing the file path.
func TestLoadPriceTableMalformedYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "bad.yaml")

	// Unclosed mapping value — yaml.Unmarshal will fail.
	bad := "models:\n  claude-3-5-sonnet-20241022:\n    input_rate_usd_per_m: [unterminated\n"
	if err := os.WriteFile(configPath, []byte(bad), 0644); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}

	pt, err := LoadPriceTable(configPath)
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	// "Clear error" means the operator can locate the offending file from
	// the message — verify the path is preserved in the wrapped error.
	if !strings.Contains(err.Error(), configPath) {
		t.Errorf("error %q does not reference path %q", err.Error(), configPath)
	}
	if pt == nil {
		t.Error("expected non-nil (empty) table on parse failure, got nil")
	}
	if len(pt) != 0 {
		t.Errorf("expected empty table on parse failure, got %d entries", len(pt))
	}
}

// TestResolveCostFromLoadedTable feeds the example file's rates into
// ResolveCost and asserts the documented cost_source='computed' value with
// known token counts.
func TestResolveCostFromLoadedTable(t *testing.T) {
	pt, err := LoadPriceTable(filepath.Join("..", "..", "config", "prices.example.yaml"))
	if err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}

	const model = "claude-3-5-sonnet-20241022"
	// Sonnet rates: input 3, output 15, cache_creation 3.75, cache_read 0.30 USD/M
	cases := []struct {
		name                                                  string
		inputTokens, outputTokens, cacheCreation, cacheRead   int
		wantCost                                              float64
	}{
		{"input-only 1M", 1_000_000, 0, 0, 0, 3.00},
		{"output-only 1M", 0, 1_000_000, 0, 0, 15.00},
		{"mixed input+output", 1_000_000, 500_000, 0, 0, 3.00 + 7.50},
		{"with cache tokens", 200_000, 100_000, 50_000, 1_000_000,
			0.6 + 1.5 + 0.1875 + 0.30},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cost, source := ResolveCost(nil, model,
				tc.inputTokens, tc.outputTokens, tc.cacheCreation, tc.cacheRead, pt)

			if source != "computed" {
				t.Fatalf("source: want %q, got %q", "computed", source)
			}
			if cost == nil {
				t.Fatal("cost: want non-nil, got nil")
			}
			// Allow tiny float drift from per-rate arithmetic.
			if diff := *cost - tc.wantCost; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("cost: want %.6f, got %.6f", tc.wantCost, *cost)
			}
		})
	}
}
