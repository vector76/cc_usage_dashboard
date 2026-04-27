# Tray app

Single Go executable for Windows. Owns the database, runs the HTTP server, tails session
JSONL, and provides a tray icon UI.

## Why one binary

- Single SQLite writer by construction (no IPC, no file locks across processes).
- Easier autostart and uninstall: one `.exe`, one shortcut.
- The dashboard is just static assets served from the same process.

## Build

- Module: `github.com/<user>/usage_dashboard`
- Tray library: `fyne.io/systray` (current maintained fork) or `getlantern/systray` if
  CGO setup is friendlier on the dev box.
- HTTP: stdlib `net/http`. No framework needed.
- DB: `modernc.org/sqlite` (pure Go, avoids CGO) or `mattn/go-sqlite3` if FTS or extensions
  are wanted later. Start with the pure-Go driver.
- Filesystem watch: `fsnotify/fsnotify` with a polling fallback.

Build command (run on the Windows host):

```
go build -ldflags="-H=windowsgui" -o trayapp.exe ./cmd/trayapp
```

The `-H=windowsgui` flag suppresses the console window.

## Tray menu (v1)

- **Open dashboard** — opens `http://localhost:PORT` in the default browser.
- **Status** — submenu showing session burn %, weekly burn %, slack fraction, last snapshot age.
- **Pause slack signal** — sets `release_recommended=false` on the slack endpoint and
  flips the tray icon to a paused state. Logging continues so no data is lost; only the
  release recommendation is suppressed. Use this when starting a heavy interactive
  session and you don't want background jobs racing you.
- **About** — version, build commit.
- **Quit** — graceful shutdown (flush DB, persist tailer offsets).

Tray icon should show distinct states:

- Green: under pace, slack available.
- Yellow: roughly on pace.
- Red: ahead of pace, baseline stale, or freshness gate failing.
- Gray: paused or no data.

## Configuration

The trayapp reads a single YAML file at startup (default path
`%APPDATA%\usage_dashboard\config.yaml`). The full schema, defaults, and
path-placeholder rules live in [`docs/configuration.md`](configuration.md).
The file is read once; changes require a restart.

## Autostart

Two reasonable options; pick one at install time:

1. **Task Scheduler "at logon"** — preferred. Runs even when the user account is logged
   in but no Explorer session is interactive (e.g. RDP). Survives standard `shell:startup`
   removal by user.
2. **`shell:startup` shortcut** — simpler, easier to disable, fine for most users.

Provide both via a small `install.ps1` script that the user runs once.

## Health and observability

- `GET /healthz` — process up, DB writable, tailer caught up.
- `GET /metrics` — minimal Prometheus-style counters (events ingested, snapshots received,
  parse errors, slack queries). Useful even without a Prometheus server: it's a quick
  status dump for debugging.
- All logs to a rotating file in `%LOCALAPPDATA%`.

## Graceful shutdown

On `SIGINT`, tray "Quit", or Windows shutdown:

1. Stop accepting new HTTP requests (drain in-flight).
2. Flush tailer reads and persist offsets.
3. Checkpoint SQLite WAL.
4. Close DB.
5. Exit 0.

A `SIGKILL` equivalent (Task Manager "End task") is fine: WAL guarantees crash recovery
and tailer offsets are persisted on every batch insert.
