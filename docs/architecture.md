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
|  |  HTTP server (binds 127.0.0.1 + detected Docker/WSL adapters;          ||
|  |               non-loopback callers need the access token)              ||
|  |    POST /log                  <- per-invocation token usage            ||
|  |    POST /snapshot             <- authoritative quota numbers           ||
|  |    POST /parse_error          <- userscript / tailer parse failures    ||
|  |    POST /slack/release        <- queue reports a released job          ||
|  |    GET  /slack                <- current slack signal (external queue) ||
|  |    GET  /consumption          <- USD + percent-of-quota over a period  ||
|  |    GET  /api/usage/breakdown  <- per-model tokens + USD over a range   ||
|  |    GET  /healthz              <- liveness                              ||
|  |    GET  /metrics              <- counters (Prometheus text format)     ||
|  |    GET  /                     <- dashboard HTML (alias of /dashboard)  ||
|  |    GET  /dashboard            <- dashboard HTML                        ||
|  |    GET  /report               <- per-model usage report HTML           ||
|  |    GET  /api/dashboard/state  <- JSON state powering the dashboard     ||
|  |    GET  /api/feedback         <- recent warnings, unknown models,      ||
|  |                                  parse errors (dashboard feedback panel)||
|  |    GET  /api/oauth/status     <- OAuth usage poller health             ||
|  |    GET  /favicon.{png,ico}    <- favicon                               ||
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

### Optional: a second machine forwarding in

The topology above describes one host. A second trayapp — typically in a VM
running Claude Code against the *same* Anthropic account — can be configured
with an uplink (`uplink.url`, see `docs/configuration.md`) so its
`usage_events` are POSTed to the host's `/log`:

```
  +------------- VM -------------+              +--------- Windows host ---------+
  |  trayapp (uplink.url set)    |  POST /log   |  trayapp (uplink.url empty)    |
  |   tailer -> own SQLite  -----+------------->|   /log -> shared SQLite        |
  |   own dashboard (partial)    |              |   dashboard (combined)         |
  +------------------------------+              +--------------------------------+
```

This exists because the quota is shared server-side but the recording is
not: Anthropic bills both machines against one account, so the host's
scraped percentages already include the VM's spend while its event stream
does not. Without forwarding, the host's tokens and its percentages
disagree — and the gap grows with VM usage.

Only events move. Snapshots stay put (a VM has no browser on claude.ai),
costs are recomputed by the receiver from its own price table, and the
receiver's windows engine derives from the combined stream as if the events
had been local — which is correct precisely because the quota is shared.
Delivery is at-least-once over a durable per-peer cursor; re-delivery
collapses against `UNIQUE(session_id, message_id)`.

The VM's own dashboard necessarily shows only its own slice. If anything on
the VM consumes `GET /slack` to release work, point it at the host's
endpoint — a sender's local slack signal does not know about the quota the
host has already spent.

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

### Container CLI (`cmd/clusage-cli`, Linux)

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

1. User opens `https://claude.ai/settings/usage` in the host browser.
2. Userscript reads the two structured usage bars Anthropic exposes
   ("Current session" and "All models"; `role="progressbar"` historically,
   `role="meter"` since July 2026 — see `docs/userscript.md`). Each has an
   `aria-valuenow` attribute (0–100) and is attributed to its section by the
   nearest preceding heading. Other rows (per-model sub-rows, routine runs,
   extra usage) are intentionally ignored.
3. Userscript POSTs `/snapshot` with `session_used` and/or `weekly_used`
   percentages — see `docs/userscript.md`.
4. Trayapp inserts into `quota_snapshots` and uses the value to set or correct
   the `baseline_percent_used` for the current session and weekly windows.

### Slack consumption

1. External queue process polls `GET /slack` periodically.
2. Trayapp computes slack per `docs/slack-indicator.md` (clamped uniform-burn expected
   consumption, applied independently to the session and weekly windows).
3. Returns per-window slack absolute and as a fraction of quota, plus gate states
   (the session and weekly headroom gates are independent — see slack-indicator).
4. The queue decides whether to release a job. The trayapp does not run jobs.

## Network and security

- The trayapp must be reachable from containers via `host.docker.internal`. On Windows
  with Docker Desktop, that name resolves to a Hyper-V virtual ethernet adapter on the
  host (commonly in the `192.168.65.0/24` or WSL `172.x.x.0/20` ranges). Native Linux
  Docker uses a `172.17.0.1`-style bridge instead. The exact interface is environment-
  dependent, so the trayapp resolves it at startup rather than hardcoding.
- Binding strategy:
  - **Default: `0.0.0.0`, every interface.** A VM, a container, or another
    machine can connect with no config change on the host; the token gate below
    is what makes that safe. The cost is that the listener is on every network
    the host joins, including ones it has no business on (the hotel Wi-Fi a
    laptop joins next week) — there the token is the only thing in the way.
    Windows Firewall prompts once for the new listener.
  - **Narrowed: an explicit `http.bind` list of specific addresses** replaces
    the default. Then:
    1. `127.0.0.1` is always bound, for local-only callers (the userscript via
       the host browser, manual `curl` from the host).
    2. The host's interfaces are enumerated and any matching well-known Docker /
       WSL ranges (Docker Desktop's vEthernet adapter, WSL adapter,
       `172.16.0.0/12`, `192.168.65.0/24`) are bound too.
    3. The listed addresses are appended.
  - An unspecified address in the list (`0.0.0.0` or `::`) stands alone — it
    already covers everything above, and binding it beside a specific address
    on the same port fails.
