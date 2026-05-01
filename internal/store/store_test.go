package store

import (
	"database/sql"
	"os"
	"testing"
	"time"
)

func TestOpen(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	// Test opening a new database
	store, err := Open(tmpFile.Name())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Verify schema version table exists
	var version int
	err = store.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
	if err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected schema version 6, got %d", version)
	}
}

func TestMigrateFromV3AddsSessionActive(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}
	defer db.Close()

	// Apply only migrations up through v3 to simulate an older database.
	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:3]
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v1-v3 migrations: %v", err)
	}

	// Confirm we're at v3 with no session_active column yet.
	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 3 {
		t.Fatalf("expected schema version 3, got %d", version)
	}
	if columnExists(t, db, "quota_snapshots", "session_active") {
		t.Fatalf("session_active column should not exist before migration")
	}

	// Restore the full set and run remaining migrations.
	migrations = saved
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v4+ migrations: %v", err)
	}

	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected schema version 6 after migrating, got %d", version)
	}
	if !columnExists(t, db, "quota_snapshots", "session_active") {
		t.Fatalf("session_active column missing after migration")
	}

	// Insert a row without session_active and confirm NULL is the default.
	now := time.Now()
	_, err = db.Exec(`
		INSERT INTO quota_snapshots (observed_at, received_at, source, raw_json)
		VALUES (?, ?, ?, ?)
	`, FormatTime(now), FormatTime(now), "userscript", "{}")
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	var sessionActive sql.NullInt64
	if err := db.QueryRow("SELECT session_active FROM quota_snapshots LIMIT 1").Scan(&sessionActive); err != nil {
		t.Fatalf("select failed: %v", err)
	}
	if sessionActive.Valid {
		t.Errorf("expected session_active to default to NULL, got %d", sessionActive.Int64)
	}
}

func TestMigrateFromV4AddsContinuousWithPrev(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}
	defer db.Close()

	// Apply only migrations up through v4 to simulate a database from before
	// the continuity flag landed.
	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:4]
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v1-v4 migrations: %v", err)
	}

	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 4 {
		t.Fatalf("expected schema version 4, got %d", version)
	}
	if columnExists(t, db, "quota_snapshots", "continuous_with_prev") {
		t.Fatalf("continuous_with_prev column should not exist before migration")
	}

	migrations = saved
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v5 migration: %v", err)
	}

	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected schema version 6 after migrating, got %d", version)
	}
	if !columnExists(t, db, "quota_snapshots", "continuous_with_prev") {
		t.Fatalf("continuous_with_prev column missing after migration")
	}

	// Insert a row without continuous_with_prev and confirm NULL is the default.
	now := time.Now()
	_, err = db.Exec(`
		INSERT INTO quota_snapshots (observed_at, received_at, source, raw_json)
		VALUES (?, ?, ?, ?)
	`, FormatTime(now), FormatTime(now), "userscript", "{}")
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	var continuousWithPrev sql.NullInt64
	if err := db.QueryRow("SELECT continuous_with_prev FROM quota_snapshots LIMIT 1").Scan(&continuousWithPrev); err != nil {
		t.Fatalf("select failed: %v", err)
	}
	if continuousWithPrev.Valid {
		t.Errorf("expected continuous_with_prev to default to NULL, got %d", continuousWithPrev.Int64)
	}
}

func TestMigrateFromV5AddsWeeklyActive(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}
	defer db.Close()

	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:5]
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v1-v5 migrations: %v", err)
	}

	var version int
	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 5 {
		t.Fatalf("expected schema version 5, got %d", version)
	}
	if columnExists(t, db, "quota_snapshots", "weekly_active") {
		t.Fatalf("weekly_active column should not exist before migration")
	}

	migrations = saved
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v6 migration: %v", err)
	}

	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected schema version 6 after migrating, got %d", version)
	}
	if !columnExists(t, db, "quota_snapshots", "weekly_active") {
		t.Fatalf("weekly_active column missing after migration")
	}

	now := time.Now()
	_, err = db.Exec(`
		INSERT INTO quota_snapshots (observed_at, received_at, source, raw_json)
		VALUES (?, ?, ?, ?)
	`, FormatTime(now), FormatTime(now), "userscript", "{}")
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	var weeklyActive sql.NullInt64
	if err := db.QueryRow("SELECT weekly_active FROM quota_snapshots LIMIT 1").Scan(&weeklyActive); err != nil {
		t.Fatalf("select failed: %v", err)
	}
	if weeklyActive.Valid {
		t.Errorf("expected weekly_active to default to NULL, got %d", weeklyActive.Int64)
	}
}

