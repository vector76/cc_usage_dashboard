package dashboard

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func TestLoadUsedSeriesContinuousWithPrev(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	windowEnds := base.Add(5 * time.Hour)

	tt := true
	ff := false

	rows := []struct {
		offsetMin int
		used      float64
		cwp       *bool
	}{
		{0, 10.0, nil},  // NULL → false
		{5, 12.0, &tt},  // continuation
		{10, 15.0, &ff}, // explicit false (reset)
		{15, 20.0, &tt}, // continuation
	}

	for _, r := range rows {
		used := r.used
		obs := base.Add(time.Duration(r.offsetMin) * time.Minute)
		ends := windowEnds
		if _, err := s.InsertQuotaSnapshot(
			obs, obs,
			"test",
			&used, &ends,
			nil, nil,
			nil, nil,
			r.cwp,
			"{}",
		); err != nil {
			t.Fatalf("InsertQuotaSnapshot: %v", err)
		}
	}

	h := &Handler{store: s, now: func() time.Time { return base }}
	series, err := h.loadUsedSeries(s.DB(), "session", base, base.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("loadUsedSeries: %v", err)
	}
	if len(series) != len(rows) {
		t.Fatalf("got %d points, want %d", len(series), len(rows))
	}

	want := []bool{false, true, false, true}
	for i, p := range series {
		if p.ContinuousWithPrev != want[i] {
			t.Errorf("point %d: ContinuousWithPrev = %v, want %v",
				i, p.ContinuousWithPrev, want[i])
		}
		if p.WindowEnds == nil || !p.WindowEnds.Equal(windowEnds) {
			t.Errorf("point %d: WindowEnds = %v, want %v", i, p.WindowEnds, windowEnds)
		}
	}
}

func TestModelFamily(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// opus — version-insensitive, case-insensitive.
		{"claude-opus-4-8", "opus"},
		{"claude-opus-4-1", "opus"},
		{"CLAUDE-OPUS", "opus"},
		{"Opus", "opus"},
		// sonnet
		{"claude-3-5-sonnet-20241022", "sonnet"},
		{"Sonnet", "sonnet"},
		// fable
		{"claude-fable-1", "fable"},
		{"some-FABLE-model", "fable"},
		// haiku
		{"claude-haiku-4", "haiku"},
		{"HAIKU", "haiku"},
		// other — empty, unmatched, cross-vendor names.
		{"", "other"},
		{"gpt-4", "other"},
		{"gemini-1.5-pro", "other"},
		{"unknown", "other"},
	}
	for _, c := range cases {
		if got := modelFamily(c.in); got != c.want {
			t.Errorf("modelFamily(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}

// TestLoadVolumeSeriesByFamily inserts usage_events across models and
// buckets and asserts loadVolumeSeries folds them into per-family sums,
// including multiple opus versions collapsing to one "opus" entry, an
// unrecognized model landing in "other", and NULL-cost events contributing
// nothing.
func TestLoadVolumeSeriesByFamily(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	bucketSecs := 15 * 60

	msgN := 0
	insert := func(offsetMin int, model string, cost *float64) {
		msgN++
		obs := base.Add(time.Duration(offsetMin) * time.Minute)
		if _, err := s.InsertUsageEvent(
			obs, "api", "sess", fmt.Sprintf("msg-%d", msgN), "/proj", model,
			1, 1, 0, 0, cost, "reported", "{}",
		); err != nil {
			t.Fatalf("InsertUsageEvent: %v", err)
		}
	}
	c := func(v float64) *float64 { return &v }

	// Bucket 0 [12:00, 12:15): opus 10 (two versions) + sonnet 6 = 16.
	insert(0, "claude-opus-4-8", c(4.0))
	insert(5, "claude-opus-4-1", c(6.0))
	insert(7, "claude-sonnet-4", c(6.0))
	// A NULL-cost event in bucket 0 must contribute nothing.
	insert(2, "claude-opus-4-8", nil)
	// Bucket 1 [12:15, 12:30): haiku 2 + fable 3 + other 1 = 6.
	insert(15, "claude-haiku-4", c(2.0))
	insert(20, "claude-fable-1", c(3.0))
	insert(25, "gpt-4", c(1.0))

	h := &Handler{store: s, now: func() time.Time { return base }}
	buckets, err := h.loadVolumeSeries(s.DB(), base, base.Add(1*time.Hour), bucketSecs)
	if err != nil {
		t.Fatalf("loadVolumeSeries: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(buckets), buckets)
	}

	b0 := buckets[0]
	if !approxEqual(b0.CostUSD, 16.0) {
		t.Errorf("bucket0 total = %v, want 16", b0.CostUSD)
	}
	if !approxEqual(b0.ByFamily["opus"], 10.0) {
		t.Errorf("bucket0 opus = %v, want 10", b0.ByFamily["opus"])
	}
	if !approxEqual(b0.ByFamily["sonnet"], 6.0) {
		t.Errorf("bucket0 sonnet = %v, want 6", b0.ByFamily["sonnet"])
	}
	if _, ok := b0.ByFamily["haiku"]; ok {
		t.Errorf("bucket0 should not contain haiku: %+v", b0.ByFamily)
	}
	if _, ok := b0.ByFamily["other"]; ok {
		t.Errorf("bucket0 should not contain other: %+v", b0.ByFamily)
	}

	b1 := buckets[1]
	if !approxEqual(b1.CostUSD, 6.0) {
		t.Errorf("bucket1 total = %v, want 6", b1.CostUSD)
	}
	if !approxEqual(b1.ByFamily["haiku"], 2.0) {
		t.Errorf("bucket1 haiku = %v, want 2", b1.ByFamily["haiku"])
	}
	if !approxEqual(b1.ByFamily["fable"], 3.0) {
		t.Errorf("bucket1 fable = %v, want 3", b1.ByFamily["fable"])
	}
	if !approxEqual(b1.ByFamily["other"], 1.0) {
		t.Errorf("bucket1 other = %v, want 1", b1.ByFamily["other"])
	}

	// Sum of family entries equals the bucket total in every bucket.
	for i, b := range buckets {
		var sum float64
		for _, v := range b.ByFamily {
			sum += v
		}
		if !approxEqual(sum, b.CostUSD) {
			t.Errorf("bucket%d family sum %v != total %v", i, sum, b.CostUSD)
		}
	}
}
