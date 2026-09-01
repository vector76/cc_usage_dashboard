package main

import (
	"path/filepath"

	"github.com/vector76/cc_usage_dashboard/internal/config"
)

// logFileName is the unconfigured log's basename, written into
// config.UserDataDir() alongside the database.
const logFileName = "trayapp.log"

// resolveLogFile decides where slog output goes when the user has not named a
// file, given whether a console is attached:
//
//   - An explicit logging.file always wins; it is an instruction, not a hint.
//   - Console attached: return "" so setupLogging keeps its stderr handler.
//     This is the build-from-checkout case, where the operator is watching the
//     terminal and a file would only hide output they asked to see.
//   - No console: stderr is not a real destination. A release build linked
//     with -H=windowsgui has no console at all, so every write to stderr is
//     discarded and the app runs blind — which is exactly how a startup
//     failure like an unopenable database becomes unreportable. Fall back to
//     a rotating file in the data dir.
//
// The file lives in UserDataDir rather than UserConfigDir because it is
// mutable state, not configuration — on Windows that means Local, never the
// roaming profile.
func resolveLogFile(configured string, consoleAttached bool) string {
	if configured != "" {
		return configured
	}
	if consoleAttached {
		return ""
	}
	return filepath.Join(config.UserDataDir(), logFileName)
}
