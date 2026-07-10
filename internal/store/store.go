// Package store provides the SQLite persistence layer for usage events and related data.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// IsUniqueConstraintViolation matches the modernc.org/sqlite error message
// for SQLITE_CONSTRAINT_UNIQUE (extended code 2067). The driver doesn't
// expose a sentinel error, so we string-match — narrow enough to catch
// only this case and not other constraint failures. Shared by every
// ingest path (HTTP /log, the tailer) that inserts into usage_events and
// treats a (session_id, message_id) collision as idempotent re-delivery
// rather than a real error.
func IsUniqueConstraintViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// FormatTime renders a time as an RFC3339 string in UTC with a fixed-width
// 9-digit fractional second. modernc.org/sqlite serializes time.Time via Go's
// default String() method ("2006-01-02 15:04:05.x +0000 UTC"), which SQLite's
// date functions (strftime, julianday, datetime) cannot parse. All call sites
// that pass a time.Time as a query parameter must funnel through this helper
// instead so that values land in the table as parseable RFC3339 strings.
//
// The fraction must be fixed-width because timestamps are compared as TEXT in
// SQL: RFC3339Nano trims trailing zeros, and ".53Z" sorts lexicographically
// after ".531Z" ('Z' > '1'), breaking range queries. That was invisible on
// Linux (nanosecond clock jitter makes trailing zeros vanishingly rare) but
// routine on Windows, whose clock ticks in ~0.5ms steps.
func FormatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
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
	// Configure the connection via DSN _pragma params rather than a one-time
	// db.Exec. database/sql maintains a POOL of connections and opens new ones
	// lazily under concurrent load; a PRAGMA issued through db.Exec lands on
	// only ONE pooled connection. Connection-scoped pragmas — crucially
	// busy_timeout — would then be missing on every other connection, so a
	// second concurrent writer (e.g. the windows ticker racing the snapshot
	// POST handler) hits busy_timeout=0 and fails immediately with SQLITE_BUSY
	// instead of waiting. modernc.org/sqlite runs each _pragma on every new
	// connection (busy_timeout first), so this applies pool-wide.
	//
	// The path is a bare filesystem path (no "file:" scheme), so modernc splits
	// on the first '?' and uses the left side verbatim as the filename — Windows
	// backslashes and drive letters are not URL-decoded. journal_mode is a
	// persistent header setting; repeating it per connection is a harmless no-op.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
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
// sessionActive / weeklyActive are nil when the source did not report them;
// persisted as NULL. continuousWithPrev is nil when absent; persisted as NULL.
//
// Plateau compaction: when the arrival is marked continuous and every
// "match" field is identical to the latest row from the same source, the
// existing row's observed_at and received_at are slid forward in place
// instead of inserting a duplicate. raw_json is preserved on the surviving
// row as an audit artifact. The slide is suppressed when the latest row is
// itself an explicit start (continuous_with_prev = 0), so a fresh page
// load always anchors a new row.
func (s *Store) InsertQuotaSnapshot(
	observedAt, receivedAt time.Time,
	source string,
	sessionUsed *float64,
	sessionWindowEnds *time.Time,
	weeklyUsed *float64,
	weeklyWindowEnds *time.Time,
	sessionActive *bool,
	weeklyActive *bool,
	continuousWithPrev *bool,
	rawJSON string,
) (int64, error) {
	sessionActiveArg := boolToNullableInt(sessionActive)
	weeklyActiveArg := boolToNullableInt(weeklyActive)
	continuousWithPrevArg := boolToNullableInt(continuousWithPrev)

	if continuousWithPrev != nil && *continuousWithPrev {
		slidID, slid, err := s.tryPlateauSlide(
			observedAt, receivedAt, source,
			sessionUsed, sessionWindowEnds,
			weeklyUsed, weeklyWindowEnds,
			sessionActive, weeklyActive,
		)
		if err != nil {
			return 0, err
		}
		if slid {
			return slidID, nil
		}
	}

	result, err := s.db.Exec(`
		INSERT INTO quota_snapshots (
			observed_at, received_at, source,
			session_used, session_window_ends,
			weekly_used, weekly_window_ends,
			session_active,
			weekly_active,
			continuous_with_prev,
			raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, FormatTime(observedAt), FormatTime(receivedAt), source,
		sessionUsed, FormatTimePtr(sessionWindowEnds),
		weeklyUsed, FormatTimePtr(weeklyWindowEnds),
		sessionActiveArg,
		weeklyActiveArg,
		continuousWithPrevArg,
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

// tryPlateauSlide refreshes the latest row's timestamps in place when the
// new arrival continues an identical plateau. Returns (id, true, nil) if a
// slide happened; (0, false, nil) means the caller should insert as usual.
func (s *Store) tryPlateauSlide(
	observedAt, receivedAt time.Time,
	source string,
	sessionUsed *float64,
	sessionWindowEnds *time.Time,
	weeklyUsed *float64,
	weeklyWindowEnds *time.Time,
	sessionActive *bool,
	weeklyActive *bool,
) (int64, bool, error) {
	var (
		prevID                 int64
		prevSessionUsed        sql.NullFloat64
		prevWeeklyUsed         sql.NullFloat64
		prevSessionWindowEnds  sql.NullString
		prevWeeklyWindowEnds   sql.NullString
		prevSessionActive      sql.NullInt64
		prevWeeklyActive       sql.NullInt64
		prevContinuousWithPrev sql.NullInt64
	)
	err := s.db.QueryRow(`
		SELECT id, session_used, weekly_used,
		       session_window_ends, weekly_window_ends,
		       session_active, weekly_active, continuous_with_prev
		FROM quota_snapshots
		WHERE source = ?
		ORDER BY observed_at DESC
		LIMIT 1
	`, source).Scan(
		&prevID, &prevSessionUsed, &prevWeeklyUsed,
		&prevSessionWindowEnds, &prevWeeklyWindowEnds,
		&prevSessionActive, &prevWeeklyActive, &prevContinuousWithPrev,
	)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to read latest snapshot for slide: %w", err)
	}

	// Don't slide on top of a row that is itself an explicit start.
	if prevContinuousWithPrev.Valid && prevContinuousWithPrev.Int64 == 0 {
		return 0, false, nil
	}

	if !nullableFloatEqual(prevSessionUsed, sessionUsed) ||
		!nullableFloatEqual(prevWeeklyUsed, weeklyUsed) ||
		!nullableTimeEqual(prevSessionWindowEnds, sessionWindowEnds) ||
		!nullableTimeEqual(prevWeeklyWindowEnds, weeklyWindowEnds) ||
		!nullableBoolEqual(prevSessionActive, sessionActive) ||
		!nullableBoolEqual(prevWeeklyActive, weeklyActive) {
		return 0, false, nil
	}

	if _, err := s.db.Exec(`
		UPDATE quota_snapshots
		SET observed_at = ?, received_at = ?
		WHERE id = ?
	`, FormatTime(observedAt), FormatTime(receivedAt), prevID); err != nil {
		return 0, false, fmt.Errorf("failed to slide quota snapshot: %w", err)
	}
	return prevID, true, nil
}

func nullableFloatEqual(a sql.NullFloat64, b *float64) bool {
	if a.Valid != (b != nil) {
		return false
	}
	if !a.Valid {
		return true
	}
	return a.Float64 == *b
}

// nullableTimeEqual compares a timestamp read back from the DB with an
// in-memory time. The comparison must be on parsed instants, not raw strings:
// the TIMESTAMP decltype makes the driver hand the column back as a time.Time,
// which database/sql re-renders into a string via RFC3339Nano — a trailing-
// zero-trimmed format that need not byte-match what FormatTime stored.
func nullableTimeEqual(a sql.NullString, b *time.Time) bool {
	if a.Valid != (b != nil) {
		return false
	}
	if !a.Valid {
		return true
	}
	return parseStoredTime(a.String).Equal(*b)
}

// boolToNullableInt converts a *bool to the interface{} that database/sql
// stores as a nullable INTEGER (NULL when nil, 0 for false, 1 for true).
func boolToNullableInt(b *bool) interface{} {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

func nullableBoolEqual(a sql.NullInt64, b *bool) bool {
	if a.Valid != (b != nil) {
		return false
	}
	if !a.Valid {
		return true
	}
	var bv int64
	if *b {
		bv = 1
	}
	return a.Int64 == bv
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

// ParseError is one row from the parse_errors table, surfaced to the
// dashboard's feedback panel via GET /api/feedback.
type ParseError struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	Source     string    `json:"source"`
	Reason     string    `json:"reason"`
	Payload    string    `json:"payload"`
}

// RecentParseErrors returns the most recent parse errors, newest first, up to
// limit rows. A limit <= 0 is coerced to 50. The returned slice is never nil.
//
// occurred_at is scanned via parseStoredTime because the column has been
// written in two formats historically (RFC3339 via FormatTime and Go's native
// time.Time string from raw driver serialization); parsing leniently keeps the
// endpoint robust across both.
func (s *Store) RecentParseErrors(limit int) ([]ParseError, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, occurred_at, source, reason, payload
		FROM parse_errors
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query parse errors: %w", err)
	}
	defer rows.Close()

	out := []ParseError{}
	for rows.Next() {
		var pe ParseError
		var occ string
		if err := rows.Scan(&pe.ID, &occ, &pe.Source, &pe.Reason, &pe.Payload); err != nil {
			return nil, fmt.Errorf("failed to scan parse error: %w", err)
		}
		pe.OccurredAt = parseStoredTime(occ)
		out = append(out, pe)
	}
	return out, rows.Err()
}

// parseStoredTime leniently parses a timestamp string as stored in the DB.
// Returns the zero time when no known layout matches rather than erroring, so a
// single malformed row can't fail an entire read.
func parseStoredTime(s string) time.Time {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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

// DeleteAllTailerOffsets wipes every persisted (file_path -> byte_offset)
// row, forcing the next poll of every tracked transcript to re-read from
// byte 0 (GetTailerOffset returns 0 for a path with no row). This is the
// recovery-only bulk reimport path: a byte offset only records how far a
// file was *read*, not whether the lines in that range actually produced a
// usage_event — a parser bug (or any other silent extraction failure) can
// leave an offset sitting at EOF for content that was never successfully
// recorded, and there is no per-line "was this ingested" marker to repair
// selectively. Wiping every offset and relying on the
// UNIQUE(session_id, message_id) constraint to skip whatever was already
// correctly recorded is the only way to guarantee recovery without knowing
// in advance which files/sessions are actually missing data.
func (s *Store) DeleteAllTailerOffsets() (int64, error) {
	result, err := s.db.Exec(`DELETE FROM tailer_offsets`)
	if err != nil {
		return 0, fmt.Errorf("failed to delete tailer offsets: %w", err)
	}
	return result.RowsAffected()
}
