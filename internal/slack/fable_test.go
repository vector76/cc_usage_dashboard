package slack

import (
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// insertSnapshot writes one quota_snapshots row. Any of the three readings may
// be nil to model a row the page did not render or the parser did not match —
// the userscript takes all three from a single DOM scan and omits what it did
// not find, so the combination of nils is itself meaningful.
func insertSnapshot(t *testing.T, s *store.Store, observedAt time.Time, session, weekly, fable *float64) {
	t.Helper()
	if _, err := s.InsertQuotaSnapshotRecord(store.QuotaSnapshotRecord{
		ObservedAt:      observedAt,
		ReceivedAt:      observedAt,
		Source:          "userscript",
		SessionUsed:     session,
		WeeklyUsed:      weekly,
		FableWeeklyUsed: fable,
	}); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

// insertFableSnapshot writes a row that parsed the weekly section. fable is nil
// to model a source that does not render the sub-row.
func insertFableSnapshot(t *testing.T, s *store.Store, observedAt time.Time, weekly, fable *float64) {
	t.Helper()
	insertSnapshot(t, s, observedAt, nil, weekly, fable)
}

// fableCalc builds a calculator whose weekly thresholds match the shipped
// defaults, so the fable gate (which reuses them) is exercised realistically.
func fableCalc(t *testing.T) (*Calculator, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := Config{
		BaselineMaxAgeSeconds:    480,
		SessionSurplusThreshold:  0.50,
		WeeklySurplusThreshold:   0.10,
		SessionAbsoluteThreshold: 0.98,
		WeeklyAbsoluteThreshold:  0.80,
	}
	return NewCalculator(s.DB(), cfg), s
}

// weeklyWindowHalfElapsed inserts a weekly window that is exactly half over,
// with the "All models" aggregate deep in the green (10% used at 50% pace →
// slack 0.40, comfortably over the 0.10 surplus threshold). Any gate failure
// in these tests therefore comes from the Fable row alone.
func weeklyWindowHalfElapsed(t *testing.T, c *Calculator, s *store.Store) time.Time {
	t.Helper()
	now := time.Now().UTC()
	start := now.Add(-84 * time.Hour)
	end := now.Add(84 * time.Hour)
	insertWindow(t, s.DB(), "weekly", start, end, 10.0, "snapshot")
	return now
}

// (a) A Fable row that is ahead of pace blocks the release even though the
// aggregate weekly row is deep in the green. This is the whole point of the
// gate: "All models" can round-trip low while Fable burns out the sub-quota.
func TestFableHeadroomGate_BlocksWhenFableAheadOfPace(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	// 95% used at 50% pace → slack -0.45, and 95 > 20 so the absolute leg
	// fails too.
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), fptr(95.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.FableWeekly == nil {
		t.Fatal("expected fable_weekly metrics to be present")
	}
	if resp.FableWeekly.PercentUsed == nil || *resp.FableWeekly.PercentUsed != 95.0 {
		t.Fatalf("expected fable percent_used=95, got %v", resp.FableWeekly.PercentUsed)
	}
	if resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom gate to fail")
	}
	if !resp.Gates["weekly_headroom"] {
		t.Error("weekly (aggregate) gate should still pass; test is not isolating fable")
	}
	if resp.ReleaseRecommended {
		t.Error("expected release_recommended=false when fable is out of the green zone")
	}
}

// (b) Both rows in the green zone → the fable gate passes and does not
// interfere with the release decision.
func TestFableHeadroomGate_PassesWhenBothInGreenZone(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	// 12% used at 50% pace → slack 0.38 ≥ 0.10.
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), fptr(12.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if !resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom gate to pass")
	}
	if resp.FableWeekly == nil || resp.FableWeekly.SlackFraction == nil {
		t.Fatal("expected a fable slack_fraction")
	}
	if got := *resp.FableWeekly.SlackFraction; got < 0.37 || got > 0.39 {
		t.Errorf("expected fable slack_fraction ≈ 0.38, got %v", got)
	}
}

