package store

import (
	"path/filepath"
	"testing"
	"time"
)

// openTestStore opens a fresh on-disk store in a temp dir (migrations applied).
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNullCostEventsSelectsOnlyNullCostWithModel(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	// Row 1: NULL cost, known model — the backfill candidate.
	id1, err := s.InsertUsageEvent(now, "tailer", "sess1", "msg1", "/p",
		"claude-fable-5", 1000, 500, 2000, 10000, nil, "", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Row 2: NULL cost, empty model — must be excluded.
	if _, err := s.InsertUsageEvent(now, "tailer", "sess1", "msg2", "/p",
		"", 10, 10, 0, 0, nil, "", "{}"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Row 3: reported cost — must be excluded.
	reported := 0.5
	if _, err := s.InsertUsageEvent(now, "tailer", "sess1", "msg3", "/p",
		"claude-fable-5", 10, 10, 0, 0, &reported, "reported", "{}"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Row 4: NULL cost, NULL model and NULL cache token columns (raw SQL —
	// InsertUsageEvent always writes non-NULL ints and strings).
	if _, err := s.DB().Exec(`
		INSERT INTO usage_events (occurred_at, source, input_tokens, output_tokens, model)
		VALUES (?, 'tailer', 5, 5, NULL)
	`, FormatTime(now)); err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	// Row 5: NULL cost, known model, NULL cache columns — candidate whose
	// cache tokens must coalesce to 0.
	res, err := s.DB().Exec(`
		INSERT INTO usage_events (occurred_at, source, input_tokens, output_tokens, model)
		VALUES (?, 'tailer', 100, 200, 'claude-fable-5')
	`, FormatTime(now))
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	id5, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	events, err := s.NullCostEvents()
	if err != nil {
		t.Fatalf("NullCostEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(events), events)
	}

	e1 := events[0]
	if e1.ID != id1 || e1.Model != "claude-fable-5" {
		t.Errorf("event 1 mismatch: %+v", e1)
	}
	if e1.InputTokens != 1000 || e1.OutputTokens != 500 ||
		e1.CacheCreationTokens != 2000 || e1.CacheReadTokens != 10000 {
		t.Errorf("event 1 tokens mismatch: %+v", e1)
	}

	e5 := events[1]
	if e5.ID != id5 {
		t.Errorf("event 5 ID mismatch: got %d want %d", e5.ID, id5)
	}
	if e5.CacheCreationTokens != 0 || e5.CacheReadTokens != 0 {
		t.Errorf("NULL cache tokens should coalesce to 0: %+v", e5)
	}
}

func TestSetComputedCostOnlyUpdatesNullRows(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	id, err := s.InsertUsageEvent(now, "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 0, 0, nil, "", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated, err := s.SetComputedCost(id, 0.021)
	if err != nil {
		t.Fatalf("SetComputedCost: %v", err)
	}
	if !updated {
		t.Fatal("expected first SetComputedCost to update the row")
	}

	var cost float64
	var source string
	if err := s.DB().QueryRow(
		"SELECT cost_usd_equivalent, cost_source FROM usage_events WHERE id = ?", id,
	).Scan(&cost, &source); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if cost != 0.021 {
		t.Errorf("cost = %v, want 0.021", cost)
	}
	if source != "computed" {
		t.Errorf("cost_source = %q, want computed", source)
	}

	// Second call must be a no-op: the row is no longer NULL.
	updated, err = s.SetComputedCost(id, 99.0)
	if err != nil {
		t.Fatalf("SetComputedCost second call: %v", err)
	}
	if updated {
		t.Fatal("expected second SetComputedCost to be a no-op")
	}
	if err := s.DB().QueryRow(
		"SELECT cost_usd_equivalent FROM usage_events WHERE id = ?", id,
	).Scan(&cost); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if cost != 0.021 {
		t.Errorf("cost overwritten to %v; must stay 0.021", cost)
	}
}

func TestSetComputedCostNeverTouchesReportedRows(t *testing.T) {
	s := openTestStore(t)
	reported := 0.5
	id, err := s.InsertUsageEvent(time.Now(), "tailer", "s", "m1", "/p",
		"claude-fable-5", 1000, 500, 0, 0, &reported, "reported", "{}")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	updated, err := s.SetComputedCost(id, 0.021)
	if err != nil {
		t.Fatalf("SetComputedCost: %v", err)
	}
	if updated {
		t.Fatal("SetComputedCost must not touch a row with a reported cost")
	}

	var cost float64
	var source string
	if err := s.DB().QueryRow(
		"SELECT cost_usd_equivalent, cost_source FROM usage_events WHERE id = ?", id,
	).Scan(&cost, &source); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if cost != 0.5 || source != "reported" {
		t.Errorf("reported row changed: cost=%v source=%q", cost, source)
	}
}
