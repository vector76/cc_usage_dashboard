package ccusage_test

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	ccusage "github.com/vector76/cc_usage_dashboard"
	"github.com/vector76/cc_usage_dashboard/internal/config"
	"github.com/vector76/cc_usage_dashboard/internal/slack"
)

// The materialized sample must be behavior-neutral: a user who never edits
// it gets exactly the built-in defaults. Its active slack profiles must
// therefore match what the calculator synthesizes from the default scalar
// thresholds, and every other field must load to the same value as no file
// at all. This is the guard that keeps config.sample.yaml honest as
// defaults evolve.
func TestDefaultConfigSampleMatchesBuiltinBehavior(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, ccusage.DefaultConfigYAML, 0644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	fromSample, err := config.Load(path)
	if err != nil {
		t.Fatalf("sample config does not load: %v", err)
	}
	defaults, err := config.Load("")
	if err != nil {
		t.Fatalf("default config: %v", err)
	}

	// The sample's active profiles must equal the synthesized legacy gate.
	sessionProfile, err := slack.ProfileFromPairs(fromSample.Slack.SessionProfile)
	if err != nil {
		t.Fatalf("sample session_profile invalid: %v", err)
	}
	weeklyProfile, err := slack.ProfileFromPairs(fromSample.Slack.WeeklyProfile)
	if err != nil {
		t.Fatalf("sample weekly_profile invalid: %v", err)
	}
	wantSession := slack.SynthesizeProfile(defaults.Slack.SessionSurplusThreshold, defaults.Slack.SessionAbsoluteThreshold)
	wantWeekly := slack.SynthesizeProfile(defaults.Slack.WeeklySurplusThreshold, defaults.Slack.WeeklyAbsoluteThreshold)
	// Synthesis goes through float arithmetic (e.g. 100*(1+0.1-0.8) is
	// 30.000000000000004), so compare with a tolerance instead of DeepEqual.
	profilesMatch := func(a, b slack.Profile) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if math.Abs(a[i].TimePct-b[i].TimePct) > 1e-9 || math.Abs(a[i].RemainingPct-b[i].RemainingPct) > 1e-9 {
				return false
			}
		}
		return true
	}
	if !profilesMatch(sessionProfile, wantSession) {
		t.Errorf("sample session_profile %v != synthesized default %v", sessionProfile, wantSession)
	}
	if !profilesMatch(weeklyProfile, wantWeekly) {
		t.Errorf("sample weekly_profile %v != synthesized default %v", weeklyProfile, wantWeekly)
	}

	// Everything else in the sample is commented out, so after neutralizing
	// the profiles the two configs must be identical.
	fromSample.Slack.SessionProfile = nil
	fromSample.Slack.WeeklyProfile = nil
	if !reflect.DeepEqual(fromSample, defaults) {
		t.Errorf("sample config diverges from built-in defaults:\n sample: %+v\ndefault: %+v", fromSample, defaults)
	}
}