// (c) The absolute leg carries the gate early in the week, mirroring the
// aggregate weekly rule: 15% used at 5% pace is behind on pace-relative
// surplus but under the 20% absolute floor.
func TestFableHeadroomGate_AbsoluteLegPassesEarlyInWeek(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := time.Now().UTC()
	start := now.Add(-8 * time.Hour)
	end := now.Add(160 * time.Hour)
	insertWindow(t, s.DB(), "weekly", start, end, 5.0, "snapshot")
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(5.0), fptr(15.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.FableWeekly == nil || resp.FableWeekly.SlackFraction == nil {
		t.Fatal("expected a fable slack_fraction")
	}
	if *resp.FableWeekly.SlackFraction >= c.config.WeeklySurplusThreshold {
		t.Fatalf("test precondition: pace leg should fail, got slack %v", *resp.FableWeekly.SlackFraction)
	}
	if !resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom to pass on the absolute leg")
	}
}

// (d) Fail open when the source never reported a Fable row. Pre-July-2026
// history, older userscripts, and accounts whose page omits the sub-row must
// behave exactly as they did before the gate existed.
func TestFableHeadroomGate_FailsOpenWhenNoFableReading(t *testing.T) {
	t.Run("snapshot with NULL fable", func(t *testing.T) {
		c, s := fableCalc(t)
		defer s.Close()

		now := weeklyWindowHalfElapsed(t, c, s)
		insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), nil)

		resp, err := c.GetSlack()
		if err != nil {
			t.Fatalf("GetSlack: %v", err)
		}
		if resp.FableWeekly != nil {
			t.Errorf("expected nil fable_weekly, got %+v", resp.FableWeekly)
		}
		if !resp.Gates["fable_headroom"] {
			t.Error("expected fable_headroom to pass when there is no reading")
		}
	})

	t.Run("no snapshots at all", func(t *testing.T) {
		c, s := fableCalc(t)
		defer s.Close()

		weeklyWindowHalfElapsed(t, c, s)

		resp, err := c.GetSlack()
		if err != nil {
			t.Fatalf("GetSlack: %v", err)
		}
		if resp.FableWeekly != nil {
			t.Errorf("expected nil fable_weekly, got %+v", resp.FableWeekly)
		}
		if !resp.Gates["fable_headroom"] {
			t.Error("expected fable_headroom to pass when there is no reading")
		}
	})

	t.Run("no weekly window", func(t *testing.T) {
		c, s := fableCalc(t)
		defer s.Close()

		resp, err := c.GetSlack()
		if err != nil {
			t.Fatalf("GetSlack: %v", err)
		}
		if resp.FableWeekly != nil {
			t.Errorf("expected nil fable_weekly, got %+v", resp.FableWeekly)
		}
		if !resp.Gates["fable_headroom"] {
			t.Error("expected fable_headroom to pass when there is no weekly window")
		}
	})
}

// (e) A Fable reading from before the current weekly window must not leak in.
// The sub-row resets with the weekly window, so a stale pre-window value would
// otherwise pin the gate shut for the whole of the new week.
func TestFableHeadroomGate_IgnoresPreWindowReadings(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	// Last week's exhausted Fable row, observed before the window opened.
	insertFableSnapshot(t, s, now.Add(-100*time.Hour), fptr(99.0), fptr(99.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.FableWeekly != nil {
		t.Errorf("expected pre-window fable reading to be ignored, got %+v", resp.FableWeekly)
	}
	if !resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom to pass; the stale reading leaked in")
	}
}

// (f) The most recent in-window reading wins over earlier ones.
func TestFableHeadroomGate_UsesLatestInWindowReading(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	insertFableSnapshot(t, s, now.Add(-3*time.Hour), fptr(8.0), fptr(20.0))
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), fptr(90.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.FableWeekly == nil || resp.FableWeekly.PercentUsed == nil {
		t.Fatal("expected a fable reading")
	}
	if *resp.FableWeekly.PercentUsed != 90.0 {
		t.Errorf("expected latest reading 90, got %v", *resp.FableWeekly.PercentUsed)
	}
	if resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom to fail on the latest reading")
	}
}

