package consumption

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/folder"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

type folderEvent struct {
	at     time.Time
	folder string
	cost   *float64
	source string
}

func insertFolderEvents(t *testing.T, s *store.Store, evs ...folderEvent) {
	t.Helper()
	for i, e := range evs {
		if _, err := s.InsertUsageEventRecord(store.UsageEventRecord{
			OccurredAt: e.at, Source: "tailer", SessionID: "s", MessageID: fmt.Sprintf("m%d", i),
			ProjectPath: e.folder, Model: "claude-opus-5-5", InputTokens: 1, OutputTokens: 1,
			CostUSD: e.cost, CostSource: e.source,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
}

func usd(v float64) *float64 { return &v }

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseBoundaries(t *testing.T) {
	ok := []string{"2026-11-15T05:00:00Z", "2026-11-16T05:00:00Z", "2026-11-17T00:00:00-05:00"}
	got, err := ParseBoundaries(ok)
	if err != nil || len(got) != 3 || !got[2].Equal(time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("ParseBoundaries(ok) = %v, %v", got, err)
	}
	tooMany := make([]string, MaxBuckets+2)
	for i := range tooMany {
		tooMany[i] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.RFC3339)
	}
	for name, in := range map[string][]string{
		"none":        nil,
		"one":         {"2026-11-15T05:00:00Z"},
		"not RFC3339": {"2026-11-15", "2026-11-16"},
		"equal":       {"2026-11-15T05:00:00Z", "2026-11-15T05:00:00Z"},
		"decreasing":  {"2026-11-16T05:00:00Z", "2026-11-15T05:00:00Z"},
		"too many":    tooMany,
	} {
		if _, err := ParseBoundaries(in); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestByProject(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	d0 := time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC) // a local midnight, say
	d1 := d0.Add(24 * time.Hour)
	d2 := d1.Add(23 * time.Hour) // a DST-short day: buckets need not be equal
	insertFolderEvents(t, s,
		folderEvent{d0, `C:\OCIS2\BNA`, usd(1), "computed"},
		folderEvent{d0.Add(time.Hour), `c:\ocis2\bna\.claude\worktrees\Alice`, usd(2), "computed"},
		folderEvent{d0.Add(2 * time.Hour), `C:\Jewel\agent\docs`, usd(4), "ceiling"},
		folderEvent{d0.Add(3 * time.Hour), `C:\Unrelated`, usd(8), "computed"},
		folderEvent{d0.Add(4 * time.Hour), ``, nil, ""},
		folderEvent{d1, `C:\OCIS2\BNA\app`, usd(16), "reported"}, // exactly on the boundary: day 2
		folderEvent{d2, `C:\OCIS2\BNA`, usd(32), "computed"},     // at the end bound: excluded
		folderEvent{d0.Add(-time.Nanosecond), `C:\OCIS2\BNA`, usd(64), "computed"},
	)

	bna, _ := s.CreateProject("BNA")
	jewel, _ := s.CreateProject("Jewel")
	idle, _ := s.CreateProject("Idle") // no usage in range
	s.AssignFolder(`C:\OCIS2\BNA`, &bna.ID)
	s.AssignFolder(`C:\Jewel`, &jewel.ID)
	s.AssignFolder(`C:\Idle`, &idle.ID)
	projects, _ := s.ListProjects()
	as, _ := s.ListFolderAssignments()

	got, err := NewCalculator(s.DB()).ByProject([]time.Time{d0, d1, d2}, projects, as)
	if err != nil {
		t.Fatalf("ByProject: %v", err)
	}

	if len(got.Buckets) != 2 || !got.Buckets[1].Start.Equal(d1) || !got.Buckets[1].End.Equal(d2) {
		t.Fatalf("buckets = %+v", got.Buckets)
	}

	// Columns: projects with usage in range, by total cost descending, then
	// Unassigned last. Idle has none and is left out.
	names := []string{}
	for _, p := range got.Projects {
		names = append(names, p.Name)
	}
	if fmt.Sprint(names) != "[BNA Jewel Unassigned]" {
		t.Fatalf("columns = %v", names)
	}
	if got.Projects[2].ID != nil {
		t.Errorf("Unassigned id = %v, want null", *got.Projects[2].ID)
	}

	cell := func(b, p int) Cell { return got.Cells[b][p] }
	if c := cell(0, 0); !approxEq(c.CostUSD, 3) || c.Events != 2 {
		t.Errorf("day1 BNA = %+v", c)
	}
	if c := cell(0, 1); !approxEq(c.CostUSD, 4) || !approxEq(c.EstimatedUSD, 4) {
		t.Errorf("day1 Jewel = %+v", c)
	}
	if c := cell(0, 2); !approxEq(c.CostUSD, 8) || c.Events != 2 || c.EventsWithoutCost != 1 {
		t.Errorf("day1 Unassigned = %+v", c)
	}
	if c := cell(1, 0); !approxEq(c.CostUSD, 16) || c.Events != 1 {
		t.Errorf("day2 BNA = %+v", c)
	}

	if !approxEq(got.ProjectTotals[0].CostUSD, 19) || !approxEq(got.BucketTotals[0].CostUSD, 15) ||
		!approxEq(got.BucketTotals[1].CostUSD, 16) || !approxEq(got.Total.CostUSD, 31) ||
		!approxEq(got.Total.EstimatedUSD, 4) || got.Total.Events != 6 {
		t.Errorf("totals: project %+v bucket %+v total %+v", got.ProjectTotals, got.BucketTotals, got.Total)
	}

	// The per-day buckets partition the range: their sum is the same range
	// asked for as one bucket.
	whole, err := NewCalculator(s.DB()).ByProject([]time.Time{d0, d2}, projects, as)
	if err != nil {
		t.Fatalf("whole: %v", err)
	}
	if !approxEq(whole.Total.CostUSD, got.Total.CostUSD) || whole.Total.Events != got.Total.Events {
		t.Errorf("one bucket %+v != sum of days %+v", whole.Total, got.Total)
	}
}

// With nothing in range, Unassigned is still the one column, so the client
// always has a well-formed matrix.
func TestByProjectEmptyRange(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	d0 := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	got, err := NewCalculator(s.DB()).ByProject([]time.Time{d0, d0.Add(time.Hour)}, nil, []folder.Assignment{})
	if err != nil {
		t.Fatalf("ByProject: %v", err)
	}
	if len(got.Projects) != 1 || got.Projects[0].Name != "Unassigned" ||
		len(got.Cells) != 1 || len(got.Cells[0]) != 1 {
		t.Errorf("got %+v", got)
	}
}
