package slack

import (
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func newCalc(t *testing.T) (*Calculator, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := Config{
		QuietPeriodSeconds:      300,
		BaselineMaxAgeHours:     48,
		SessionSurplusThreshold: 0.50,
		WeeklySurplusThreshold:  0.10,
	}
	return NewCalculator(s.DB(), cfg), s
}

func insertWindow(t *testing.T, db *sql.DB, kind string, startedAt, endsAt time.Time, baselineTotal float64, baselineSource string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO windows (kind, started_at, ends_at, baseline_total, baseline_source, closed)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		kind, store.FormatTime(startedAt), store.FormatTime(endsAt), baselineTotal, baselineSource,
	)
	if err != nil {
		t.Fatalf("insert window: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func fptr(v float64) *float64 { return &v }

// (a) combineSlackFractions returns min(a, b), propagates nil correctly,
// and returns nil if either input is nil.
func TestCombineSlackFractions(t *testing.T) {
	c := &Calculator{}

	tests := []struct {
		name string
		a, b *WindowMetrics
		want *float64
	}{
		{"both present, a smaller", &WindowMetrics{SlackFraction: fptr(0.10)}, &WindowMetrics{SlackFraction: fptr(0.50)}, fptr(0.10)},
		{"both present, b smaller", &WindowMetrics{SlackFraction: fptr(0.50)}, &WindowMetrics{SlackFraction: fptr(0.10)}, fptr(0.10)},
		{"both present, equal", &WindowMetrics{SlackFraction: fptr(0.30)}, &WindowMetrics{SlackFraction: fptr(0.30)}, fptr(0.30)},
		{"both present, negative wins", &WindowMetrics{SlackFraction: fptr(0.20)}, &WindowMetrics{SlackFraction: fptr(-0.05)}, fptr(-0.05)},
		{"both metrics nil", nil, nil, nil},
		{"a metric nil", nil, &WindowMetrics{SlackFraction: fptr(0.5)}, nil},
		{"b metric nil", &WindowMetrics{SlackFraction: fptr(0.5)}, nil, nil},
		{"a SlackFraction nil", &WindowMetrics{SlackFraction: nil}, &WindowMetrics{SlackFraction: fptr(0.5)}, nil},
		{"b SlackFraction nil", &WindowMetrics{SlackFraction: fptr(0.5)}, &WindowMetrics{SlackFraction: nil}, nil},
		{"both SlackFraction nil", &WindowMetrics{SlackFraction: nil}, &WindowMetrics{SlackFraction: nil}, nil},
	}

	fmtFrac := func(p *float64) string {
		if p == nil {
			return "<nil>"
		}
		return fmt.Sprintf("%v", *p)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.combineSlackFractions(tt.a, tt.b)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("nil-ness mismatch: got=%s want=%s", fmtFrac(got), fmtFrac(tt.want))
			}
			if got != nil && *got != *tt.want {
				t.Errorf("got %v, want %v", *got, *tt.want)
			}
		})
	}
}

// (b) GetSlack returns null slack_combined_fraction when the session window
// has no events yet (per docs/slack-indicator.md: window is undefined until
// first use). Two sub-cases: the simple "no windows at all" case and the
// more discriminating case where a weekly window exists with events but no
// session window does — combined must still be nil.
func TestGetSlack_NullCombinedFractionWhenNoEvents(t *testing.T) {
	t.Run("empty database", func(t *testing.T) {
		c, s := newCalc(t)
		defer s.Close()

		resp, err := c.GetSlack()
		if err != nil {
			t.Fatalf("GetSlack: %v", err)
		}
		if resp.SlackCombinedFraction != nil {
			t.Errorf("expected nil slack_combined_fraction, got %v", *resp.SlackCombinedFraction)
		}
		if resp.ReleaseRecommended {
			t.Error("expected release_recommended=false")
		}
	})

	t.Run("weekly window present, no session window", func(t *testing.T) {
		c, s := newCalc(t)
		defer s.Close()

		now := time.Now().UTC()
		insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0, "snapshot:1")

		// Event inside the weekly window — gives weekly a non-nil
		// SlackFraction. The session window does not exist.
		cost := 5.0
		if _, err := s.InsertUsageEvent(
			now.Add(-1*time.Hour), "api",
			"sess-1", "msg-1", "", "claude-3-5-sonnet-20241022",
			1000, 500, 0, 0,
			&cost, "reported", "{}",
		); err != nil {
			t.Fatalf("insert event: %v", err)
		}

		resp, err := c.GetSlack()
		if err != nil {
			t.Fatalf("GetSlack: %v", err)
		}
		if resp.SlackCombinedFraction != nil {
			t.Errorf("expected nil slack_combined_fraction when session absent, got %v", *resp.SlackCombinedFraction)
		}
		if resp.ReleaseRecommended {
			t.Error("expected release_recommended=false when session window absent")
		}
	})
}

