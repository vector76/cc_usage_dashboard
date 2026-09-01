package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// setFakeUserDirs pins every environment variable the data/config dir
// resolvers read, so the tests below assert on a known tree rather than the
// developer's real profile.
func setFakeUserDirs(t *testing.T) (appData, localAppData, home string) {
	t.Helper()
	root := t.TempDir()
	appData = filepath.Join(root, "AppData", "Roaming")
	localAppData = filepath.Join(root, "AppData", "Local")
	home = filepath.Join(root, "home")
	t.Setenv("APPDATA", appData)
	t.Setenv("LOCALAPPDATA", localAppData)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("HOME", home)        // os.UserHomeDir elsewhere
	return appData, localAppData, home
}

func TestUserDataDirIsPlatformSpecific(t *testing.T) {
	_, localAppData, home := setFakeUserDirs(t)

	want := filepath.Join(home, ".local", "share", "usage-dashboard")
	if runtime.GOOS == "windows" {
		want = filepath.Join(localAppData, "usage_dashboard")
	}
	if got := UserDataDir(); got != want {
		t.Errorf("UserDataDir() = %q, want %q", got, want)
	}
}

func TestUserConfigDirIsPlatformSpecific(t *testing.T) {
	appData, _, home := setFakeUserDirs(t)

	want := filepath.Join(home, ".config", "usage-dashboard")
	if runtime.GOOS == "windows" {
		want = filepath.Join(appData, "usage_dashboard")
	}
	if got := UserConfigDir(); got != want {
		t.Errorf("UserConfigDir() = %q, want %q", got, want)
	}
}

// UserConfigDir must agree with the directory PriceTableSearchDirs already
// probes, so a config.yaml materialized there is found again on next start.
func TestUserConfigDirIsInSearchChain(t *testing.T) {
	setFakeUserDirs(t)

	want := UserConfigDir()
	for _, dir := range PriceTableSearchDirs() {
		if dir == want {
			return
		}
	}
	t.Errorf("UserConfigDir() = %q is not among PriceTableSearchDirs() %v", want, PriceTableSearchDirs())
}

func TestResolveDBPathPrefersEnvVar(t *testing.T) {
	setFakeUserDirs(t)
	t.Setenv("USAGE_DASHBOARD_DB", "/explicit/override.db")

	// Even with a cwd usage.db present, the explicit override wins.
	t.Chdir(t.TempDir())
	if err := os.WriteFile("usage.db", nil, 0644); err != nil {
		t.Fatal(err)
	}

	if got := ResolveDBPath(); got != "/explicit/override.db" {
		t.Errorf("ResolveDBPath() = %q, want the USAGE_DASHBOARD_DB override", got)
	}
}

// The build-from-checkout workflow runs the exe with an already-populated
// usage.db in the working directory; that database must keep being used
// rather than silently moving to the per-user data dir.
func TestResolveDBPathPrefersExistingDBInWorkingDir(t *testing.T) {
	setFakeUserDirs(t)
	t.Setenv("USAGE_DASHBOARD_DB", "")

	t.Chdir(t.TempDir())
	if err := os.WriteFile("usage.db", nil, 0644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	got := ResolveDBPath()
	want := filepath.Join(cwd, "usage.db")
	if got != want {
		t.Errorf("ResolveDBPath() = %q, want the existing checkout database %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveDBPath() = %q, want an absolute path", got)
	}
}

// A directory named usage.db is not a database; it must not shadow the
// per-user data dir.
func TestResolveDBPathIgnoresUsageDBDirectoryInWorkingDir(t *testing.T) {
	setFakeUserDirs(t)
	t.Setenv("USAGE_DASHBOARD_DB", "")

	t.Chdir(t.TempDir())
	if err := os.Mkdir("usage.db", 0755); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(UserDataDir(), "usage.db")
	if got := ResolveDBPath(); got != want {
		t.Errorf("ResolveDBPath() = %q, want %q", got, want)
	}
}

// The `go install` case: launched from an arbitrary, possibly unwritable
// working directory with no usage.db in sight. The result must be an
// absolute path inside the user's own data dir, never a bare relative name
// that lands wherever the shell happened to start.
func TestResolveDBPathFallsBackToUserDataDir(t *testing.T) {
	setFakeUserDirs(t)
	t.Setenv("USAGE_DASHBOARD_DB", "")
	t.Chdir(t.TempDir())

	got := ResolveDBPath()
	want := filepath.Join(UserDataDir(), "usage.db")
	if got != want {
		t.Errorf("ResolveDBPath() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveDBPath() = %q, want an absolute path", got)
	}
}

// Regression guard for the go-install bug: config.Load must leave
// Database.Path empty so the caller's resolution chain actually runs. A
// literal "usage.db" default made ResolveDBPath unreachable and pinned the
// database to the process working directory.
func TestLoadLeavesDatabasePathUnresolved(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Database.Path != "" {
		t.Errorf("Load default Database.Path = %q, want \"\" so ResolveDBPath applies", cfg.Database.Path)
	}
}

// The per-user config dir does not exist before first run, so
// EnsureDefaultConfig must create it rather than failing the write.
func TestEnsureDefaultConfigCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage_dashboard")

	path, err := EnsureDefaultConfig(dir, []byte("# sample\n"))
	if err != nil {
		t.Fatalf("EnsureDefaultConfig failed: %v", err)
	}
	if want := filepath.Join(dir, "config.yaml"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading materialized config: %v", err)
	}
	if string(got) != "# sample\n" {
		t.Errorf("materialized config = %q, want the sample", got)
	}
}
