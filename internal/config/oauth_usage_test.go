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

// TestOAuthRefreshOffByDefault keeps running Claude Code a deliberate
// choice: it is a second program acting on the user's account, not a side
// effect an upgrade should switch on.
func TestOAuthRefreshOffByDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.OAuthUsage.RefreshWithClaude {
		t.Error("expected oauth_usage.refresh_with_claude false by default")
	}
	if cfg.OAuthUsage.ClaudePath != "" {
		t.Errorf("expected empty claude_path by default, got %q", cfg.OAuthUsage.ClaudePath)
	}
}

func TestLoadOAuthRefreshSettings(t *testing.T) {
	path := writeConfig(t, `
oauth_usage:
  enabled: true
  refresh_with_claude: true
  claude_path: "%USERPROFILE%/.local/bin/claude.exe"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !cfg.OAuthUsage.RefreshWithClaude {
		t.Error("expected oauth_usage.refresh_with_claude true")
	}
	if strings.Contains(cfg.OAuthUsage.ClaudePath, "%USERPROFILE%") {
		t.Errorf("expected claude_path placeholders expanded, got %q", cfg.OAuthUsage.ClaudePath)
	}
}

// TestOAuthRefreshCommand pins the rule that the refresh only takes effect
// alongside polling: with polling off there is no stale credential to act
// on, so refresh_with_claude alone must run nothing.
func TestOAuthRefreshCommand(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		refresh bool
		path    string
		want    string
	}{
		{name: "both off", want: ""},
		{name: "refresh without polling", refresh: true, want: ""},
		{name: "refresh with explicit path but no polling", refresh: true, path: `C:\bin\claude.exe`, want: ""},
		{name: "polling without refresh", enabled: true, want: ""},
		{name: "both on, default path", enabled: true, refresh: true, want: "claude"},
		{name: "both on, explicit path", enabled: true, refresh: true, path: `C:\bin\claude.exe`, want: `C:\bin\claude.exe`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			cfg.OAuthUsage.Enabled = tc.enabled
			cfg.OAuthUsage.RefreshWithClaude = tc.refresh
			cfg.OAuthUsage.ClaudePath = tc.path
			if got := cfg.OAuthRefreshCommand(); got != tc.want {
				t.Errorf("OAuthRefreshCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}
