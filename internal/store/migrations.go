package store

import (
	"database/sql"
	"fmt"
)

// Migration represents a single schema migration.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

var migrations = []Migration{
	{
		Version: 1,
		Name:    "create_initial_schema",
		SQL: `
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS usage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	occurred_at TIMESTAMP NOT NULL,
	source TEXT NOT NULL,
	session_id TEXT,
	message_id TEXT,
	project_path TEXT,
	input_tokens INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL,
	cache_creation_tokens INTEGER,
	cache_read_tokens INTEGER,
	cost_usd_equivalent REAL,
	cost_source TEXT,
	model TEXT,
	raw_json TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_session_message ON usage_events (session_id, message_id)
	WHERE session_id IS NOT NULL AND message_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_usage_events_occurred_at ON usage_events (occurred_at);

CREATE TABLE IF NOT EXISTS quota_snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	observed_at TIMESTAMP NOT NULL,
	received_at TIMESTAMP NOT NULL,
	source TEXT NOT NULL,
	session_used REAL,
	session_window_ends TIMESTAMP,
	weekly_used REAL,
	weekly_window_ends TIMESTAMP,
	raw_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS windows (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	started_at TIMESTAMP NOT NULL,
	ends_at TIMESTAMP NOT NULL,
	baseline_total REAL,
	baseline_source TEXT,
	closed INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS slack_samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	sampled_at TIMESTAMP NOT NULL,
	slack_fraction REAL NOT NULL,
	window_id INTEGER NOT NULL,
	FOREIGN KEY (window_id) REFERENCES windows(id)
);

CREATE TABLE IF NOT EXISTS slack_releases (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	released_at TIMESTAMP NOT NULL,
	received_at TIMESTAMP NOT NULL,
	job_tag TEXT NOT NULL,
	estimated_cost REAL,
	slack_at_release REAL,
	window_id INTEGER NOT NULL,
	FOREIGN KEY (window_id) REFERENCES windows(id)
);

CREATE TABLE IF NOT EXISTS parse_errors (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	occurred_at TIMESTAMP NOT NULL,
	source TEXT NOT NULL,
	reason TEXT NOT NULL,
	payload TEXT NOT NULL
);
`,
	},
	{
		Version: 2,
		Name:    "create_tailer_offsets",
		SQL: `
CREATE TABLE IF NOT EXISTS tailer_offsets (
	file_path TEXT PRIMARY KEY,
	byte_offset INTEGER NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
`,
	},
	{
		Version: 3,
		Name:    "rename_windows_baseline_total",
		// The column historically held a dollar-denominated quota total;
		// it now holds the latest in-window snapshot's percent_used (0–100).
		// Rename to match what the value actually is. Requires SQLite 3.25+
		// (2018), which the modernc.org driver provides.
		SQL: `
ALTER TABLE windows RENAME COLUMN baseline_total TO baseline_percent_used;
`,
	},
	{
		Version: 4,
		Name:    "add_quota_snapshots_session_active",
		// Nullable INTEGER; NULL means the field was not reported by the source.
		SQL: `
ALTER TABLE quota_snapshots ADD COLUMN session_active INTEGER;
`,
	},
	{
		Version: 5,
		Name:    "add_quota_snapshots_continuous_with_prev",
		// Nullable INTEGER; NULL means absent — downstream consumers treat
		// NULL as "start"/"unknown" for safety.
		SQL: `
ALTER TABLE quota_snapshots ADD COLUMN continuous_with_prev INTEGER;
`,
	},
	{
		Version: 6,
		Name:    "add_quota_snapshots_weekly_active",
		// Nullable INTEGER; NULL means the field was not reported by the
		// source. Symmetric with session_active (migration v4) — see
		// docs/no-active-session.md.
		SQL: `
ALTER TABLE quota_snapshots ADD COLUMN weekly_active INTEGER;
`,
	},
}

// ApplyMigrations applies all pending migrations to the database.
func ApplyMigrations(db *sql.DB) error {
	// Create schema_version table if it doesn't exist
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	// Get the current schema version
	var currentVersion int
	err = db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("failed to query schema_version: %w", err)
	}

	// Apply pending migrations
	for _, m := range migrations {
		if m.Version <= currentVersion {
			continue
		}

		_, err := db.Exec(m.SQL)
		if err != nil {
			return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Name, err)
		}

		_, err = db.Exec("INSERT INTO schema_version (version) VALUES (?)", m.Version)
		if err != nil {
			return fmt.Errorf("failed to record migration %d: %w", m.Version, err)
		}
	}

	return nil
}
