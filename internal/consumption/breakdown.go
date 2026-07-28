package consumption

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// Breakdown is the JSON response from GET /api/usage/breakdown: what an
// arbitrary time range cost, split per model.
//
// The three cost totals are reported separately rather than as one number
// because they are not the same kind of quantity. MeasuredCostUSD comes from
// rates we actually have (reported by the transcript, or computed from the
// price table); EstimatedCostUSD comes from events whose model the price table
// has never heard of and which ingest therefore priced at the table's
// pessimistic ceiling (see ingest.ResolveCost). Those dollars are deliberately
// overstated, so folding them into a single headline total would present a
// guess as a measurement. TotalCostUSD is their sum, offered for convenience.
//
// Token counts carry no such caveat — they are recorded per event regardless of
// whether a price was available — so the token columns are trustworthy even on
// rows whose dollars are not.
type Breakdown struct {
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	TotalCostUSD     float64   `json:"total_cost_usd"`
	MeasuredCostUSD  float64   `json:"measured_cost_usd"`
	EstimatedCostUSD float64   `json:"estimated_cost_usd"`
	EventsTotal      int64     `json:"events_total"`
	// EventsWithoutCost counts events with a NULL cost_usd_equivalent. Their
	// tokens are included in the per-model token sums; their dollars are
	// unknown and contribute nothing to any total.
	EventsWithoutCost int64 `json:"events_without_cost"`
	// Models is sorted by CostUSD descending, ties broken by model name. Never
	// nil, so the client can iterate without a guard.
	Models []ModelBreakdown `json:"models"`
}

