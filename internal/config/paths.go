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

// PriceTableSearchDirs returns the directories to probe for a user-supplied
// prices.yaml that overrides the embedded default without a rebuild, in
// precedence order:
//
//  1. The directory containing the running executable — drop a prices.yaml
//     next to trayapp.exe to override.
//  2. The current working directory.
//  3. %APPDATA%\usage_dashboard\ — the Windows app config dir (matches the
//     location documented for config.yaml).
//  4. ~/.config/usage-dashboard/ — the Linux/macOS config dir the config
//     loader already honors (see ResolveConfigPath).
//
// Directories whose backing environment/OS lookup fails are silently skipped;
// ResolvePriceTable only reads a candidate that actually exists.
func PriceTableSearchDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, ".")
	if appData := os.Getenv("APPDATA"); appData != "" {
		dirs = append(dirs, filepath.Join(appData, "usage_dashboard"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "usage-dashboard"))
	}
	return dirs
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
