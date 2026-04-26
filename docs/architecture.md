# Architecture

## Deployment topology

Everything runs on a single Windows host. There is no online backend, no managed database,
and no cross-machine state.

```
+------------------------------- Windows host -------------------------------+
|                                                                            |
|  Browser (logged in to claude.ai)                                          |
|     |                                                                      |
|     | userscript POST                                                      |
|     v                                                                      |
|  +----------------------------- trayapp.exe ------------------------------+|
|  |                                                                        ||
|  |  HTTP server (binds 127.0.0.1 + detected Docker/WSL adapters)          ||
|  |    POST /log              <- per-invocation token usage                ||
|  |    POST /snapshot         <- authoritative quota numbers               ||
|  |    POST /parse_error      <- userscript / tailer parse failures        ||
|  |    POST /slack/release    <- queue reports a released job              ||
|  |    GET  /slack            <- current slack signal (for external queue) ||
|  |    GET  /discount         <- effective-discount summary                ||
|  |    GET  /healthz          <- liveness                                 ||
|  |    GET  /metrics          <- counters                                  ||
|  |    GET  /dashboard        <- HTML/JS UI                                ||
|  |                                                                        ||
|  |  SQLite DB (single file, WAL mode)                                     ||
|  |                                                                        ||
|  |  Session log tailer  -> reads ~/.claude/projects/**/*.jsonl            ||
|  |                                                                        ||
|  |  Tray UI  -> shows burn %, slack indicator, opens dashboard            ||
|  +------------------------------------------------------------------------+|
|         ^                                                                  |
|         |  HTTP POST via host.docker.internal:PORT                         |
|         |                                                                  |
|  +-----------------------+   +-----------------------+                     |
|  | Linux container A     |   | Linux container B     |   ...               |
|  | (~/.claude shared)    |   | (~/.claude unshared)  |                     |
|  | host tailer sees JSONL|   | Stop hook -> CLI POST |                     |
|  +-----------------------+   +-----------------------+                     |
|                                                                            |
+----------------------------------------------------------------------------+
```

## Components

### Tray app (`cmd/trayapp`, Windows)

A single Go executable. Responsibilities:

- HTTP server on a configurable port (default suggested: `27812`).
- Owns the SQLite DB file (one writer, no contention).
- Tails session JSONL files in `~/.claude/projects/` and ingests them into the DB.
- Renders the local dashboard from the same process (static HTML+JS, served over HTTP).
- Tray icon with status tooltip and menu items: "Open dashboard", "Pause slack signal",
  "Quit", "About". (See `docs/tray-app.md` for the rationale on pause.)
- Autostart via Task Scheduler "at logon" or `shell:startup` shortcut.

Why a single binary: simplifies install, eliminates IPC, keeps the SQLite writer
single-threaded by construction.

### Container CLI (`cmd/cli`, Linux)

A small Go binary. Subcommands:

- `log` — POST `/log` with token counts and dollar-equivalent cost.
- `slack` — GET `/slack` and print the signal (for queue scripts).
- `ping` — health check.

Defaults to `host.docker.internal:27812` but reads `CLUSAGE_HOST` and `CLUSAGE_PORT`.

For environments where adding a binary is awkward, the same endpoints are reachable with
plain `curl` — the CLI is convenience, not requirement.

### Userscript (`userscript/`)

Runs in Tampermonkey/Violentmonkey on `claude.ai/*`. Reads visible quota numbers from the
DOM and POSTs `/snapshot`. Fires on page load and on a debounced interval while the page
remains open. Provides free recalibration whenever the user happens to visit the dashboard.

### Session log tailer (in-process, part of trayapp)

The session JSONL files Claude Code writes contain per-message token usage. The tailer:

- Watches `~/.claude/projects/` for new and modified files (fsnotify).
- Parses appended lines incrementally, extracts `usage` blocks.
- Inserts events into the DB, deduplicating by message ID.
- Persists per-file read offsets so restarts resume cleanly.

The tailer is one of two equal Tier-1 (passive) paths: it covers host sessions and
containers with bind-mounted `~/.claude`. Containers without a shared `~/.claude`
report via the CLI Stop hook (`POST /log`) instead. Both paths land in `usage_events`
and dedup against the same key. See `docs/data-sources.md` for the full taxonomy.

## Data flow

