# Implementation Status

## Completed Phases

### Phase 0: Project Skeleton ✅
- Go module initialized
- Directory structure created (cmd/, internal/, config/, userscript/)
- Makefile with build, test, vet targets
- GitHub Actions CI workflow
- Test helper utilities (TempDB, TempDir)
- Development conventions documented (logging, error handling, testing)

### Phase 1: Persistence Layer ✅
- SQLite database with WAL mode
- Migration system with schema versioning
- All v1 tables created: usage_events, quota_snapshots, windows, slack_samples, slack_releases, parse_errors
- UNIQUE index on (session_id, message_id) for dedup
- Insert/query functions for all tables
- Retention/housekeeping: prune parse_errors (30d) and slack_samples (90d)
- Comprehensive unit tests for all operations

### Phase 2: HTTP Server & Logging ✅
- Config loader with YAML support and sensible defaults
- HTTP server scaffolding with request logging
- POST /log: accepts usage events with validation and dedup
- POST /parse_error: records parsing errors
- GET /healthz: database health check
- Cost resolution helper: computes costs from token counts and price table
- CLI Mode A: clusage-cli log with explicit flags
- clusage-cli ping command
- Localhost-only binding (127.0.0.1)
- Comprehensive handler and config tests

### Phase 3: Transcript Parser & Tailer ✅
- JSONL parser: extracts usage events from Claude Code transcripts
- Parser resilience: handles malformed lines, preserves original JSON
- Host tailer: watches projects directory with fsnotify + polling fallback
- Tailer maintains byte offsets for resume-safe restarts
- CLI Mode B: processes Claude Code Stop hook payloads from stdin
- Both paths share parser for consistency
- Comprehensive parser tests (14 test cases covering edge cases)

### Phase 4: Snapshots & Windows ✅
- POST /snapshot handler: stores authoritative quota snapshots
- Records both observed_at and received_at for clock skew analysis
- Windows engine: derives 5-hour and weekly windows
- Window start detection from first event after gap
- Baseline assignment from most recent snapshot
- Window expiry and closure for historical queries
- Drift computation between baseline and actual consumption

### Phase 5: Slack Signal & Release Logging ✅
- Slack calculator: computes available capacity for queuing
- Combines metrics from 5-hour and weekly windows (minimum)
- Gate checks: headroom, priority quiet period, snapshot freshness, paused state
- GET /slack: returns full metrics and release_recommended boolean
- POST /slack/release: audit log of released jobs with estimated cost
- Optional slack sampling (slack_samples table)
- CLI slack command with output formats: json, release-bool, fraction

### Phase 6: Dashboard & Discount ✅
- Discount calculation matching `docs/discount-calculation.md` field names
  (`savings_usd`, `value_ratio`, `consumed_usd_equivalent`,
  `events_with_reported_cost`, `events_with_computed_cost`,
  `events_without_cost`)
- GET /discount endpoint: cost period analysis with reported/computed split
- Dashboard JSON endpoint and HTML/JS shell served from the same process

### Phase 7: Tray UI & Config ✅
- Extended config loader (Windows `%APPDATA%` path resolution, env-style
  placeholder expansion, network-bind block, log-file path,
  `release_threshold` / `baseline_max_age_hours` /
  `baseline_drift_threshold` slack fields)
- Network binding strategy (enumerate interfaces, Docker/WSL ranges,
  `127.0.0.1` fallback)
- Tray icon and menu skeleton via `fyne.io/systray` with the documented v1
  items (Open dashboard, Status, Pause slack signal, About, Quit). Pause
  and Quit handlers are wired through to `*slack.Calculator` and the
  graceful-shutdown path; Open dashboard, About, the Status submenu, and
  the icon color states are TODO stubs (logged on click) pending Windows
  hands-on time
- Graceful shutdown phases: HTTP server drain (10s deadline), background
  goroutines stopped (retention pruner, windows ticker, tailer), WAL
  checkpoint, DB close. Tailer offsets are persisted on every read so no
  end-of-run flush is required.
- Rotating log file (size-based, capped backups) opt-in via
  `logging.file` in the config; default is stdout
- `install.ps1` registers a per-user Task Scheduler "at logon" task
- Build-tag isolation: `tray_windows.go` (`//go:build windows`) is the only
  file that imports `fyne.io/systray`; `tray_stub.go` (`//go:build
  !windows`) keeps the Linux build headless and dependency-free

### Phase 8: E2E Validation & Polish ✅
- End-to-end Go integration suite in `internal/integration/e2e_test.go`,
  cross-referenced from `testdata/e2e_test.md`:
  - `TestE2E_CLIModeA_DiscountAndSlack`
  - `TestE2E_DuplicateDetection`
  - `TestE2E_SnapshotAndWindowDerivation`
  - `TestE2E_CostResolution`
  - `TestE2E_SlackReleaseFlow`
  - `TestE2E_ParseErrorRoundTrip`
- Userscript shipped with auto-update headers
  (`userscript/claude-usage-snapshot.user.js`)
- `make test` green; Windows manual verification checklist captured in
  `docs/test-plan.md`

## Architecture Highlights

- **Test-driven**: Unit tests for all logic, integration tests for HTTP endpoints
- **Single binary**: Both trayapp and CLI from same module
- **No ORM**: Plain SQL, explicit query construction
- **Build-tagged**: Windows-specific code isolated, everything compiles on Linux
- **Resilient parsing**: Malformed input recorded, not lost
- **Dedup-safe**: Tailer, hook path, and replayed events converge on (session_id, message_id)
- **Forensic**: All raw payloads stored, drift surfaces as data not correction

## Test Coverage

- ✅ Store: migrations, uniqueness, CRUD operations, retention
- ✅ Config: defaults, file loading, YAML parsing, env expansion
- ✅ Cost: reported vs computed, cache tokens, unknown models
- ✅ Parser: valid/invalid JSON, missing fields, timestamps, errors
- ✅ Server: validation, UNIQUE constraint, handler round-trips
- ✅ Windows engine: derivation, baseline assignment, drift computation
- ✅ Slack: gate checks, fraction combination, release recording, pause flag
- ✅ E2E: full integration scenarios in `internal/integration/e2e_test.go`

## Known Limitations (v1)

- Slack **pause state is transient** — it lives only in the calculator's
  in-memory flag and resets to `false` on every trayapp restart. This is
  intentional; see `docs/design-decisions.md` for the rationale (pause is a
  session-bounded operator override, not a configuration setting).
- Tailer uses file walking + stat-based offset tracking (not ideal for huge projects)
- No CLI offset file (server dedupes instead)
- No forecasting (uniform E(t) only)
- No job runner (exposes signal, doesn't act)
- No multi-host / Cloudflare tunnel

## Build & Run

```bash
# Build both binaries
make build-cli build-trayapp

# Run tests
make test

# Start trayapp
./trayapp

# Post a test event
clusage-cli log --input-tokens 100 --output-tokens 50 --session-id test-1 --message-id msg-1

# Check health
clusage-cli ping

# Query slack signal
clusage-cli slack --format fraction
```

## Next Steps

- Run the Windows manual verification checklist in `docs/test-plan.md`
  against a built `trayapp.exe` (tray UX, autostart, graceful shutdown on
  logoff).
- Iterate on dashboard HTML/JS as needed once real usage data accumulates
  on a Windows host.
