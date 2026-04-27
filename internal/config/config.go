// Package config provides configuration loading and management.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
type Config struct {
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	HTTP struct {
		Port           int      `yaml:"port"`
		Bind           []string `yaml:"bind"`
		EnableFallback bool     `yaml:"enable_fallback"`
	} `yaml:"http"`

	Claude struct {
		ProjectsDir string `yaml:"projects_dir"`
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
		HeadroomThreshold       float64 `yaml:"headroom_threshold"`
		QuietPeriodSeconds      int     `yaml:"quiet_period_seconds"`
		FreshnessThresholdMs    int     `yaml:"freshness_threshold_ms"`
		BaselineMaxAgeHours     int     `yaml:"baseline_max_age_hours"`
		SessionSurplusThreshold float64 `yaml:"session_surplus_threshold"`
		WeeklySurplusThreshold  float64 `yaml:"weekly_surplus_threshold"`
		// Weekly headroom additionally passes when percent_remaining is at
		// or above this fraction (0–1). Lets the gate fire early in the
		// week before pace-relative surplus has accumulated.
		WeeklyAbsoluteThreshold float64 `yaml:"weekly_absolute_threshold"`
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
	cfg.HTTP.EnableFallback = false
	cfg.Claude.ProjectsDir = expandHome("~/.claude/projects")
	cfg.Pricing.TablePath = "config/prices.example.yaml"
	cfg.Tailer.PollIntervalMs = 1000
	cfg.Logging.Level = "info"
	cfg.Logging.File = ""
	cfg.Slack.HeadroomThreshold = 10.0
	cfg.Slack.QuietPeriodSeconds = 300
	cfg.Slack.FreshnessThresholdMs = 60000
	cfg.Slack.BaselineMaxAgeHours = 48
	cfg.Slack.SessionSurplusThreshold = 0.50
	cfg.Slack.WeeklySurplusThreshold = 0.10
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
	cfg.Pricing.TablePath = expandPlaceholders(cfg.Pricing.TablePath)

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
