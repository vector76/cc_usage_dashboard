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
	{
		Version: 7,
		Name:    "add_quota_snapshots_fable_weekly_used",
		// Nullable REAL, 0–100. The claude.ai usage page grew a per-model
		// sub-row ("Fable") under the Weekly limits heading; this column
		// holds that row's percentage. NULL means the source did not report
		// it — every row written before this migration, and any row from a
		// userscript predating the extractor change. There is no backfill:
		// the value was never observed, so the dashboard's fable series
		// simply starts where the data does.
		//
		// Deliberately NOT a separate window kind. The Fable row shares the
		// weekly reset boundary ("Resets Thu 10:59 PM" on both rows), so it
		// rides the existing weekly window rather than minting its own.
		SQL: `
ALTER TABLE quota_snapshots ADD COLUMN fable_weekly_used REAL;
`,
	},
	{
		Version: 8,
		Name:    "create_uplink_cursor",
		// How far a sending trayapp has forwarded its usage_events to a
		// peer's POST /log. Keyed by the peer's base URL, not a singleton
		// row: retargeting the uplink then re-sends the backlog to the new
		// receiver, which is what you want — the new receiver holds none of
		// it, and the old one's UNIQUE(session_id, message_id) discards any
		// re-delivery. The table stays empty on a host-role trayapp.
		SQL: `
CREATE TABLE IF NOT EXISTS uplink_cursor (
	url TEXT PRIMARY KEY,
	last_event_id INTEGER NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
`,
	},
	{
		Version: 9,
		Name:    "add_usage_events_cache_creation_1h_tokens",
		// The 1-hour-TTL share of cache_creation_tokens, which stays the total
		// of all cache writes. 1h writes bill at 2x input against the 5m TTL's
		// 1.25x, and Claude Code writes almost entirely with the 1h TTL, so
		// pricing the whole total at the 5m rate undercounted cache writes by
		// 37.5%. NULL means the split is unknown and the row is priced as all
		// 5m, the pre-migration behavior.
		//
		// Existing rows recover the split from raw_json where it was kept (the
		// tailer stores the whole transcript line; the hook and uplink don't
		// send one). Rows that gained a split and carry a computed or ceiling
		// cost have that cost cleared, so ingest.BackfillCosts re-prices them
		// with the 1h rate at startup. Reported costs are measurements and are
		// left alone. The LIKE prefilter keeps json_valid/json_extract off the
		// rows that can't match, and json_valid keeps one malformed line from
		// aborting the migration.
		SQL: `
ALTER TABLE usage_events ADD COLUMN cache_creation_1h_tokens INTEGER;

UPDATE usage_events
SET cache_creation_1h_tokens = CAST(json_extract(raw_json, '$.message.usage.cache_creation.ephemeral_1h_input_tokens') AS INTEGER)
WHERE raw_json LIKE '%ephemeral_1h_input_tokens%'
  AND json_valid(raw_json)
  AND json_extract(raw_json, '$.message.usage.cache_creation.ephemeral_1h_input_tokens') IS NOT NULL;

UPDATE usage_events
SET cost_usd_equivalent = NULL, cost_source = ''
WHERE cache_creation_1h_tokens > 0
  AND cost_source IN ('computed', 'ceiling');
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
