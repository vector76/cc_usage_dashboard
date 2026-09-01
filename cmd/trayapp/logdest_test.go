package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vector76/cc_usage_dashboard/internal/config"
)

// pinUserDirs makes config.UserDataDir deterministic regardless of the
// developer's real profile.
func pinUserDirs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "Local"))
	t.Setenv("APPDATA", filepath.Join(root, "Roaming"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	return root
}

// An explicit logging.file is an instruction, not a hint: it wins whether or
// not a console is attached.
func TestResolveLogFileHonorsExplicitConfig(t *testing.T) {
	pinUserDirs(t)
	for _, console := range []bool{true, false} {
		if got := resolveLogFile("/custom/trayapp.log", console); got != "/custom/trayapp.log" {
			t.Errorf("resolveLogFile(explicit, console=%v) = %q, want the configured path", console, got)
		}
	}
}

// With a console attached, stderr is a real destination the user is watching.
// Returning "" keeps setupLogging on its stderr handler.
func TestResolveLogFileKeepsStderrWhenConsoleAttached(t *testing.T) {
	pinUserDirs(t)
	if got := resolveLogFile("", true); got != "" {
		t.Errorf("resolveLogFile(\"\", console=true) = %q, want \"\" so logs stay on stderr", got)
	}
}

// The -H=windowsgui case: no console means stderr goes nowhere, so an
// unconfigured log must land in a file or the app runs blind.
func TestResolveLogFileFallsBackToDataDirWhenNoConsole(t *testing.T) {
	pinUserDirs(t)

	got := resolveLogFile("", false)
	want := filepath.Join(config.UserDataDir(), "trayapp.log")
	if got != want {
		t.Errorf("resolveLogFile(\"\", console=false) = %q, want %q", got, want)
	}
	if got == "" {
		t.Fatal("resolveLogFile returned \"\" with no console; logs would be discarded")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolveLogFile = %q, want an absolute path", got)
	}
}

// The log file must sit beside the database, not in the config dir — it is
// mutable state, and on Windows that means Local, never Roaming.
func TestResolveLogFileSitsBesideTheDatabase(t *testing.T) {
	pinUserDirs(t)

	logDir := filepath.Dir(resolveLogFile("", false))
	if logDir != config.UserDataDir() {
		t.Errorf("log dir = %q, want the data dir %q", logDir, config.UserDataDir())
	}
	if runtime.GOOS == "windows" && logDir == config.UserConfigDir() {
		t.Error("log file landed in the roaming config dir; it belongs in Local")
	}
}

// On Unix stderr is always a valid fd (a terminal, a pipe, or systemd's
// journal), so the no-console fallback must never trigger there.
func TestConsoleAttachedIsTrueOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves console attachment via GetConsoleWindow")
	}
	if !consoleAttached() {
		t.Error("consoleAttached() = false on Unix, want true")
	}
}
