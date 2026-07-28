// Package slack provides the slack (available capacity) signal for queue scheduling.
package slack

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// SlackResponse is the response from GET /slack endpoint. JSON keys match
// docs/slack-indicator.md.
type SlackResponse struct {
	Now     time.Time      `json:"now"`
	Session *WindowMetrics `json:"session"`
	Weekly  *WindowMetrics `json:"weekly"`
	// FableWeekly is the "Fable" sub-row measured against the same weekly
	// window as Weekly. Nil whenever the sub-row is not currently being
	// reported — see getFableWeeklyUsed.
	FableWeekly *WindowMetrics `json:"fable_weekly"`
	// SlackCombinedFraction stays min(session, weekly) and deliberately
	// excludes fable: consumers read it as the two-window pace signal, and
	// the fable constraint is expressed through the fable_headroom gate.
	SlackCombinedFraction *float64        `json:"slack_combined_fraction"`
	Paused                bool            `json:"paused"`
	ReleaseRecommended    bool            `json:"release_recommended"`
	Gates                 map[string]bool `json:"gates"`
}

// WindowMetrics holds the computed metrics for a single window.
//
// All percent fields are in 0–100 (not 0–1). SlackFraction is in
// [-1, +1] and represents (PercentExpected − PercentUsed) / 100, i.e.
// the fraction of the *full* quota currently held in surplus relative to
// uniform pace. Positive = under pace; negative = over pace.
//
// PercentUsed and SlackFraction are nil whenever no in-window snapshot
// has arrived yet — we don't synthesize an "assumed 0% used" value.
// Consumers (the headroom gates, dashboards) should treat nil as
// "couldn't measure" and fail safe rather than infer.
type WindowMetrics struct {
	WindowStart     time.Time `json:"window_start"`
	WindowEnd       time.Time `json:"window_end"`
	PercentUsed     *float64  `json:"percent_used"`
	PercentExpected float64   `json:"percent_expected"`
	SlackFraction   *float64  `json:"slack_fraction"`
}

// Config holds slack calculation configuration.
//
// The headroom gates are driven entirely by SessionProfile / WeeklyProfile.
// The legacy scalar thresholds remain as synthesis inputs: when a profile is
// nil, NewCalculator derives it from the corresponding surplus + absolute
// pair via SynthesizeProfile, which reproduces the pre-profile gate exactly.
type Config struct {
	BaselineMaxAgeSeconds   int
	SessionSurplusThreshold float64
	WeeklySurplusThreshold  float64
	// SessionAbsoluteThreshold is the percent_remaining floor (0–1) at or
	// above which the session headroom gate also passes, regardless of
	// pace. A value of 1.0 disables the absolute branch.
	SessionAbsoluteThreshold float64
	// WeeklyAbsoluteThreshold is the percent_remaining floor (0–1) at or
	// above which the weekly headroom gate also passes, regardless of
	// pace. Lets the gate fire early in the week.
	WeeklyAbsoluteThreshold float64
	// SessionProfile, when non-nil, is the session headroom gate: the gate
	// passes iff percent_remaining is at or above the profile's threshold
	// at the window's elapsed fraction (see Profile).
	SessionProfile Profile
	// WeeklyProfile is the same for the weekly window, and is shared by the
	// Fable sub-row gate — the two rows are measured against the same
	// window, so a separate pace horizon would be meaningless.
	WeeklyProfile Profile
}

// Calculator computes the slack signal. It is safe for concurrent use; a
// single instance is shared across HTTP handlers so the in-memory pause flag
// persists between requests.
type Calculator struct {
	db     *sql.DB
	config Config

	mu     sync.RWMutex
	paused bool
}

// NewCalculator creates a new slack calculator. Nil profiles are synthesized
// from the legacy scalar thresholds so every caller — including ones predating
// profiles — gets gate behavior identical to the two-leg rule those scalars
// used to drive directly.
func NewCalculator(db *sql.DB, cfg Config) *Calculator {
	if cfg.SessionProfile == nil {
		cfg.SessionProfile = SynthesizeProfile(cfg.SessionSurplusThreshold, cfg.SessionAbsoluteThreshold)
	}
	if cfg.WeeklyProfile == nil {
		cfg.WeeklyProfile = SynthesizeProfile(cfg.WeeklySurplusThreshold, cfg.WeeklyAbsoluteThreshold)
	}
	return &Calculator{
		db:     db,
		config: cfg,
	}
}