### Per-turn logging (the common path)

1. Claude Code completes a turn (one or more tool calls + an assistant response).
2. Whichever Tier-1 path is configured for that environment fires:
   - **Shared `~/.claude` (host or bind-mounted container):** the host-side tailer
     reads the new transcript line(s) and POSTs internally. No external action.
   - **Unshared container:** the Stop hook runs `clusage-cli log --from-hook`, which
     reads the transcript referenced in the hook payload and POSTs new events.
3. The trayapp `/log` handler validates and inserts into `usage_events`.
4. The slack indicator and burn-down derivations re-read on next request; no push.

When both paths see the same session (e.g. transitional configurations), the DB
deduplicates by `(session_id, message_id)`. But for unshared containers there is no
host-side tailer fallback — the Stop hook is the only data path, and a delivery
failure means that turn is lost. See the failure-modes table below.

### Snapshot recalibration

1. User opens claude.ai dashboard in the host browser.
2. Userscript reads the visible quota numbers (5-hour remaining, weekly remaining).
3. Userscript POSTs `/snapshot` with the full payload defined in
   `docs/userscript.md` (remaining + total + window-end + raw DOM text).
4. Trayapp inserts into `quota_snapshots` and uses it to set or correct the baseline for
   the current 5-hour and weekly windows.
5. If snapshot disagrees with derived state by more than the drift threshold, an alert
   bit is set and surfaced in the tray UI.

### Slack consumption

1. External queue process polls `GET /slack` periodically.
2. Trayapp computes slack per `docs/slack-indicator.md` (clamped uniform-burn expected
   consumption, applied independently to the 5-hour and weekly windows, combined via
   `min`).
3. Returns slack absolute and as a fraction of quota, plus gate states.
4. The queue decides whether to release a job. The trayapp does not run jobs.

## Network and security

- The trayapp must be reachable from containers via `host.docker.internal`. On Windows
  with Docker Desktop, that name resolves to a Hyper-V virtual ethernet adapter on the
  host (commonly in the `192.168.65.0/24` or WSL `172.x.x.0/20` ranges). Native Linux
  Docker uses a `172.17.0.1`-style bridge instead. The exact interface is environment-
  dependent, so the trayapp resolves it at startup rather than hardcoding.
- Preferred binding strategy:
  1. Always bind `127.0.0.1` for local-only callers (the userscript via the host
     browser, manual `curl` from the host).
  2. Enumerate the host's network interfaces and additionally bind any that match
     well-known Docker / WSL ranges (Docker Desktop's vEthernet adapter, WSL adapter,
     `172.16.0.0/12`, `192.168.65.0/24`). The user can override the list in config.
  3. If neither (1) nor (2) is reachable from a container, fall back to `0.0.0.0`
     **and** install a Windows Defender inbound rule restricting the port to
     `LocalSubnet` only. The fallback is surfaced in the tray UI so the user knows
     they're in the looser configuration.
- No authentication. The trust boundary is the host. Anything able to reach the bound
  interface is already running on this machine or its containers.
- If the user later wants remote access, route via Cloudflare tunnel + cloudflared
  Access policy. Do not add bespoke auth to the local server.

## Failure modes and recovery

| Failure                             | Behavior                                               |
|-------------------------------------|--------------------------------------------------------|
| Trayapp crashes mid-write           | SQLite WAL recovers on restart. Tailer offsets persist.|
| Container can't reach host          | Hook POST fails. If `~/.claude` is shared with the host,|
|                                     | the host tailer covers the gap. If not, that turn is    |
|                                     | lost; a future `/log` retry from the same hook would    |
|                                     | succeed because dedup is by message ID.                 |
| User never opens browser dashboard  | No snapshots. Derivation from passive logs continues.  |
| Quota baseline becomes stale        | Drift surfaced in tray; user opens browser to refresh. |
| Session JSONL format changes        | Tailer logs parse errors loudly; passive data drops    |
|                                     | until parser updated. Userscript snapshots still work. |

## Why this shape

- **Single host, single process, single file:** simplest possible deployment for one user.
- **Passive primary, active fallback:** avoids the central trap that polling perturbs the
  quota.
- **Clean HTTP boundary:** lets the CLI, userscript, and any future tunnel coexist.
- **No auth:** the trust boundary is already the host; layering auth adds risk without
  added safety in this topology.
