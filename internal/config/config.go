// Package config provides configuration loading and management.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/vector76/cc_usage_dashboard/internal/slack"
)

// Config holds the application configuration.
type Config struct {
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	HTTP struct {
		Port int      `yaml:"port"`
		Bind []string `yaml:"bind"`
	} `yaml:"http"`

	Claude struct {
		ProjectsDir string `yaml:"projects_dir"`
		// CoworkSessionsDir is the root of the desktop app's Cowork
		// ("local agent mode") session tree. Each Cowork session nests its
		// own private .claude home several levels under this root
		// (<workspace>/<id>/local_<uuid>/.claude/projects/<encoded-cwd>/
		// <session>.jsonl), so the tailer walks it recursively rather than
		// treating it as a projects dir directly. Empty disables it (e.g.
		// non-Windows, where APPDATA isn't set).
		CoworkSessionsDir string `yaml:"cowork_sessions_dir"`
	} `yaml:"claude"`

	Pricing struct {
		TablePath string `yaml:"table_path"`
	} `yaml:"pricing"`

	Tailer struct {
		PollIntervalMs int `yaml:"poll_interval_ms"`
	} `yaml:"tailer"`

	Logging struct {
		Level string `yaml:"level"`
		File  string `yaml:"file"`
	} `yaml:"logging"`

	Slack struct {
		BaselineMaxAgeSeconds   int     `yaml:"baseline_max_age_seconds"`
		SessionSurplusThreshold float64 `yaml:"session_surplus_threshold"`
		WeeklySurplusThreshold  float64 `yaml:"weekly_surplus_threshold"`
		// Session headroom additionally passes when percent_remaining is at
		// or above this fraction (0–1). A value of 1.0 disables the
		// absolute branch.
		SessionAbsoluteThreshold float64 `yaml:"session_absolute_threshold"`
		// Weekly headroom additionally passes when percent_remaining is at
		// or above this fraction (0–1). Lets the gate fire early in the
		// week before pace-relative surplus has accumulated.
		WeeklyAbsoluteThreshold float64 `yaml:"weekly_absolute_threshold"`
		// SessionProfile / WeeklyProfile define the slack-activation
		// boundary as [time_pct, remaining_pct] points in burn-down chart
		// coordinates: at time_pct percent of the window elapsed, slack is
		// available while percent-remaining is at or above the boundary
		// (linear interpolation between points, flat beyond the endpoints).
		// Absent (nil) means "derive the boundary from the scalar
		// thresholds above", which reproduces the pre-profile behavior.
		// Validated at load by slack.ProfileFromPairs.
		SessionProfile [][]float64 `yaml:"session_profile"`
		WeeklyProfile  [][]float64 `yaml:"weekly_profile"`
	} `yaml:"slack"`

	Retention struct {
		ParseErrorsDays  int `yaml:"parse_errors_days"`
		SlackSamplesDays int `yaml:"slack_samples_days"`
	} `yaml:"retention"`

	EnableSlackSampling bool `yaml:"enable_slack_sampling"`
}

// Load loads configuration from a YAML file, applying defaults.
func Load(path string) (*Config, error) {
	var cfg Config

	// Set defaults
	cfg.Database.Path = "usage.db"
	cfg.HTTP.Port = 27812
	cfg.HTTP.Bind = []string{"127.0.0.1"}
	cfg.Claude.ProjectsDir = expandHome("~/.claude/projects")
	cfg.Claude.CoworkSessionsDir = defaultCoworkSessionsDir()
	// Empty means "use the resolution chain" (executable dir / app config
	// dir override, else the embedded built-in table). A non-empty value is
	// an explicit override. See ingest.ResolvePriceTable.
	cfg.Pricing.TablePath = ""
	cfg.Tailer.PollIntervalMs = 1000
	cfg.Logging.Level = "info"
	cfg.Logging.File = ""
	cfg.Slack.BaselineMaxAgeSeconds = 480
	cfg.Slack.SessionSurplusThreshold = 0.50
	cfg.Slack.WeeklySurplusThreshold = 0.10
	cfg.Slack.SessionAbsoluteThreshold = 0.98
	cfg.Slack.WeeklyAbsoluteThreshold = 0.80
	cfg.Retention.ParseErrorsDays = 30
	cfg.Retention.SlackSamplesDays = 90
	cfg.EnableSlackSampling = false

	// If no path provided, return defaults
	if path == "" {
		return &cfg, nil
	}

	// Load from file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Resolve env-style placeholders in path/dir fields.
	cfg.Database.Path = expandPlaceholders(cfg.Database.Path)
	cfg.Claude.ProjectsDir = expandPlaceholders(cfg.Claude.ProjectsDir)
	cfg.Claude.CoworkSessionsDir = expandPlaceholders(cfg.Claude.CoworkSessionsDir)
	cfg.Pricing.TablePath = expandPlaceholders(cfg.Pricing.TablePath)

	// Reject malformed slack profiles at startup with the offending key in
	// the message, rather than letting the gate misbehave silently later.
	if _, err := slack.ProfileFromPairs(cfg.Slack.SessionProfile); err != nil {
		return nil, fmt.Errorf("config slack.session_profile: %w", err)
	}
	if _, err := slack.ProfileFromPairs(cfg.Slack.WeeklyProfile); err != nil {
		return nil, fmt.Errorf("config slack.weekly_profile: %w", err)
	}

	return &cfg, nil
}

// expandPlaceholders replaces Windows-style environment placeholders
// (%APPDATA%, %LOCALAPPDATA%, %USERPROFILE%, %HOME%) with values from the
// environment. On Linux those vars are typically empty, so we fall back to
// the user's home directory to keep cross-platform config files testable.
func expandPlaceholders(s string) string {
	if s == "" {
		return s
	}
	tokens := []string{"APPDATA", "LOCALAPPDATA", "USERPROFILE", "HOME"}
	var home string
	homeResolved := false
	for _, name := range tokens {
		token := "%" + name + "%"
		if !strings.Contains(s, token) {
			continue
		}
		val := os.Getenv(name)
		if val == "" {
			if !homeResolved {
				if h, err := os.UserHomeDir(); err == nil {
					home = h
				}
				homeResolved = true
			}
			val = home
		}
		if val == "" {
			continue
		}
		s = strings.ReplaceAll(s, token, val)
	}
	return s
}

// defaultCoworkSessionsDir returns the root of the Claude desktop app's
// Cowork ("local agent mode") session tree on Windows. Each Cowork session
// runs against its own private, sandboxed .claude home nested several
// levels under this root rather than the user's real ~/.claude — the
// tailer walks it recursively to reach those nested projects/ dirs (see
// docs/data-sources.md, Tier 1a). Returns "" when APPDATA isn't set (e.g.
// non-Windows), which disables this second root without erroring.
func defaultCoworkSessionsDir() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "Claude", "local-agent-mode-sessions")
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
