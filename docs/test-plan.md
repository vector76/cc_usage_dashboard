# Test Plan

This document is the verification matrix for the project. It pairs the
**Linux automated suite** (the source of truth for correctness, runs on
CI) with the **Windows manual checklist** (covers UX surfaces the Go
suite cannot exercise: tray icon, autostart, logoff shutdown).

The Go suite is the canonical runner; the manual list is a one-shot
sign-off before declaring a Windows release good.

## Linux: automated test plan

All targets run from the repository root and require only the standard
Go toolchain plus a C compiler for the SQLite cgo driver.

### Single command

```bash
make test
```

Expected: every package passes, no `FAIL` lines. CI runs the same
command (`make ci` adds vet and the two Linux builds).

### Per-package coverage

Run only the tests relevant to the area you're changing during
development; run the full suite before declaring done.

| Area                                | Command                                            | Asserts                                                                          |
|-------------------------------------|----------------------------------------------------|----------------------------------------------------------------------------------|
| Persistence and migrations          | `go test ./internal/store/...`                     | Schema migration, UNIQUE on `(session_id, message_id)`, retention pruning        |
| Config loader                       | `go test ./internal/config/...`                    | Defaults, YAML parsing, `%APPDATA%`/env-style expansion, slack threshold fields  |
| Cost resolution                     | `go test ./internal/ingest/...`                    | Reported vs. computed cost, cache token rates, unknown-model fallthrough         |
| Transcript parser                   | `go test ./internal/ingest/...`                    | Valid/invalid JSONL, missing fields, timestamp parsing, parse-error capture     |
| Tailer                              | `go test ./internal/ingest/...`                    | `.jsonl` matching, offset persistence, advance past skipped lines                |
| Windows engine                      | `go test ./internal/windows/...`                   | session and weekly derivation, baseline assignment, clock injection              |
| Slack calculator                    | `go test ./internal/slack/...`                     | Per-window slack fractions, gates (session/weekly headroom, baseline freshness, not-paused), pause |
| Consumption calculator              | `go test ./internal/consumption/...`               | Documented field names; snapshot-derived `consumed_session_pct` / `consumed_weekly_pct` with cross-window resets |
| HTTP server (handlers + dashboard)  | `go test ./internal/server/...`                    | `/log`, `/parse_error`, `/snapshot`, `/slack`, `/slack/release`, `/consumption`  |
| CLI hook payload parsing            | `go test ./cmd/clusage-cli/...`                    | Hook stdin payload → `/log` POST                                                 |
| End-to-end integration              | `go test ./internal/integration/...`               | Six scenarios documented in `testdata/e2e_test.md`                                |

### Userscript unit tests

Pure-JS logic destined for `userscript/claude-usage-snapshot.user.js`
lives in `userscript/lib/*.js` (CommonJS modules) and is exercised by
`userscript/test/*.test.js` using Node's built-in `node:test` runner —
no extra dependencies. Run with:

```bash
make test-userscript        # equivalent to: npm --prefix userscript test
```

Expected: every `*.test.js` file under `userscript/test/` reports
`pass`. The Go `make test` target does **not** invoke this; CI runs it
via `make ci`.

### Build verification

```bash
make build-cli            # produces ./clusage-cli
make build-trayapp        # produces ./trayapp (headless on Linux)
```

Expected: both binaries build, `./trayapp -h` and `./clusage-cli --help`
print usage, no missing-symbol errors. The Linux trayapp build
exercises the `//go:build !windows` stub from `cmd/trayapp/tray_stub.go`
— see `docs/design-decisions.md`.

### Cross-compile to Windows (optional, requires mingw-w64)

```bash
make build-trayapp-windows   # produces ./trayapp.exe
```

Expected: builds without errors. Functionality is verified manually on
a Windows host (next section).

### Smoke test against a running server

A minimal end-to-end sanity check from a shell, against a freshly built
trayapp:

```bash
./trayapp &
sleep 1
# CLI defaults to host.docker.internal (the in-container case); override
# to 127.0.0.1 for a host-side smoke test.
export CLUSAGE_HOST=127.0.0.1
./clusage-cli ping                                                                           # expect "ok"
./clusage-cli log --input-tokens 100 --output-tokens 50 --session-id s1 --message-id m1      # expect 200
./clusage-cli slack --format json                                                            # expect documented keys
./clusage-cli consumption --period 24h                                                       # expect documented keys
kill %1
```

This duplicates `internal/integration/e2e_test.go` but is useful when
debugging shell/transport issues that the in-process test bypasses.

## Windows: manual verification checklist

These items require a real Windows host with a logged-in user and a
browser. They are not covered by the Go suite. Run through the list
once per release candidate.

### Build and install

