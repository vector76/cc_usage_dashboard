package slack

import "fmt"

// ProfilePoint is one vertex of a slack-activation profile. Both axes are
// percentages in the dashboard burn-down chart's orientation: TimePct is the
// fraction of the window elapsed (0 = window start, 100 = window end) and
// RemainingPct is the percent of quota still unused (100 = untouched quota).
type ProfilePoint struct {
	TimePct      float64
	RemainingPct float64
}

// Profile is a piecewise-linear slack-activation threshold: at a given
// fraction of the window elapsed, slack is available iff the observed
// percent-remaining is at or above the interpolated threshold. Points are
// ordered by strictly increasing TimePct; outside the covered time range the
// nearest endpoint's threshold extends flat.
type Profile []ProfilePoint

// ThresholdAt returns the percent-remaining threshold at timePct by linear
// interpolation between the surrounding points, clamping flat beyond the
// first and last points. Must not be called on an empty profile (an empty
// profile is a configuration error caught by ProfileFromPairs).
func (p Profile) ThresholdAt(timePct float64) float64 {
	if timePct <= p[0].TimePct {
		return p[0].RemainingPct
	}
	last := p[len(p)-1]
	if timePct >= last.TimePct {
		return last.RemainingPct
	}
	for i := 1; i < len(p); i++ {
		if timePct <= p[i].TimePct {
			a, b := p[i-1], p[i]
			frac := (timePct - a.TimePct) / (b.TimePct - a.TimePct)
			return a.RemainingPct + frac*(b.RemainingPct-a.RemainingPct)
		}
	}
	return last.RemainingPct
}

// ProfileFromPairs converts config-file [time_pct, remaining_pct] pairs into
// a Profile, validating shape and ranges. A nil/absent input returns a nil
// Profile with no error — "not configured" is the caller's signal to fall
// back to SynthesizeProfile. An explicitly present but empty list is an
// error: it would leave the gate with no threshold at all.
func ProfileFromPairs(pairs [][]float64) (Profile, error) {
	if pairs == nil {
		return nil, nil
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("profile must contain at least one [time_pct, remaining_pct] point")
	}
	p := make(Profile, 0, len(pairs))
	for i, pair := range pairs {
		if len(pair) != 2 {
			return nil, fmt.Errorf("profile point %d: expected [time_pct, remaining_pct], got %d values", i, len(pair))
		}
		tp, rp := pair[0], pair[1]
		if tp < 0 || tp > 100 {
			return nil, fmt.Errorf("profile point %d: time_pct %v out of range [0, 100]", i, tp)
		}
		if rp < 0 || rp > 100 {
			return nil, fmt.Errorf("profile point %d: remaining_pct %v out of range [0, 100]", i, rp)
		}
		if i > 0 && tp <= p[i-1].TimePct {
			return nil, fmt.Errorf("profile point %d: time_pct %v must be greater than previous point's %v", i, tp, p[i-1].TimePct)
		}
		p = append(p, ProfilePoint{TimePct: tp, RemainingPct: rp})
	}
	return p, nil
}

// SynthesizeProfile expresses the legacy two-leg headroom gate as a Profile.
// The legacy gate passed iff
//
//	slack_fraction >= surplus            (pace leg)
//	OR percent_used <= 100*(1-absolute)  (absolute-floor leg)
//
// which in remaining-vs-time coordinates is
//
//	remaining >= min(100*absolute, 100*(1+surplus) - elapsed_pct)
//
// i.e. a flat floor at 100*absolute until the descending pace boundary dips
// below it, then the pace boundary. The crossing sits at
// elapsed = 100*(1 + surplus - absolute). Threshold values are clamped into
// [0, 100]: remaining can never exceed 100, so a clamped-at-100 segment
// (absolute = 1.0, the "disabled" sentinel) passes only at exactly-untouched
// quota — the same condition percent_used <= 0 expressed by the legacy leg.
func SynthesizeProfile(surplus, absolute float64) Profile {
	clamp := func(v float64) float64 {
		if v < 0 {
			return 0
		}
		if v > 100 {
			return 100
		}
		return v
	}
	floor := clamp(100 * absolute)
	paceAt := func(elapsedPct float64) float64 {
		return clamp(100*(1+surplus) - elapsedPct)
	}
	crossing := 100 * (1 + surplus - absolute)

	// Pace boundary already below the floor at window start: pure pace line.
	if crossing <= 0 {
		return Profile{{0, paceAt(0)}, {100, paceAt(100)}}
	}
	// Pace boundary never dips below the floor inside the window: pure floor.
	if crossing >= 100 {
		return Profile{{0, floor}, {100, floor}}
	}
	return Profile{{0, floor}, {crossing, floor}, {100, paceAt(100)}}
}
