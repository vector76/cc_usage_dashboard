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

func insertSnapshot(t *testing.T, s *store.Store, observed time.Time, sessionUsed, weeklyUsed *float64, sessionEnds, weeklyEnds *time.Time) {
	t.Helper()
	_, err := s.InsertQuotaSnapshot(observed, observed, "test", sessionUsed, sessionEnds, weeklyUsed, weeklyEnds, "{}")
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

func ptrF(f float64) *float64 { return &f }
func ptrT(t time.Time) *time.Time {
	return &t
}

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
// baseline; subsequent in-window snapshots accumulate deltas.
func TestPercent_AnchorAndDeltaSameWindow(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	sessionEnds := now.Add(2 * time.Hour) // same session end across snapshots
	// Anchor 1 hour before period start at 20% used.
	insertSnapshot(t, s, now.Add(-25*time.Hour), ptrF(20), nil, ptrT(sessionEnds), nil)
	// In-period: 35% then 60%.
	insertSnapshot(t, s, now.Add(-12*time.Hour), ptrF(35), nil, ptrT(sessionEnds), nil)
	insertSnapshot(t, s, now.Add(-2*time.Hour), ptrF(60), nil, ptrT(sessionEnds), nil)

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

// TestPercent_WindowResetsAccumulate: at a window reset the new window
// contributes only its curr.used; the unobserved tail of the prior window
// is dropped. The 24h period spanning multiple sessions can still exceed
// 100% when each session is heavily used.
func TestPercent_WindowResetsAccumulate(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	end1 := now.Add(-20 * time.Hour)
	end2 := now.Add(-15 * time.Hour)
	end3 := now.Add(-10 * time.Hour)

	// Anchor: 80% used in window ending at end1.
	insertSnapshot(t, s, now.Add(-25*time.Hour), ptrF(80), nil, ptrT(end1), nil)
	// Window 2 (ends end2): two snapshots 10% then 90%.
	insertSnapshot(t, s, now.Add(-19*time.Hour), ptrF(10), nil, ptrT(end2), nil)
	insertSnapshot(t, s, now.Add(-16*time.Hour), ptrF(90), nil, ptrT(end2), nil)
	// Window 3 (ends end3): 25%.
	insertSnapshot(t, s, now.Add(-12*time.Hour), ptrF(25), nil, ptrT(end3), nil)

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.ConsumedSessionPct == nil {
		t.Fatal("expected non-nil session pct")
	}
	// Walk:
	//  anchor(80,end1) → (10,end2): reset → curr.used = 10
	//  (10,end2)       → (90,end2): same  → 90-10     = 80
	//  (90,end2)       → (25,end3): reset → curr.used = 25
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

	sessionEnds := now.Add(2 * time.Hour)
	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(40), nil, ptrT(sessionEnds), nil)
	insertSnapshot(t, s, now.Add(-2*time.Hour), ptrF(75), nil, ptrT(sessionEnds), nil)

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

	sessionEnds := now.Add(2 * time.Hour)
	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(60), nil, ptrT(sessionEnds), nil)
	insertSnapshot(t, s, now.Add(-5*time.Hour), ptrF(55), nil, ptrT(sessionEnds), nil) // dip
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(80), nil, ptrT(sessionEnds), nil)

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// Deltas: max(0, 55-60)=0, then 80-55=25
	if !near(*res.ConsumedSessionPct, 25, 1e-9) {
		t.Errorf("session pct: got %v want 25", *res.ConsumedSessionPct)
	}
}

// TestPercent_ToleranceMatchesNearbyEnds: minute-level rounding in snapshot
// reset times must not be misread as a window reset.
func TestPercent_ToleranceMatchesNearbyEnds(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	endA := now.Add(2 * time.Hour)
	endB := endA.Add(60 * time.Second) // 1 min jitter, within windowMatchTolerance

	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(20), nil, ptrT(endA), nil)
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(30), nil, ptrT(endB), nil)

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	// Same window per tolerance → delta 10, not (100-20)+30 = 110.
	if !near(*res.ConsumedSessionPct, 10, 1e-9) {
		t.Errorf("session pct: got %v want 10 (tolerance not applied)", *res.ConsumedSessionPct)
	}
}

// TestPercent_WeeklyAndSessionAreIndependent: the same snapshot stream
// drives separate session_pct and weekly_pct walks against their own
// *_window_ends columns.
func TestPercent_WeeklyAndSessionAreIndependent(t *testing.T) {
	now := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	sessionEnds := now.Add(2 * time.Hour)
	weeklyEnds := now.Add(48 * time.Hour)
	insertSnapshot(t, s, now.Add(-10*time.Hour), ptrF(20), ptrF(5), ptrT(sessionEnds), ptrT(weeklyEnds))
	insertSnapshot(t, s, now.Add(-1*time.Hour), ptrF(60), ptrF(8), ptrT(sessionEnds), ptrT(weeklyEnds))

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