- [ ] `make build-trayapp-windows` (or the equivalent `go build` from
      `install.ps1`'s comment) produces `trayapp.exe`.
- [ ] `powershell -ExecutionPolicy Bypass -File .\install.ps1` runs to
      completion with no errors and reports "Registered scheduled task".
- [ ] `%APPDATA%\usage_dashboard\prices.yaml` exists after install
      (copied from `config\prices.example.yaml`).
- [ ] Re-running `install.ps1` is idempotent: existing `prices.yaml` is
      left untouched and the scheduled task is replaced cleanly.

### Tray UX — currently wired

The current scaffolding wires the Pause and Quit handlers end-to-end and
sets a static title/tooltip. The other menu items are deliberate TODO
stubs (they log on click); see "Tray UX — deferred" below.

- [ ] Tray icon appears in the system tray after launching `trayapp.exe`.
- [ ] Tooltip on hover shows the static `Claude Usage Dashboard` text.
- [ ] All five menu items are present in the documented order: Open
      dashboard, Status, Pause slack signal, About, Quit.
- [ ] **Pause slack signal** toggles the check mark and:
      - [ ] `GET /slack` returns `paused: true` while toggled on.
      - [ ] Toggling off restores `paused: false`.
      - [ ] **Pause does NOT persist across a trayapp restart** — see
            `docs/design-decisions.md`. Verify by toggling on, killing
            the trayapp, restarting it, and confirming `paused: false`.
- [ ] **Quit** triggers a graceful shutdown (see below).

### Tray UX — deferred (handlers are TODOs in `tray_windows.go`)

These items are part of the documented v1 contract in `docs/tray-app.md`
but are not yet wired. They are listed here so they're not forgotten when
the handlers land. Until then, expect each to log a `tray:` line on
click instead of taking action.

- [ ] Tooltip dynamically shows current burn rate / slack fraction
      (currently static text).
- [ ] Icon color reflects state per `docs/tray-app.md` (green / yellow /
      red / gray when paused or no data). The current build does not
      ship an icon image at all.
- [ ] **Open dashboard** opens `http://localhost:<port>` in the default
      browser (currently logs only).
- [ ] **Status** submenu populates with current session and weekly burn
      (currently a flat menu item with no children and no handler).
- [ ] **About** shows the version string baked into the build (currently
      logs only).

### Autostart

- [ ] Log off and back on; the tray icon reappears without manual launch.
- [ ] `Get-ScheduledTask -TaskName ClaudeUsageDashboard` shows the task
      with `State = Ready`.
- [ ] Reboot; the tray icon appears after logon.

### Graceful shutdown

- [ ] Quit from the tray menu: process exits cleanly. The configured log
      sink (stdout when `logging.file` is empty, otherwise the rotating
      log file) records `shutdown complete`. After exit,
      `sqlite3 usage.db "PRAGMA wal_checkpoint;"` returns
      `0|0|0` (no busy, no frames pending, no frames re-checkpointed).
- [ ] Sign out / restart Windows: the trayapp receives the OS shutdown
      signal, drains in-flight HTTP requests, persists tailer offsets,
      and exits before the OS force-kills it.
- [ ] Tailer resumes from the persisted offset on next launch (verify
      by appending lines to a watched JSONL while the trayapp is down,
      then confirming they appear in `usage_events` after restart).

### Network binding

- [ ] `netstat -an | findstr :27812` shows a listener on `127.0.0.1` and
      one on the auto-detected Docker/WSL adapter IP (the IP that
      `host.docker.internal` resolves to from inside containers).
- [ ] A Linux container started with `--add-host=host.docker.internal:host-gateway`
      can `curl http://host.docker.internal:27812/healthz` and get `ok`.
- [ ] No `0.0.0.0:27812` or public-IP listener is present.

### Userscript end-to-end

The userscript only writes `console.warn('[claude-usage-snapshot]', ...)`
on transport or parse failures — successful posts are silent by design.
Posts go through `GM.xmlHttpRequest`, which the userscript manager runs
in its own privileged context, so they may or may not show up in the
page's DevTools Network tab depending on the manager. Verify success at
the database, which is authoritative.

- [ ] Install `userscript/claude-usage-snapshot.user.js` per the README.
- [ ] Open `https://claude.ai/settings/usage`. Within ~30 s of the
      progressbars rendering, at least one row appears in
      `quota_snapshots` with `source='userscript'`
      (`sqlite3 usage.db "SELECT COUNT(*) FROM quota_snapshots WHERE source='userscript';"`).
- [ ] Navigating to any other route (e.g. `/chat/...`) does **not** add
      new `quota_snapshots` rows — the userscript no-ops off the usage page.
- [ ] After running some Claude Code activity that moves the displayed
      percentages, a follow-up row appears within ~1 minute. (The
      userscript dedupes on `(session_used, weekly_used)`, so identical-
      value snapshots are intentionally skipped.)
- [ ] The page console has no `[claude-usage-snapshot]` warnings during
      a healthy run.
- [ ] If the DOM changes and the script can't find quota nodes for >5
      minutes, a `parse_errors` row appears with `source = 'userscript'`.

### Sign-off

- [ ] Every checkbox above ticked, or the failure logged as a known
      issue with a tracking bead. Otherwise the build is not a Windows
      release candidate.