// (c) RecordRelease writes a row whose window_id resolves to the session
// window containing released_at.
func TestRecordRelease_ResolvesWindowID(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	startedAt := now.Add(-1 * time.Hour)
	endsAt := now.Add(4 * time.Hour)
	wantID := insertWindow(t, s.DB(), "session", startedAt, endsAt, 1000.0, "snapshot:1")

	// Also insert a non-overlapping older session window to ensure the
	// resolver picks the one bracketing released_at, not just the latest.
	insertWindow(t, s.DB(), "session", now.Add(-10*time.Hour), now.Add(-5*time.Hour), 800.0, "snapshot:0")

	cost := 1.20
	slackVal := 8.40
	releaseID, err := c.RecordRelease(now, "nightly-lint", &cost, &slackVal, "session")
	if err != nil {
		t.Fatalf("RecordRelease: %v", err)
	}

	var gotWindowID int64
	err = s.DB().QueryRow(`SELECT window_id FROM slack_releases WHERE id = ?`, releaseID).Scan(&gotWindowID)
	if err != nil {
		t.Fatalf("query slack_releases: %v", err)
	}
	if gotWindowID != wantID {
		t.Errorf("window_id: got %d, want %d", gotWindowID, wantID)
	}
}

// (d) RecordRelease returns an error when no matching session window
// contains released_at.
func TestRecordRelease_ErrorWhenNoWindow(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	// Insert a weekly window that contains released_at, but no session
	// window — RecordRelease must still error.
	insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0, "snapshot:1")

	if _, err := c.RecordRelease(now, "nightly-lint", nil, nil, "session"); err == nil {
		t.Error("expected error when no session window matches released_at")
	}
}

// priority_quiet gate must pass when the user has been idle long enough
// (quietFor >= QuietPeriodSeconds) and fail when the most recent event is
// within the quiet window. Guards against re-introducing the historical
// inversion where the gate was `quietFor > 0`.
func TestPriorityQuietGate_NotInverted(t *testing.T) {
	tests := []struct {
		name        string
		eventOffset time.Duration // negative = in the past
		wantPass    bool
	}{
		{"recent activity fails the gate", -10 * time.Second, false},
		{"old activity passes the gate", -10 * time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newCalc(t)
			defer s.Close()

			now := time.Now().UTC()
			insertWindow(t, s.DB(), "session", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")

			cost := 1.0
			if _, err := s.InsertUsageEvent(
				now.Add(tt.eventOffset), "api",
				"sess-x", "msg-x", "", "claude-3-5-sonnet-20241022",
				100, 50, 0, 0,
				&cost, "reported", "{}",
			); err != nil {
				t.Fatalf("insert event: %v", err)
			}

			resp, err := c.GetSlack()
			if err != nil {
				t.Fatalf("GetSlack: %v", err)
			}
			got := resp.Gates["priority_quiet"]
			if got != tt.wantPass {
				t.Errorf("priority_quiet gate: got %v, want %v (quiet_for=%ds)",
					got, tt.wantPass, resp.PriorityQuietForSeconds)
			}
		})
	}
}

// (e) SetPaused(true) forces release_recommended=false even when slack is
// positive. Asserted via the typed SlackResponse.ReleaseRecommended field
// rather than any gate-map key.
func TestSetPaused_ForcesReleaseRecommendedFalse(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	// percent_used kept tiny on both windows so the slack fractions are
	// solidly positive; the point of the test is that pause overrides
	// them.
	insertWindow(t, s.DB(), "session", now.Add(-1*time.Hour), now.Add(4*time.Hour), 5.0, "snapshot:1")
	insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 3.0, "snapshot:1")

	// Fresh quota snapshot satisfies the freshness gate.
	seedFreshSnapshot(t, s, now, 5.0)

	c.SetPaused(true)
	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}

	if !resp.Paused {
		t.Error("expected Paused=true")
	}
	if resp.SlackCombinedFraction == nil {
		t.Fatal("test setup invalid: expected positive slack fraction, got nil")
	}
	if *resp.SlackCombinedFraction <= 0 {
		t.Fatalf("test setup invalid: expected positive slack fraction, got %v", *resp.SlackCombinedFraction)
	}
	if resp.ReleaseRecommended {
		t.Error("expected ReleaseRecommended=false when paused, regardless of slack")
	}
}

