package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/ingest"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// A fetched price list is layered under the local table, pushed to every
// consumer, clears the models it prices from the unknown-model prompt, and
// re-prices their stored ceiling estimates.
func TestApplyFetchedPrices(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ceiling := 0.09
	id, err := db.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-new-9", 1_000_000, 0, 0, 0, &ceiling, "ceiling", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	unknown := feedback.NewUnknownModels()
	unknown.Record("claude-new-9", time.Now())
	unknown.Record("claude-still-unknown", time.Now())

	base := ingest.PriceTable{"claude-known": {InputRate: 1}}
	fetched := ingest.PriceTable{
		"claude-known": {InputRate: 100}, // the local table wins
		"claude-new-9": {InputRate: 7},
	}

	var got []ingest.PriceTable
	sink := func(pt ingest.PriceTable) { got = append(got, pt) }
	eff := applyFetchedPrices(base, fetched, db, unknown, sink, sink)

	if len(got) != 2 {
		t.Fatalf("sinks called %d times, want 2", len(got))
	}
	if eff["claude-known"].InputRate != 1 || eff["claude-new-9"].InputRate != 7 {
		t.Errorf("effective table = known %v / new %v, want 1 / 7",
			eff["claude-known"].InputRate, eff["claude-new-9"].InputRate)
	}

	snap := unknown.Snapshot()
	if len(snap) != 1 || snap[0].Model != "claude-still-unknown" {
		t.Errorf("unknown models after apply = %+v, want only claude-still-unknown", snap)
	}

	var cost float64
	var source string
	if err := db.DB().QueryRow(`SELECT cost_usd_equivalent, cost_source FROM usage_events WHERE id = ?`, id).Scan(&cost, &source); err != nil {
		t.Fatalf("query: %v", err)
	}
	if source != "computed" || cost != 7 {
		t.Errorf("stored cost = %v (%s), want 7 (computed)", cost, source)
	}
}
