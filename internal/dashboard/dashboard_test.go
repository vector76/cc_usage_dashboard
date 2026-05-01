package dashboard

import (
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
