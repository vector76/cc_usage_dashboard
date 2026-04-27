// Package store provides the SQLite persistence layer for usage events and related data.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// FormatTime renders a time as an RFC3339Nano string in UTC. modernc.org/sqlite
// serializes time.Time via Go's default String() method
// ("2006-01-02 15:04:05.x +0000 UTC"), which SQLite's date functions
// (strftime, julianday, datetime) cannot parse. All call sites that pass a
// time.Time as a query parameter must funnel through this helper instead so
// that values land in the table as parseable RFC3339 strings.
func FormatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// FormatTimePtr is the *time.Time variant: nil maps to a typed nil so the
// driver writes SQL NULL instead of attempting to format a zero pointer.
func FormatTimePtr(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}

// Store provides access to the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens or creates a SQLite database at the given path and applies migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure database for reliability and performance
	if _, err := db.Exec(`
		PRAGMA foreign_keys = ON;
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA synchronous = NORMAL;
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to configure database: %w", err)
	}

	// Test that the database is writable
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database is not accessible: %w", err)
	}

	// Apply migrations
	if err := ApplyMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) to flush the WAL into
// the main DB file and shrink the -wal sidecar back to zero bytes.
// Called during shutdown so the on-disk DB is fully consolidated before
// the process exits.
func (s *Store) Checkpoint() error {
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("failed to run wal_checkpoint: %w", err)
	}
	return nil
}

// DB returns the underlying sql.DB for direct access when needed.
func (s *Store) DB() *sql.DB {
	return s.db
}

// InsertUsageEvent inserts a usage event and returns its ID.
func (s *Store) InsertUsageEvent(
	occurredAt time.Time,
	source string,
	sessionID, messageID, projectPath, model string,
	inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int,
	costUSD *float64,
	costSource, rawJSON string,
) (int64, error) {
	result, err := s.db.Exec(`
		INSERT INTO usage_events (
			occurred_at, source, session_id, message_id, project_path,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
			cost_usd_equivalent, cost_source, model, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, FormatTime(occurredAt), source, sessionID, messageID, projectPath,
		inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens,
		costUSD, costSource, model, rawJSON)

	if err != nil {
		return 0, fmt.Errorf("failed to insert usage event: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get inserted ID: %w", err)
	}

	return id, nil
}

// InsertQuotaSnapshot inserts a quota snapshot and returns its ID.
// session_used and weekly_used are 0–100 percentages.
// sessionActive is nil when the source did not report it; persisted as NULL.
func (s *Store) InsertQuotaSnapshot(
	observedAt, receivedAt time.Time,
	source string,
	sessionUsed *float64,
	sessionWindowEnds *time.Time,
	weeklyUsed *float64,
	weeklyWindowEnds *time.Time,
	sessionActive *bool,
	rawJSON string,
) (int64, error) {
	var sessionActiveArg interface{}
	if sessionActive != nil {
		if *sessionActive {
			sessionActiveArg = 1
		} else {
			sessionActiveArg = 0
		}
	}
	result, err := s.db.Exec(`
		INSERT INTO quota_snapshots (
			observed_at, received_at, source,
			session_used, session_window_ends,
			weekly_used, weekly_window_ends,
			session_active,
			raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, FormatTime(observedAt), FormatTime(receivedAt), source,
		sessionUsed, FormatTimePtr(sessionWindowEnds),
		weeklyUsed, FormatTimePtr(weeklyWindowEnds),
		sessionActiveArg,
		rawJSON)

	if err != nil {
		return 0, fmt.Errorf("failed to insert quota snapshot: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get inserted ID: %w", err)
	}

	return id, nil
}

// InsertParseError inserts a parse error record.
func (s *Store) InsertParseError(
	occurredAt time.Time,
	source, reason, payload string,
) (int64, error) {
	result, err := s.db.Exec(`
		INSERT INTO parse_errors (occurred_at, source, reason, payload)
		VALUES (?, ?, ?, ?)
	`, FormatTime(occurredAt), source, reason, payload)

	if err != nil {
		return 0, fmt.Errorf("failed to insert parse error: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get inserted ID: %w", err)
	}

	return id, nil
}

// PruneParseErrors removes parse errors older than the given duration,
// keeping only a summary of how many were deleted.
func (s *Store) PruneParseErrors(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan)
	_, err := s.db.Exec("DELETE FROM parse_errors WHERE occurred_at < ?", FormatTime(cutoff))
	if err != nil {
		return fmt.Errorf("failed to prune parse errors: %w", err)
	}
	return nil
}

// PruneSlackSamples removes slack samples older than the given duration.
func (s *Store) PruneSlackSamples(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan)
	_, err := s.db.Exec("DELETE FROM slack_samples WHERE sampled_at < ?", FormatTime(cutoff))
	if err != nil {
		return fmt.Errorf("failed to prune slack samples: %w", err)
	}
	return nil
}

// GetTailerOffset retrieves the last known byte offset for a transcript file.
// Returns 0 if no offset has been recorded (file is new).
func (s *Store) GetTailerOffset(filePath string) (int64, error) {
	var offset int64
	err := s.db.QueryRow("SELECT byte_offset FROM tailer_offsets WHERE file_path = ?", filePath).Scan(&offset)
	if err == sql.ErrNoRows {
		return 0, nil // No offset recorded yet
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get tailer offset: %w", err)
	}
	return offset, nil
}

// LoadAllTailerOffsets returns every persisted (file_path -> byte_offset)
// entry. Used by the tailer at startup to populate its in-memory map so
// previously-tracked files resume at the correct position.
func (s *Store) LoadAllTailerOffsets() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT file_path, byte_offset FROM tailer_offsets")
	if err != nil {
		return nil, fmt.Errorf("failed to load tailer offsets: %w", err)
	}
	defer rows.Close()

	offsets := make(map[string]int64)
	for rows.Next() {
		var path string
		var offset int64
		if err := rows.Scan(&path, &offset); err != nil {
			return nil, fmt.Errorf("failed to scan tailer offset row: %w", err)
		}
		offsets[path] = offset
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate tailer offsets: %w", err)
	}
	return offsets, nil
}

// SetTailerOffset records the byte offset for a transcript file.
func (s *Store) SetTailerOffset(filePath string, offset int64) error {
	_, err := s.db.Exec(`
		INSERT INTO tailer_offsets (file_path, byte_offset, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			byte_offset = excluded.byte_offset,
			updated_at = excluded.updated_at
	`, filePath, offset, FormatTime(time.Now()))

	if err != nil {
		return fmt.Errorf("failed to set tailer offset: %w", err)
	}
	return nil
}
