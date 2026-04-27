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
	if version != 4 {
		t.Errorf("expected schema version 4, got %d", version)
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
		t.Fatalf("failed to apply v4 migration: %v", err)
	}

	if err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("failed to query schema version: %v", err)
	}
	if version != 4 {
		t.Errorf("expected schema version 4 after migrating, got %d", version)
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

func timePtr(t time.Time) *time.Time {
	return &t
}
