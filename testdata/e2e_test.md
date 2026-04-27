# End-to-End Test Scenarios

The canonical runner for these scenarios is the Go test file
[`internal/integration/e2e_test.go`](../internal/integration/e2e_test.go).
Each scenario below maps to one `TestE2E_*` function that exercises the
server in-process — no separate binaries, no shell, no listening port — so
the suite is Linux-runnable and runs as part of `make test`.

```bash
go test ./internal/integration/...
```

The shell snippets below describe the same flows as they would look against
a running `trayapp`/`clusage-cli` install; treat them as documentation, not
as the runnable spec.

## Scenario 1: CLI Mode A posts events

Go test: `TestE2E_CLIModeA_ConsumptionAndSlack`

```bash
# Start the server
./trayapp &
SERVER_PID=$!
sleep 1

# Post events using CLI Mode A
./clusage-cli log --input-tokens 100 --output-tokens 50 --session-id s1 --message-id m1
./clusage-cli log --input-tokens 200 --output-tokens 100 --session-id s1 --message-id m2
./clusage-cli log --input-tokens 150 --output-tokens 75 --session-id s2 --message-id m1

# Verify /consumption works
./clusage-cli consumption --period 24h

# Verify /slack works
./clusage-cli slack --format json

# Cleanup
kill $SERVER_PID
```

**Expected:**
- All CLI posts succeed (exit 0)
- /consumption returns the documented fields: `consumed_usd_equivalent`,
  `consumed_session_pct`, `consumed_weekly_pct`, `events_total`,
  `events_with_reported_cost`, `events_with_computed_cost`,
  `events_without_cost`
- /slack returns documented top-level keys (`now`, `session`, `weekly`,
  `slack_combined_fraction`, `paused`, `release_recommended`, `gates`)
  with the documented gate keys (`session_headroom`, `weekly_headroom`,
  `baseline_freshness`, `not_paused`)

## Scenario 2: Duplicate detection

Go test: `TestE2E_DuplicateDetection`

```bash
# Start server
./trayapp &
SERVER_PID=$!
sleep 1

# Post the same event twice
./clusage-cli log --input-tokens 100 --output-tokens 50 --session-id s1 --message-id m1
./clusage-cli log --input-tokens 100 --output-tokens 50 --session-id s1 --message-id m1

# Query database directly
sqlite3 usage.db "SELECT COUNT(*) FROM usage_events WHERE session_id='s1' AND message_id='m1';"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- First POST succeeds (200)
- Second POST returns 500 from the UNIQUE constraint
- Database contains exactly 1 row for (s1, m1)

## Scenario 3: Snapshot and window derivation

Go test: `TestE2E_SnapshotAndWindowDerivation`

```bash
# Start server
./trayapp &
SERVER_PID=$!
sleep 1

# Post a snapshot
curl -X POST http://localhost:27812/snapshot \
  -H "Content-Type: application/json" \
  -d '{
    "observed_at": "2026-04-26T10:00:00Z",
    "source": "userscript",
    "session_used": 6.0,
    "session_window_ends": "2026-04-26T15:00:00Z",
    "weekly_used": 23.0,
    "weekly_window_ends": "2026-04-28T00:00:00Z"
  }'

# Query windows table
sqlite3 usage.db "SELECT kind, baseline_percent_used FROM windows;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- Snapshot stored successfully
- Windows created for both `session` and `weekly`
- session window has `baseline_percent_used = session_used`
- Weekly window has `baseline_percent_used = weekly_used` (set by the in-window
  baseline correction pass)

## Scenario 4: Cost resolution

Go test: `TestE2E_CostResolution`

```bash
# Start server with price table configured
PRICING_TABLE_PATH=config/prices.example.yaml ./trayapp &
SERVER_PID=$!
sleep 1

# Post event with reported cost
./clusage-cli log --input-tokens 1000 --output-tokens 500 --cost-usd 0.05 --model claude-3-5-sonnet-20241022

# Post event with model (cost will be computed)
./clusage-cli log --input-tokens 1000 --output-tokens 500 --model claude-3-5-sonnet-20241022

# Post event without cost info (cost will be null)
./clusage-cli log --input-tokens 1000 --output-tokens 500

# Query costs
sqlite3 usage.db "SELECT cost_source, cost_usd_equivalent FROM usage_events ORDER BY id;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- First event: `cost_source='reported'`, `cost_usd_equivalent=0.05`
- Second event: `cost_source='computed'`, `cost_usd_equivalent=0.0105`
- Third event: `cost_usd_equivalent` is NULL

## Scenario 5: Slack release flow

Go test: `TestE2E_SlackReleaseFlow`

```bash
# Start server
./trayapp &
SERVER_PID=$!
sleep 1

# Establish an active session window (snapshot, or seeded directly in tests)
curl -X POST http://localhost:27812/snapshot \
  -H "Content-Type: application/json" \
  -d '{
    "observed_at": "2026-04-26T10:00:00Z",
    "source": "userscript",
    "session_used": 12.0,
    "session_window_ends": "2026-04-26T15:00:00Z"
  }'

# Post some consumption
./clusage-cli log --input-tokens 100 --output-tokens 50 --cost-usd 0.01

# Get slack signal
./clusage-cli slack --format json

# Record a release
curl -X POST http://localhost:27812/slack/release \
  -H "Content-Type: application/json" \
  -d '{
    "released_at": "2026-04-26T11:00:00Z",
    "job_tag": "batch-job-1",
    "estimated_cost": 0.02,
    "slack_at_release": 0.49,
    "window_kind": "session"
  }'

# Verify release recorded
sqlite3 usage.db "SELECT job_tag, estimated_cost, window_id FROM slack_releases;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- /slack succeeds and exposes `release_recommended`
- /slack/release returns 200
- `slack_releases` row contains `job_tag='batch-job-1'`,
  `estimated_cost=0.02`, `slack_at_release=0.49`, and `window_id` referring
  to the active session window

## Scenario 6: Parse error recording

Go test: `TestE2E_ParseErrorRoundTrip`

```bash
# Start server
./trayapp &
SERVER_PID=$!
sleep 1

# Post a malformed event via curl
curl -X POST http://localhost:27812/parse_error \
  -H "Content-Type: application/json" \
  -d '{
    "source": "tailer",
    "reason": "malformed JSON line",
    "payload": "{bad: json}"
  }'

# Query parse errors
sqlite3 usage.db "SELECT source, reason, payload FROM parse_errors LIMIT 1;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- Parse error stored successfully
- Query returns `source='tailer'`, `reason='malformed JSON line'`, and the
  original payload verbatim

## Manual Verification (Windows host only)

The following items require Windows and are not covered by the Go suite:

- Tray icon displays and responds to clicks
- Tray menu items (Dashboard, Status, Pause, About, Quit) work
- Color state changes (green/yellow/red) reflect slack state
- Tooltip shows current burn rate and slack fraction
- Task Scheduler autostart works
- Graceful shutdown on logoff/shutdown

See `CLAUDE.md` Phase 6 for the Windows checklist.