// ModelBreakdown is one model's row in the report.
//
// CostSource records how this row's dollars were arrived at: "reported",
// "computed", or "ceiling" when every costed event agrees, "mixed" when they
// don't, and "none" when the row has no costed events at all. Estimated is the
// bit the UI actually gates presentation on: true when any part of CostUSD came
// from the ceiling. An absent cost is not a provenance — a row of uncosted
// events alongside one computed event reads "computed", not "mixed".
type ModelBreakdown struct {
	Model  string `json:"model"`
	Family string `json:"family"`
	Events int64  `json:"events"`
	// EventsWithoutCost is this model's share of Breakdown.EventsWithoutCost.
	EventsWithoutCost   int64   `json:"events_without_cost"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	CostSource          string  `json:"cost_source"`
	Estimated           bool    `json:"estimated"`
}

// ModelFamily classifies a usage_events.model value into a coarse family. It is
// the single source of truth for the classification, shared by the dashboard's
// stacked volume bars and the range report's per-model table so the two can
// never colour the same model differently.
//
// Matching is case-insensitive by substring and deliberately version-
// insensitive: claude-opus-4-8 and claude-opus-4-1 both map to "opus". An empty
// model, or any unrecognized name, maps to "other".
func ModelFamily(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "fable"):
		return "fable"
	case strings.Contains(m, "haiku"):
		return "haiku"
	default:
		return "other"
	}
}

// ParseRange resolves the three range query parameters into a concrete
// [start, end) instant pair in UTC.
//
// Precedence: an explicit startStr wins over periodStr, so a client that leaves
// a stale preset in the query string still gets the range its date inputs
// describe. Accepted forms:
//
//   - startStr + endStr — both RFC3339. Any offset is normalized to UTC, which
//     is what makes a local-time picker work: the browser converts its
//     datetime-local value to an absolute instant and the server never has to
//     guess a zone for it.
//   - startStr alone — runs to now.
//   - periodStr alone — "24h", "7d", "30d"; resolves to [now-period, now).
//   - nothing — defaults to the last 24h.
//
// An endStr with neither a start nor a period is rejected: there is no sensible
// implied start, and picking one silently would report a range nobody asked
// for. A non-positive span is rejected for the same reason.
func (c *Calculator) ParseRange(startStr, endStr, periodStr string) (time.Time, time.Time, error) {
	now := c.now().UTC()

	if startStr == "" && endStr != "" && periodStr == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("end given without start or period")
	}

	var start, end time.Time
	switch {
	case startStr != "":
		var err error
		if start, err = parseInstant(startStr); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("start: %w", err)
		}
		if endStr == "" {
			end = now
		} else if end, err = parseInstant(endStr); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("end: %w", err)
		}
	default:
		if periodStr == "" {
			periodStr = "24h"
		}
		d, err := parsePeriod(periodStr)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("period: %w", err)
		}
		if d <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("period: must be positive, got %q", periodStr)
		}
		end = now
		start = end.Add(-d)
	}

	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end %s must be after start %s",
			end.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	return start, end, nil
}

// parseInstant parses an RFC3339 timestamp and normalizes it to UTC.
func parseInstant(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not an RFC3339 timestamp: %q", s)
	}
	return t.UTC(), nil
}

// Breakdown aggregates usage over the half-open range [start, end).
//
// Half-open rather than the inclusive bound Calculate uses, so that adjacent
// ranges partition the data: an event landing exactly on a boundary belongs to
// the later range only, and "this week" plus "last week" sum to the two weeks
// with nothing counted twice.
func (c *Calculator) Breakdown(start, end time.Time) (*Breakdown, error) {
	if !end.After(start) {
		return nil, fmt.Errorf("end %s must be after start %s",
			end.Format(time.RFC3339), start.Format(time.RFC3339))
	}

	res := &Breakdown{
		Start:  start,
		End:    end,
		Models: []ModelBreakdown{},
	}

	// Grouping by cost_source as well as model is what keeps measured and
	// ceiling-priced dollars separable; Go folds the source rows back into one
	// row per model below.
	rows, err := c.db.Query(`
		SELECT
			model,
			cost_source,
			COUNT(*),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cost_usd_equivalent), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY model, cost_source
	`, store.FormatTime(start), store.FormatTime(end))
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate usage by model: %w", err)
	}
	defer rows.Close()

	// byModel accumulates across the cost_source rows for each model; sources
	// tracks which provenances actually contributed dollars, so CostSource can
	// collapse to a single name or to "mixed".
	byModel := map[string]*ModelBreakdown{}
	order := []string{}
	sources := map[string]map[string]bool{}

	for rows.Next() {
		var (
			model, costSource                       sql.NullString
			count, nullCost                         int64
			inTok, outTok, cacheWriteTok, cacheRead int64
			cost                                    float64
		)
		if err := rows.Scan(&model, &costSource, &count, &nullCost,
			&inTok, &outTok, &cacheWriteTok, &cacheRead, &cost); err != nil {
			return nil, fmt.Errorf("failed to scan model breakdown row: %w", err)
		}

		// NullString.String is "" for a NULL model column, which ModelFamily
		// also maps to "other" — an unnamed model and an unrecognized one are
		// equally unclassifiable.
		name := model.String
		m := byModel[name]
		if m == nil {
			m = &ModelBreakdown{Model: name, Family: ModelFamily(name)}
			byModel[name] = m
			order = append(order, name)
			sources[name] = map[string]bool{}
		}

		m.Events += count
		m.EventsWithoutCost += nullCost
		m.InputTokens += inTok
		m.OutputTokens += outTok
		m.CacheCreationTokens += cacheWriteTok
		m.CacheReadTokens += cacheRead
		m.CostUSD += cost

		res.EventsTotal += count
		res.EventsWithoutCost += nullCost

		// A group is only a provenance if at least one of its events carried a
		// cost. Uncosted events sit in the cost_source='' group and must not
		// register as a third source that turns an otherwise-uniform row
		// "mixed".
		if count > nullCost && costSource.String != "" {
			sources[name][costSource.String] = true
			// Ceiling-priced dollars are a guess at invented rates; everything
			// else is measured. Splitting here rather than per model keeps a
			// mixed row's measured portion out of the estimated total.
			if costSource.String == "ceiling" {
				res.EstimatedCostUSD += cost
				m.Estimated = true
			} else {
				res.MeasuredCostUSD += cost
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate model breakdown: %w", err)
	}

	res.TotalCostUSD = res.MeasuredCostUSD + res.EstimatedCostUSD

	for _, name := range order {
		m := byModel[name]
		m.CostSource = collapseSources(sources[name])
		res.Models = append(res.Models, *m)
	}

	// Biggest spender first so the table answers "where did the money go"
	// without the reader scanning it. Ties break on model name to keep the
	// order stable across requests — map iteration order is not.
	sort.Slice(res.Models, func(i, j int) bool {
		if res.Models[i].CostUSD != res.Models[j].CostUSD {
			return res.Models[i].CostUSD > res.Models[j].CostUSD
		}
		return res.Models[i].Model < res.Models[j].Model
	})

	return res, nil
}

// collapseSources reduces the set of cost provenances that contributed dollars
// to a single label: the lone source when they agree, "mixed" when they don't,
// "none" when nothing in the row was costed.
func collapseSources(set map[string]bool) string {
	switch len(set) {
	case 0:
		return "none"
	case 1:
		for s := range set {
			return s
		}
	}
	return "mixed"
}
