package config

import (
	"os"
	"path/filepath"
)

// ResolveConfigPath finds the config file, checking standard locations.
func ResolveConfigPath() string {
	// Check current directory first
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}

	// Check user config directory
	home, err := os.UserHomeDir()
	if err == nil {
		// Linux/macOS: ~/.config/usage-dashboard/
		xdgPath := filepath.Join(home, ".config", "usage-dashboard", "config.yaml")
		if _, err := os.Stat(xdgPath); err == nil {
			return xdgPath
		}
	}

	// No config found, will use defaults
	return ""
}

// ResolveDBPath returns the database path.
func ResolveDBPath() string {
	// Check for USAGE_DASHBOARD_DB env var
	if dbPath := os.Getenv("USAGE_DASHBOARD_DB"); dbPath != "" {
		return dbPath
	}

	// Try to find in home directory
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".local", "share", "usage-dashboard", "usage.db")
	}

	// Fallback to current directory
	return "usage.db"
}

// ResolveProjectsDir returns the Claude projects directory.
func ResolveProjectsDir() string {
	// Check env var first
	if dir := os.Getenv("CLAUDE_PROJECTS_DIR"); dir != "" {
		return dir
	}

	// Default: ~/.claude/projects
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".claude", "projects")
	}

	return ".claude/projects"
}