func TestFreshDatabaseHasSessionActiveColumn(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}
	defer db.Close()

	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("ApplyMigrations failed: %v", err)
	}
	if !columnExists(t, db, "quota_snapshots", "session_active") {
		t.Fatalf("session_active column missing on fresh database")
	}
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("PRAGMA table_info failed: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func TestMigrations(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := Open(tmpFile.Name())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	// Check that all tables exist
	tables := []string{
		"schema_version", "usage_events", "quota_snapshots",
		"windows", "slack_samples", "slack_releases", "parse_errors",
	}

	for _, table := range tables {
		var exists int
		err := store.db.QueryRow(`
			SELECT COUNT(1) FROM sqlite_master
			WHERE type='table' AND name=?
		`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("failed to check table %s: %v", table, err)
		}
		if exists != 1 {
			t.Errorf("table %s does not exist", table)
		}
	}
}

func TestInsertUsageEvent(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	occurredAt := time.Now()
	id, err := store.InsertUsageEvent(
		occurredAt,
		"cli",
		"session-123",
		"msg-456",
		"/path/to/project",
		"claude-3-5-sonnet-20241022",
		1000, 500, 100, 50,
		floatPtr(0.05),
		"computed",
		`{"input_tokens": 1000}`,
	)

	if err != nil {
		t.Fatalf("InsertUsageEvent failed: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	// Verify the event was inserted
	var inputTokens int
	var costSource string
	var retrievedID int64
	err = store.db.QueryRow(`
		SELECT id, input_tokens, cost_source FROM usage_events WHERE id = ?
	`, id).Scan(&retrievedID, &inputTokens, &costSource)
	if err != nil {
		t.Fatalf("failed to retrieve event: %v", err)
	}
	if inputTokens != 1000 {
		t.Errorf("expected 1000 input tokens, got %d", inputTokens)
	}
	if costSource != "computed" {
		t.Errorf("expected cost_source 'computed', got %s", costSource)
	}
}

func TestUsageEventUniqueness(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	occurredAt := time.Now()
	sessionID := "session-123"
	messageID := "msg-456"

	// Insert first event
	_, err := store.InsertUsageEvent(
		occurredAt, "cli", sessionID, messageID, "/path",
		"claude-3-5-sonnet-20241022",
		1000, 500, 0, 0, nil, "", "",
	)
	if err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	// Try to insert duplicate
	_, err = store.InsertUsageEvent(
		occurredAt.Add(1*time.Second), "cli", sessionID, messageID, "/path",
		"claude-3-5-sonnet-20241022",
		2000, 1000, 0, 0, nil, "", "",
	)

	if err == nil {
		t.Error("expected UNIQUE constraint violation, got nil")
	}
}

func TestInsertQuotaSnapshot(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	observedAt := time.Now()
	receivedAt := time.Now().Add(1 * time.Second)

	id, err := store.InsertQuotaSnapshot(
		observedAt, receivedAt,
		"userscript",
		floatPtr(25.0), timePtr(time.Now().Add(5*time.Hour)),
		floatPtr(40.0), timePtr(time.Now().Add(7*24*time.Hour)),
		nil,
		nil,
		nil,
		`{"session_used": 25.0}`,
	)

	if err != nil {
		t.Fatalf("InsertQuotaSnapshot failed: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	// Verify insertion
	var sessionUsed float64
	err = store.db.QueryRow(`
		SELECT session_used FROM quota_snapshots WHERE id = ?
	`, id).Scan(&sessionUsed)
	if err != nil {
		t.Fatalf("failed to retrieve snapshot: %v", err)
	}
	if sessionUsed != 25.0 {
		t.Errorf("expected 25.0, got %f", sessionUsed)
	}
}

func TestInsertQuotaSnapshotSessionActive(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	cases := []struct {
		name  string
		input *bool
		want  sql.NullInt64
	}{
		{"true", boolPtr(true), sql.NullInt64{Int64: 1, Valid: true}},
		{"false", boolPtr(false), sql.NullInt64{Int64: 0, Valid: true}},
		{"nil", nil, sql.NullInt64{Valid: false}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observedAt := time.Now()
			id, err := store.InsertQuotaSnapshot(
				observedAt, observedAt,
				"userscript",
				nil, nil,
				nil, nil,
				tc.input,
				nil,
				nil,
				"{}",
			)
			if err != nil {
				t.Fatalf("InsertQuotaSnapshot failed: %v", err)
			}

			var got sql.NullInt64
			if err := store.db.QueryRow(
				`SELECT session_active FROM quota_snapshots WHERE id = ?`, id,
			).Scan(&got); err != nil {
				t.Fatalf("select failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("session_active: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInsertQuotaSnapshotWeeklyActive(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	cases := []struct {
		name  string
		input *bool
		want  sql.NullInt64
	}{
		{"true", boolPtr(true), sql.NullInt64{Int64: 1, Valid: true}},
		{"false", boolPtr(false), sql.NullInt64{Int64: 0, Valid: true}},
		{"nil", nil, sql.NullInt64{Valid: false}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observedAt := time.Now()
			id, err := store.InsertQuotaSnapshot(
				observedAt, observedAt,
				"userscript",
				nil, nil,
				nil, nil,
				nil,
				tc.input,
				nil,
				"{}",
			)
			if err != nil {
				t.Fatalf("InsertQuotaSnapshot failed: %v", err)
			}

			var got sql.NullInt64
			if err := store.db.QueryRow(
				`SELECT weekly_active FROM quota_snapshots WHERE id = ?`, id,
			).Scan(&got); err != nil {
				t.Fatalf("select failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("weekly_active: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInsertQuotaSnapshotContinuousWithPrev(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	cases := []struct {
		name  string
		input *bool
		want  sql.NullInt64
	}{
		{"true", boolPtr(true), sql.NullInt64{Int64: 1, Valid: true}},
		{"false", boolPtr(false), sql.NullInt64{Int64: 0, Valid: true}},
		{"nil", nil, sql.NullInt64{Valid: false}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observedAt := time.Now()
			id, err := store.InsertQuotaSnapshot(
				observedAt, observedAt,
				"userscript",
				nil, nil,
				nil, nil,
				nil,
				nil,
				tc.input,
				"{}",
			)
			if err != nil {
				t.Fatalf("InsertQuotaSnapshot failed: %v", err)
			}

			var got sql.NullInt64
			if err := store.db.QueryRow(
				`SELECT continuous_with_prev FROM quota_snapshots WHERE id = ?`, id,
			).Scan(&got); err != nil {
				t.Fatalf("select failed: %v", err)
			}
			if got != tc.want {
				t.Errorf("continuous_with_prev: got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestInsertQuotaSnapshotSlideCollapsesPlateau(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	sessionEnds := base.Add(5 * time.Hour)
	weeklyEnds := base.Add(7 * 24 * time.Hour)

	// Start row: explicit-false continuity, never slid through.
	startID, err := store.InsertQuotaSnapshot(
		base, base, "userscript",
		floatPtr(25.0), &sessionEnds,
		floatPtr(40.0), &weeklyEnds,
		boolPtr(true),
		nil,
		boolPtr(false),
		`{"start":1}`,
	)
	if err != nil {
		t.Fatalf("insert start: %v", err)
	}

	// First continuation: identical match fields. Slide is suppressed
	// because the prior row is an explicit start, so a new row is added.
	firstContObserved := base.Add(1 * time.Minute)
	firstContID, err := store.InsertQuotaSnapshot(
		firstContObserved, firstContObserved, "userscript",
		floatPtr(25.0), &sessionEnds,
		floatPtr(40.0), &weeklyEnds,
		boolPtr(true),
		nil,
		boolPtr(true),
		`{"first_continuation":1}`,
	)
	if err != nil {
		t.Fatalf("insert first continuation: %v", err)
	}
	if firstContID == startID {
		t.Fatalf("first continuation should not slide over start row")
	}

	// Four further continuations should all slide over the first.
	var lastObserved, lastReceived time.Time
	for i := 2; i <= 5; i++ {
		observed := base.Add(time.Duration(i) * time.Minute)
		received := observed.Add(2 * time.Second)
		id, err := store.InsertQuotaSnapshot(
			observed, received, "userscript",
			floatPtr(25.0), &sessionEnds,
			floatPtr(40.0), &weeklyEnds,
			boolPtr(true),
			nil,
			boolPtr(true),
			`{"later_continuation":`+time.Duration(i).String()+`}`,
		)
		if err != nil {
			t.Fatalf("insert continuation %d: %v", i, err)
		}
		if id != firstContID {
			t.Fatalf("continuation %d should slide onto id=%d, got %d", i, firstContID, id)
		}
		lastObserved, lastReceived = observed, received
	}

	var rowCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("expected 2 rows after plateau, got %d", rowCount)
	}

	var observedStr, receivedStr, rawJSON string
	if err := store.db.QueryRow(`
		SELECT observed_at, received_at, raw_json FROM quota_snapshots WHERE id = ?
	`, firstContID).Scan(&observedStr, &receivedStr, &rawJSON); err != nil {
		t.Fatalf("select slid row: %v", err)
	}
	if observedStr != FormatTime(lastObserved) {
		t.Errorf("observed_at: got %s, want %s", observedStr, FormatTime(lastObserved))
	}
	if receivedStr != FormatTime(lastReceived) {
		t.Errorf("received_at: got %s, want %s", receivedStr, FormatTime(lastReceived))
	}
	if rawJSON != `{"first_continuation":1}` {
		t.Errorf("raw_json should be preserved as the first continuation's, got %s", rawJSON)
	}
}

func TestInsertQuotaSnapshotSlideSuppressedWhenValuesDiffer(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	sessionEnds := base.Add(5 * time.Hour)
	weeklyEnds := base.Add(7 * 24 * time.Hour)

	if _, err := store.InsertQuotaSnapshot(
		base, base, "userscript",
		floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
		boolPtr(true), nil, boolPtr(false), `{}`,
	); err != nil {
		t.Fatalf("insert start: %v", err)
	}
	for i := 1; i <= 3; i++ {
		observed := base.Add(time.Duration(i) * time.Minute)
		if _, err := store.InsertQuotaSnapshot(
			observed, observed, "userscript",
			floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
			boolPtr(true), nil, boolPtr(true), `{}`,
		); err != nil {
			t.Fatalf("insert continuation %d: %v", i, err)
		}
	}

	// Different session_used + continuous=true → slide suppressed.
	observed := base.Add(10 * time.Minute)
	if _, err := store.InsertQuotaSnapshot(
		observed, observed, "userscript",
		floatPtr(26.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
		boolPtr(true), nil, boolPtr(true), `{}`,
	); err != nil {
		t.Fatalf("insert differing continuation: %v", err)
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 rows after differing continuation, got %d", count)
	}
}

func TestInsertQuotaSnapshotSlideDoesNotCrossStart(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	sessionEnds := base.Add(5 * time.Hour)
	weeklyEnds := base.Add(7 * 24 * time.Hour)

	if _, err := store.InsertQuotaSnapshot(
		base, base, "userscript",
		floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
		boolPtr(true), nil, boolPtr(false), `{}`,
	); err != nil {
		t.Fatalf("insert start: %v", err)
	}
	for i := 1; i <= 3; i++ {
		observed := base.Add(time.Duration(i) * time.Minute)
		if _, err := store.InsertQuotaSnapshot(
			observed, observed, "userscript",
			floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
			boolPtr(true), nil, boolPtr(true), `{}`,
		); err != nil {
			t.Fatalf("insert continuation %d: %v", i, err)
		}
	}

	// All match fields identical, but continuous_with_prev=false marks a
	// fresh start. The slide must NOT cross it.
	newStartObserved := base.Add(10 * time.Minute)
	newStartID, err := store.InsertQuotaSnapshot(
		newStartObserved, newStartObserved, "userscript",
		floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
		boolPtr(true), nil, boolPtr(false), `{"new_start":1}`,
	)
	if err != nil {
		t.Fatalf("insert new start: %v", err)
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 rows after new start, got %d", count)
	}

	var latestID int64
	if err := store.db.QueryRow(`
		SELECT id FROM quota_snapshots ORDER BY observed_at DESC LIMIT 1
	`).Scan(&latestID); err != nil {
		t.Fatalf("select latest: %v", err)
	}
	if latestID != newStartID {
		t.Fatalf("latest row should be the new start (id=%d), got id=%d", newStartID, latestID)
	}
}

func TestInsertQuotaSnapshotSlideSuppressedPerField(t *testing.T) {
	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	sessionEnds := base.Add(5 * time.Hour)
	weeklyEnds := base.Add(7 * 24 * time.Hour)

	cases := []struct {
		name string
		// mutate produces the new arrival's match fields starting from the
		// plateau values.
		mutate func() (sUsed *float64, sEnds *time.Time, wUsed *float64, wEnds *time.Time, sActive, wActive *bool)
	}{
		{
			name: "session_used",
			mutate: func() (*float64, *time.Time, *float64, *time.Time, *bool, *bool) {
				return floatPtr(26.0), &sessionEnds, floatPtr(40.0), &weeklyEnds, boolPtr(true), boolPtr(true)
			},
		},
		{
			name: "weekly_used",
			mutate: func() (*float64, *time.Time, *float64, *time.Time, *bool, *bool) {
				return floatPtr(25.0), &sessionEnds, floatPtr(41.0), &weeklyEnds, boolPtr(true), boolPtr(true)
			},
		},
		{
			name: "session_window_ends",
			mutate: func() (*float64, *time.Time, *float64, *time.Time, *bool, *bool) {
				newEnds := sessionEnds.Add(1 * time.Minute)
				return floatPtr(25.0), &newEnds, floatPtr(40.0), &weeklyEnds, boolPtr(true), boolPtr(true)
			},
		},
		{
			name: "weekly_window_ends",
			mutate: func() (*float64, *time.Time, *float64, *time.Time, *bool, *bool) {
				newEnds := weeklyEnds.Add(1 * time.Minute)
				return floatPtr(25.0), &sessionEnds, floatPtr(40.0), &newEnds, boolPtr(true), boolPtr(true)
			},
		},
		{
			name: "session_active",
			mutate: func() (*float64, *time.Time, *float64, *time.Time, *bool, *bool) {
				return floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds, boolPtr(false), boolPtr(true)
			},
		},
		{
			name: "weekly_active",
			mutate: func() (*float64, *time.Time, *float64, *time.Time, *bool, *bool) {
				return floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds, boolPtr(true), boolPtr(false)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := createTestStore(t)
			defer store.Close()

			if _, err := store.InsertQuotaSnapshot(
				base, base, "userscript",
				floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
				boolPtr(true), boolPtr(true), boolPtr(false), `{}`,
			); err != nil {
				t.Fatalf("insert start: %v", err)
			}
			if _, err := store.InsertQuotaSnapshot(
				base.Add(1*time.Minute), base.Add(1*time.Minute), "userscript",
				floatPtr(25.0), &sessionEnds, floatPtr(40.0), &weeklyEnds,
				boolPtr(true), boolPtr(true), boolPtr(true), `{}`,
			); err != nil {
				t.Fatalf("insert plateau seed: %v", err)
			}

			sUsed, sEnds, wUsed, wEnds, sActive, wActive := tc.mutate()
			if _, err := store.InsertQuotaSnapshot(
				base.Add(2*time.Minute), base.Add(2*time.Minute), "userscript",
				sUsed, sEnds, wUsed, wEnds,
				sActive, wActive, boolPtr(true), `{}`,
			); err != nil {
				t.Fatalf("insert differing continuation: %v", err)
			}

			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
				t.Fatalf("count rows: %v", err)
			}
			if count != 3 {
				t.Errorf("differing %s should suppress slide; expected 3 rows, got %d", tc.name, count)
			}
		})
	}
}

func TestInsertQuotaSnapshotContinuationOnColdDB(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	id, err := store.InsertQuotaSnapshot(
		now, now, "userscript",
		floatPtr(25.0), nil,
		floatPtr(40.0), nil,
		nil,
		nil,
		boolPtr(true),
		`{}`,
	)
	if err != nil {
		t.Fatalf("insert into empty DB: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected non-zero id")
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row on cold DB insert, got %d", count)
	}
}

func TestInsertQuotaSnapshotSlideScopedBySource(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	sessionEnds := base.Add(5 * time.Hour)

	// Plateau on source A.
	if _, err := store.InsertQuotaSnapshot(
		base, base, "userscript",
		floatPtr(25.0), &sessionEnds, nil, nil,
		nil, nil, boolPtr(false), `{}`,
	); err != nil {
		t.Fatalf("insert A start: %v", err)
	}
	if _, err := store.InsertQuotaSnapshot(
		base.Add(1*time.Minute), base.Add(1*time.Minute), "userscript",
		floatPtr(25.0), &sessionEnds, nil, nil,
		nil, nil, boolPtr(true), `{}`,
	); err != nil {
		t.Fatalf("insert A continuation: %v", err)
	}

	// A continuation from a *different* source must not slide on A's
	// plateau even with identical match fields.
	if _, err := store.InsertQuotaSnapshot(
		base.Add(2*time.Minute), base.Add(2*time.Minute), "headless",
		floatPtr(25.0), &sessionEnds, nil, nil,
		nil, nil, boolPtr(true), `{}`,
	); err != nil {
		t.Fatalf("insert B continuation: %v", err)
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 rows (slide must be source-scoped), got %d", count)
	}
}

func TestInsertParseError(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	occurredAt := time.Now()

	id, err := store.InsertParseError(
		occurredAt,
		"tailer",
		"malformed JSON",
		`{"bad": json}`,
	)

	if err != nil {
		t.Fatalf("InsertParseError failed: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero ID")
	}

	// Verify insertion
	var reason string
	err = store.db.QueryRow(`
		SELECT reason FROM parse_errors WHERE id = ?
	`, id).Scan(&reason)
	if err != nil {
		t.Fatalf("failed to retrieve error: %v", err)
	}
	if reason != "malformed JSON" {
		t.Errorf("expected 'malformed JSON', got %s", reason)
	}
}

func TestPruneParseErrors(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	now := time.Now()

	// Insert an old error
	store.db.Exec(
		"INSERT INTO parse_errors (occurred_at, source, reason, payload) VALUES (?, ?, ?, ?)",
		FormatTime(now.Add(-40*24*time.Hour)), "tailer", "old error", "payload",
	)

	// Insert a recent error
	store.db.Exec(
		"INSERT INTO parse_errors (occurred_at, source, reason, payload) VALUES (?, ?, ?, ?)",
		FormatTime(now.Add(-10*24*time.Hour)), "tailer", "recent error", "payload",
	)

	// Prune errors older than 30 days
	err := store.PruneParseErrors(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneParseErrors failed: %v", err)
	}

	// Verify old error is gone
	var count int
	store.db.QueryRow("SELECT COUNT(*) FROM parse_errors WHERE reason = 'old error'").Scan(&count)
	if count != 0 {
		t.Error("expected old error to be pruned")
	}

	// Verify recent error remains
	store.db.QueryRow("SELECT COUNT(*) FROM parse_errors WHERE reason = 'recent error'").Scan(&count)
	if count != 1 {
		t.Error("expected recent error to remain")
	}
}

func TestPruneSlackSamples(t *testing.T) {
	store := createTestStore(t)
	defer store.Close()

	now := time.Now()

	// Create a window to reference
	result, err := store.db.Exec(`
		INSERT INTO windows (kind, started_at, ends_at) VALUES (?, ?, ?)
	`, "session", FormatTime(now), FormatTime(now.Add(5*time.Hour)))
	if err != nil {
		t.Fatalf("failed to create window: %v", err)
	}
	windowID, _ := result.LastInsertId()

	// Insert an old sample
	store.db.Exec(`
		INSERT INTO slack_samples (sampled_at, slack_fraction, window_id) VALUES (?, ?, ?)
	`, FormatTime(now.Add(-100*24*time.Hour)), 0.5, windowID)

	// Insert a recent sample
	store.db.Exec(`
		INSERT INTO slack_samples (sampled_at, slack_fraction, window_id) VALUES (?, ?, ?)
	`, FormatTime(now.Add(-10*24*time.Hour)), 0.3, windowID)

	// Prune samples older than 90 days
	err = store.PruneSlackSamples(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneSlackSamples failed: %v", err)
	}

	// Verify old sample is gone
	var count int
	store.db.QueryRow("SELECT COUNT(*) FROM slack_samples WHERE slack_fraction = 0.5").Scan(&count)
	if count != 0 {
		t.Error("expected old sample to be pruned")
	}

	// Verify recent sample remains
	store.db.QueryRow("SELECT COUNT(*) FROM slack_samples WHERE slack_fraction = 0.3").Scan(&count)
	if count != 1 {
		t.Error("expected recent sample to remain")
	}
}

// Helper functions
func createTestStore(t *testing.T) *Store {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}

	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	return &Store{db: db}
}

func floatPtr(f float64) *float64 {
	return &f
}

func boolPtr(b bool) *bool {
	return &b
}

func timePtr(t time.Time) *time.Time {
	return &t
}
