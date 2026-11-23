package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadPlanYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestPlanDefaultsToAuto(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Plan != "auto" {
		t.Errorf("Plan = %q, want auto", cfg.Plan)
	}
	cfg, err = loadPlanYAML(t, "http:\n  port: 27812\n")
	if err != nil || cfg.Plan != "auto" {
		t.Errorf("file without plan: %q, %v", cfg.Plan, err)
	}
}

func TestPlanAcceptsKnownValues(t *testing.T) {
	for _, v := range []string{"auto", "enterprise", "quota", "Enterprise", " QUOTA "} {
		cfg, err := loadPlanYAML(t, "plan: \""+v+"\"\n")
		if err != nil {
			t.Errorf("plan %q rejected: %v", v, err)
			continue
		}
		if want := strings.ToLower(strings.TrimSpace(v)); cfg.Plan != want {
			t.Errorf("plan %q normalized to %q, want %q", v, cfg.Plan, want)
		}
	}
}

func TestPlanRejectsUnknownValue(t *testing.T) {
	_, err := loadPlanYAML(t, "plan: team\n")
	if err == nil || !strings.Contains(err.Error(), "plan") {
		t.Errorf("err = %v, want a message naming the plan key", err)
	}
}
