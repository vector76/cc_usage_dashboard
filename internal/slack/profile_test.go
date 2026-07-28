package slack

import (
	"math"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// almostEq compares floats with a tolerance appropriate for percent math.
func almostEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// (a) ThresholdAt interpolates linearly between points and clamps flat
// beyond the endpoints. Exercised against the synthesized default session
// profile shape: flat at 98 until 52% elapsed, then descending to 50.
func TestProfileThresholdAt(t *testing.T) {
	p := Profile{{0, 98}, {52, 98}, {100, 50}}

	tests := []struct {
		name    string
		timePct float64
		want    float64
	}{
		{"at first point", 0, 98},
		{"on flat segment", 10, 98},
		{"at knee", 52, 98},
		{"midway down the slope", 76, 74},
		{"at last point", 100, 50},
		{"before first point clamps", -5, 98},
		{"past last point clamps", 120, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.ThresholdAt(tt.timePct)
			if !almostEq(got, tt.want) {
				t.Errorf("ThresholdAt(%v) = %v, want %v", tt.timePct, got, tt.want)
			}
		})
	}
}

// (b) A single-point profile is a flat threshold everywhere.
func TestProfileThresholdAtSinglePoint(t *testing.T) {
	p := Profile{{40, 60}}
	for _, x := range []float64{0, 40, 100} {
		if got := p.ThresholdAt(x); !almostEq(got, 60) {
			t.Errorf("ThresholdAt(%v) = %v, want 60", x, got)
		}
	}
}

// (c) ProfileFromPairs validates shape and ranges: pairs of two values,
// times strictly increasing, everything within [0, 100].
func TestProfileFromPairs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		p, err := ProfileFromPairs([][]float64{{0, 98}, {52, 98}, {100, 50}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := Profile{{0, 98}, {52, 98}, {100, 50}}
		if len(p) != len(want) {
			t.Fatalf("got %d points, want %d", len(p), len(want))
		}
		for i := range want {
			if !almostEq(p[i].TimePct, want[i].TimePct) || !almostEq(p[i].RemainingPct, want[i].RemainingPct) {
				t.Errorf("point %d: got %+v, want %+v", i, p[i], want[i])
			}
		}
	})

	invalid := []struct {
		name  string
		pairs [][]float64
	}{
		{"empty list", [][]float64{}},
		{"wrong arity", [][]float64{{0, 98, 5}}},
		{"single value", [][]float64{{50}}},
		{"time not increasing", [][]float64{{0, 98}, {0, 90}}},
		{"time decreasing", [][]float64{{50, 98}, {20, 90}}},
		{"time below range", [][]float64{{-1, 98}}},
		{"time above range", [][]float64{{101, 98}}},
		{"remaining below range", [][]float64{{0, -0.5}}},
		{"remaining above range", [][]float64{{0, 100.5}}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ProfileFromPairs(tt.pairs); err == nil {
				t.Errorf("ProfileFromPairs(%v): expected error, got nil", tt.pairs)
			}
		})
	}

	t.Run("nil input yields nil profile without error", func(t *testing.T) {
		p, err := ProfileFromPairs(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p != nil {
			t.Errorf("expected nil profile, got %v", p)
		}
	})
}

// (d) SynthesizeProfile reproduces the legacy two-leg gate exactly:
// pass iff remaining >= min(100*absolute, 100*(1+surplus) - elapsedPct).
func TestSynthesizeProfile(t *testing.T) {
	tests := []struct {
		name              string
		surplus, absolute float64
		want              Profile
	}{
		{"session defaults", 0.50, 0.98, Profile{{0, 98}, {52, 98}, {100, 50}}},
		{"weekly defaults", 0.10, 0.80, Profile{{0, 80}, {30, 80}, {100, 10}}},
		{"absolute disabled entirely", 0, 1.0, Profile{{0, 100}, {100, 0}}},
		{"absolute zero collapses to always-pass", 0.50, 0, Profile{{0, 0}, {100, 0}}},
		{"absolute below surplus stays flat", 0.50, 0.30, Profile{{0, 30}, {100, 30}}},
		{"surplus with disabled absolute clamps at 100", 0.50, 1.0, Profile{{0, 100}, {50, 100}, {100, 50}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SynthesizeProfile(tt.surplus, tt.absolute)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d points %v, want %d points %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range tt.want {
				if !almostEq(got[i].TimePct, tt.want[i].TimePct) || !almostEq(got[i].RemainingPct, tt.want[i].RemainingPct) {
					t.Errorf("point %d: got %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// (e) A configured profile replaces the legacy gate: the same window state
// flips the headroom gate depending on the profile in force. Window is half
// elapsed with 30% used (70% remaining): the default session profile
// (threshold 98 at 50% elapsed) fails, while a flat-60 custom profile passes.
func TestGetSlack_CustomProfileDrivesHeadroomGate(t *testing.T) {
	run := func(t *testing.T, sessionProfile Profile, wantGate bool) {
		t.Helper()
		s, err := store.Open(":memory:")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer s.Close()
		c := NewCalculator(s.DB(), Config{
			BaselineMaxAgeSeconds:    480,
			SessionSurplusThreshold:  0.50,
			SessionAbsoluteThreshold: 0.98,
			SessionProfile:           sessionProfile,
		})

		now := time.Now().UTC()
		insertWindow(t, s.DB(), "session", now.Add(-150*time.Minute), now.Add(150*time.Minute), 30.0, "snapshot:1")

		resp, err := c.GetSlack()
		if err != nil {
			t.Fatalf("GetSlack: %v", err)
		}
		if got := resp.Gates["session_headroom"]; got != wantGate {
			t.Errorf("session_headroom = %v, want %v", got, wantGate)
		}
	}

	t.Run("default synthesized profile fails at 70% remaining", func(t *testing.T) {
		run(t, nil, false)
	})
	t.Run("flat-60 custom profile passes at 70% remaining", func(t *testing.T) {
		run(t, Profile{{0, 60}, {100, 60}}, true)
	})
	t.Run("flat-80 custom profile fails at 70% remaining", func(t *testing.T) {
		run(t, Profile{{0, 80}, {100, 80}}, false)
	})
}

// (f) Property check: across a grid of elapsed/used values, the synthesized
// profile's pass/fail decision matches the legacy disjunction it replaces.
func TestSynthesizeProfileMatchesLegacyGate(t *testing.T) {
	configs := []struct{ surplus, absolute float64 }{
		{0.50, 0.98},
		{0.10, 0.80},
		{0.50, 0},
		{0, 1.0},
		{0.25, 0.60},
	}
	for _, c := range configs {
		p := SynthesizeProfile(c.surplus, c.absolute)
		for elapsed := 0.0; elapsed <= 100.0; elapsed += 2.5 {
			for used := 0.0; used <= 100.0; used += 2.5 {
				legacyPace := (elapsed-used)/100 >= c.surplus
				legacyAbsolute := used <= (1-c.absolute)*100
				legacy := legacyPace || legacyAbsolute

				threshold := p.ThresholdAt(elapsed)
				// Grid points landing exactly on the gate boundary are
				// decided by float rounding in both formulations; the
				// interesting property is agreement away from the edge.
				if math.Abs((100-used)-threshold) < 1e-6 {
					continue
				}
				got := (100 - used) >= threshold
				if got != legacy {
					t.Fatalf("surplus=%v absolute=%v elapsed=%v used=%v: profile=%v legacy=%v (threshold=%v)",
						c.surplus, c.absolute, elapsed, used, got, legacy, p.ThresholdAt(elapsed))
				}
			}
		}
	}
}
