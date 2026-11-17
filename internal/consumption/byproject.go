package consumption

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/folder"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// MaxBuckets caps one by-project request: a year of daily buckets, with room
// for a leap year.
const MaxBuckets = 400

// UnassignedName labels the column for every event no project covers.
const UnassignedName = "Unassigned"

// Bucket is one half-open period [Start, End) of a by-project report.
type Bucket struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Cell is the usage of one project (or a total) over one bucket (or all of
// them). CostUSD includes ceiling-priced dollars, and EstimatedUSD is that
// share, so the client can mark it "est." the way /report does.
// EventsWithoutCost counts events whose dollars are unknown; they add
// nothing to CostUSD.
type Cell struct {
	CostUSD           float64 `json:"cost_usd"`
	EstimatedUSD      float64 `json:"estimated_usd"`
	Events            int64   `json:"events"`
	EventsWithoutCost int64   `json:"events_without_cost"`
}

func (c *Cell) add(o Cell) {
	c.CostUSD += o.CostUSD
	c.EstimatedUSD += o.EstimatedUSD
	c.Events += o.Events
	c.EventsWithoutCost += o.EventsWithoutCost
}

// ProjectColumn names one column. ID is nil for Unassigned.
type ProjectColumn struct {
	ID   *int64 `json:"id"`
	Name string `json:"name"`
}

// ByProjectReport is the JSON response from GET /api/usage/by-project.
// Cells is indexed [bucket][project], parallel to Buckets and Projects.
type ByProjectReport struct {
	Buckets       []Bucket        `json:"buckets"`
	Projects      []ProjectColumn `json:"projects"`
	Cells         [][]Cell        `json:"cells"`
	BucketTotals  []Cell          `json:"bucket_totals"`
	ProjectTotals []Cell          `json:"project_totals"`
	Total         Cell            `json:"total"`
}

// ParseBoundaries parses bucket boundaries sent by the client: RFC3339
// instants, at least two, strictly increasing, at most MaxBuckets+1.
//
// The client computes them (local midnights, Sunday-start weeks) because
// only it knows the user's zone; see docs/usage-report.md "Timezone". The
// server never turns a calendar date into an instant itself.
func ParseBoundaries(raw []string) ([]time.Time, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("need at least two boundaries, got %d", len(raw))
	}
	if len(raw) > MaxBuckets+1 {
		return nil, fmt.Errorf("at most %d buckets, got %d", MaxBuckets, len(raw)-1)
	}
	out := make([]time.Time, len(raw))
	for i, s := range raw {
		t, err := parseInstant(s)
		if err != nil {
			return nil, fmt.Errorf("boundary %d: %w", i, err)
		}
		if i > 0 && !t.After(out[i-1]) {
			return nil, fmt.Errorf("boundary %d is not after boundary %d", i, i-1)
		}
		out[i] = t
	}
	return out, nil
}

// ByProject aggregates cost per bucket per project. Bounds are the bucket
// edges (len(bounds)-1 buckets), each bucket half-open so adjacent buckets
// partition the data exactly, as Breakdown's ranges do.
//
// Attribution is by each event's own project_path through folder.Resolver:
// the longest assigned ancestor wins, and anything uncovered — including an
// event with no folder — is Unassigned. Grouping in SQL is by folder, and
// the folder → project fold happens here: "longest prefix wins" with
// Windows case rules is not expressible as a clean SQL join, and the number
// of distinct folders is small.
//
// Columns are the projects with any events in range, by total cost
// descending (then name), followed by Unassigned, which is always present.
func (c *Calculator) ByProject(bounds []time.Time, projects []store.Project, assignments []folder.Assignment) (*ByProjectReport, error) {
	if len(bounds) < 2 {
		return nil, fmt.Errorf("need at least two boundaries")
	}
	nb := len(bounds) - 1

	var values []string
	args := make([]any, 0, nb*3)
	for i := 0; i < nb; i++ {
		values = append(values, "(?, ?, ?)")
		args = append(args, i, store.FormatTime(bounds[i]), store.FormatTime(bounds[i+1]))
	}
	rows, err := c.db.Query(`
		WITH b(i, s, e) AS (VALUES `+strings.Join(values, ", ")+`)
		SELECT b.i,
		       COALESCE(u.project_path, ''),
		       COALESCE(u.cost_source, ''),
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN u.cost_usd_equivalent IS NULL THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(u.cost_usd_equivalent), 0)
		FROM b JOIN usage_events u ON u.occurred_at >= b.s AND u.occurred_at < b.e
		GROUP BY b.i, 2, 3
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate usage by folder: %w", err)
	}
	defer rows.Close()

	names := map[int64]string{}
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	resolver := folder.NewResolver(assignments)
	resolved := map[string]int64{} // folder -> project id, 0 = unassigned

	// perBucket[i][projectID] accumulates; projectID 0 is Unassigned.
	perBucket := make([]map[int64]*Cell, nb)
	for i := range perBucket {
		perBucket[i] = map[int64]*Cell{}
	}
	totals := map[int64]*Cell{}

	for rows.Next() {
		var (
			bi                int
			path, source      string
			events, uncounted int64
			cost              float64
		)
		if err := rows.Scan(&bi, &path, &source, &events, &uncounted, &cost); err != nil {
			return nil, fmt.Errorf("failed to scan folder usage: %w", err)
		}
		pid, ok := resolved[path]
		if !ok {
			if a, hit := resolver.Resolve(path); hit {
				if _, known := names[a.ProjectID]; known {
					pid = a.ProjectID
				}
			}
			resolved[path] = pid
		}
		cell := Cell{CostUSD: cost, Events: events, EventsWithoutCost: uncounted}
		if source == "ceiling" {
			cell.EstimatedUSD = cost
		}
		if perBucket[bi][pid] == nil {
			perBucket[bi][pid] = &Cell{}
		}
		perBucket[bi][pid].add(cell)
		if totals[pid] == nil {
			totals[pid] = &Cell{}
		}
		totals[pid].add(cell)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate folder usage: %w", err)
	}

	var ids []int64
	for id := range totals {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := totals[ids[i]], totals[ids[j]]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return strings.ToLower(names[ids[i]]) < strings.ToLower(names[ids[j]])
	})
	ids = append(ids, 0)

	rep := &ByProjectReport{
		Buckets:       make([]Bucket, nb),
		Projects:      make([]ProjectColumn, len(ids)),
		Cells:         make([][]Cell, nb),
		BucketTotals:  make([]Cell, nb),
		ProjectTotals: make([]Cell, len(ids)),
	}
	for j, id := range ids {
		if id == 0 {
			rep.Projects[j] = ProjectColumn{Name: UnassignedName}
		} else {
			id := id
			rep.Projects[j] = ProjectColumn{ID: &id, Name: names[id]}
		}
		if t := totals[id]; t != nil {
			rep.ProjectTotals[j] = *t
			rep.Total.add(*t)
		}
	}
	for i := 0; i < nb; i++ {
		rep.Buckets[i] = Bucket{Start: bounds[i], End: bounds[i+1]}
		rep.Cells[i] = make([]Cell, len(ids))
		for j, id := range ids {
			if c := perBucket[i][id]; c != nil {
				rep.Cells[i][j] = *c
				rep.BucketTotals[i].add(*c)
			}
		}
	}
	return rep, nil
}
