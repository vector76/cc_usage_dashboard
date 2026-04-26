# usage_dashboard

A self-hosted tool to track, visualize, and exploit your Claude Code subscription usage.

## Why this exists

Anthropic's Claude Code subscription enforces two rolling quotas:

- A **5-hour rolling window** that begins on first use after a reset.
- A **weekly quota** that caps total usage across all 5-hour windows.

The official UI shows a current point-in-time view, but does not let you:

- See the **history** of usage (how fast did you burn through the last window?).
- Track the **effective discount** of the subscription (tokens billed at API-equivalent
  dollar amounts vs. what the subscription actually costs).
- Detect **slack** — when you are under-utilizing your allocation and have "free" capacity
  that will otherwise expire at the window boundary.
- Trigger **low-priority background jobs** opportunistically when slack is available.

This project records usage continuously, renders burn-down charts for both the 5-hour and
weekly windows, computes effective discount, and exposes a slack signal that a job queue
can consume to run cheap-but-not-worth-real-money work for free.

## High-level architecture

Single Windows host, no online backend. Docker containers on the same host POST per-invocation
token usage to the host server.

```
+---------------------------- Windows host -----------------------------+
|                                                                       |
|   [Browser] -- userscript (when claude.ai is open) --+                |
|                                                       \               |
|                                                        v              |
|   [Tray app: Go .exe]   <----- HTTP -----   [Containers: Linux CLI]   |
|     - HTTP server                                                     |
|     - SQLite DB                                                       |
|     - tail of ~/.claude session JSONL (primary data source)           |
|     - tray UI (status, slack indicator)                               |
|     - dashboard (served from same process at http://localhost:PORT)   |
|                                                                       |
+-----------------------------------------------------------------------+
```

Containers reach the host via `host.docker.internal:PORT`. The trayapp binds `127.0.0.1`
plus whichever local interface `host.docker.internal` resolves into (auto-detected at
startup, since this varies between Docker Desktop on Windows, WSL, and native Linux).
See `docs/architecture.md` for the precise binding strategy and the fallback rules.

## Data sources, in priority order

1. **Passive observation (primary).** Two paths, same tier:
   - *Host JSONL tailing* — the trayapp tails Claude Code's session JSONL files in
     `~/.claude/projects/`. Sees host sessions and any container that bind-mounts
     `~/.claude`.
   - *Container Stop hook* — a CLI Stop hook in containers without a shared
     `~/.claude` POSTs the same per-message usage to the host. Same dedup key, same
     "passive" property: never perturbs the quota.
2. **Userscript snapshots (anchor).** Tampermonkey/Violentmonkey on `claude.ai/*`
   posts the dashboard's reported quota numbers whenever the page is open. Used to
   set/correct the baseline that Tier 1 burns down from.
3. **Headless scrape (escalation only).** Playwright using a copy of the Chrome
   profile, only if Tiers 1+2 drift in practice. Not built unless needed.

`clusage` (the existing CLI) is **not** used for automated polling because it triggers a
new 5-hour window on cold start and would perturb what it measures. It remains useful for
manual on-demand reads.

## Components

| Component         | Language | Runs on        | Purpose                                |
|-------------------|----------|----------------|----------------------------------------|
| Tray app + server | Go       | Windows host   | DB, HTTP API, dashboard, tray UI, in-process JSONL tailer |
| Container CLI     | Go       | Linux (Docker) | POSTs token usage to host (Stop hook)  |
| Userscript        | JS       | Browser        | Posts dashboard snapshots to host      |

The tray app and CLI are built from the same Go module with different build targets.
The session log tailer is a goroutine inside the tray app, not a separate binary.

## Repository layout (planned)

```
.
├── README.md                     # this file
├── docs/                         # design docs
│   ├── overview.md
│   ├── architecture.md
│   ├── data-sources.md
│   ├── data-model.md
│   ├── slack-indicator.md
│   ├── discount-calculation.md
│   ├── roadmap.md
│   └── components/
│       ├── tray-app.md
│       ├── cli.md
│       └── userscript.md
├── cmd/
│   ├── trayapp/                  # Windows tray + server binary
│   └── cli/                      # Linux container CLI
├── internal/
│   ├── store/                    # SQLite schema, queries
│   ├── ingest/                   # session JSONL tailer, HTTP handlers
│   ├── slack/                    # slack-indicator math
│   └── dashboard/                # HTML/JS for the local dashboard
├── userscript/
│   └── claude-usage-snapshot.user.js
├── config/
│   └── prices.example.yaml       # model price table for cost computation
└── go.mod
```

## Quick start (intended end state)

```powershell
# On the Windows host: starts server + tray icon, autostarts on logon
.\trayapp.exe
```

```bash
# In a Linux dev container: install the Stop hook once, then activity is logged
# automatically. ~/.claude/settings.json:
#   "Stop": [{ "hooks": [{ "type": "command", "command": "clusage-cli log --from-hook || true" }] }]

# Or, for ad-hoc tests:
clusage-cli log --input-tokens 1234 --output-tokens 567 --cost-usd 0.0123
```

The dashboard is then visible at `http://localhost:PORT` on the host.

## Status

Pre-implementation. See `docs/roadmap.md` for the build order and `docs/overview.md`
for full design rationale.

## Why no online backend

The user has exactly one always-on host (Windows desktop) which is the only place with
a logged-in browser. Containers already need to reach this host. Adding a hosted DB
would mean auth, hosting cost, sync logic, and a second source of truth for no added
capability. If the deployment ever needs to span machines, the local HTTP API can be
exposed via a Cloudflare tunnel without schema changes.
