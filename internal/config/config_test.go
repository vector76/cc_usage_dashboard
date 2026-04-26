package config

import (
	"os"
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
	if cfg.Subscription.MonthlyUSD != 20.0 {
		t.Errorf("expected monthly_usd 20.0, got %f", cfg.Subscription.MonthlyUSD)
	}
	if cfg.Subscription.BillingCycleDays != 30 {
		t.Errorf("expected billing_cycle_days 30, got %d", cfg.Subscription.BillingCycleDays)
	}
	if cfg.Retention.ParseErrorsDays != 30 {
		t.Errorf("expected parse_errors retention 30 days, got %d", cfg.Retention.ParseErrorsDays)
	}
	if cfg.EnableSlackSampling {
		t.Error("expected slack sampling disabled by default")
	}
}

func TestLoadFromFile(t *testing.T) {
	// Create a temporary config file
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
subscription:
  monthly_usd: 50.0
  billing_cycle_days: 31
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
	if cfg.Subscription.MonthlyUSD != 50.0 {
		t.Errorf("expected monthly_usd 50.0, got %f", cfg.Subscription.MonthlyUSD)
	}
	if cfg.Subscription.BillingCycleDays != 31 {
		t.Errorf("expected billing_cycle_days 31, got %d", cfg.Subscription.BillingCycleDays)
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

	if _, err := tmpFile.WriteString("invalid: yaml: content:"); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	tmpFile.Close()

	_, err = Load(tmpFile.Name())
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
