package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ForwardableEvent is the subset of a usage_events row that an uplink sends
// to a peer trayapp.
//
// Two columns are deliberately absent. cost_usd_equivalent is omitted so the
// receiver prices the event from its own prices.yaml — a stale price table on
// the sender would otherwise skew the combined numbers, and cost is derivable
// from what is sent. raw_json is omitted because it can carry a whole
// transcript line (the /log body cap is 1 MiB for exactly that reason) and the
// sender retains its own copy for forensics.
type ForwardableEvent struct {
	ID                    int64
	OccurredAt            time.Time
	SessionID             string
	MessageID             string
	ProjectPath           string
	Model                 string
	InputTokens           int
	OutputTokens          int
	CacheCreationTokens   int
	CacheCreation1hTokens int
	CacheReadTokens       int
}

// GetUplinkCursor returns the highest usage_events.id already forwarded to
// the given peer. An unknown peer returns 0, meaning "forward everything" —
// a sender pointed at a fresh receiver must not skip the history it holds.
func (s *Store) GetUplinkCursor(url string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT last_event_id FROM uplink_cursor WHERE url = ?`, url).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get uplink cursor: %w", err)
	}
	return id, nil
}

// SetUplinkCursor records how far forwarding has progressed for a peer.
func (s *Store) SetUplinkCursor(url string, lastEventID int64) error {
	_, err := s.db.Exec(`
		INSERT INTO uplink_cursor (url, last_event_id, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			last_event_id = excluded.last_event_id,
			updated_at = excluded.updated_at
	`, url, lastEventID, FormatTime(time.Now()))

	if err != nil {
		return fmt.Errorf("failed to set uplink cursor: %w", err)
	}
	return nil
}

// ForwardableEventsAfter returns up to limit events with an id greater than
// afterID, oldest first.
//
// Events missing either dedup key are skipped. The receiver's uniqueness
// constraint is UNIQUE(session_id, message_id) and SQLite does not apply it
// to NULLs, so an event without both keys could be delivered twice by a
// retry and counted twice. Dropping those is the conservative choice: the
// alternative is inflating the combined total, which is the one number the
// uplink exists to get right.
//
// Ordering by id rather than occurred_at is deliberate — id is the insertion
// sequence the cursor advances through, and a backfill can legitimately
// insert an old occurred_at after a newer one.
func (s *Store) ForwardableEventsAfter(afterID int64, limit int) ([]ForwardableEvent, error) {
	rows, err := s.db.Query(`
		SELECT id, occurred_at, session_id, message_id,
		       COALESCE(project_path, ''), COALESCE(model, ''),
		       input_tokens, output_tokens,
		       COALESCE(cache_creation_tokens, 0), COALESCE(cache_creation_1h_tokens, 0),
		       COALESCE(cache_read_tokens, 0)
		FROM usage_events
		WHERE id > ?
		  AND session_id IS NOT NULL AND session_id != ''
		  AND message_id IS NOT NULL AND message_id != ''
		ORDER BY id
		LIMIT ?
	`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query forwardable events: %w", err)
	}
	defer rows.Close()

	var events []ForwardableEvent
	for rows.Next() {
		var e ForwardableEvent
		var occurredAt string
		if err := rows.Scan(
			&e.ID, &occurredAt, &e.SessionID, &e.MessageID,
			&e.ProjectPath, &e.Model,
			&e.InputTokens, &e.OutputTokens,
			&e.CacheCreationTokens, &e.CacheCreation1hTokens, &e.CacheReadTokens,
		); err != nil {
			return nil, fmt.Errorf("failed to scan forwardable event: %w", err)
		}
		e.OccurredAt = parseStoredTime(occurredAt)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate forwardable events: %w", err)
	}
	return events, nil
}
