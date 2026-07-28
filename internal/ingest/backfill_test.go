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

	// NULL cost, model still missing from the table — gets the pessimistic
	// ceiling estimate rather than staying NULL. A NULL cost is invisible
	// twice over: excluded from every dollar total, and drawn as a
	// zero-height segment in the stacked bars, so an unrecognized model
	// could not be seen at all until it carried a number.
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

	n, err := BackfillCosts(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 2 {
		t.Errorf("backfilled %d events, want 2 (known model + ceiling estimate)", n)
	}

	// 1000*3.0/1e6 + 500*15.0/1e6 + 2000*3.75/1e6 + 10000*0.30/1e6 = 0.021
	cost, source := eventCost(t, s, idKnown)
	if !cost.Valid || cost.Float64 < 0.020999 || cost.Float64 > 0.021001 {
		t.Errorf("known-model row cost = %+v, want ~0.021", cost)
	}
	if source.String != "computed" {
		t.Errorf("known-model row cost_source = %q, want computed", source.String)
	}

	// The one-entry table makes its own rates the ceiling:
	// 1000*3.0/1e6 + 500*15.0/1e6 = 0.0105.
	cost, source = eventCost(t, s, idUnknown)
	if !cost.Valid || cost.Float64 < 0.010499 || cost.Float64 > 0.010501 {
		t.Errorf("unknown-model row cost = %+v, want ~0.0105 at the ceiling", cost)
	}
	if source.String != "ceiling" {
		t.Errorf("unknown-model row cost_source = %q, want ceiling", source.String)
	}

	cost, source = eventCost(t, s, idReported)
	if cost.Float64 != 0.5 || source.String != "reported" {
		t.Errorf("reported row changed: cost=%v source=%q", cost.Float64, source.String)
	}
}

// TestBackfillNullCostsResolvesDatedModelIDs covers the live-DB shape that
// motivated the dated-id fallback: events store a dated snapshot id while the
// price table lists only the undated family name.
func TestBackfillNullCostsResolvesDatedModelIDs(t *testing.T) {
	s := openBackfillStore(t)
	id, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-haiku-4-5-20251001", 1000, 1000, 0, 0, nil, "", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	table := PriceTable{
		"claude-haiku-4-5": &ModelPrices{InputRate: 1.0, OutputRate: 5.0},
	}
	n, err := BackfillCosts(s, table)
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 1 {
		t.Errorf("backfilled %d events, want 1", n)
	}

	cost, source := eventCost(t, s, id)
	if !cost.Valid || cost.Float64 < 0.005999 || cost.Float64 > 0.006001 {
		t.Errorf("dated-id row cost = %+v, want ~0.006", cost)
	}
	if source.String != "computed" {
		t.Errorf("cost_source = %q, want computed", source.String)
	}
}

// A ceiling estimate is provisional: the moment the price table learns the
// model, the real rates must replace it. Without this, adding a newly released
// model to prices.yaml would fix new events while leaving history permanently
// overstated at the ceiling — and the ceiling can be 3x the true rate.
func TestBackfillUpgradesCeilingEstimateToRealRates(t *testing.T) {
	s := openBackfillStore(t)

	// Ingested when the model was unknown: priced at the ceiling.
	id, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-opus-5", 1000, 1000, 0, 0, ptrFloat(0.09), "ceiling", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// prices.yaml now lists it, at rates well below the ceiling.
	table := backfillPriceTable() // ceiling = 3.0 / 15.0
	table["claude-opus-5"] = &ModelPrices{InputRate: 1.0, OutputRate: 5.0}

	n, err := BackfillCosts(s, table)
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 1 {
		t.Errorf("backfilled %d events, want 1", n)
	}

	// 1000*1.0/1e6 + 1000*5.0/1e6 = 0.006, replacing the 0.09 estimate.
	cost, source := eventCost(t, s, id)
	if !cost.Valid || cost.Float64 < 0.005999 || cost.Float64 > 0.006001 {
		t.Errorf("cost = %+v, want ~0.006 from the real rates", cost)
	}
	if source.String != "computed" {
		t.Errorf("cost_source = %q, want computed", source.String)
	}
}

// While the model stays unknown the estimate must be left exactly as it is: no
// churn, and in particular no re-estimating on every startup.
func TestBackfillLeavesCeilingEstimateAloneWhileModelUnknown(t *testing.T) {
	s := openBackfillStore(t)
	id, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-opus-5", 1000, 1000, 0, 0, ptrFloat(0.09), "ceiling", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err := BackfillCosts(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 0 {
		t.Errorf("backfilled %d events, want 0 while the model is still unknown", n)
	}
	cost, source := eventCost(t, s, id)
	if cost.Float64 != 0.09 || source.String != "ceiling" {
		t.Errorf("estimate changed: cost=%v source=%q, want 0.09/ceiling", cost.Float64, source.String)
	}
}

// A measured cost is never displaced, in either direction.
func TestBackfillNeverOverwritesMeasuredCosts(t *testing.T) {
	s := openBackfillStore(t)
	table := backfillPriceTable()

	for _, src := range []string{"reported", "computed"} {
		t.Run(src, func(t *testing.T) {
			id, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m-"+src, "/p",
				"claude-fable-5", 1000, 1000, 0, 0, ptrFloat(0.5), src, "{}")
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			if _, err := BackfillCosts(s, table); err != nil {
				t.Fatalf("BackfillCosts: %v", err)
			}
			cost, source := eventCost(t, s, id)
			if cost.Float64 != 0.5 || source.String != src {
				t.Errorf("row changed: cost=%v source=%q, want 0.5/%s", cost.Float64, source.String, src)
			}
		})
	}
}

func ptrFloat(v float64) *float64 { return &v }

func TestBackfillNullCostsIsIdempotent(t *testing.T) {
	s := openBackfillStore(t)
	if _, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 0, 0, nil, "", "{}"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	n, err := BackfillCosts(s, backfillPriceTable())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if n != 1 {
		t.Errorf("first run backfilled %d, want 1", n)
	}

	n, err = BackfillCosts(s, backfillPriceTable())
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

	n, err := BackfillCosts(s, PriceTable{})
	if err != nil {
		t.Fatalf("BackfillCosts: %v", err)
	}
	if n != 0 {
		t.Errorf("backfilled %d with empty table, want 0", n)
	}

	n, err = BackfillCosts(s, nil)
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

	if _, err := BackfillCosts(s, backfillPriceTable()); err != nil {
		t.Fatalf("BackfillCosts: %v", err)
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
	if _, err := BackfillCosts(s, backfillPriceTable()); err != nil {
		t.Fatalf("second BackfillCosts: %v", err)
	}
	if got := buf.Len(); got != 0 {
		t.Errorf("no-op backfill buffered %d records, want 0: %+v", got, buf.Recent())
	}
}
