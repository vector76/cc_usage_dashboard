package consumption

import (
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newCalc(t *testing.T, now time.Time) (*Calculator, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	c := NewCalculator(s.DB())
	c.SetNow(fixedNow(now))
	return c, s
}

func insertEvent(t *testing.T, s *store.Store, occurred time.Time, costUSD *float64, costSource string) {
	t.Helper()
	_, err := s.InsertUsageEvent(
		occurred, "api",
		"sess-"+occurred.Format(time.RFC3339Nano), "msg-"+occurred.Format(time.RFC3339Nano),
		"", "claude-3-5-sonnet-20241022",
		1000, 500, 0, 0,
		costUSD, costSource, "{}",
	)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

// insertSnapshot inserts a quota snapshot. continuousWithPrev maps to the
// persisted column the consumption walker now keys off; pass nil to leave it
// NULL (which is treated as "start" by percentConsumed). sessionEnds /
// weeklyEnds are no longer read by consumption but are still accepted so
// callers exercising other engines can supply them.
func insertSnapshot(t *testing.T, s *store.Store, observed time.Time, sessionUsed, weeklyUsed *float64, sessionEnds, weeklyEnds *time.Time, continuousWithPrev *bool) {
	t.Helper()
	_, err := s.InsertQuotaSnapshot(observed, observed, "test", sessionUsed, sessionEnds, weeklyUsed, weeklyEnds, nil, nil, continuousWithPrev, "{}")
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

func ptrF(f float64) *float64 { return &f }
func ptrT(t time.Time) *time.Time {
	return &t
}
func ptrB(b bool) *bool { return &b }

func near(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func TestCalculate_USDAndEventBuckets(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertEvent(t, s, now.Add(-1*time.Hour), ptrF(10.00), "reported")
	insertEvent(t, s, now.Add(-2*time.Hour), ptrF(20.00), "computed")
	insertEvent(t, s, now.Add(-3*time.Hour), nil, "unknown")
	insertEvent(t, s, now.Add(-48*time.Hour), ptrF(99.0), "reported") // outside

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}

	if !near(res.ConsumedUSDEquivalent, 30.00, 1e-9) {
		t.Errorf("ConsumedUSDEquivalent: got %v want 30.00", res.ConsumedUSDEquivalent)
	}
	if res.EventsTotal != 3 || res.EventsWithReportedCost != 1 ||
		res.EventsWithComputedCost != 1 || res.EventsWithoutCost != 1 {
		t.Errorf("event bucket counts wrong: %+v", res)
	}
	// No snapshots inserted → percent fields are nil ("couldn't measure").
	if res.ConsumedSessionPct != nil {
		t.Errorf("expected nil session pct without snapshots, got %v", *res.ConsumedSessionPct)
	}
	if res.ConsumedWeeklyPct != nil {
		t.Errorf("expected nil weekly pct without snapshots, got %v", *res.ConsumedWeeklyPct)
	}
}

// TestPercent_AnchorAndDeltaSameWindow: anchor at period start sets the
// baseline; subsequent continuous snapshots accumulate deltas.
func TestPercent_AnchorAndDeltaSameWindow(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	sessionEnds := now.Add(2 * time.Hour)
	// Anchor 1 hour before period start at 20% used.
	insertSnapshot(t, s, now.Add(-25*time.Hour), ptrF(20), nil, ptrT(sessionEnds), nil, nil)
	// In-period: 35% then 60%, both continuations of their predecessors.
	insertSnapshot(t, s, now.Add(-12*time.Hour), ptrF(35), nil, ptrT(sessionEnds), nil, ptrB(true))
	insertSnapshot(t, s, now.Add(-2*time.Hour), ptrF(60), nil, ptrT(sessionEnds), nil, ptrB(true))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.ConsumedSessionPct == nil {
		t.Fatal("expected non-nil session pct")
	}
	// Deltas: (35-20) + (60-35) = 40
	if !near(*res.ConsumedSessionPct, 40, 1e-9) {
		t.Errorf("session pct: got %v want 40", *res.ConsumedSessionPct)
	}
}

// TestPercent_WindowResetsAccumulate: a snapshot whose continuity flag is
// false ("start") contributes its full *_used as a fresh window's worth.
// The 24h period spanning multiple sessions can still exceed 100% when each
// session is heavily used.
func TestPercent_WindowResetsAccumulate(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	// Anchor: 80% used.
	insertSnapshot(t, s, now.Add(-25*time.Hour), ptrF(80), nil, nil, nil, nil)
	// New session begins: 10% (start), then 90% (continuation).
	insertSnapshot(t, s, now.Add(-19*time.Hour), ptrF(10), nil, nil, nil, ptrB(false))
	insertSnapshot(t, s, now.Add(-16*time.Hour), ptrF(90), nil, nil, nil, ptrB(true))
	// Another reset: 25% (start).
	insertSnapshot(t, s, now.Add(-12*time.Hour), ptrF(25), nil, nil, nil, ptrB(false))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.ConsumedSessionPct == nil {
		t.Fatal("expected non-nil session pct")
	}
	// anchor → 10 (start) = 10
	// 10     → 90 (cont)  = 80
	// 90     → 25 (start) = 25
	// Total = 115.
	if !near(*res.ConsumedSessionPct, 115, 1e-9) {
		t.Errorf("session pct: got %v want 115", *res.ConsumedSessionPct)
	}
}

// TestPercent_NoAnchorUsesFirstInPeriodAsBaseline: with no snapshot prior
// to period_start, the first in-period snapshot only establishes baseline.
func TestPercent_NoAnchorUsesFirstInPeriodAsBaseline(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(40), nil, nil, nil, nil)
	insertSnapshot(t, s, now.Add(-2*time.Hour), ptrF(75), nil, nil, nil, ptrB(true))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.ConsumedSessionPct == nil {
		t.Fatal("expected non-nil session pct")
	}
	// Only the (40 → 75) delta counts; no anchor before period_start to
	// charge the leading-up consumption against.
	if !near(*res.ConsumedSessionPct, 35, 1e-9) {
		t.Errorf("session pct: got %v want 35", *res.ConsumedSessionPct)
	}
}

// TestPercent_NegativeDeltaClampsToZero: corrected-down snapshots within the
// same window must not subtract from the running total.
func TestPercent_NegativeDeltaClampsToZero(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(60), nil, nil, nil, nil)
	insertSnapshot(t, s, now.Add(-5*time.Hour), ptrF(55), nil, nil, nil, ptrB(true)) // dip
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(80), nil, nil, nil, ptrB(true))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// Deltas: max(0, 55-60)=0, then 80-55=25
	if !near(*res.ConsumedSessionPct, 25, 1e-9) {
		t.Errorf("session pct: got %v want 25", *res.ConsumedSessionPct)
	}
}

