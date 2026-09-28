package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/vector76/cc_usage_dashboard/internal/config"
)

// fatalStartup reports an error that ends startup, then exits with status 1.
// logFileOpen says whether setupLogging has installed a rotating log file.
func fatalStartup(logFileOpen bool, format string, args ...any) {
	reportStartupFailure(os.Stderr, fmt.Sprintf(format, args...), logFileOpen, consoleAttached(),
		filepath.Join(config.UserDataDir(), logFileName))
	os.Exit(1)
}

// reportStartupFailure writes msg to stderr, as startup failures always
// were, and also somewhere a -H=windowsgui build can be diagnosed from,
// since that build discards stderr:
//
//   - With the rotating log open, through slog, so it lands in that log.
//   - Otherwise, with no console attached, appended as a JSON record to
//     fallbackPath — the data-dir trayapp.log an unconfigured headless build
//     writes to anyway. This covers failures before logging exists (a
//     config.yaml typo) and a log file that could not be opened.
//
// Writing the fallback is best-effort: the process is exiting regardless.
func reportStartupFailure(stderr io.Writer, msg string, logFileOpen, console bool, fallbackPath string) {
	fmt.Fprintln(stderr, msg)
	if logFileOpen {
		slog.Error(msg)
		return
	}
	if console {
		return
	}
	if err := os.MkdirAll(filepath.Dir(fallbackPath), 0755); err != nil {
		return
	}
	f, err := os.OpenFile(fallbackPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	slog.New(slog.NewJSONHandler(f, nil)).Error(msg)
}
