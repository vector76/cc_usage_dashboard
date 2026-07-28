package store

import "fmt"

// NullCostEvent is a usage_events row that does not yet carry a trustworthy
// cost but whose model is known, making it a candidate for backfill: either the
// cost is NULL, or it is a `ceiling` estimate that real rates should supersede.
// Cache token columns are nullable in the schema; they are coalesced to 0 here
// so cost math can treat them uniformly.
type NullCostEvent struct {
	ID                  int64
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// NullCostEvents returns every usage event with a non-empty model whose cost is
// either NULL or a ceiling estimate, oldest-first by id. Rows with no model are
// excluded: without a model there is nothing to price, and that case is already
// surfaced separately.
//
// Ceiling rows are included because the estimate is provisional. It is priced at
// the table's maximum rates, which can be several times the model's real ones,
// so once prices.yaml learns the model the historical dollars must come back
// down — otherwise adding a newly released model would fix new events while
// leaving every past one overstated.
func (s *Store) NullCostEvents() ([]NullCostEvent, error) {
	rows, err := s.db.Query(`
		SELECT id, model, input_tokens, output_tokens,
		       COALESCE(cache_creation_tokens, 0), COALESCE(cache_read_tokens, 0)
		FROM usage_events
		WHERE (cost_usd_equivalent IS NULL OR cost_source = 'ceiling')
		  AND model IS NOT NULL AND model != ''
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

// SetComputedCost fills in a cost on a single usage event, recording how it was
// arrived at in cost_source ("computed" from the model's own rates, "ceiling"
// from the table's pessimistic maximum for an unrecognized model). The source
// is passed in rather than hardcoded because the dashboard colors ceiling
// estimates differently — they are guesses, not measurements.
//
// The update is guarded so a measured value can never be overwritten: it
// applies only where the cost is still NULL, or where it is a `ceiling`
// estimate being replaced by a real `computed` figure. A `reported` or
// `computed` cost is therefore immovable, and one ceiling estimate never
// replaces another (which would rewrite the same rows on every startup for as
// long as the model stays unlisted). Returns whether the row was updated.
func (s *Store) SetComputedCost(id int64, costUSD float64, costSource string) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE usage_events
		SET cost_usd_equivalent = ?, cost_source = ?
		WHERE id = ?
		  AND (cost_usd_equivalent IS NULL
		       OR (cost_source = 'ceiling' AND ? = 'computed'))
	`, costUSD, costSource, id, costSource)
	if err != nil {
		return false, fmt.Errorf("failed to set computed cost for event %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to read rows affected for event %d: %w", id, err)
	}
	return n > 0, nil
}
