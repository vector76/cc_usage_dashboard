package config

import (
	"os"
	"path/filepath"
)

// ResolveConfigPath finds the config file, probing the same directory chain
// as the prices.yaml override (see PriceTableSearchDirs): exe dir, current
// directory, %APPDATA%\usage_dashboard, ~/.config/usage-dashboard. Returns ""
// when no config.yaml exists anywhere — the trayapp then materializes the
// embedded sample via EnsureDefaultConfig rather than running fileless.
func ResolveConfigPath() string {
	for _, dir := range PriceTableSearchDirs() {
		candidate := filepath.Join(dir, "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// EnsureDefaultConfig writes sample to dir/config.yaml if the file does not
// already exist, returning the file's path. An existing file is never
// touched — the sample is a first-run scaffold, not something to reconcile
// against later. Callers treat a write failure as non-fatal: the app can
// always run on built-in defaults.
func EnsureDefaultConfig(dir string, sample []byte) (string, error) {
	path := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.WriteFile(path, sample, 0644); err != nil {
		return "", err
	}
	return path, nil
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
