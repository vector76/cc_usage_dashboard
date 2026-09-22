package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOAuthUsageDisabledByDefault pins the source as opt-in. It polls
// Anthropic's API on a timer, which is a behavior change no existing
// install should acquire just by upgrading; the userscript keeps working
// untouched until the user turns this on deliberately.
func TestOAuthUsageDisabledByDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.OAuthUsage.Enabled {
		t.Error("expected oauth_usage.enabled false by default")
	}
}

// TestOAuthUsagePollIntervalDefault pins the cadence. The endpoint is
// undocumented and its rate limits are unpublished, so the default is
// deliberately unhurried — the quota figures it reports move in whole
// percentage points, far slower than three minutes.
func TestOAuthUsagePollIntervalDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := cfg.OAuthUsage.PollIntervalSeconds; got != 180 {
		t.Errorf("expected default poll interval 180s, got %d", got)
	}
}

// TestOAuthUsageCredentialsPathEmptyByDefault keeps resolution in one
// place: empty means "use the standard location, honoring
// CLAUDE_CONFIG_DIR". A literal default here would shadow that env var.
func TestOAuthUsageCredentialsPathEmptyByDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.OAuthUsage.CredentialsPath != "" {
		t.Errorf("expected empty credentials_path by default, got %q",
			cfg.OAuthUsage.CredentialsPath)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadOAuthUsageSettings(t *testing.T) {
	path := writeConfig(t, `
oauth_usage:
  enabled: true
  poll_interval_seconds: 300
  credentials_path: "%USERPROFILE%/.claude/.credentials.json"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.OAuthUsage.Enabled {
		t.Error("expected oauth_usage.enabled true")
	}
	if cfg.OAuthUsage.PollIntervalSeconds != 300 {
		t.Errorf("expected poll interval 300, got %d", cfg.OAuthUsage.PollIntervalSeconds)
	}
	// Placeholder resolution must apply here as it does to the other path
	// fields, or a config written on Windows fails only at request time.
	if strings.Contains(cfg.OAuthUsage.CredentialsPath, "%USERPROFILE%") {
		t.Errorf("expected credentials_path placeholders expanded, got %q",
			cfg.OAuthUsage.CredentialsPath)
	}
}

// TestLoadRejectsTooFrequentPolling guards the undocumented endpoint
// against a typo. Rejecting at startup with the offending key named beats
// discovering the mistake as a rate-limit ban later.
func TestLoadRejectsTooFrequentPolling(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
	}{
		{name: "below the floor", seconds: 5},
		{name: "zero is not a valid interval", seconds: 0},
		{name: "negative", seconds: -60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, `
oauth_usage:
  enabled: true
  poll_interval_seconds: `+strconv.Itoa(tc.seconds)+`
`)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected Load to reject the interval")
			}
			if !strings.Contains(err.Error(), "oauth_usage.poll_interval_seconds") {
				t.Errorf("error should name the offending key, got: %v", err)
			}
		})
	}
}

// TestLoadIgnoresIntervalWhenDisabled keeps a stale or nonsense value in a
// disabled block from blocking startup entirely. Nothing polls, so nothing
// is at risk.
func TestLoadIgnoresIntervalWhenDisabled(t *testing.T) {
	path := writeConfig(t, `
oauth_usage:
  enabled: false
  poll_interval_seconds: 1
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load should tolerate a bad interval while disabled: %v", err)
	}
}
