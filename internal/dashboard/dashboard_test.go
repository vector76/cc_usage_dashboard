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

// The fable series reads its own column but borrows the weekly window's
// boundary, and must skip snapshots that didn't report the row rather than
// treating them as zero.
func TestLoadUsedSeriesFableWeekly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	weeklyEnds := base.Add(3 * 24 * time.Hour)

	rows := []struct {
		offsetMin int
		weekly    float64
		fable     *float64
	}{
		{0, 44.0, nil},          // pre-column / older userscript: no fable row
		{5, 44.0, floatPtr(70)}, // fable starts reporting
		{10, 45.0, floatPtr(77)},
	}

	for _, r := range rows {
		obs := base.Add(time.Duration(r.offsetMin) * time.Minute)
		weekly, ends := r.weekly, weeklyEnds
		if _, err := s.InsertQuotaSnapshotRecord(store.QuotaSnapshotRecord{
			ObservedAt: obs, ReceivedAt: obs,
			Source:           "test",
			WeeklyUsed:       &weekly,
			WeeklyWindowEnds: &ends,
			FableWeeklyUsed:  r.fable,
			RawJSON:          "{}",
		}); err != nil {
			t.Fatalf("InsertQuotaSnapshotRecord: %v", err)
		}
	}

	h := &Handler{store: s, now: func() time.Time { return base }}

	weekly, err := h.loadUsedSeries(s.DB(), "weekly", base, base.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("loadUsedSeries weekly: %v", err)
	}
	if len(weekly) != 3 {
		t.Fatalf("weekly series: got %d points, want 3", len(weekly))
	}

	fable, err := h.loadUsedSeries(s.DB(), "fable_weekly", base, base.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("loadUsedSeries fable_weekly: %v", err)
	}
	if len(fable) != 2 {
		t.Fatalf("fable series: got %d points, want 2 (the NULL row is skipped)", len(fable))
	}
	if fable[0].PercentUsed != 70.0 || fable[1].PercentUsed != 77.0 {
		t.Errorf("fable percents = %v, %v; want 70, 77", fable[0].PercentUsed, fable[1].PercentUsed)
	}
	// The boundary comes from weekly_window_ends — there is no fable-specific
	// ends column, and the pace diagonal depends on this being populated.
	for i, p := range fable {
		if p.WindowEnds == nil || !p.WindowEnds.Equal(weeklyEnds) {
			t.Errorf("fable point %d: WindowEnds = %v, want the weekly boundary %v", i, p.WindowEnds, weeklyEnds)
		}
	}
}

// The weekly WindowState carries the fable curve; the session one never
// does, and a weekly window with no fable observations omits it entirely so
// the client can distinguish "no data" from "zero".
func TestLoadActiveWindowFableSeries(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	setup := func(t *testing.T, fable *float64) *store.Store {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "test.db")
		s, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })

		if _, err := s.DB().Exec(`
			INSERT INTO windows (kind, started_at, ends_at, baseline_percent_used, closed)
			VALUES ('weekly', ?, ?, 40.0, 0), ('session', ?, ?, 20.0, 0)
		`, store.FormatTime(base), store.FormatTime(base.Add(3*24*time.Hour)),
			store.FormatTime(base), store.FormatTime(base.Add(5*time.Hour))); err != nil {
			t.Fatalf("insert windows: %v", err)
		}

		obs := base.Add(10 * time.Minute)
		weekly, ends := 44.0, base.Add(3*24*time.Hour)
		session := 27.0
		if _, err := s.InsertQuotaSnapshotRecord(store.QuotaSnapshotRecord{
			ObservedAt: obs, ReceivedAt: obs,
			Source:           "test",
			SessionUsed:      &session,
			WeeklyUsed:       &weekly,
			WeeklyWindowEnds: &ends,
			FableWeeklyUsed:  fable,
			RawJSON:          "{}",
		}); err != nil {
			t.Fatalf("InsertQuotaSnapshotRecord: %v", err)
		}
		return s
	}

	t.Run("weekly window carries the fable series", func(t *testing.T) {
		s := setup(t, floatPtr(77))
		h := &Handler{store: s, now: func() time.Time { return base.Add(time.Hour) }}

		ws, err := h.loadActiveWindow(s.DB(), "weekly")
		if err != nil {
			t.Fatalf("loadActiveWindow weekly: %v", err)
		}
		if len(ws.FableSeries) != 1 || ws.FableSeries[0].PercentUsed != 77.0 {
			t.Fatalf("FableSeries = %+v, want one point at 77", ws.FableSeries)
		}

		sess, err := h.loadActiveWindow(s.DB(), "session")
		if err != nil {
			t.Fatalf("loadActiveWindow session: %v", err)
		}
		if sess.FableSeries != nil {
			t.Errorf("session window should not carry a fable series, got %+v", sess.FableSeries)
		}
	})

	t.Run("no fable observations omits the series", func(t *testing.T) {
		s := setup(t, nil)
		h := &Handler{store: s, now: func() time.Time { return base.Add(time.Hour) }}

		ws, err := h.loadActiveWindow(s.DB(), "weekly")
		if err != nil {
			t.Fatalf("loadActiveWindow weekly: %v", err)
		}
		if ws.FableSeries != nil {
			t.Errorf("FableSeries should be nil when nothing reported the row, got %+v", ws.FableSeries)
		}
		if len(ws.Series) != 1 {
			t.Errorf("weekly series should still be populated, got %d points", len(ws.Series))
		}
	})
}

