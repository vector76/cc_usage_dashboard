package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Database.Path != "usage.db" {
		t.Errorf("expected database path 'usage.db', got %s", cfg.Database.Path)
	}
	if cfg.HTTP.Port != 27812 {
		t.Errorf("expected port 27812, got %d", cfg.HTTP.Port)
	}
	if len(cfg.HTTP.Bind) != 1 || cfg.HTTP.Bind[0] != "127.0.0.1" {
		t.Errorf("expected default bind [127.0.0.1], got %v", cfg.HTTP.Bind)
	}
	if cfg.HTTP.EnableFallback {
		t.Error("expected enable_fallback false by default")
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("expected logging level 'info', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.File != "" {
		t.Errorf("expected logging file empty by default, got %q", cfg.Logging.File)
	}
	if cfg.Slack.SessionSurplusThreshold != 0.50 {
		t.Errorf("expected session_surplus_threshold 0.50, got %f", cfg.Slack.SessionSurplusThreshold)
	}
	if cfg.Slack.WeeklySurplusThreshold != 0.10 {
		t.Errorf("expected weekly_surplus_threshold 0.10, got %f", cfg.Slack.WeeklySurplusThreshold)
	}
	if cfg.Slack.WeeklyAbsoluteThreshold != 0.80 {
		t.Errorf("expected weekly_absolute_threshold 0.80, got %f", cfg.Slack.WeeklyAbsoluteThreshold)
	}
	if cfg.Slack.BaselineMaxAgeSeconds != 480 {
		t.Errorf("expected baseline_max_age_seconds 480, got %d", cfg.Slack.BaselineMaxAgeSeconds)
	}
	if cfg.Tailer.PollIntervalMs != 1000 {
		t.Errorf("expected poll_interval_ms 1000, got %d", cfg.Tailer.PollIntervalMs)
	}
	if cfg.Retention.ParseErrorsDays != 30 {
		t.Errorf("expected parse_errors retention 30 days, got %d", cfg.Retention.ParseErrorsDays)
	}
	if cfg.EnableSlackSampling {
		t.Error("expected slack sampling disabled by default")
	}
}

func TestLoadFromFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	content := `
database:
  path: "/tmp/custom.db"
http:
  port: 8080
  bind:
    - 127.0.0.1
    - 172.17.0.1
  enable_fallback: true
tailer:
  poll_interval_ms: 500
logging:
  level: debug
  file: "/tmp/trayapp.log"
slack:
  session_surplus_threshold: 0.75
  weekly_surplus_threshold: 0.05
  weekly_absolute_threshold: 0.90
  baseline_max_age_seconds: 240
`

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Database.Path != "/tmp/custom.db" {
		t.Errorf("expected database path '/tmp/custom.db', got %s", cfg.Database.Path)
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.HTTP.Port)
	}
	if len(cfg.HTTP.Bind) != 2 || cfg.HTTP.Bind[1] != "172.17.0.1" {
		t.Errorf("expected bind [127.0.0.1, 172.17.0.1], got %v", cfg.HTTP.Bind)
	}
	if !cfg.HTTP.EnableFallback {
		t.Error("expected enable_fallback true")
	}
	if cfg.Tailer.PollIntervalMs != 500 {
		t.Errorf("expected poll_interval_ms 500, got %d", cfg.Tailer.PollIntervalMs)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("expected logging level 'debug', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.File != "/tmp/trayapp.log" {
		t.Errorf("expected logging file '/tmp/trayapp.log', got %q", cfg.Logging.File)
	}
	if cfg.Slack.SessionSurplusThreshold != 0.75 {
		t.Errorf("expected session_surplus_threshold 0.75, got %f", cfg.Slack.SessionSurplusThreshold)
	}
	if cfg.Slack.WeeklySurplusThreshold != 0.05 {
		t.Errorf("expected weekly_surplus_threshold 0.05, got %f", cfg.Slack.WeeklySurplusThreshold)
	}
	if cfg.Slack.WeeklyAbsoluteThreshold != 0.90 {
		t.Errorf("expected weekly_absolute_threshold 0.90, got %f", cfg.Slack.WeeklyAbsoluteThreshold)
	}
	if cfg.Slack.BaselineMaxAgeSeconds != 240 {
		t.Errorf("expected baseline_max_age_seconds 240, got %d", cfg.Slack.BaselineMaxAgeSeconds)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/to/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("invalid: yaml: content: [unterminated"); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	tmpFile.Close()

	_, err = Load(tmpFile.Name())
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), "failed to parse config file") {
		t.Errorf("expected wrapped parse error, got: %v", err)
	}
}

