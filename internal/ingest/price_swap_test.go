package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// A table handed to SetPriceTable prices the events ingested after it: this
// is how a price list fetched from GitHub reaches a running tailer.
func TestTailerSetPriceTable(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	dir := t.TempDir()
	tailer := NewTailer(dir, s, PriceTable{"known-model": {InputRate: 1, OutputRate: 1}})
	tailer.unknown = feedback.NewUnknownModels()
	tailer.SetPriceTable(PriceTable{"new-model": {InputRate: 2, OutputRate: 2}})

	path := filepath.Join(dir, "s.jsonl")
	line := `{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:00:00Z","message":{"id":"m1","model":"new-model","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0644); err != nil {
		t.Fatal(err)
	}
	tailer.processFile(path)

	var cost float64
	var source string
	if err := s.DB().QueryRow(`SELECT cost_usd_equivalent, cost_source FROM usage_events`).Scan(&cost, &source); err != nil {
		t.Fatalf("query: %v", err)
	}
	if source != "computed" || cost != 2 {
		t.Errorf("cost = %v (%s), want 2 (computed) from the swapped-in table", cost, source)
	}
}

// BackfillCostsUnpriced names every model still priced at the ceiling,
// whether or not this pass changed its rows, so startup can ask GitHub for
// them. Claude Code's "<synthetic>" placeholder is never named.
func TestBackfillCostsUnpriced(t *testing.T) {
	s := openBackfillStore(t)
	now := time.Now()
	insert := func(id, model string, cost *float64, source string) {
		t.Helper()
		if _, err := s.InsertUsageEvent(now, "tailer", "s", id, "/p", model, 1000, 1000, 0, 0, cost, source, "{}"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert("m1", "claude-mystery-9", ptrFloat(0.09), "ceiling") // old estimate, left alone
	insert("m2", "claude-mystery-8", nil, "")                   // newly estimated now
	insert("m3", "claude-fable-5", nil, "")                     // priced now
	insert("m4", syntheticModel, nil, "")

	n, unpriced, err := BackfillCostsUnpriced(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("BackfillCostsUnpriced: %v", err)
	}
	if n != 3 {
		t.Errorf("changed %d events, want 3", n)
	}
	want := []string{"claude-mystery-8", "claude-mystery-9"}
	if len(unpriced) != len(want) || unpriced[0] != want[0] || unpriced[1] != want[1] {
		t.Errorf("unpriced = %v, want %v", unpriced, want)
	}
}
