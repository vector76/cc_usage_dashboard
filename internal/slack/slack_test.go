package slack

import (
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/anthropics/usage-dashboard/internal/store"
)

func newCalc(t *testing.T) (*Calculator, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := Config{
		HeadroomThreshold:    0.10,
		QuietPeriodSeconds:   300,
		FreshnessThresholdMs: 48 * 3600 * 1000, // 48 hours
	}
	return NewCalculator(s.DB(), cfg), s
}

func insertWindow(t *testing.T, db *sql.DB, kind string, startedAt, endsAt time.Time, baselineTotal float64, baselineSource string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO windows (kind, started_at, ends_at, baseline_total, baseline_source, closed)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		kind, startedAt, endsAt, baselineTotal, baselineSource,
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
		if resp.SlackFraction != nil {
			t.Errorf("expected nil slack_combined_fraction, got %v", *resp.SlackFraction)
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
		if resp.SlackFraction != nil {
			t.Errorf("expected nil slack_combined_fraction when 5-hour absent, got %v", *resp.SlackFraction)
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
	releaseID, err := c.RecordRelease(now, "nightly-lint", &cost, &slackVal)
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

	if _, err := c.RecordRelease(now, "nightly-lint", nil, nil); err == nil {
		t.Error("expected error when no five_hour window matches released_at")
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
	if resp.SlackFraction == nil {
		t.Fatal("test setup invalid: expected positive slack fraction, got nil")
	}
	if *resp.SlackFraction <= 0 {
		t.Fatalf("test setup invalid: expected positive slack fraction, got %v", *resp.SlackFraction)
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
	m := resp.FiveHourWindow
	if m == nil {
		t.Fatal("expected FiveHourWindow metrics, got nil")
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

	// Progress (cumulative consumed) must equal the inserted cost exactly.
	if math.Abs(m.Progress-consumed) > 1e-6 {
		t.Errorf("Progress (consumed): got %v, want %v", m.Progress, consumed)
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