// TestPercent_ContinuationIgnoresWindowEndsDrift: with the explicit
// continuity flag, drift in *_window_ends between adjacent snapshots — even
// well beyond the old 10-minute heuristic tolerance — must not be
// misinterpreted as a window reset.
func TestPercent_ContinuationIgnoresWindowEndsDrift(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	endA := now.Add(2 * time.Hour)
	endB := endA.Add(45 * time.Minute) // far past the old 10-minute tolerance

	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(20), nil, ptrT(endA), nil, nil)
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(30), nil, ptrT(endB), nil, ptrB(true))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// Continuation per flag → delta 10, not (30 as a fresh start).
	if !near(*res.ConsumedSessionPct, 10, 1e-9) {
		t.Errorf("session pct: got %v want 10 (flag-driven continuation)", *res.ConsumedSessionPct)
	}
}

// TestPercent_NullFlagTreatedAsStart: a NULL continuity flag mid-series is
// treated as a start, matching the migration default for snapshots written
// before the flag existed.
func TestPercent_NullFlagTreatedAsStart(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(20), nil, nil, nil, nil)
	// NULL flag in the middle: contributes its raw used as a fresh start.
	insertSnapshot(t, s, now.Add(-5*time.Hour), ptrF(30), nil, nil, nil, nil)
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(50), nil, nil, nil, ptrB(true))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// No anchor (all snapshots fall inside the period); the first
	// in-period row only sets a baseline.
	// (20)  → 30 (NULL=start) = 30
	// 30    → 50 (cont)       = 20
	// Total = 50.
	if !near(*res.ConsumedSessionPct, 50, 1e-9) {
		t.Errorf("session pct: got %v want 50", *res.ConsumedSessionPct)
	}
}

// TestPercent_WeeklyAndSessionAreIndependent: the same snapshot stream
// drives separate session_pct and weekly_pct walks against their own
// *_used columns.
func TestPercent_WeeklyAndSessionAreIndependent(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	sessionEnds := now.Add(2 * time.Hour)
	weeklyEnds := now.Add(48 * time.Hour)
	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(20), ptrF(5), ptrT(sessionEnds), ptrT(weeklyEnds), nil)
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(60), ptrF(8), ptrT(sessionEnds), ptrT(weeklyEnds), ptrB(true))

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.ConsumedSessionPct == nil || !near(*res.ConsumedSessionPct, 40, 1e-9) {
		t.Errorf("session pct: got %v want 40", res.ConsumedSessionPct)
	}
	if res.ConsumedWeeklyPct == nil || !near(*res.ConsumedWeeklyPct, 3, 1e-9) {
		t.Errorf("weekly pct: got %v want 3", res.ConsumedWeeklyPct)
	}
}

// TestPercent_NoSnapshotsAtAll: fields are nil, signalling "couldn't
// measure", not 0.
func TestPercent_NoSnapshotsAtAll(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.ConsumedSessionPct != nil {
		t.Errorf("expected nil session pct, got %v", *res.ConsumedSessionPct)
	}
	if res.ConsumedWeeklyPct != nil {
		t.Errorf("expected nil weekly pct, got %v", *res.ConsumedWeeklyPct)
	}
}

func TestCalculate_DefaultPeriod(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	res, err := c.Calculate("")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.Period != "24h" {
		t.Errorf("default Period: got %q want 24h", res.Period)
	}
}

func TestCalculate_InvalidPeriod(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	cases := []string{"banana", "5xd", "-1h"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if _, err := c.Calculate(p); err == nil {
				t.Errorf("expected error for period %q", p)
			}
		})
	}
}

func TestParsePeriod(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"banana", 0, true},
		{"7days", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePeriod(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parsePeriod(%q) = %v want err", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePeriod(%q) err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePeriod(%q) = %v want %v", tc.in, got, tc.want)
			}
		})
	}
}