// Profiles returns the active session and weekly slack-activation profiles
// (post-synthesis, so never nil). The dashboard renders these as the green
// slack zone so the chart can never disagree with the gate.
func (c *Calculator) Profiles() (session, weekly Profile) {
	return c.config.SessionProfile, c.config.WeeklyProfile
}

// SetPaused sets the pause state.
func (c *Calculator) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	c.mu.Unlock()
}

// IsPaused reports the current pause state.
func (c *Calculator) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// GetSlack computes the current slack signal.
func (c *Calculator) GetSlack() (*SlackResponse, error) {
	now := time.Now().UTC()

	c.mu.RLock()
	paused := c.paused
	c.mu.RUnlock()

	sessionWindow, err := c.getActiveWindow("session")
	if err != nil {
		return nil, fmt.Errorf("failed to get 5-hour window: %w", err)
	}

	weeklyWindow, err := c.getActiveWindow("weekly")
	if err != nil {
		return nil, fmt.Errorf("failed to get weekly window: %w", err)
	}

	resp := &SlackResponse{
		Now:    now,
		Paused: paused,
		Gates:  make(map[string]bool),
	}

	if sessionWindow != nil {
		metrics, err := c.computeMetrics(sessionWindow, now)
		if err != nil {
			return nil, fmt.Errorf("failed to compute session metrics: %w", err)
		}
		resp.Session = metrics
	}

	if weeklyWindow != nil {
		metrics, err := c.computeMetrics(weeklyWindow, now)
		if err != nil {
			return nil, fmt.Errorf("failed to compute weekly metrics: %w", err)
		}
		resp.Weekly = metrics

		// The Fable sub-row is not a window kind of its own (see
		// docs/data-model.md): it rides the weekly window and borrows its
		// bounds and pace, differing only in percent_used.
		fableUsed, err := c.getFableWeeklyUsed(weeklyWindow)
		if err != nil {
			return nil, fmt.Errorf("failed to get fable weekly usage: %w", err)
		}
		if fableUsed != nil {
			fableWindow := *weeklyWindow
			fableWindow.baselineTotal = fableUsed
			fableMetrics, err := c.computeMetrics(&fableWindow, now)
			if err != nil {
				return nil, fmt.Errorf("failed to compute fable metrics: %w", err)
			}
			resp.FableWeekly = fableMetrics
		}
	}

	resp.SlackCombinedFraction = c.combineSlackFractions(resp.Session, resp.Weekly)

	// Each headroom gate compares percent-remaining against its profile's
	// threshold at the window's elapsed fraction. A nil session window is
	// the deadlock-breaker: when no active session row exists, getActiveWindow
	// returns nil and the gate passes so slack can fire during inactive limbo.
	sessionHeadroomOk := resp.Session == nil ||
		profilePasses(c.config.SessionProfile, resp.Session)
	// A nil weekly window is the symmetric deadlock-breaker (mirrors the
	// session path): when the windows engine refuses to mint a phantom
	// weekly row under limbo (see docs/no-active-session.md), there is
	// no row to gate against and the queue would otherwise deadlock with
	// the most quota free.
	weeklyHeadroomOk := resp.Weekly == nil ||
		profilePasses(c.config.WeeklyProfile, resp.Weekly)

	// The Fable sub-row gets the same profile as the weekly aggregate,
	// because it is measured against the same window. It exists because
	// "All models" can sit deep in the green while the Fable sub-quota is
	// nearly exhausted; releasing free work then would spend exactly the
	// capacity the user is about to need.
	//
	// Fails OPEN whenever the sub-row is not being reported (nil FableWeekly):
	// pre-July-2026 history, userscripts predating the extractor change,
	// accounts whose page does not render the sub-row, and the day the page
	// stops rendering it altogether must all behave as they did before this
	// gate existed. There is no backfill for fable_weekly_used, so "absent"
	// genuinely means "not observed" rather than "zero" — and a reading that
	// a later observation has superseded is not a reading (see
	// getFableWeeklyUsed).
	fableHeadroomOk := resp.FableWeekly == nil ||
		profilePasses(c.config.WeeklyProfile, resp.FableWeekly)

	freshOk, err := c.baselineFreshnessOk(now)
	if err != nil {
		return nil, err
	}

	resp.Gates["session_headroom"] = sessionHeadroomOk
	resp.Gates["weekly_headroom"] = weeklyHeadroomOk
	resp.Gates["fable_headroom"] = fableHeadroomOk
	resp.Gates["baseline_freshness"] = freshOk
	resp.Gates["not_paused"] = !paused

	resp.ReleaseRecommended = sessionHeadroomOk && weeklyHeadroomOk && fableHeadroomOk && freshOk && !paused

	return resp, nil
}

