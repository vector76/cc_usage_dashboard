package store

import "fmt"

// NullCostEvent is a usage_events row whose cost_usd_equivalent is NULL but
// whose model is known, making it a candidate for cost backfill once the
// price table gains that model. Cache token columns are nullable in the
// schema; they are coalesced to 0 here so cost math can treat them uniformly.
type NullCostEvent struct {
	ID                  int64
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// NullCostEvents returns every usage event with a NULL cost and a non-empty
// model, oldest-first by id. Rows with no model are excluded: without a model
// there is nothing to price, and that case is already surfaced separately.
func (s *Store) NullCostEvents() ([]NullCostEvent, error) {
	rows, err := s.db.Query(`
		SELECT id, model, input_tokens, output_tokens,
		       COALESCE(cache_creation_tokens, 0), COALESCE(cache_read_tokens, 0)
		FROM usage_events
		WHERE cost_usd_equivalent IS NULL AND model IS NOT NULL AND model != ''
		ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query null-cost events: %w", err)
	}
	defer rows.Close()

	var out []NullCostEvent
	for rows.Next() {
		var e NullCostEvent
		if err := rows.Scan(&e.ID, &e.Model, &e.InputTokens, &e.OutputTokens,
			&e.CacheCreationTokens, &e.CacheReadTokens); err != nil {
			return nil, fmt.Errorf("failed to scan null-cost event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate null-cost events: %w", err)
	}
	return out, nil
}

// SetComputedCost fills in a computed cost on a single usage event. The
// update is guarded by `cost_usd_equivalent IS NULL` so a reported or
// previously computed value can never be overwritten — the backfill only
// repairs rows that were unpriceable at ingest. Returns whether the row was
// actually updated.
func (s *Store) SetComputedCost(id int64, costUSD float64) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE usage_events
		SET cost_usd_equivalent = ?, cost_source = 'computed'
		WHERE id = ? AND cost_usd_equivalent IS NULL
	`, costUSD, id)
	if err != nil {
		return false, fmt.Errorf("failed to set computed cost for event %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to read rows affected for event %d: %w", id, err)
	}
	return n > 0, nil
}