// (f) Computed PercentExpected and SlackFraction match the percent-only
// formulas from docs/slack-indicator.md:
//
//	progress(t)       = clamp((t - t0) / (t1 - t0), 0, 1)
//	percent_expected  = 100 * progress(t)
//	slack_fraction    = (percent_expected - percent_used) / 100
//
// percent_used comes from the latest in-window snapshot (= the window's
// baseline_total). Window bounds bracket time.Now() so the test doesn't
// depend on wall-clock alignment.
func TestComputeMetrics_FormulasMatchDocs(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	const percentUsed = 5.0 // baseline_total stored on the window row

	now := time.Now().UTC()
	startedAt := now.Add(-1 * time.Hour)
	endsAt := now.Add(4 * time.Hour) // 5h session window
	insertWindow(t, s.DB(), "session", startedAt, endsAt, percentUsed, "snapshot:1")
	// A fresh snapshot keeps the freshness gate happy and proves
	// computeMetrics reads percent from the windows row, not the snapshot
	// table directly.
	seedFreshSnapshot(t, s, now, percentUsed)

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	m := resp.Session
	if m == nil {
		t.Fatal("expected Session metrics, got nil")
	}

	// Sub-second wall-clock drift between insert and read is negligible
	// against the tolerances below (a 5h window).
	windowDur := endsAt.Sub(startedAt).Seconds()
	elapsed := time.Since(startedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > windowDur {
		elapsed = windowDur
	}
	progress := elapsed / windowDur
	wantExpected := 100 * progress
	wantSlackFrac := (wantExpected - percentUsed) / 100

	if m.PercentUsed == nil || math.Abs(*m.PercentUsed-percentUsed) > 1e-6 {
		t.Errorf("PercentUsed: got %v, want %v", m.PercentUsed, percentUsed)
	}
	const tol = 0.05 // half-a-percent tolerance on percent_expected
	if math.Abs(m.PercentExpected-wantExpected) > tol {
		t.Errorf("PercentExpected: got %v, want ~%v (tol %v)", m.PercentExpected, wantExpected, tol)
	}
	if m.SlackFraction == nil {
		t.Fatal("expected non-nil SlackFraction")
	}
	if math.Abs(*m.SlackFraction-wantSlackFrac) > tol/100 {
		t.Errorf("SlackFraction: got %v, want ~%v", *m.SlackFraction, wantSlackFrac)
	}
}

// PercentUsed and SlackFraction must be nil when no in-window snapshot has
// arrived yet — the headroom gate then fails safe rather than assuming 0%.
func TestComputeMetrics_NilWhenNoInWindowSnapshot(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	// Insert a window with a NULL baseline_total — i.e. no in-window
	// snapshot has ever set it.
	if _, err := s.DB().Exec(
		`INSERT INTO windows (kind, started_at, ends_at, baseline_total, baseline_source, closed)
		 VALUES ('session', ?, ?, NULL, 'default', 0)`,
		store.FormatTime(now.Add(-1*time.Hour)),
		store.FormatTime(now.Add(4*time.Hour)),
	); err != nil {
		t.Fatalf("insert window: %v", err)
	}

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.Session == nil {
		t.Fatal("expected Session metrics, got nil")
	}
	if resp.Session.PercentUsed != nil {
		t.Errorf("PercentUsed: got %v, want nil", *resp.Session.PercentUsed)
	}
	if resp.Session.SlackFraction != nil {
		t.Errorf("SlackFraction: got %v, want nil", *resp.Session.SlackFraction)
	}
	if resp.Gates["session_headroom"] {
		t.Error("session_headroom gate must fail when percent_used is unknown")
	}
	if resp.ReleaseRecommended {
		t.Error("release_recommended must be false when percent_used is unknown")
	}
}

// seedFreshSnapshot inserts a quota snapshot at receivedAt with the given
// session-used percentage. Used by gate-boundary tests.
func seedFreshSnapshot(t *testing.T, s *store.Store, receivedAt time.Time, sessionUsed float64) {
	t.Helper()
	if _, err := s.InsertQuotaSnapshot(
		receivedAt, receivedAt, "userscript",
		&sessionUsed, nil,
		&sessionUsed, nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

// newCalcWithThresholds builds a Calculator with custom surplus thresholds.
// Used by TestHeadroomGates_DualThresholdsBoundaries to put both gates near
// their boundaries with the same window/event setup.
func newCalcWithThresholds(t *testing.T, sessionThresh, weeklyThresh float64) (*Calculator, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := Config{
		QuietPeriodSeconds:      300,
		BaselineMaxAgeHours:     48,
		SessionSurplusThreshold: sessionThresh,
		WeeklySurplusThreshold:  weeklyThresh,
	}
	return NewCalculator(s.DB(), cfg), s
}

// (g) session_headroom and weekly_headroom gates fire independently against
// their own configured surplus thresholds, driven by each window's
// percent_used (= windows.baseline_total) — not by usage_events.
// release_recommended requires both to pass (plus the other gates).
//
// Setup: session window started_at=now-1h, ends_at=now+4h
//
//	progress=0.2, percent_expected=20,
//	slack_fraction(session) = (20 - percent_used_session) / 100
//
// weekly window started_at=now-24h, ends_at=now+6*24h
//
//	progress=24/168≈0.1428, percent_expected≈14.28,
//	slack_fraction(weekly) = (14.28 - percent_used_weekly) / 100
//
// Both gate thresholds set to 0.10 below.
func TestHeadroomGates_DualThresholdsBoundaries(t *testing.T) {
	tests := []struct {
		name                                       string
		sessionPercentUsed, weeklyPercentUsed      float64
		wantSession, wantWeekly, wantRelease       bool
	}{
		{
			// session_frac = (20 - 5)/100 = 0.15 ≥ 0.10 ✓
			// weekly_frac  = (14.28 - 3)/100 ≈ 0.113 ≥ 0.10 ✓
			name:               "both pass",
			sessionPercentUsed: 5.0, weeklyPercentUsed: 3.0,
			wantSession: true, wantWeekly: true, wantRelease: true,
		},
		{
			// session_frac = (20 - 15)/100 = 0.05 < 0.10 ✗
			name:               "session fails, weekly passes",
			sessionPercentUsed: 15.0, weeklyPercentUsed: 3.0,
			wantSession: false, wantWeekly: true, wantRelease: false,
		},
		{
			// weekly_frac = (14.28 - 10)/100 ≈ 0.043 < 0.10 ✗
			name:               "weekly fails, session passes",
			sessionPercentUsed: 5.0, weeklyPercentUsed: 10.0,
			wantSession: true, wantWeekly: false, wantRelease: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newCalcWithThresholds(t, 0.10, 0.10)
			defer s.Close()

			now := time.Now().UTC()
			insertWindow(t, s.DB(), "session", now.Add(-1*time.Hour), now.Add(4*time.Hour), tt.sessionPercentUsed, "snapshot:1")
			insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), tt.weeklyPercentUsed, "snapshot:1")

			// Fresh snapshot so the freshness gate doesn't block release.
			seedFreshSnapshot(t, s, now, tt.sessionPercentUsed)

			resp, err := c.GetSlack()
			if err != nil {
				t.Fatalf("GetSlack: %v", err)
			}
			if got := resp.Gates["session_headroom"]; got != tt.wantSession {
				t.Errorf("session_headroom: got %v want %v", got, tt.wantSession)
			}
			if got := resp.Gates["weekly_headroom"]; got != tt.wantWeekly {
				t.Errorf("weekly_headroom: got %v want %v", got, tt.wantWeekly)
			}
			if resp.ReleaseRecommended != tt.wantRelease {
				t.Errorf("ReleaseRecommended: got %v want %v", resp.ReleaseRecommended, tt.wantRelease)
			}
		})
	}
}

