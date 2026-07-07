package ingest

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func openBackfillStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func backfillPriceTable() PriceTable {
	return PriceTable{
		"claude-fable-5": &ModelPrices{
			InputRate:         3.0,
			OutputRate:        15.0,
			CacheCreationRate: 3.75,
			CacheReadRate:     0.30,
		},
	}
}

func eventCost(t *testing.T, s *store.Store, id int64) (cost sql.NullFloat64, source sql.NullString) {
	t.Helper()
	if err := s.DB().QueryRow(
		"SELECT cost_usd_equivalent, cost_source FROM usage_events WHERE id = ?", id,
	).Scan(&cost, &source); err != nil {
		t.Fatalf("scan event %d: %v", id, err)
	}
	return cost, source
}

func TestBackfillNullCostsComputesOnlyKnownModelNullRows(t *testing.T) {
	s := openBackfillStore(t)
	now := time.Now()

	// NULL cost, model now present in the price table — must be backfilled.
	idKnown, err := s.InsertUsageEvent(now, "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 2000, 10000, nil, "", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// NULL cost, model still missing from the table — must stay NULL.
	idUnknown, err := s.InsertUsageEvent(now, "tailer", "s", "m2", "/p",
		"claude-mystery-9", 1000, 500, 0, 0, nil, "", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Reported cost — must never change.
	reported := 0.5
	idReported, err := s.InsertUsageEvent(now, "tailer", "s", "m3", "/p",
		"claude-fable-5", 1000, 500, 0, 0, &reported, "reported", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err := BackfillNullCosts(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("BackfillNullCosts: %v", err)
	}
	if n != 1 {
		t.Errorf("backfilled %d events, want 1", n)
	}

	// 1000*3.0/1e6 + 500*15.0/1e6 + 2000*3.75/1e6 + 10000*0.30/1e6 = 0.021
	cost, source := eventCost(t, s, idKnown)
	if !cost.Valid || cost.Float64 < 0.020999 || cost.Float64 > 0.021001 {
		t.Errorf("known-model row cost = %+v, want ~0.021", cost)
	}
	if source.String != "computed" {
		t.Errorf("known-model row cost_source = %q, want computed", source.String)
	}

	cost, _ = eventCost(t, s, idUnknown)
	if cost.Valid {
		t.Errorf("unknown-model row must stay NULL, got %v", cost.Float64)
	}

	cost, source = eventCost(t, s, idReported)
	if cost.Float64 != 0.5 || source.String != "reported" {
		t.Errorf("reported row changed: cost=%v source=%q", cost.Float64, source.String)
	}
}

func TestBackfillNullCostsIsIdempotent(t *testing.T) {
	s := openBackfillStore(t)
	if _, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 0, 0, nil, "", "{}"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err := BackfillNullCosts(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if n != 1 {
		t.Errorf("first run backfilled %d, want 1", n)
	}

	n, err = BackfillNullCosts(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if n != 0 {
		t.Errorf("second run backfilled %d, want 0", n)
	}
}

func TestBackfillNullCostsEmptyTableIsNoOp(t *testing.T) {
	s := openBackfillStore(t)
	if _, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 0, 0, nil, "", "{}"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err := BackfillNullCosts(s, PriceTable{})
	if err != nil {
		t.Fatalf("BackfillNullCosts: %v", err)
	}
	if n != 0 {
		t.Errorf("backfilled %d with empty table, want 0", n)
	}

	n, err = BackfillNullCosts(s, nil)
	if err != nil {
		t.Fatalf("BackfillNullCosts(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("backfilled %d with nil table, want 0", n)
	}
}

// TestBackfillNullCostsSurfacesViaFeedback locks in that a successful backfill
// emits a warn-level record, which the feedback tee copies into the buffer the
// dashboard's /api/feedback panel reads — the tray app has no console, so this
// is the only place the user learns historical numbers just changed.
func TestBackfillNullCostsSurfacesViaFeedback(t *testing.T) {
	s := openBackfillStore(t)
	if _, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 0, 0, nil, "", "{}"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	buf := feedback.NewBuffer(10)
	prev := slog.Default()
	slog.SetDefault(slog.New(feedback.NewHandler(
		slog.NewTextHandler(io.Discard, nil), buf)))
	defer slog.SetDefault(prev)

	if _, err := BackfillNullCosts(s, backfillPriceTable()); err != nil {
		t.Fatalf("BackfillNullCosts: %v", err)
	}

	recs := buf.Recent()
	found := false
	for _, r := range recs {
		if strings.Contains(r.Message, "backfilled") {
			found = true
			if r.Attrs["events"] != "1" {
				t.Errorf("events attr = %q, want 1", r.Attrs["events"])
			}
			if !strings.Contains(r.Attrs["models"], "claude-fable-5") {
				t.Errorf("models attr = %q, want to contain claude-fable-5", r.Attrs["models"])
			}
		}
	}
	if !found {
		t.Fatalf("no backfill record in feedback buffer; got %+v", recs)
	}

	// A run that backfills nothing must NOT add feedback noise.
	buf.Reset()
	if _, err := BackfillNullCosts(s, backfillPriceTable()); err != nil {
		t.Fatalf("second BackfillNullCosts: %v", err)
	}
	if got := buf.Len(); got != 0 {
		t.Errorf("no-op backfill buffered %d records, want 0: %+v", got, buf.Recent())
	}
}
