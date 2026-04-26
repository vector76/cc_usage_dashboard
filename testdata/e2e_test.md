# End-to-End Test Scenarios

These scenarios exercise the major code paths and should be validated before shipping v1.

## Scenario 1: CLI Mode A posts events

```bash
# Start the server
./trayapp &
SERVER_PID=$!
sleep 1

# Post events using CLI Mode A
./clusage-cli log --input-tokens 100 --output-tokens 50 --session-id s1 --message-id m1
./clusage-cli log --input-tokens 200 --output-tokens 100 --session-id s1 --message-id m2
./clusage-cli log --input-tokens 150 --output-tokens 75 --session-id s2 --message-id m1

# Verify /discount works
./clusage-cli discount --period 24h

# Verify /slack works
./clusage-cli slack --format json

# Cleanup
kill $SERVER_PID
```

**Expected:**
- All CLI posts succeed (exit 0)
- /discount returns event counts and costs
- /slack returns window metrics and release_recommended

## Scenario 2: Duplicate detection

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
- Second POST still succeeds (exit code 4 or 5 from UNIQUE constraint)
- Database contains exactly 1 row for (s1, m1)

## Scenario 3: Snapshot and window derivation

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
    "five_hour_remaining": 80.0,
    "five_hour_total": 100.0,
    "five_hour_window_ends": "2026-04-26T15:00:00Z",
    "weekly_remaining": 1500.0,
    "weekly_total": 2000.0,
    "weekly_window_ends": "2026-04-28T00:00:00Z"
  }'

# Post some events
./clusage-cli log --input-tokens 100 --output-tokens 50
./clusage-cli log --input-tokens 200 --output-tokens 100

# Query windows table
sqlite3 usage.db "SELECT kind, strftime('%Y-%m-%d %H:%M:%S', started_at) FROM windows;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- Snapshot stored successfully
- Windows created for both 5-hour and weekly
- Windows have correct baseline_total from snapshot

## Scenario 4: Cost resolution

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
- First event: cost_source='reported', cost_usd_equivalent=0.05
- Second event: cost_source='computed', cost_usd_equivalent~0.018
- Third event: cost_source=NULL, cost_usd_equivalent=NULL

## Scenario 5: Slack signal

```bash
# Start server
./trayapp &
SERVER_PID=$!
sleep 1

# Create a window and snapshot
curl -X POST http://localhost:27812/snapshot \
  -H "Content-Type: application/json" \
  -d '{
    "observed_at": "2026-04-26T10:00:00Z",
    "source": "userscript",
    "five_hour_remaining": 50.0,
    "five_hour_total": 100.0,
    "five_hour_window_ends": "2026-04-26T15:00:00Z"
  }'

# Post some consumption
./clusage-cli log --input-tokens 100 --output-tokens 50 --cost-usd 0.01

# Get slack signal
./clusage-cli slack --format release-bool
./clusage-cli slack --format fraction

# Record a release
curl -X POST http://localhost:27812/slack/release \
  -H "Content-Type: application/json" \
  -d '{
    "released_at": "2026-04-26T11:00:00Z",
    "job_tag": "batch-job-1",
    "estimated_cost": 0.02,
    "slack_at_release": 0.49
  }'

# Verify release recorded
sqlite3 usage.db "SELECT job_tag, estimated_cost FROM slack_releases;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- Slack calculations succeed
- release-bool returns true or false
- fraction returns decimal value
- Release is recorded in database

## Scenario 6: Parse error recording

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
sqlite3 usage.db "SELECT source, reason FROM parse_errors LIMIT 1;"

# Cleanup
kill $SERVER_PID
```

**Expected:**
- Parse error stored successfully
- Query returns source='tailer' and reason='malformed JSON line'

## Manual Verification (Windows host only)

The following items require Windows:
- Tray icon displays and responds to clicks
- Tray menu items (Dashboard, Status, Pause, About, Quit) work
- Color state changes (green/yellow/red) reflect slack state
- Tooltip shows current burn rate and slack fraction
- Task Scheduler autostart works
- Graceful shutdown on logoff/shutdown

See `CLAUDE.md` Phase 6 for Windows checklist.
