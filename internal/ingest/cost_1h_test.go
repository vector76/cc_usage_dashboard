package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// Claude Code writes almost all of its prompt cache with the 1-hour TTL, which
// bills at 2x base input rather than the 5-minute TTL's 1.25x. The usage block
// reports the split under cache_creation.ephemeral_{5m,1h}_input_tokens while
// cache_creation_input_tokens stays the total, so the 1h count is a subset of
// the total and the 5m portion is the remainder.

func TestResolveCostPrices1hCacheWritesAtTheirOwnRate(t *testing.T) {
	table := PriceTable{
		"claude-sonnet-4-6": &ModelPrices{
			InputRate:           3.0,
			OutputRate:          15.0,
			CacheCreationRate:   3.75,
			CacheCreation1hRate: 6.0,
			CacheReadRate:       0.30,
		},
	}

	// 1M cache-write tokens in total, 600k of them 1h.
	cost, source := ResolveCost(nil, "claude-sonnet-4-6", 0, 0, 1_000_000, 600_000, 0, table)
	if source != "computed" || cost == nil {
		t.Fatalf("cost=%v source=%q, want a computed cost", cost, source)
	}
	want := 0.4*3.75 + 0.6*6.0 // 1.50 + 3.60
	if diff := *cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost = %.6f, want %.6f", *cost, want)
	}
}

// A 1h count larger than the total is malformed input; the 5m remainder must
// not go negative and subtract dollars.
func TestResolveCost1hLargerThanTotalDoesNotGoNegative(t *testing.T) {
	table := PriceTable{
		"m": &ModelPrices{CacheCreationRate: 1.25, CacheCreation1hRate: 2.0},
	}
	cost, _ := ResolveCost(nil, "m", 0, 0, 100, 1_000_000, 0, table)
	if cost == nil {
		t.Fatal("expected a cost")
	}
	if want := 2.0; *cost < want-1e-9 || *cost > want+1e-9 {
		t.Errorf("cost = %v, want %v (1h only, no negative 5m term)", *cost, want)
	}
}

func TestCeilingPricesIncludes1hCacheWriteRate(t *testing.T) {
	table := PriceTable{
		"a": &ModelPrices{CacheCreationRate: 18.75, CacheCreation1hRate: 3.0},
		"b": &ModelPrices{CacheCreationRate: 1.0, CacheCreation1hRate: 30.0},
	}
	got := CeilingPrices(table)
	if got.CacheCreation1hRate != 30.0 {
		t.Errorf("ceiling 1h rate = %v, want 30", got.CacheCreation1hRate)
	}
	if got.CacheCreationRate != 18.75 {
		t.Errorf("ceiling 5m rate = %v, want 18.75", got.CacheCreationRate)
	}
}

// A prices.yaml override written before the 1h key existed must not price 1h
// writes at $0. Anthropic's published rule is 2x base input.
func TestParsePriceTableDefaults1hRateToTwiceInput(t *testing.T) {
	yaml := strings.Join([]string{
		"models:",
		"  old-style:",
		"    input_rate_usd_per_m: 3.0",
		"    cache_creation_rate_usd_per_m: 3.75",
		"  explicit:",
		"    input_rate_usd_per_m: 3.0",
		"    cache_creation_1h_rate_usd_per_m: 7.0",
	}, "\n")
	pt, err := ParsePriceTable([]byte(yaml), "test")
	if err != nil {
		t.Fatalf("ParsePriceTable: %v", err)
	}
	if got := pt["old-style"].CacheCreation1hRate; got != 6.0 {
		t.Errorf("defaulted 1h rate = %v, want 6.0", got)
	}
	if got := pt["explicit"].CacheCreation1hRate; got != 7.0 {
		t.Errorf("explicit 1h rate = %v, want 7.0", got)
	}
}

