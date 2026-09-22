package store

import (
	"database/sql"
	"testing"
	"time"
)

// A transcript line whose cache writes were 7000 tokens of 1h out of 7465.
const rawLineWith1h = `{"type":"assistant","sessionId":"s","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":7465,"cache_read_input_tokens":84151,"cache_creation":{"ephemeral_5m_input_tokens":465,"ephemeral_1h_input_tokens":7000}}}}`

func TestMigrateFromV8AddsCacheCreation1hTokens(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}
	defer db.Close()

	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:8]
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v1-v8 migrations: %v", err)
	}
	if columnExists(t, db, "usage_events", "cache_creation_1h_tokens") {
		t.Fatal("cache_creation_1h_tokens should not exist before migration")
	}

	now := FormatTime(time.Now())
	insert := func(msgID string, cost any, source, raw string) {
		t.Helper()
		if _, err := db.Exec(`
			INSERT INTO usage_events (occurred_at, source, session_id, message_id,
				input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
				cost_usd_equivalent, cost_source, model, raw_json)
			VALUES (?, 'tailer', 's', ?, 2, 5, 7465, 84151, ?, ?, 'claude-opus-5-5', ?)
		`, now, msgID, cost, source, raw); err != nil {
			t.Fatalf("insert %s: %v", msgID, err)
		}
	}
	// Priced at the 5m rate before the 1h split existed: must be re-priced.
	insert("computed-1h", 0.04, "computed", rawLineWith1h)
	// A ceiling guess with a 1h split: also re-priced (at the ceiling again).
	insert("ceiling-1h", 0.50, "ceiling", rawLineWith1h)
	// A reported cost is a measurement and is never touched.
	insert("reported-1h", 0.07, "reported", rawLineWith1h)
	// No split in the raw line (a hook row, or an old transcript): left alone.
	insert("computed-nosplit", 0.04, "computed", "")
	// Malformed raw_json that happens to mention the key must not abort the
	// migration.
	insert("garbage", 0.04, "computed", `not json ephemeral_1h_input_tokens`)

	migrations = saved
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v9 migration: %v", err)
	}

	type row struct {
		oneH   sql.NullInt64
		cost   sql.NullFloat64
		source sql.NullString
	}
	get := func(msgID string) row {
		t.Helper()
		var r row
		if err := db.QueryRow(`
			SELECT cache_creation_1h_tokens, cost_usd_equivalent, cost_source
			FROM usage_events WHERE message_id = ?`, msgID).Scan(&r.oneH, &r.cost, &r.source); err != nil {
			t.Fatalf("select %s: %v", msgID, err)
		}
		return r
	}

	for _, id := range []string{"computed-1h", "ceiling-1h"} {
		r := get(id)
		if r.oneH.Int64 != 7000 {
			t.Errorf("%s: 1h tokens = %+v, want 7000", id, r.oneH)
		}
		if r.cost.Valid {
			t.Errorf("%s: cost = %v, want NULL so BackfillCosts re-prices it", id, r.cost.Float64)
		}
	}

	r := get("reported-1h")
	if r.oneH.Int64 != 7000 {
		t.Errorf("reported-1h: 1h tokens = %+v, want 7000", r.oneH)
	}
	if !r.cost.Valid || r.cost.Float64 != 0.07 || r.source.String != "reported" {
		t.Errorf("reported-1h: cost=%+v source=%+v, want reported 0.07 untouched", r.cost, r.source)
	}

	for _, id := range []string{"computed-nosplit", "garbage"} {
		r := get(id)
		if r.oneH.Valid {
			t.Errorf("%s: 1h tokens = %d, want NULL (split unknown)", id, r.oneH.Int64)
		}
		if !r.cost.Valid || r.cost.Float64 != 0.04 {
			t.Errorf("%s: cost = %+v, want 0.04 untouched", id, r.cost)
		}
	}
}

func TestInsertUsageEventRecordStores1hTokens(t *testing.T) {
	s := openTestStore(t)
	cost := 0.1
	id, err := s.InsertUsageEventRecord(UsageEventRecord{
		OccurredAt:            time.Now(),
		Source:                "tailer",
		SessionID:             "s",
		MessageID:             "m",
		Model:                 "claude-opus-5-5",
		InputTokens:           2,
		OutputTokens:          5,
		CacheCreationTokens:   7465,
		CacheCreation1hTokens: 7000,
		CacheReadTokens:       84151,
		CostUSD:               &cost,
		CostSource:            "computed",
		RawJSON:               "{}",
	})
	if err != nil {
		t.Fatalf("InsertUsageEventRecord: %v", err)
	}
	var total, oneH int
	if err := s.DB().QueryRow(`SELECT cache_creation_tokens, cache_creation_1h_tokens FROM usage_events WHERE id = ?`, id).
		Scan(&total, &oneH); err != nil {
		t.Fatalf("select: %v", err)
	}
	if total != 7465 || oneH != 7000 {
		t.Errorf("total=%d 1h=%d, want 7465/7000", total, oneH)
	}
}

func TestNullCostEventsCarries1hTokens(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.InsertUsageEventRecord(UsageEventRecord{
		OccurredAt: time.Now(), Source: "tailer", SessionID: "s", MessageID: "m",
		Model: "claude-opus-5-5", CacheCreationTokens: 100, CacheCreation1hTokens: 80,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	events, err := s.NullCostEvents()
	if err != nil {
		t.Fatalf("NullCostEvents: %v", err)
	}
	if len(events) != 1 || events[0].CacheCreation1hTokens != 80 {
		t.Errorf("events = %+v, want one row with 80 1h tokens", events)
	}
}

func TestForwardableEventsCarry1hTokens(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.InsertUsageEventRecord(UsageEventRecord{
		OccurredAt: time.Now(), Source: "tailer", SessionID: "s", MessageID: "m",
		InputTokens: 1, CacheCreationTokens: 100, CacheCreation1hTokens: 80,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	events, err := s.ForwardableEventsAfter(0, 10)
	if err != nil {
		t.Fatalf("ForwardableEventsAfter: %v", err)
	}
	if len(events) != 1 || events[0].CacheCreation1hTokens != 80 {
		t.Errorf("events = %+v, want one row with 80 1h tokens", events)
	}
}
