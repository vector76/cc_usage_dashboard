package dashboard

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func TestRollingBoundsSessionSnapsTo15Min(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 23, 10, 7, 30, 0, loc)
	start, end, bucket := rollingBounds("session", now, loc)
	if bucket != 15*60 {
		t.Fatalf("bucket = %d, want 900", bucket)
	}
	// The in-progress bucket is the last bar, so end is the next boundary.
	if want := time.Date(2026, 11, 23, 10, 15, 0, 0, loc); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
	if got := end.Sub(start); got != 15*time.Hour {
		t.Errorf("span = %v, want 15h", got)
	}
}

func TestRollingBoundsWeeklySpan(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 23, 10, 7, 0, 0, loc)
	start, end, bucket := rollingBounds("weekly", now, loc)
	if bucket != 6*3600 {
		t.Fatalf("bucket = %d, want 21600", bucket)
	}
	if want := time.Date(2026, 11, 23, 12, 0, 0, 0, loc); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
	if got := end.Sub(start); got != 7*24*time.Hour {
		t.Errorf("span = %v, want 7d", got)
	}
}

// Bars sit on a fixed grid: as now advances within a bucket the window does
// not move, and crossing a boundary moves it by exactly one bucket. Otherwise
// past bars would change width or contents between refreshes.
func TestRollingBoundsGridIsStable(t *testing.T) {
	loc := time.UTC
	for _, kind := range []string{"session", "weekly"} {
		t0 := time.Date(2026, 11, 23, 12, 0, 1, 0, loc)
		s0, e0, b := rollingBounds(kind, t0, loc)
		s1, e1, _ := rollingBounds(kind, t0.Add(time.Duration(b-2)*time.Second), loc)
		if !s0.Equal(s1) || !e0.Equal(e1) {
			t.Errorf("%s: window moved within a bucket", kind)
		}
		s2, e2, _ := rollingBounds(kind, t0.Add(time.Duration(b)*time.Second), loc)
		step := time.Duration(b) * time.Second
		if s2.Sub(s0) != step || e2.Sub(e0) != step {
			t.Errorf("%s: window did not advance by exactly one bucket", kind)
		}
	}
}

// Weekly bars follow the local clock: 5h behind UTC, 6h bars start at local
// 00/06/12/18, i.e. 05/11/17/23 UTC.
func TestRollingBoundsWeeklyUsesLocalPhase(t *testing.T) {
	loc := time.FixedZone("x", -5*3600)
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) // 07:00 local
	_, end, _ := rollingBounds("weekly", now, loc)
	if want := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC); !end.Equal(want) { // 12:00 local
		t.Errorf("end = %v, want %v", end, want)
	}
}

func TestLoadRollingWindowBucketsAndFlags(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	now := time.Date(2026, 11, 23, 10, 7, 0, 0, time.UTC)
	cost := 2.0
	ins := func(n int, at time.Time) {
		if _, err := s.InsertUsageEvent(at, "tailer", "sess", fmt.Sprintf("m%d", n), "/p",
			"claude-opus-4-8", 1, 1, 0, 0, &cost, "computed", "{}"); err != nil {
			t.Fatalf("InsertUsageEvent: %v", err)
		}
	}
	ins(1, time.Date(2026, 11, 23, 10, 3, 0, 0, time.UTC))  // current bucket
	ins(2, time.Date(2026, 11, 22, 19, 30, 0, 0, time.UTC)) // inside the 15h span
	ins(3, time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))  // older than 15h: excluded

	h := &Handler{store: s, now: func() time.Time { return now }, loc: time.UTC}
	ws, err := h.loadRollingWindow(s.DB(), "session")
	if err != nil {
		t.Fatalf("loadRollingWindow: %v", err)
	}
	if !ws.Rolling || ws.Hypothetical || ws.Kind != "session" || ws.BucketSecs != 900 {
		t.Errorf("flags wrong: %+v", ws)
	}
	if len(ws.Series) != 0 {
		t.Errorf("series = %v, want empty", ws.Series)
	}
	if len(ws.Volume) != 2 {
		t.Fatalf("volume = %+v, want 2 buckets", ws.Volume)
	}
	if want := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC); !ws.Volume[1].BucketStart.Equal(want) {
		t.Errorf("last bucket %v, want %v", ws.Volume[1].BucketStart, want)
	}
	// Bucket edges must coincide with the window's own edges.
	if got := ws.EndsAt.Sub(ws.StartedAt); got != 15*time.Hour {
		t.Errorf("window span = %v", got)
	}
}

// In a zone whose offset is not a multiple of the bucket width the bars must
// still start on local boundaries (here local :00/:15 in UTC+5:45).
func TestLoadRollingWindowBucketsUseLocalPhase(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	loc := time.FixedZone("npt", 5*3600+45*60)
	now := time.Date(2026, 11, 23, 10, 7, 0, 0, time.UTC)
	cost := 1.0
	if _, err := s.InsertUsageEvent(now.Add(-time.Hour), "tailer", "s", "m1", "/p",
		"claude-opus-4-8", 1, 1, 0, 0, &cost, "computed", "{}"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: s, now: func() time.Time { return now }, loc: loc}
	ws, err := h.loadRollingWindow(s.DB(), "weekly")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Volume) != 1 {
		t.Fatalf("volume = %+v", ws.Volume)
	}
	_, off := ws.Volume[0].BucketStart.In(loc).Zone()
	local := ws.Volume[0].BucketStart.Unix() + int64(off)
	if local%(6*3600) != 0 {
		t.Errorf("bucket start %v is not on a local 6h boundary", ws.Volume[0].BucketStart)
	}
}
