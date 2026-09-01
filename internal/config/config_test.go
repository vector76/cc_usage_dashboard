package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Empty by design — see TestLoadLeavesDatabasePathUnresolved.
	if cfg.Database.Path != "" {
		t.Errorf("expected empty database path, got %s", cfg.Database.Path)
	}
	if cfg.HTTP.Port != 27812 {
		t.Errorf("expected port 27812, got %d", cfg.HTTP.Port)
	}
	if len(cfg.HTTP.Bind) != 1 || cfg.HTTP.Bind[0] != "127.0.0.1" {
		t.Errorf("expected default bind [127.0.0.1], got %v", cfg.HTTP.Bind)
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
	if cfg.Slack.SessionAbsoluteThreshold != 0.98 {
		t.Errorf("expected session_absolute_threshold 0.98, got %f", cfg.Slack.SessionAbsoluteThreshold)
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
tailer:
  poll_interval_ms: 500
logging:
  level: debug
  file: "/tmp/trayapp.log"
slack:
  session_surplus_threshold: 0.75
  weekly_surplus_threshold: 0.05
  session_absolute_threshold: 0.95
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
	if cfg.Slack.SessionAbsoluteThreshold != 0.95 {
		t.Errorf("expected session_absolute_threshold 0.95, got %f", cfg.Slack.SessionAbsoluteThreshold)
	}
	if cfg.Slack.BaselineMaxAgeSeconds != 240 {
		t.Errorf("expected baseline_max_age_seconds 240, got %d", cfg.Slack.BaselineMaxAgeSeconds)
	}
}

// writeTempConfig writes content to a temp YAML file and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })
	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	tmpFile.Close()
	return tmpFile.Name()
}

func TestLoadSlackProfiles(t *testing.T) {
	t.Run("absent profiles stay nil", func(t *testing.T) {
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.Slack.SessionProfile != nil {
			t.Errorf("expected nil session_profile by default, got %v", cfg.Slack.SessionProfile)
		}
		if cfg.Slack.WeeklyProfile != nil {
			t.Errorf("expected nil weekly_profile by default, got %v", cfg.Slack.WeeklyProfile)
		}
	})

	t.Run("valid profiles parse as pair lists", func(t *testing.T) {
		path := writeTempConfig(t, `
slack:
  session_profile:
    - [0, 98]
    - [52, 98]
    - [100, 50]
  weekly_profile:
    - [0, 80]
    - [30, 80]
    - [100, 10]
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		wantSession := [][]float64{{0, 98}, {52, 98}, {100, 50}}
		if len(cfg.Slack.SessionProfile) != len(wantSession) {
			t.Fatalf("session_profile: got %v, want %v", cfg.Slack.SessionProfile, wantSession)
		}
		for i, pair := range wantSession {
			got := cfg.Slack.SessionProfile[i]
			if len(got) != 2 || got[0] != pair[0] || got[1] != pair[1] {
				t.Errorf("session_profile[%d]: got %v, want %v", i, got, pair)
			}
		}
		if len(cfg.Slack.WeeklyProfile) != 3 || cfg.Slack.WeeklyProfile[1][0] != 30 {
			t.Errorf("weekly_profile: got %v", cfg.Slack.WeeklyProfile)
		}
	})

	invalid := []struct {
		name, yaml, wantErr string
	}{
		{
			"wrong arity",
			"slack:\n  session_profile:\n    - [0, 98, 5]\n",
			"session_profile",
		},
		{
			"time not increasing",
			"slack:\n  weekly_profile:\n    - [50, 80]\n    - [50, 70]\n",
			"weekly_profile",
		},
		{
			"remaining out of range",
			"slack:\n  session_profile:\n    - [0, 150]\n",
			"session_profile",
		},
		{
			"explicitly empty list",
			"slack:\n  session_profile: []\n",
			"session_profile",
		},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempConfig(t, tt.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestEnsureDefaultConfig(t *testing.T) {
	sample := []byte("# sample\nhttp:\n  port: 12345\n")

	t.Run("creates the file when absent", func(t *testing.T) {
		dir := t.TempDir()
		path, err := EnsureDefaultConfig(dir, sample)
		if err != nil {
			t.Fatalf("EnsureDefaultConfig: %v", err)
		}
		if path != filepath.Join(dir, "config.yaml") {
			t.Errorf("unexpected path %q", path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != string(sample) {
			t.Errorf("content mismatch: got %q", got)
		}
	})

	t.Run("never touches an existing file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		userContent := []byte("http:\n  port: 9999\n")
		if err := os.WriteFile(path, userContent, 0644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		got, err := EnsureDefaultConfig(dir, sample)
		if err != nil {
			t.Fatalf("EnsureDefaultConfig: %v", err)
		}
		if got != path {
			t.Errorf("unexpected path %q", got)
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(userContent) {
			t.Errorf("existing file was modified: %q", after)
		}
	})
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
	if runtime.GOOS == "windows" {
		// os.UserHomeDir reads USERPROFILE (not HOME) on Windows, so the
		// HOME-based fallback under test is unreachable there.
		t.Skip("HOME fallback of expandPlaceholders is Linux-specific")
	}
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
  cowork_sessions_dir: "%APPDATA%/Claude/local-agent-mode-sessions"
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
	if cfg.Claude.CoworkSessionsDir != "/fake/appdata/Claude/local-agent-mode-sessions" {
		t.Errorf("expected expanded cowork_sessions_dir, got %q", cfg.Claude.CoworkSessionsDir)
	}
	if cfg.Pricing.TablePath != "/fake/appdata/prices.yaml" {
		t.Errorf("expected expanded table_path, got %q", cfg.Pricing.TablePath)
	}
}

func TestDefaultCoworkSessionsDir(t *testing.T) {
	t.Setenv("APPDATA", `C:\fake\appdata`)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	want := filepath.Join(`C:\fake\appdata`, "Claude", "local-agent-mode-sessions")
	if cfg.Claude.CoworkSessionsDir != want {
		t.Errorf("expected default cowork_sessions_dir %q, got %q", want, cfg.Claude.CoworkSessionsDir)
	}

	t.Setenv("APPDATA", "")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Claude.CoworkSessionsDir != "" {
		t.Errorf("expected empty cowork_sessions_dir when APPDATA is unset, got %q", cfg.Claude.CoworkSessionsDir)
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