// profilePasses reports whether a window's observed percent-remaining sits at
// or above the profile's threshold at the window's elapsed fraction. A nil
// PercentUsed fails: no measurement = don't release (the fail-open cases are
// the callers' nil-metrics disjuncts, decided before this is consulted).
// PercentExpected doubles as the elapsed fraction in percent — computeMetrics
// clamps it to [0, 100], and it is 0 before the window starts, which matches
// the profile's flat extension to the left of its first point.
func profilePasses(p Profile, m *WindowMetrics) bool {
	if m.PercentUsed == nil {
		return false
	}
	return (100 - *m.PercentUsed) >= p.ThresholdAt(m.PercentExpected)
}

// activeWindow holds the fields we need from the windows table.
type activeWindow struct {
	startedAt     time.Time
	endsAt        time.Time
	baselineTotal *float64
}

// getActiveWindow fetches the active window of a given kind.
func (c *Calculator) getActiveWindow(kind string) (*activeWindow, error) {
	var w activeWindow
	var baselineTotal sql.NullFloat64

	err := c.db.QueryRow(`
		SELECT started_at, ends_at, baseline_percent_used
		FROM windows
		WHERE kind = ? AND closed = 0
		ORDER BY started_at DESC
		LIMIT 1
	`, kind).Scan(&w.startedAt, &w.endsAt, &baselineTotal)

	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to query window: %w", err)
	}

	if baselineTotal.Valid {
		v := baselineTotal.Float64
		w.baselineTotal = &v
	}
	return &w, nil
}

// getFableWeeklyUsed returns the current reading of the "Fable" weekly sub-row,
// or nil when the sub-row is not being reported — which is the ordinary case for
// accounts whose plan does not render it, for history predating July 2026, and
// for whatever comes after Anthropic stops rendering it.
//
// Unlike the aggregate weekly percentage there is no windows column to read:
// the sub-row is not modelled as its own window kind, so we go to
// quota_snapshots directly. Two filters carry the semantics:
//
// `weekly_used IS NOT NULL` picks the newest row that actually parsed the weekly
// section. The userscript takes all its bars from a single DOM scan and omits
// what it did not find (see docs/userscript.md), so such a row reporting no
// fable value is a positive statement that the sub-row was absent at that
// instant — and it supersedes any earlier reading. Rows that never parsed the
// weekly section say nothing either way and must not answer the question;
// letting a session-only row through would open the gate on no evidence.
//
// The window bounds are the second filter. The sub-row resets with the weekly
// window, so a reading from before started_at describes last week's quota and
// must not pin the gate shut for the whole of the new week.
//
// A sub-row that is present but transiently unparsed (weekly section rendered
// before the sub-row hydrates) therefore reads as absent for one observation.
// That fails open, which is the intended direction, and self-corrects on the
// next post rather than persisting.
func (c *Calculator) getFableWeeklyUsed(w *activeWindow) (*float64, error) {
	var used sql.NullFloat64
	err := c.db.QueryRow(`
		SELECT fable_weekly_used
		FROM quota_snapshots
		WHERE weekly_used IS NOT NULL
		  AND observed_at >= ? AND observed_at <= ?
		ORDER BY observed_at DESC
		LIMIT 1
	`, store.FormatTime(w.startedAt), store.FormatTime(w.endsAt)).Scan(&used)

	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to query fable snapshot: %w", err)
	}
	// The newest weekly-bearing row reported no sub-row: absent, not zero.
	if !used.Valid {
		return nil, nil
	}
	v := used.Float64
	return &v, nil
}