func TestExpandPlaceholdersResolvesEnvVars(t *testing.T) {
	t.Setenv("APPDATA", "/fake/appdata")
	t.Setenv("LOCALAPPDATA", "/fake/localappdata")
	t.Setenv("USERPROFILE", "/fake/userprofile")
	t.Setenv("HOME", "/fake/home")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"appdata", `%APPDATA%\usage_dashboard\config.yaml`, `/fake/appdata\usage_dashboard\config.yaml`},
		{"localappdata", `%LOCALAPPDATA%\usage.db`, `/fake/localappdata\usage.db`},
		{"userprofile", `%USERPROFILE%\.claude\projects`, `/fake/userprofile\.claude\projects`},
		{"home", `%HOME%/.claude/projects`, `/fake/home/.claude/projects`},
		{"multiple in one string", `%APPDATA%/x/%HOME%`, `/fake/appdata/x//fake/home`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandPlaceholders(tc.in); got != tc.want {
				t.Errorf("expandPlaceholders(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandPlaceholdersNoTokenLeavesIntact(t *testing.T) {
	t.Setenv("APPDATA", "/fake/appdata")
	cases := []string{
		"",
		"plain/path/no/placeholders",
		"/absolute/unix/path",
		`C:\windows\style\nothing\to\replace`,
	}
	for _, in := range cases {
		if got := expandPlaceholders(in); got != in {
			t.Errorf("expandPlaceholders(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestExpandPlaceholdersFallsBackToHomeOnLinux(t *testing.T) {
	// Empty Windows-style env vars on Linux should fall back to UserHomeDir.
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("USERPROFILE", "")
	// Force HOME to a known value so UserHomeDir is deterministic.
	t.Setenv("HOME", "/fake/home")

	got := expandPlaceholders(`%APPDATA%\usage.db`)
	want := `/fake/home\usage.db`
	if got != want {
		t.Errorf("expandPlaceholders fallback = %q, want %q", got, want)
	}
}

func TestLoadAppliesPlaceholderResolution(t *testing.T) {
	t.Setenv("APPDATA", "/fake/appdata")
	t.Setenv("LOCALAPPDATA", "/fake/localappdata")
	t.Setenv("USERPROFILE", "/fake/userprofile")

	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	content := `
database:
  path: "%LOCALAPPDATA%/usage.db"
claude:
  projects_dir: "%USERPROFILE%/.claude/projects"
pricing:
  table_path: "%APPDATA%/prices.yaml"
`
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Database.Path != "/fake/localappdata/usage.db" {
		t.Errorf("expected expanded database path, got %q", cfg.Database.Path)
	}
	if cfg.Claude.ProjectsDir != "/fake/userprofile/.claude/projects" {
		t.Errorf("expected expanded projects_dir, got %q", cfg.Claude.ProjectsDir)
	}
	if cfg.Pricing.TablePath != "/fake/appdata/prices.yaml" {
		t.Errorf("expected expanded table_path, got %q", cfg.Pricing.TablePath)
	}
}

func TestExpandHomeShortStringDoesNotPanic(t *testing.T) {
	cases := []string{"", "/", "~", "a"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("expandHome(%q) panicked: %v", in, r)
				}
			}()
			if got := expandHome(in); got != in {
				t.Errorf("expandHome(%q) = %q, want unchanged", in, got)
			}
		})
	}
}