func floatPtr(f float64) *float64 { return &f }

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

// A ceiling-priced event is a guess, not a measurement, so it belongs in the
// gray "other" family regardless of what its name looks like — claude-opus-5
// contains "opus" and would otherwise be stacked in with real Opus dollars at a
// rate we invented. OtherModels then names the guesses for the tooltip, since
// "other" alone gives the reader no way to tell what is being estimated.
func TestLoadVolumeSeriesCeilingPricedIsOtherAndNamed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	bucketSecs := 15 * 60

	msgN := 0
	insert := func(offsetMin int, model string, cost float64, costSource string) {
		msgN++
		obs := base.Add(time.Duration(offsetMin) * time.Minute)
		if _, err := s.InsertUsageEvent(
			obs, "tailer", "sess", fmt.Sprintf("msg-%d", msgN), "/proj", model,
			1, 1, 0, 0, &cost, costSource, "{}",
		); err != nil {
			t.Fatalf("InsertUsageEvent: %v", err)
		}
	}

	// Real opus dollars, plus two ceiling-priced models whose names would
	// otherwise classify as opus and fable, plus an unnamed-family event that
	// reaches "other" the old way.
	insert(0, "claude-opus-4-8", 4.0, "computed")
	insert(1, "claude-opus-5", 10.0, "ceiling")
	insert(2, "claude-fable-6", 5.0, "ceiling")
	insert(3, "gpt-4", 1.0, "computed")

	h := &Handler{store: s, now: func() time.Time { return base }}
	buckets, err := h.loadVolumeSeries(s.DB(), base, base.Add(1*time.Hour), bucketSecs)
	if err != nil {
		t.Fatalf("loadVolumeSeries: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("got %d buckets, want 1: %+v", len(buckets), buckets)
	}
	b := buckets[0]

	if !approxEqual(b.ByFamily["opus"], 4.0) {
		t.Errorf("opus = %v, want 4 (the ceiling-priced opus-5 must not join it)", b.ByFamily["opus"])
	}
	if _, ok := b.ByFamily["fable"]; ok {
		t.Errorf("fable must be absent; the ceiling-priced fable-6 belongs to other: %+v", b.ByFamily)
	}
	if !approxEqual(b.ByFamily["other"], 16.0) {
		t.Errorf("other = %v, want 16 (10 + 5 ceiling + 1 gpt-4)", b.ByFamily["other"])
	}
	if !approxEqual(b.CostUSD, 20.0) {
		t.Errorf("total = %v, want 20", b.CostUSD)
	}

	want := []string{"claude-fable-6", "claude-opus-5", "gpt-4"}
	if len(b.OtherModels) != len(want) {
		t.Fatalf("OtherModels = %v, want %v", b.OtherModels, want)
	}
	for i, m := range want {
		if b.OtherModels[i] != m {
			t.Errorf("OtherModels = %v, want %v (sorted)", b.OtherModels, want)
			break
		}
	}
}

// A model contributing exactly nothing is not worth naming: the live DB carries
// "<synthetic>" rows with zero tokens, which cost $0 at any rate, and listing
// them in the tooltip implies they are part of the segment's dollars.
func TestLoadVolumeSeriesOtherModelsSkipsZeroCost(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	insert := func(msg, model string, cost float64, costSource string) {
		if _, err := s.InsertUsageEvent(
			base, "tailer", "sess", msg, "/proj", model,
			0, 0, 0, 0, &cost, costSource, "{}",
		); err != nil {
			t.Fatalf("InsertUsageEvent: %v", err)
		}
	}
	insert("m1", "claude-opus-5", 10.0, "ceiling")
	insert("m2", "<synthetic>", 0.0, "ceiling")

	h := &Handler{store: s, now: func() time.Time { return base }}
	buckets, err := h.loadVolumeSeries(s.DB(), base, base.Add(1*time.Hour), 15*60)
	if err != nil {
		t.Fatalf("loadVolumeSeries: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(buckets))
	}
	got := buckets[0].OtherModels
	if len(got) != 1 || got[0] != "claude-opus-5" {
		t.Errorf("OtherModels = %v, want just [claude-opus-5]", got)
	}
}

// An event with no model name has nothing to name in the tooltip, but its
// dollars must still land in "other" rather than vanishing.
func TestLoadVolumeSeriesUnnamedModelHasNoTooltipEntry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cost := 2.0
	if _, err := s.InsertUsageEvent(
		base, "tailer", "sess", "msg-1", "/proj", "",
		1, 1, 0, 0, &cost, "computed", "{}",
	); err != nil {
		t.Fatalf("InsertUsageEvent: %v", err)
	}

	h := &Handler{store: s, now: func() time.Time { return base }}
	buckets, err := h.loadVolumeSeries(s.DB(), base, base.Add(1*time.Hour), 15*60)
	if err != nil {
		t.Fatalf("loadVolumeSeries: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(buckets))
	}
	if !approxEqual(buckets[0].ByFamily["other"], 2.0) {
		t.Errorf("other = %v, want 2", buckets[0].ByFamily["other"])
	}
	if len(buckets[0].OtherModels) != 0 {
		t.Errorf("OtherModels = %v, want empty for an unnamed model", buckets[0].OtherModels)
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
