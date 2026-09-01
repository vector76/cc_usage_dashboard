package config

import (
	"os"
	"path/filepath"
	"runtime"
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
// against later. dir is created when missing, since the per-user config dir
// this normally targets does not exist before first run. Callers treat a
// write failure as non-fatal: the app can always run on built-in defaults.
func EnsureDefaultConfig(dir string, sample []byte) (string, error) {
	path := filepath.Join(dir, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, sample, 0644); err != nil {
		return "", err
	}
	return path, nil
}

// UserDataDir returns the per-user directory for mutable application state —
// the database and its WAL sidecars. On Windows that is
// %LOCALAPPDATA%\usage_dashboard: Local rather than Roaming deliberately,
// since a roaming profile would sync a live SQLite file and its WAL between
// machines. Elsewhere it is ~/.local/share/usage-dashboard.
//
// Falls back to the working directory only when every home/profile lookup
// fails, which in practice means a broken environment.
func UserDataDir() string {
	if runtime.GOOS == "windows" {
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			return filepath.Join(localAppData, "usage_dashboard")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "AppData", "Local", "usage_dashboard")
		}
		return "."
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "usage-dashboard")
	}
	return "."
}

// UserConfigDir returns the per-user directory for hand-edited configuration:
// %APPDATA%\usage_dashboard on Windows, ~/.config/usage-dashboard elsewhere.
// This is where the trayapp materializes config.yaml on first run, so it must
// stay a member of PriceTableSearchDirs — otherwise the file it writes would
// not be found on the next start. Pinned by TestUserConfigDirIsInSearchChain.
//
// Deliberately distinct from UserDataDir: configuration may roam with the
// user, a database must not.
func UserConfigDir() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "usage_dashboard")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "AppData", "Roaming", "usage_dashboard")
		}
		return "."
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "usage-dashboard")
	}
	return "."
}

// ResolveDBPath returns the database path to use when config.yaml does not
// name one explicitly, in precedence order:
//
//  1. $USAGE_DASHBOARD_DB — the explicit override, used verbatim.
//  2. An existing usage.db in the current working directory. This is the
//     build-from-checkout workflow, where the exe runs from the repo root
//     with its database beside it; honoring it means installing a newer
//     binary never strands accumulated history in the old location.
//  3. UserDataDir()/usage.db — the installed case.
//
// Cases 2 and 3 are always absolute. The default was previously the bare
// relative name "usage.db", which resolved against whatever directory the
// process happened to start in — so a `go install`ed binary launched from,
// say, C:\Windows\system32 tried to create its database there and died with
// SQLITE_CANTOPEN. See docs/configuration.md "Database location".
func ResolveDBPath() string {
	if dbPath := os.Getenv("USAGE_DASHBOARD_DB"); dbPath != "" {
		return dbPath
	}

	if info, err := os.Stat("usage.db"); err == nil && !info.IsDir() {
		if abs, err := filepath.Abs("usage.db"); err == nil {
			return abs
		}
	}

	return filepath.Join(UserDataDir(), "usage.db")
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
// Both 3 and 4 are probed on every platform, so a config.yaml or prices.yaml
// placed by hand in either spot is honored regardless of OS; UserConfigDir
// picks the platform-native one of the two to write to.
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