// (f2) The sub-row disappearing mid-window must open the gate, not freeze it.
//
// The userscript reads all three bars in one DOM scan and omits what it did not
// find, so a row that carries a weekly reading but no fable is a positive
// statement that the sub-row was gone at that instant — an account whose plan
// stopped showing it, or a label rename the matcher no longer recognizes. The
// last real reading must not keep gating for the rest of the week.
func TestFableHeadroomGate_FailsOpenWhenFableRowDisappears(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	// A real, blocking reading — then the row goes away and stays away.
	insertFableSnapshot(t, s, now.Add(-3*time.Hour), fptr(9.0), fptr(95.0))
	insertFableSnapshot(t, s, now.Add(-2*time.Hour), fptr(10.0), nil)
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), nil)

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.FableWeekly != nil {
		t.Errorf("expected nil fable_weekly once the row disappeared, got %+v", resp.FableWeekly)
	}
	if !resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom to pass; the superseded reading is still gating")
	}
}

// (f3) A snapshot that never parsed the weekly section says nothing about
// whether the sub-row exists, so it must not supersede a live reading. Only a
// row that read the weekly section can report the sub-row absent.
func TestFableHeadroomGate_SessionOnlySnapshotDoesNotAnswerForFable(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	insertFableSnapshot(t, s, now.Add(-3*time.Hour), fptr(10.0), fptr(95.0))
	// Weekly section unparsed: session bar only, both weekly readings absent.
	insertSnapshot(t, s, now.Add(-time.Minute), fptr(4.0), nil, nil)

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.FableWeekly == nil || resp.FableWeekly.PercentUsed == nil {
		t.Fatal("expected the fable reading to survive a session-only snapshot")
	}
	if *resp.FableWeekly.PercentUsed != 95.0 {
		t.Errorf("expected fable percent_used=95, got %v", *resp.FableWeekly.PercentUsed)
	}
	if resp.Gates["fable_headroom"] {
		t.Error("expected fable_headroom to still fail; a session-only row must not open the gate")
	}
}

// (g) The fable row rides the weekly window: it borrows the weekly bounds and
// pace rather than inventing its own.
func TestFableWeekly_SharesWeeklyWindowBounds(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), fptr(12.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.Weekly == nil || resp.FableWeekly == nil {
		t.Fatal("expected both weekly and fable_weekly metrics")
	}
	if !resp.FableWeekly.WindowStart.Equal(resp.Weekly.WindowStart) {
		t.Errorf("window_start mismatch: %v vs %v", resp.FableWeekly.WindowStart, resp.Weekly.WindowStart)
	}
	if !resp.FableWeekly.WindowEnd.Equal(resp.Weekly.WindowEnd) {
		t.Errorf("window_end mismatch: %v vs %v", resp.FableWeekly.WindowEnd, resp.Weekly.WindowEnd)
	}
	if resp.FableWeekly.PercentExpected != resp.Weekly.PercentExpected {
		t.Errorf("percent_expected mismatch: %v vs %v", resp.FableWeekly.PercentExpected, resp.Weekly.PercentExpected)
	}
}

// (h) slack_combined_fraction stays min(session, weekly). Fable gates the
// release decision but is deliberately kept out of the published combined
// fraction, which existing consumers read as a two-window signal.
func TestFableDoesNotEnterCombinedFraction(t *testing.T) {
	c, s := fableCalc(t)
	defer s.Close()

	now := weeklyWindowHalfElapsed(t, c, s)
	sessionStart := now.Add(-1 * time.Hour)
	insertWindow(t, s.DB(), "session", sessionStart, sessionStart.Add(5*time.Hour), 5.0, "snapshot")
	insertFableSnapshot(t, s, now.Add(-time.Minute), fptr(10.0), fptr(95.0))

	resp, err := c.GetSlack()
	if err != nil {
		t.Fatalf("GetSlack: %v", err)
	}
	if resp.SlackCombinedFraction == nil {
		t.Fatal("expected a combined fraction")
	}
	want := min(*resp.Session.SlackFraction, *resp.Weekly.SlackFraction)
	if *resp.SlackCombinedFraction != want {
		t.Errorf("combined fraction = %v, want min(session, weekly) = %v", *resp.SlackCombinedFraction, want)
	}
}