// (h) Baseline freshness gate is purely an age check: passes iff a snapshot
// exists and is no older than BaselineMaxAgeHours.
func TestBaselineFreshness_AgeBoundary(t *testing.T) {
	tests := []struct {
		name     string
		ageHours int
		wantPass bool
	}{
		{"snapshot well inside max age", 47, true},
		{"snapshot just past max age", 49, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newCalc(t)
			defer s.Close()

			now := time.Now().UTC()
			receivedAt := now.Add(-time.Duration(tt.ageHours) * time.Hour)
			seedFreshSnapshot(t, s, receivedAt, 5.0)

			resp, err := c.GetSlack()
			if err != nil {
				t.Fatalf("GetSlack: %v", err)
			}
			if got := resp.Gates["baseline_freshness"]; got != tt.wantPass {
				t.Errorf("baseline_freshness gate: got %v, want %v (age=%dh)",
					got, tt.wantPass, tt.ageHours)
			}
		})
	}
}

// (j) Baseline freshness gate fails when no snapshot has ever been recorded
// (failure mode "no baseline snapshot ever recorded" → release_recommended=false).
func TestBaselineFreshness_NoSnapshot(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	insertWindow(t, s.DB(), "session", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.Gates["baseline_freshness"] {
		t.Error("baseline_freshness must fail when no snapshot exists")
	}
	if resp.ReleaseRecommended {
		t.Error("release_recommended must be false when no snapshot exists")
	}
}
