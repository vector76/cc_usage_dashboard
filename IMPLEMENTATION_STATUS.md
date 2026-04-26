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

### Phase 4: Snapshots & Windows (Partial) ✅
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

## In Progress / Planned

### Phase 6: Dashboard & Discount
- Discount calculation (docs/discount-calculation.md schema exists)
- GET /discount endpoint: cost period analysis with reported/computed split
- Dashboard JSON endpoints for charts
- Health/status panel data
- Metrics endpoint for observability

### Phase 7: Tray UI & Config
- Extended config loader (Windows %APPDATA% path resolution, env placeholders)
- Network binding strategy (enumerate interfaces, Docker/WSL ranges, fallback)
- Tray icon color states (green/yellow/red/gray)
- Tray menu items (Open dashboard, Status, Pause, About, Quit)
- Graceful shutdown (flush offsets, checkpoint WAL)
- install.ps1 for Task Scheduler autostart
- Linux headless build works; Windows build tagged

### Phase 8: E2E Validation & Polish
- End-to-end integration scenarios
- E2E test 1: CLI Mode A posts → /discount and /slack work
- E2E test 2: Tailer watches directory → usage_events appear
- E2E test 3: CLI Mode B with fixture hook payload → same
- E2E test 4: POST /snapshot → baseline update, drift detection
- E2E test 5: Release flow → audit row in slack_releases
- Full test suite passing
- Windows manual verification checklist
- README Quick Start accuracy review
- Userscript auto-update headers

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
- ⏳ Windows: derivation, baseline assignment, drift computation
- ⏳ Slack: gate checks, fraction combination, release recording
- ⏳ E2E: full integration scenarios

## Known Limitations (v1)

- Tailer uses file walking + stat-based offset tracking (not ideal for huge projects)
- Windows interface enumeration not yet implemented
- Headroom gate is currently always true (placeholder)
- Dashboard UI not yet built (JSON endpoints ready)
- Userscript is a placeholder
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

- Implement Phase 6 discount calculation and dashboard endpoints
- Add Phase 7 network binding and graceful shutdown
- E2E integration tests for Phase 8
- Windows-specific testing and UI polish
- Documentation review and README update
