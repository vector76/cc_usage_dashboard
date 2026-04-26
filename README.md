# usage_dashboard

A self-hosted tool to track, visualize, and exploit your Claude Code subscription usage.

## What it does

Anthropic's Claude Code subscription enforces two rolling quotas:

- A **5-hour rolling window** that begins on first use after a reset.
- A **weekly quota** that caps total usage across all 5-hour windows.

The official UI shows a current point-in-time view. This project adds:

- **History** — burn-down charts for both the 5-hour and weekly windows.
- **Effective discount** — what your subscription actually costs vs. the API-equivalent
  dollar value of the tokens you ran through it.
- **Slack signal** — when you are under-utilizing your allocation, an HTTP endpoint
  surfaces the unused capacity that would otherwise expire at the window boundary, so a
  job queue can opportunistically run cheap-but-not-worth-real-money work for free.

## How it works

The trayapp is the server, the database (SQLite), and the dashboard, all in one Go binary
that lives in your system tray. It tails Claude Code's session JSONL files in
`~/.claude/projects/`, and also accepts HTTP POSTs from container-side Stop hooks and
from a browser userscript.

```
+---------------------------- Windows host -----------------------------+
|                                                                       |
|   [Browser] -- userscript (when claude.ai is open) --+                |
|                                                       \               |
|                                                        v              |
|   [Tray app: Go .exe]   <----- HTTP -----   [Containers: Linux CLI]   |
|     - HTTP server                                                     |
|     - SQLite DB                                                       |
|     - passive ingest: ~/.claude JSONL tail + container Stop hooks     |
|     - tray UI (status, slack indicator)                               |
|     - dashboard (served from same process at http://localhost:27812)  |
|                                                                       |
+-----------------------------------------------------------------------+
```

Containers reach the host via `host.docker.internal:27812`. See `docs/architecture.md`
for binding details and `docs/data-sources.md` for the ingest tiers.

## Getting started — Windows

The trayapp uses CGO for the system-tray integration, so you need a C toolchain on
`PATH` before building. Either of these works:

- [TDM-GCC](https://jmeubank.github.io/tdm-gcc/)
- [MSYS2](https://www.msys2.org/) with the `mingw-w64-x86_64-gcc` package

You also need [Go 1.26.2 or newer](https://go.dev/dl/).

### Option A — install with `go install`

```powershell
go install github.com/vector76/cc_usage_dashboard/cmd/trayapp@latest
```

The binary lands in `%USERPROFILE%\go\bin\trayapp.exe`. Run it directly:

```powershell
& "$env:USERPROFILE\go\bin\trayapp.exe"
```

### Option B — clone and build from source

Useful if you want to read or modify the code. No `make` required.

```powershell
git clone https://github.com/vector76/cc_usage_dashboard.git
cd cc_usage_dashboard
go build -ldflags="-H=windowsgui" -o trayapp.exe .\cmd\trayapp
.\trayapp.exe
```

The `-H=windowsgui` flag suppresses the console window so the app runs purely in the tray.

### Autostart on logon

Once you have a `trayapp.exe` built, the included PowerShell script registers a per-user
Task Scheduler entry that launches it at logon and bootstraps a default `prices.yaml` in
`%APPDATA%\usage_dashboard\`:

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

If `trayapp.exe` is not next to `install.ps1` (e.g. you used `go install` and it is in
`%USERPROFILE%\go\bin\`), pass its location:

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1 -ExePath "$env:USERPROFILE\go\bin\trayapp.exe"
```

### Confirming it works

The dashboard is served from the same process at <http://localhost:27812>. Open it in a
browser to confirm the trayapp is running and to see live usage.

## Container CLI (optional)

If you run Claude Code inside dev containers that don't bind-mount `~/.claude`, install
the CLI in the container so its Stop hook POSTs per-message usage to the host:

```bash
go install github.com/vector76/cc_usage_dashboard/cmd/cli@latest

# Wire it into ~/.claude/settings.json:
#   "Stop": [{ "hooks": [{ "type": "command", "command": "cli log --from-hook || true" }] }]
```

For ad-hoc tests:

```bash
cli log --input-tokens 1234 --output-tokens 567 --cost-usd 0.0123
```

## Building on Linux

The `Makefile` is the convenience entry point on Linux:

```bash
make build-trayapp   # headless server-mode binary
make build-cli       # container CLI
make test            # full Go test suite
```

The trayapp's tray UI is Windows-only; on Linux it builds as a headless server.

## Userscript installation

The userscript posts the dashboard's reported quota numbers to the trayapp so
Tier 1 (passive observation) has an authoritative anchor. It lives at
[`userscript/claude-usage-snapshot.user.js`](userscript/claude-usage-snapshot.user.js).

1. Install [Tampermonkey](https://www.tampermonkey.net/) or
   [Violentmonkey](https://violentmonkey.github.io/) in your browser.
2. Open `userscript/claude-usage-snapshot.user.js` and let the manager
   install it (drag the file in, or open the GitHub raw URL).
3. Grant `GM.xmlHttpRequest` and `@connect localhost` / `@connect 127.0.0.1`
   when prompted.
4. Make sure the trayapp is running (default port `27812`).
5. Open `https://claude.ai/` in any tab — the script posts one snapshot per
   minute once the quota DOM nodes are detected.

See `userscript/README.md` for troubleshooting and `docs/userscript.md` for
the snapshot payload schema and the rationale (mixed content, CORS, Private
Network Access).

## Documentation

Design and architecture docs live in [`docs/`](docs/) — start with `docs/overview.md`
and `docs/architecture.md`.
