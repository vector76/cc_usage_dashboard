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
		QuietPeriodSeconds:     300,
		ReleaseThreshold:       0.10,
		BaselineMaxAgeHours:    48,
		BaselineDriftThreshold: 0.25,
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

// (b) GetSlack returns null slack_combined_fraction when the 5-hour window
// has no events yet (per docs/slack-indicator.md: window is undefined until
// first use). Two sub-cases: the simple "no windows at all" case and the
// more discriminating case where a weekly window exists with events but no
// 5-hour window does — combined must still be nil.
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

	t.Run("weekly window present, no five_hour window", func(t *testing.T) {
		c, s := newCalc(t)
		defer s.Close()

		now := time.Now().UTC()
		insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0, "snapshot:1")

		// Event inside the weekly window — gives weekly a non-nil
		// SlackFraction. The 5-hour window does not exist.
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
			t.Errorf("expected nil slack_combined_fraction when 5-hour absent, got %v", *resp.SlackCombinedFraction)
		}
		if resp.ReleaseRecommended {
			t.Error("expected release_recommended=false when 5-hour window absent")
		}
	})
}

// (c) RecordRelease writes a row whose window_id resolves to the 5-hour
// window containing released_at.
func TestRecordRelease_ResolvesWindowID(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	startedAt := now.Add(-1 * time.Hour)
	endsAt := now.Add(4 * time.Hour)
	wantID := insertWindow(t, s.DB(), "five_hour", startedAt, endsAt, 1000.0, "snapshot:1")

	// Also insert a non-overlapping older five_hour window to ensure the
	// resolver picks the one bracketing released_at, not just the latest.
	insertWindow(t, s.DB(), "five_hour", now.Add(-10*time.Hour), now.Add(-5*time.Hour), 800.0, "snapshot:0")

	cost := 1.20
	slackVal := 8.40
	releaseID, err := c.RecordRelease(now, "nightly-lint", &cost, &slackVal, "five_hour")
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

// (d) RecordRelease returns an error when no matching 5-hour window
// contains released_at.
func TestRecordRelease_ErrorWhenNoWindow(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	// Insert a weekly window that contains released_at, but no five_hour
	// window — RecordRelease must still error.
	insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0, "snapshot:1")

	if _, err := c.RecordRelease(now, "nightly-lint", nil, nil, "five_hour"); err == nil {
		t.Error("expected error when no five_hour window matches released_at")
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
			insertWindow(t, s.DB(), "five_hour", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")

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
	insertWindow(t, s.DB(), "five_hour", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")
	insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 5000.0, "snapshot:1")

	// Small consumption keeps slack positive on both windows.
	cost := 5.0
	if _, err := s.InsertUsageEvent(
		now.Add(-30*time.Minute), "api",
		"sess-1", "msg-1", "", "claude-3-5-sonnet-20241022",
		1000, 500, 0, 0,
		&cost, "reported", "{}",
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Fresh quota snapshot satisfies any freshness gate.
	rem, total := 950.0, 1000.0
	if _, err := s.InsertQuotaSnapshot(
		now, now, "userscript",
		&rem, &total, nil,
		&rem, &total, nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

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

// (f) Computed Progress/Expected/Slack match the formulas from
// docs/slack-indicator.md:
//
//	progress(t)       = clamp((t - t0) / (t1 - t0), 0, 1)
//	E(t)              = Q * progress(t)
//	slack(t)          = E(t) - U(t)
//	slack_fraction(t) = slack(t) / Q
//
// Window bounds bracket time.Now() so the test does not depend on
// wall-clock alignment.
func TestComputeMetrics_FormulasMatchDocs(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	const baseline = 1000.0
	const consumed = 50.0

	now := time.Now().UTC()
	startedAt := now.Add(-1 * time.Hour)
	endsAt := now.Add(4 * time.Hour) // 5-hour window total
	insertWindow(t, s.DB(), "five_hour", startedAt, endsAt, baseline, "snapshot:1")

	cost := consumed
	if _, err := s.InsertUsageEvent(
		now.Add(-30*time.Minute), "api",
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
	m := resp.FiveHour
	if m == nil {
		t.Fatal("expected FiveHour metrics, got nil")
	}

	// Recompute the expected values relative to a "now" sampled inside
	// the test, using the same formulas the docs prescribe. The window is
	// 5 hours; sub-second drift between inserting and reading is
	// negligible relative to the tolerances below (0.5 of $1000).
	windowDur := endsAt.Sub(startedAt).Seconds()
	elapsed := time.Since(startedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > windowDur {
		elapsed = windowDur
	}
	progress := elapsed / windowDur
	expectedE := baseline * progress
	expectedSlack := expectedE - consumed
	expectedSlackFrac := expectedSlack / baseline

	// Consumed must equal the inserted cost exactly.
	if math.Abs(m.Consumed-consumed) > 1e-6 {
		t.Errorf("Consumed: got %v, want %v", m.Consumed, consumed)
	}

	// Allow a small tolerance for time-since-insertion drift.
	const tol = 0.5
	if math.Abs(m.Expected-expectedE) > tol {
		t.Errorf("Expected (E): got %v, want ~%v (tol %v)", m.Expected, expectedE, tol)
	}
	if math.Abs(m.Slack-expectedSlack) > tol {
		t.Errorf("Slack (E - U): got %v, want ~%v (tol %v)", m.Slack, expectedSlack, tol)
	}
	if m.SlackFraction == nil {
		t.Fatal("expected non-nil SlackFraction")
	}
	if math.Abs(*m.SlackFraction-expectedSlackFrac) > tol/baseline {
		t.Errorf("SlackFraction: got %v, want ~%v", *m.SlackFraction, expectedSlackFrac)
	}
}

// seedFreshSnapshot inserts a quota snapshot at receivedAt with a fixed
// five-hour total (=quotaTotal). Returns nothing; used by gate-boundary tests.
func seedFreshSnapshot(t *testing.T, s *store.Store, receivedAt time.Time, quotaTotal float64) {
	t.Helper()
	rem := quotaTotal
	if _, err := s.InsertQuotaSnapshot(
		receivedAt, receivedAt, "userscript",
		&rem, &quotaTotal, nil,
		&rem, &quotaTotal, nil,
		"{}",
	); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

// (g) Headroom gate fires off ReleaseThreshold, not the historical
// HeadroomThreshold. Boundary: gate passes at exactly threshold and below
// threshold fails.
func TestHeadroomGate_ReleaseThresholdBoundary(t *testing.T) {
	tests := []struct {
		name     string
		consumed float64 // dollars consumed in 5h window (baseline=1000)
		// Window: started_at = now-1h, ends_at = now+4h, baseline=1000.
		// progress=0.2, expected=200, slack=200-consumed,
		// slack_fraction(5h) = (200-consumed)/1000.
		// Weekly: started_at = now-24h, ends_at = now+6*24h, baseline=10000.
		// progress=24/(7*24)=1/7≈0.1428, expected=1428.6,
		// slack=1428.6-consumed, slack_fraction(weekly)≈0.1428 - consumed/10000.
		// 5h is the binding (smaller) fraction in this layout.
		wantHeadroom bool
	}{
		// Threshold = 0.10. consumed=100 → 5h slack_fraction = (200-100)/1000 = 0.10.
		{"exactly at threshold", 100.0, true},
		// consumed=101 → 5h slack_fraction = 0.099 < 0.10.
		{"just below threshold", 101.0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newCalc(t)
			defer s.Close()

			now := time.Now().UTC()
			insertWindow(t, s.DB(), "five_hour", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")
			insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 10000.0, "snapshot:1")

			cost := tt.consumed
			if _, err := s.InsertUsageEvent(
				now.Add(-30*time.Minute), "api",
				"sess-h", "msg-h", "", "claude-3-5-sonnet-20241022",
				1, 1, 0, 0,
				&cost, "reported", "{}",
			); err != nil {
				t.Fatalf("insert event: %v", err)
			}
			// Fresh snapshot so freshness gate doesn't interfere.
			seedFreshSnapshot(t, s, now, 1000.0)

			resp, err := c.GetSlack()
			if err != nil {
				t.Fatalf("GetSlack: %v", err)
			}
			if resp.SlackCombinedFraction == nil {
				t.Fatal("expected non-nil slack_combined_fraction")
			}
			if got := resp.Gates["headroom"]; got != tt.wantHeadroom {
				t.Errorf("headroom gate: got %v, want %v (combined=%v)",
					got, tt.wantHeadroom, *resp.SlackCombinedFraction)
			}
		})
	}
}

// (h) Baseline freshness gate boundary on BaselineMaxAgeHours: a snapshot
// within max age passes regardless of drift (the AND-gate short-circuits on
// the age leg). The complementary "stale + small drift" case is covered by
// the drift-threshold-boundary test below.
func TestBaselineFreshness_MaxAgeBoundary(t *testing.T) {
	c, s := newCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	insertWindow(t, s.DB(), "five_hour", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")
	insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 10000.0, "snapshot:1")

	// Snapshot well inside max age (47h < 48h) — gate must pass even though
	// drift is huge (50% of quota), because the age leg of the AND is false.
	receivedAt := now.Add(-47 * time.Hour)
	seedFreshSnapshot(t, s, receivedAt, 1000.0)

	hugeCost := 500.0 // 50% of quota — would trip drift if age check engaged
	if _, err := s.InsertUsageEvent(
		receivedAt.Add(1*time.Hour), "api",
		"sess-d", "msg-d", "", "claude-3-5-sonnet-20241022",
		1, 1, 0, 0,
		&hugeCost, "reported", "{}",
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if !resp.Gates["baseline_freshness"] {
		t.Errorf("baseline_freshness gate: got false, want true (age < max_age, drift must be ignored)")
	}
}

// (i) Baseline freshness gate boundary on BaselineDriftThreshold once the
// snapshot is older than max age. Threshold = 25% of quota_total = 250.
func TestBaselineFreshness_DriftThresholdBoundary(t *testing.T) {
	tests := []struct {
		name     string
		drift    float64 // consumption since stale snapshot
		wantPass bool
	}{
		// drift exactly at threshold — gate passes (we use <=).
		{"drift at threshold", 250.0, true},
		// drift just over threshold — gate fails.
		{"drift just over threshold", 250.01, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := newCalc(t)
			defer s.Close()

			now := time.Now().UTC()
			insertWindow(t, s.DB(), "five_hour", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")
			insertWindow(t, s.DB(), "weekly", now.Add(-24*time.Hour), now.Add(6*24*time.Hour), 10000.0, "snapshot:1")

			// Snapshot 49 hours ago — older than max_age=48h, so drift gate engages.
			receivedAt := now.Add(-49 * time.Hour)
			seedFreshSnapshot(t, s, receivedAt, 1000.0)

			cost := tt.drift
			if _, err := s.InsertUsageEvent(
				receivedAt.Add(1*time.Hour), "api",
				"sess-d", "msg-d", "", "claude-3-5-sonnet-20241022",
				1, 1, 0, 0,
				&cost, "reported", "{}",
			); err != nil {
				t.Fatalf("insert event: %v", err)
			}

			resp, err := c.GetSlack()
			if err != nil {
				t.Fatalf("GetSlack: %v", err)
			}
			if got := resp.Gates["baseline_freshness"]; got != tt.wantPass {
				t.Errorf("baseline_freshness gate: got %v, want %v (drift=%v)",
					got, tt.wantPass, tt.drift)
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
	insertWindow(t, s.DB(), "five_hour", now.Add(-1*time.Hour), now.Add(4*time.Hour), 1000.0, "snapshot:1")

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