// Every model in the shipped table carries an explicit 1h rate of 2x input,
// so the default above is a safety net rather than the source of truth.
func TestShippedPriceTableHas1hRates(t *testing.T) {
	pt, err := LoadPriceTable(filepath.Join("..", "..", "prices.yaml"))
	if err != nil {
		t.Fatalf("LoadPriceTable: %v", err)
	}
	for model, p := range pt {
		if want := 2 * p.InputRate; p.CacheCreation1hRate != want {
			t.Errorf("%s: 1h rate = %v, want %v (2x input)", model, p.CacheCreation1hRate, want)
		}
	}
}

func TestParserExtracts1hCacheWrites(t *testing.T) {
	jsonl := `{"type":"assistant","sessionId":"s","message":{"id":"m","usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":7465,"cache_read_input_tokens":84151,"cache_creation":{"ephemeral_5m_input_tokens":465,"ephemeral_1h_input_tokens":7000}}}}`
	event, err := NewParser(strings.NewReader(jsonl)).ParseNext()
	if err != nil || event == nil {
		t.Fatalf("event=%v err=%v", event, err)
	}
	if event.CacheCreationTokens != 7465 {
		t.Errorf("total cache creation = %d, want 7465", event.CacheCreationTokens)
	}
	if event.CacheCreation1hTokens != 7000 {
		t.Errorf("1h cache creation = %d, want 7000", event.CacheCreation1hTokens)
	}
}

func TestParserWithoutCacheCreationSplitReportsZero1h(t *testing.T) {
	jsonl := `{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":100}}}`
	event, err := NewParser(strings.NewReader(jsonl)).ParseNext()
	if err != nil || event == nil {
		t.Fatalf("event=%v err=%v", event, err)
	}
	if event.CacheCreation1hTokens != 0 {
		t.Errorf("1h cache creation = %d, want 0", event.CacheCreation1hTokens)
	}
}

func TestTailerStores1hSplitAndPricesIt(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	pt := PriceTable{"claude-opus-5-5": &ModelPrices{CacheCreationRate: 5.0, CacheCreation1hRate: 8.0}}
	dir := t.TempDir()
	tailer := NewTailer(dir, s, pt)

	path := filepath.Join(dir, "session-1h.jsonl")
	line := `{"type":"assistant","sessionId":"s","timestamp":"2026-09-22T10:00:00Z","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":1000000,"cache_creation":{"ephemeral_5m_input_tokens":250000,"ephemeral_1h_input_tokens":750000}}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tailer.processFile(path)

	var oneH int
	var cost float64
	if err := s.DB().QueryRow(`SELECT cache_creation_1h_tokens, cost_usd_equivalent FROM usage_events`).
		Scan(&oneH, &cost); err != nil {
		t.Fatalf("select: %v", err)
	}
	if oneH != 750000 {
		t.Errorf("1h tokens = %d, want 750000", oneH)
	}
	if want := 0.25*5.0 + 0.75*8.0; cost < want-1e-9 || cost > want+1e-9 {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}

// Rows the v9 migration cleared are re-priced by BackfillCosts with the 1h
// rate, which is how historical dollars pick up the correction.
func TestBackfillCostsUses1hRate(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	id, err := s.InsertUsageEventRecord(store.UsageEventRecord{
		OccurredAt: time.Now(), Source: "tailer", SessionID: "s", MessageID: "m",
		Model: "claude-opus-5-5", CacheCreationTokens: 1_000_000, CacheCreation1hTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	pt := PriceTable{"claude-opus-5-5": &ModelPrices{CacheCreationRate: 5.0, CacheCreation1hRate: 8.0}}
	if _, err := BackfillCosts(s, pt); err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	var cost float64
	if err := s.DB().QueryRow(`SELECT cost_usd_equivalent FROM usage_events WHERE id = ?`, id).Scan(&cost); err != nil {
		t.Fatalf("select: %v", err)
	}
	if cost != 8.0 {
		t.Errorf("cost = %v, want 8.0 (all 1h)", cost)
	}
}
