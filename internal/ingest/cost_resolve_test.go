package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	ccusage "github.com/vector76/cc_usage_dashboard"
)

// sentinelYAML is a tiny, uniquely-identifiable price table used to prove which
// source a resolution step actually loaded. The model name is deliberately
// unique so it can never collide with the real embedded/canonical table.
func sentinelYAML(model string, inputRate float64) []byte {
	return []byte(fmt.Sprintf("models:\n  %s:\n"+
		"    input_rate_usd_per_m: %g\n"+
		"    output_rate_usd_per_m: 0.0\n"+
		"    cache_creation_rate_usd_per_m: 0.0\n"+
		"    cache_read_rate_usd_per_m: 0.0\n", model, inputRate))
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestEmbeddedPriceTableParses is the guarantee behind precedence step (c):
// the embedded default must always parse and must contain the expected model
// keys with their documented rates. If the embed is missing or malformed this
// fails at test time rather than in production.
func TestEmbeddedPriceTableParses(t *testing.T) {
	if len(ccusage.DefaultPriceTableYAML) == 0 {
		t.Fatal("embedded DefaultPriceTableYAML is empty")
	}

	pt, err := ParsePriceTable(ccusage.DefaultPriceTableYAML, "embedded default")
	if err != nil {
		t.Fatalf("ParsePriceTable(embedded): %v", err)
	}

	cases := []struct {
		model  string
		input  float64
		output float64
	}{
		{"claude-opus-4-8", 5.00, 25.00},
		{"claude-sonnet-4-6", 3.00, 15.00},
		{"claude-haiku-4-5", 1.00, 5.00},
		{"claude-opus-4-1", 15.00, 75.00},
		{"claude-3-5-sonnet-20241022", 3.00, 15.00},
	}
	for _, tc := range cases {
		mp, ok := pt[tc.model]
		if !ok {
			t.Errorf("embedded table missing model %q", tc.model)
			continue
		}
		if mp.InputRate != tc.input || mp.OutputRate != tc.output {
			t.Errorf("%s: got input=%.2f output=%.2f, want %.2f/%.2f",
				tc.model, mp.InputRate, mp.OutputRate, tc.input, tc.output)
		}
	}
}

// TestResolvePriceTableExplicitWins covers precedence step (a): a present
// explicit path beats both a local override and the embedded default.
func TestResolvePriceTableExplicitWins(t *testing.T) {
	explicitDir := t.TempDir()
	searchDir := t.TempDir()

	explicit := writeFile(t, explicitDir, "explicit.yaml", sentinelYAML("sentinel-explicit", 111))
	writeFile(t, searchDir, "prices.yaml", sentinelYAML("sentinel-local", 222))

	pt, source, err := ResolvePriceTable(explicit, []string{searchDir}, ccusage.DefaultPriceTableYAML)
	if err != nil {
		t.Fatalf("ResolvePriceTable: %v", err)
	}
	if source != explicit {
		t.Errorf("source = %q, want %q", source, explicit)
	}
	if _, ok := pt["sentinel-explicit"]; !ok {
		t.Errorf("expected explicit table loaded; got models %v", keys(pt))
	}
	if _, ok := pt["sentinel-local"]; ok {
		t.Error("explicit path should have won over the local override")
	}
}

// TestResolvePriceTableLocalOverridesEmbedded covers precedence step (b): with
// no explicit path, a prices.yaml in a search dir beats the embedded default.
func TestResolvePriceTableLocalOverridesEmbedded(t *testing.T) {
	searchDir := t.TempDir()
	local := writeFile(t, searchDir, "prices.yaml", sentinelYAML("sentinel-local", 333))

	pt, source, err := ResolvePriceTable("", []string{searchDir}, ccusage.DefaultPriceTableYAML)
	if err != nil {
		t.Fatalf("ResolvePriceTable: %v", err)
	}
	if source != local {
		t.Errorf("source = %q, want %q", source, local)
	}
	if _, ok := pt["sentinel-local"]; !ok {
		t.Errorf("expected local override loaded; got models %v", keys(pt))
	}
	// The embedded default's models must NOT leak through when an override wins.
	if _, ok := pt["claude-opus-4-8"]; ok {
		t.Error("embedded model present; local override should have replaced it entirely")
	}
}

// TestResolvePriceTableEmbeddedFallback covers precedence step (c): no explicit
// path and no local override -> embedded default is used.
func TestResolvePriceTableEmbeddedFallback(t *testing.T) {
	emptyDir := t.TempDir() // contains no prices.yaml

	pt, source, err := ResolvePriceTable("", []string{emptyDir}, ccusage.DefaultPriceTableYAML)
	if err != nil {
		t.Fatalf("ResolvePriceTable: %v", err)
	}
	if source != "embedded default" {
		t.Errorf("source = %q, want %q", source, "embedded default")
	}
	if _, ok := pt["claude-opus-4-8"]; !ok {
		t.Errorf("expected embedded default loaded with claude-opus-4-8; got models %v", keys(pt))
	}
}

// TestResolvePriceTableExplicitMissingFallsThrough documents and locks in the
// precedence decision in (3a): an explicit path that does not exist is a
// non-fatal warning and resolution continues to the search dirs / embedded
// default rather than disabling cost computation.
func TestResolvePriceTableExplicitMissingFallsThrough(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	// Falls through to the local override when one exists.
	searchDir := t.TempDir()
	writeFile(t, searchDir, "prices.yaml", sentinelYAML("sentinel-local", 111))

	pt, source, err := ResolvePriceTable(missing, []string{searchDir}, ccusage.DefaultPriceTableYAML)
	if err != nil {
		t.Fatalf("ResolvePriceTable: %v", err)
	}
	if _, ok := pt["sentinel-local"]; !ok {
		t.Errorf("expected fall-through to local override; source=%q models=%v", source, keys(pt))
	}

	// With nothing else present, falls through all the way to embedded.
	pt2, source2, err := ResolvePriceTable(missing, []string{t.TempDir()}, ccusage.DefaultPriceTableYAML)
	if err != nil {
		t.Fatalf("ResolvePriceTable(embedded fallthrough): %v", err)
	}
	if source2 != "embedded default" {
		t.Errorf("source = %q, want embedded default", source2)
	}
	if _, ok := pt2["claude-opus-4-8"]; !ok {
		t.Error("expected embedded default after explicit-missing fall-through")
	}
}

// TestResolvePriceTableMalformedExplicitIsFatal confirms a broken explicit
// override is surfaced (not silently masked by the embedded default).
func TestResolvePriceTableMalformedExplicitIsFatal(t *testing.T) {
	dir := t.TempDir()
	bad := writeFile(t, dir, "bad.yaml", []byte("models:\n  x:\n    input_rate_usd_per_m: [unterminated\n"))

	_, _, err := ResolvePriceTable(bad, nil, ccusage.DefaultPriceTableYAML)
	if err == nil {
		t.Fatal("expected error for malformed explicit price table, got nil")
	}
}

func keys(pt PriceTable) []string {
	out := make([]string, 0, len(pt))
	for k := range pt {
		out = append(out, k)
	}
	return out
}
