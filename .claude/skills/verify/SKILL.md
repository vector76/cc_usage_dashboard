---
name: verify
description: Run the trayapp against a snapshot of the live usage.db to verify changes end-to-end on port 27899.
---

# Verifying trayapp changes against a live-DB snapshot

The production trayapp runs on port 27812. Its database lives in the per-user data dir, `%LOCALAPPDATA%\usage_dashboard\usage.db` (`config.UserDataDir`), unless `database.path` in the live `config.yaml` says otherwise; `prices.yaml` is at the repo root. Verify changes on a **copy** of that DB on port **27899** — never against the live file.

## Recipe

1. Check the live app is stopped or at least don't write to its DB:
   `(Test-NetConnection 127.0.0.1 -Port 27812).TcpTestSucceeded` — if the app is running, a `usage.db-wal` sidecar exists; copy it (and `-shm`) along with `usage.db`.
2. Copy `usage.db*` from `%LOCALAPPDATA%\usage_dashboard\` (not the repo root or a worktree) to a scratch dir. It is several hundred MB with the WAL; delete the copy when done.
3. Write a scratch `config.yaml` (all paths absolute, double-backslash escaped):
   ```yaml
   database:  { path: "<scratch>\\usage.db" }
   http:      { port: 27899 }
   # empty dir + no cowork root isolates both tailers
   claude:    { projects_dir: "<scratch>\\projects_empty", cowork_sessions_dir: "" }
   pricing:   { table_path: "<repo>\\prices.yaml" }
   ```
   To exercise OAuth polling (off in the live config), add `oauth_usage: { enabled: true, poll_interval_seconds: 600 }`. For the stale-credential path without a network call, also set `credentials_path` to a scratch file containing `{"claudeAiOauth":{"accessToken":"fake","expiresAt":<ms in the past>}}`. Check the result at `/api/oauth/status`.
4. Build and run headless (works fine; a tray icon appears and the process is killable):
   `go build -o <scratch>\trayapp.exe ./cmd/trayapp`
   `Start-Process <scratch>\trayapp.exe -ArgumentList "-config","<scratch>\config.yaml" -RedirectStandardError <scratch>\run.log -WindowStyle Hidden -PassThru`
5. Drive the surfaces: `http://127.0.0.1:27899/` (dashboard), `/api/feedback`, `/consumption?period=24h`, `/slack`, `/healthz`, `/api/oauth/status`. Startup log lands in `run.log` (stderr, text format).
6. `Stop-Process` when done.

## Inspecting DB state

No sqlite3 CLI on this machine. Build a one-off Go query tool: copy the repo `go.mod`/`go.sum` into a scratch module dir, rename the module line (write WITHOUT a UTF-8 BOM — PS 5.1 `Set-Content -Encoding utf8` adds one and breaks `go build`), import `modernc.org/sqlite`, then `go -C <dir> build` (avoids `cd`).

## Gotchas

- `go test ./...` passes clean on Windows since 2026-07-07 — zero failures is the bar.
- The feedback panel (`/api/feedback`) only shows warn+ log records; Info-level lines never appear there.
- Live DB models use dated ids (e.g. `claude-haiku-4-5-20251001`). `ingest.ResolveCost` falls back from a dated id to its undated family entry (`claude-haiku-4-5`); an exact dated key in `prices.yaml` wins over the fallback.
- The production app is often running while you verify — check port 27812 and copy the `-wal`/`-shm` sidecars along with `usage.db`.
- `POST /log` events without session_id/message_id all map to `("","")` on the unique index, so the second such event is silently deduplicated — give probe events distinct ids.