- **Access token for every non-loopback caller.** A request whose peer address
  is loopback (`127.0.0.0/8`, `::1`) is exempt. Every other request — a VM's
  uplink, a LAN host, and Docker/WSL containers alike — must carry
  `Authorization: Bearer <token>`, or it is answered `401` before it reaches
  any handler. Reads are gated too: `/slack`, `/consumption`, and the dashboard
  expose usage data, not just `/log`.
  - The trust boundary is the local machine. Anything that can open a loopback
    connection is already running here, which is why the userscript, the
    dashboard, and host-side `curl` need no setup.
  - The decision is made from the TCP peer address, never from a header. A
    proxy that relays container traffic onto loopback (some Docker Desktop and
    WSL networking modes do) therefore makes those containers exempt; that is
    the same trust as the host they run on.
  - The token is 32 random bytes, base64url-encoded, compared in constant time,
    and never logged. It lives in `auth_token` in the per-user data dir, not in
    `config.yaml`: a checkout's `config.yaml` takes precedence and is one
    `git add` from being committed, and the token must rotate at runtime while
    the config is read once. It is generated on first start.
  - The tray menu copies it ("Copy access token") and rotates it ("Rotate
    access token…"). Rotation takes effect on the next request with no restart;
    every client holding the old token is refused until updated.
  - If the token file cannot be read or created, the gate fails closed:
    loopback keeps working and every other caller is refused.
  - Rejections are counted in `auth_rejected_total` on `/metrics` and logged at
    most once a minute, since a client with a stale token retries constantly.
  - This is plain HTTP. The token stops accidental and casual writes from the
    network; it does not stop someone who can read traffic on the wire. Remote
    access beyond a trusted local network still belongs behind TLS (see the
    Cloudflare tunnel note below).
- **Uplink senders.** A second machine forwarding to `/log` needs only
  `uplink.token` set to the receiver's token; the receiver's default bind already
  reaches it.
  A `401` or `403` holds the sender's cursor rather than skipping the event —
  both mean "this sender is not allowed in", which no later event would fix — so
  a rotation costs latency, not data. A host-only adapter is still the better
  topology than a bridged one: the token keeps strangers out, but a bridged
  adapter still puts the unencrypted traffic on a shared LAN.
- Inbound `occurred_at` on `/log` is bounded (one hour ahead, a year behind)
  and out-of-range events are rejected with 400. Until the uplink existed every
  writer was local and shared the host's clock; a forwarding sender makes a
  foreign clock a real input, and the windows engine anchors new session windows
  on the newest event's timestamp. See `docs/design-decisions.md`, "Clock skew
  on forwarded events is filtered, not corrected".
- Browser-mounted CSRF defence: every POST handler requires
  `Content-Type: application/json` and caps the body at a per-endpoint limit. The
  Content-Type check rejects "simple" cross-origin form posts a malicious site could
  mount against `http://localhost:27812/...` from the user's browser; the body cap
  prevents a hostile caller from exhausting RAM or filling the DB with junk.
- DNS-rebinding defence: a loopback request's `Host` header must match the
  allow-list (`localhost`, `127.0.0.1`, `host.docker.internal`, plus each bound
  interface IP — or, under a `0.0.0.0` bind, every interface's IP). Without this,
  a malicious site could rebind its hostname to 127.0.0.1 and ride the token
  gate's loopback exemption. Token-bearing (non-loopback) requests skip the
  check: a rebound page cannot present a token it does not know, so the check
  would add nothing there except refusing a VM that names the host by its
  machine name. A loopback request is checked even if it carries a token, so the
  rebinding defence never hinges on a header.
- If the user later wants remote access beyond a trusted local network, route via
  Cloudflare tunnel + cloudflared Access policy. The token gate is sized for a
  VM or container next to the host, not for the internet.

## Failure modes and recovery

| Failure                             | Behavior                                               |
|-------------------------------------|--------------------------------------------------------|
| Trayapp crashes mid-write           | SQLite WAL recovers on restart. Tailer offsets persist.|
| Container can't reach host          | Hook POST fails. If `~/.claude` is shared with the host,|
|                                     | the host tailer covers the gap. If not, that turn is    |
|                                     | lost; a future `/log` retry from the same hook would    |
|                                     | succeed because dedup is by message ID.                 |
| User never opens browser dashboard  | No snapshots. Derivation from passive logs continues.  |
| Quota baseline becomes stale        | Snapshot age surfaced in dashboard status; user opens browser to refresh. |
| Session JSONL format changes        | Tailer logs parse errors loudly; passive data drops    |
|                                     | until parser updated. Userscript snapshots still work. |

## Why this shape

- **Single host, single process, single file:** simplest possible deployment for one user.
- **Passive primary, active fallback:** avoids the central trap that polling perturbs the
  quota.
- **Clean HTTP boundary:** lets the CLI, userscript, and any future tunnel coexist.
- **No auth:** the trust boundary is already the host; layering auth adds risk without
  added safety in this topology.
