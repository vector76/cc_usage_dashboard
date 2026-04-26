// Package config provides configuration loading and management.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds the application configuration.
type Config struct {
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`

	HTTP struct {
		Port int `yaml:"port"`
	} `yaml:"http"`

	Claude struct {
		ProjectsDir string `yaml:"projects_dir"`
	} `yaml:"claude"`

	Pricing struct {
		TablePath string `yaml:"table_path"`
	} `yaml:"pricing"`

	Slack struct {
		HeadroomThreshold    float64 `yaml:"headroom_threshold"`
		QuietPeriodSeconds   int     `yaml:"quiet_period_seconds"`
		FreshnessThresholdMs int     `yaml:"freshness_threshold_ms"`
	} `yaml:"slack"`

	Subscription struct {
		MonthlyUSD   float64 `yaml:"monthly_usd"`
		BillingCycleDays int `yaml:"billing_cycle_days"`
	} `yaml:"subscription"`

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
	cfg.Claude.ProjectsDir = expandHome("~/.claude/projects")
	cfg.Pricing.TablePath = "config/prices.example.yaml"
	cfg.Slack.HeadroomThreshold = 10.0
	cfg.Slack.QuietPeriodSeconds = 300
	cfg.Slack.FreshnessThresholdMs = 60000
	cfg.Subscription.MonthlyUSD = 20.0
	cfg.Subscription.BillingCycleDays = 30
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

	return &cfg, nil
}

// expandHome expands ~/ to the user's home directory.
func expandHome(path string) string {
	if path[:2] != "~/" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