// computeMetrics computes window metrics for an active window using
// percent-of-quota math only. PercentUsed comes from the latest in-window
// snapshot (windows.baseline_percent_used, which the windows engine keeps current);
// dollar consumption from usage_events does not enter the slack signal.
//
//	progress(t)        = clamp((t - t0) / (t1 - t0), 0, 1)
//	percent_expected   = 100 * progress(t)              # uniform pace to 100% by t1
//	slack_fraction     = (percent_expected - percent_used) / 100   # in [-1, +1]
//
// percent_used and slack_fraction are nil when no in-window snapshot has
// been recorded yet — we fail safe rather than assume 0% used.
func (c *Calculator) computeMetrics(w *activeWindow, now time.Time) (*WindowMetrics, error) {
	m := &WindowMetrics{
		WindowStart: w.startedAt,
		WindowEnd:   w.endsAt,
	}
	if w.baselineTotal != nil {
		v := *w.baselineTotal
		m.PercentUsed = &v
	}

	// Window has not started yet.
	if now.Before(w.startedAt) {
		return m, nil
	}

	windowDuration := w.endsAt.Sub(w.startedAt)
	if windowDuration <= 0 {
		return m, nil
	}
	elapsed := now.Sub(w.startedAt)
	progress := float64(elapsed) / float64(windowDuration)
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	m.PercentExpected = 100 * progress

	if m.PercentUsed != nil {
		frac := (m.PercentExpected - *m.PercentUsed) / 100
		m.SlackFraction = &frac
	}

	return m, nil
}

// combineSlackFractions returns min(a, b) of the two windows' slack fractions.
// Per docs: combined is null whenever either window's slack_fraction is null.
func (c *Calculator) combineSlackFractions(session, weekly *WindowMetrics) *float64 {
	if session == nil || session.SlackFraction == nil {
		return nil
	}
	if weekly == nil || weekly.SlackFraction == nil {
		return nil
	}
	combined := min(*session.SlackFraction, *weekly.SlackFraction)
	return &combined
}

// baselineFreshnessOk implements the freshness gate from
// docs/slack-indicator.md: the gate passes iff a snapshot exists and is no
// older than BaselineMaxAgeSeconds. Missing snapshot fails the gate.
//
// This gate is the sole defence against a stale snapshot: if the userscript
// stops posting (page closed, tampermonkey down), release_recommended must
// flip to false within BaselineMaxAgeSeconds — otherwise queued work would
// keep draining quota against a frozen percent_used.
func (c *Calculator) baselineFreshnessOk(now time.Time) (bool, error) {
	var receivedAt time.Time
	err := c.db.QueryRow(`
		SELECT received_at FROM quota_snapshots
		ORDER BY received_at DESC LIMIT 1
	`).Scan(&receivedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to query snapshot: %w", err)
	}

	age := now.Sub(receivedAt)
	maxAge := time.Duration(c.config.BaselineMaxAgeSeconds) * time.Second
	return age <= maxAge, nil
}

// RecordRelease records a release event to the database, resolving it to the
// active window of the requested kind containing releasedAt.
func (c *Calculator) RecordRelease(releasedAt time.Time, jobTag string, estimatedCost *float64, slackAtRelease *float64, windowKind string) (int64, error) {
	if windowKind == "" {
		windowKind = "session"
	}

	var windowID int64
	err := c.db.QueryRow(`
		SELECT id FROM windows
		WHERE kind = ? AND started_at <= ? AND ends_at > ?
		ORDER BY started_at DESC
		LIMIT 1
	`, windowKind, store.FormatTime(releasedAt), store.FormatTime(releasedAt)).Scan(&windowID)

	if err == sql.ErrNoRows {
		return 0, ErrNoActiveWindow
	} else if err != nil {
		return 0, fmt.Errorf("failed to find window: %w", err)
	}

	result, err := c.db.Exec(`
		INSERT INTO slack_releases (released_at, received_at, job_tag, estimated_cost, slack_at_release, window_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, store.FormatTime(releasedAt), store.FormatTime(time.Now()), jobTag, estimatedCost, slackAtRelease, windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to insert release: %w", err)
	}

	return result.LastInsertId()
}

// ErrNoActiveWindow is returned by RecordRelease when no window of the
// requested kind contains the releasedAt timestamp.
var ErrNoActiveWindow = fmt.Errorf("no active window")

// RecordSample records a slack sample if sampling is enabled.
func (c *Calculator) RecordSample(fraction *float64) (int64, error) {
	var windowID int64
	err := c.db.QueryRow(`
		SELECT id FROM windows
		WHERE kind = 'session' AND closed = 0
		LIMIT 1
	`).Scan(&windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to find active window: %w", err)
	}

	result, err := c.db.Exec(`
		INSERT INTO slack_samples (sampled_at, slack_fraction, window_id)
		VALUES (?, ?, ?)
	`, store.FormatTime(time.Now()), fraction, windowID)

	if err != nil {
		return 0, fmt.Errorf("failed to insert sample: %w", err)
	}

	return result.LastInsertId()
}
