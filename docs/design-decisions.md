# Design Decisions

Topic-oriented record of decisions that aren't obvious from the code or
schemas. Each entry captures the decision, the rationale, and the
implications a future reader needs to know.

## Slack pause state is transient

**Decision.** The slack-signal pause flag (toggled from the tray menu's
"Pause slack signal" item, exposed as `paused` in `GET /slack`) is held
only in the in-memory state of `internal/slack.Calculator.paused`. It is
**not** persisted to SQLite, the config file, or any sidecar file. When
the trayapp restarts — for any reason: graceful shutdown, crash, logoff,
host reboot — the pause clears and the slack signal resumes from
`paused = false`.

**Why.** Pause is a session-bounded operator override, not a setting:

- The use case captured in `docs/tray-app.md` ("Use this when starting a
  heavy interactive session and you don't want background jobs racing
  you") is inherently short-lived — it lasts for one foreground work
  session. By the time the host reboots, the original reason has almost
  certainly elapsed.
- Persisting pause across restarts is a footgun. The most common way a
  user "forgets" they paused the signal is by walking away and coming
  back to a different boot. A pause flag silently surviving a Windows
  Update reboot would suppress releases for days with no visible signal
  beyond a tray-icon color the user has stopped looking at.
- Pause is intentionally distinct from configuration. Anything the user
  wants to make permanent (thresholds, headroom floor, quiet period)
  belongs in the YAML config, which is loaded at startup and is the
  single source of truth for durable behavior.

**Implications.**

- Documentation must avoid implying persistence. `docs/slack-indicator.md`
  and `docs/tray-app.md` describe pause as a tray toggle without a
  durability claim; new docs should follow that lead.
- The HTTP API has no pause/unpause endpoint. Pause is changed via the
  in-process `Calculator.SetPaused` from the tray UI, not over the wire.
  External callers that need a permanent suppression should adjust
  thresholds in config instead.
- Tests assert that pause persists *across requests within one process*
  (`internal/server/slack_test.go::TestSlackPausePersistsAcrossRequests`)
  — they do not assert and must not assert persistence across restarts.

## Windows-only code is isolated by build tag

**Decision.** All platform-dependent code in the trayapp lives behind
`//go:build` tags so the entire repository compiles on Linux without
Windows-specific dependencies. The trayapp UI is the canonical example:

- `cmd/trayapp/tray_windows.go` carries `//go:build windows` and is the
  **only** file that imports `fyne.io/systray`. It wires the documented
  v1 menu (Open dashboard, Status, Pause slack signal, About, Quit) and
  drives the real systray event loop.
- `cmd/trayapp/tray_stub.go` carries `//go:build !windows` and exposes
  exactly the same `StartTray(ctx, srv, paused)` signature. Its body
  logs that the tray is unavailable and blocks on `ctx.Done()` so the
  calling goroutine has a stable lifetime regardless of platform.
- `cmd/trayapp/main.go` calls `StartTray` from a goroutine on every
  platform, passing a small `interface{ Toggle() }` adapter around the
  shared `*slack.Calculator`. The Pause menu item flips the same
  in-memory pause flag the HTTP handlers read; nothing in `main.go`
  needs to know whether the tray is real or stubbed.

**Why.**

- `make test` and `make build-trayapp` must work on Linux CI without
  installing Windows toolchains. `fyne.io/systray` is platform-specific
  (cgo + Win32 on Windows, cgo + libappindicator/GTK on Linux); the
  Linux trayapp build is headless server-only, so the build tag keeps
  the dependency out of the Linux binary entirely.
- Keeping the public entry point (`StartTray`) identical on both sides
  means `main.go` has no `if runtime.GOOS == "windows"` branches and no
  conditional imports. Adding a future platform (e.g. macOS) is a matter
  of adding one more tagged file with the same signature.
- Cross-compilation stays simple: the documented Windows build
  (`make build-trayapp-windows`, see `Makefile`) flips `GOOS=windows`
  and the build tag does the rest. There is no separate
  `cmd/trayapp_windows/` tree to keep in sync.

**Implications.**

- New tray functionality (icon color states, status submenu population,
  about dialog, etc.) goes in `tray_windows.go`. The stub stays minimal;
  do not let it accumulate logic that only exists to satisfy tests.
- Anything the tray needs from the rest of the program must be passed
  in via the `StartTray` parameters (currently `srv` and `paused`),
  not pulled from package-level globals — globals would force the stub
  to duplicate state and break the "stub does nothing" invariant.
- `docs/development.md` ("Build tags for Windows-specific code") is the
  procedural reference; this document is the architectural rationale.
  Update both if the strategy changes.

## Clock skew on forwarded events is filtered, not corrected

**Context.** With an uplink configured (`docs/configuration.md`,
"Uplink"), a second machine's `usage_events` are POSTed to a host
trayapp's `/log`. Those events carry the *sender's* `occurred_at`, and a
VM's clock drifts — badly and discontinuously, because suspend/resume
moves it in jumps rather than at a steady rate.

This matters more than a timestamp usually would. `usage_events` is
otherwise decoupled from the quota machinery — `internal/slack` computes
purely from `windows.baseline_percent_used`, which snapshots populate,
and no dollar figure from an event reaches the slack signal. But there
is one coupling: `windows.Engine.findEventEvidenceForOpen` anchors a new
session window directly on `MAX(occurred_at)`, taking `startTime =
eventTime` and `endsAt = eventTime + 5h`. A future-dated event therefore
mints a window in the future, and stays the maximum until wall-clock time
catches up to it.

**Decision.** The receiver bounds an inbound `occurred_at` and rejects
anything outside it with 400 (`validateLogOccurredAt`). Inside the
bounds, the timestamp is stored exactly as sent. Nothing measures the
offset between the two machines, and nothing rewrites a timestamp.

The bounds are asymmetric — one hour ahead, a year behind:

- Ahead is the dangerous direction, per the window-minting behavior
  above. One hour matches `maxObservedFuture` on the snapshot path.
- Behind is mostly harmless: an old event cannot be `MAX(occurred_at)`
  unless the table is otherwise empty, and a window minted in the past
  closes immediately. The bound only needs to catch a clock that has
  collapsed entirely (an epoch reset, a VM restored from an old image),
  and it must stay well clear of legitimate backfills — the Stop hook
  re-walks whole transcripts and re-posts events that are genuinely
  weeks old.

**Why not correct the skew.** Measuring the offset is cheap — every HTTP
response already carries a `Date` header, and an explicit round-trip
against `/healthz` would bound the uncertainty too. Applying it is where
it stops being cheap:

- A static offset is the wrong model. Suspend/resume moves the clock in
  jumps, so an offset sampled at forward time is not the one that was
  true at ingest time, and a buffered backlog would be corrected with a
  number that belongs to a different era than the events it is applied
  to.
- Silent timestamp rewriting is out of character for this project.
  `cost_source` records whether a number was `reported`, `computed`, or
  a `ceiling` estimate rather than hiding the approximation;
  `quota_snapshots` uses tri-state NULLs rather than inferring a value
  it did not observe. A corrected `occurred_at` with no marker would be
  the first place the database asserts something it did not see.
- The failure is rare and diagnosable. A clock far enough off to matter
  produces rejected events and a visible warning; one close enough not
  to trip the bound produces an error smaller than the 5-hour window it
  would have to cross to change anything.

**Implications.**

- A 400 from this filter is *permanent* from the sender's point of view.
  `internal/uplink` therefore steps its cursor past a rejected event
  rather than retrying it, because holding the cursor there would wedge
  every later event behind one row that can never land. The rejection is
  logged on both sides: as a warning on the receiver, and as a warning
  naming the session and message on the sender.
- This means a badly-skewed sender loses data silently-ish — the events
  are dropped, not queued. That is the intended trade: the alternative
  is either a stalled uplink or fabricated timestamps.
- If skew detection is wanted later, the natural shape is a periodic
  `/healthz` round-trip on the sender that raises an alert (tray icon,
  feedback panel) without touching the data. The feedback buffer already
  tees `slog.Warn` into the dashboard panel, so the reporting half needs
  no new plumbing.

## Non-loopback callers need a token; loopback does not

**Context.** The HTTP server originally had no authentication. The trust
boundary was the host: it bound only loopback and auto-detected Docker/WSL
adapters, so anything that could connect was already on this machine or
its containers, and `docs/architecture.md` said outright not to add bespoke
auth. The uplink broke that assumption. A VM on a bridged adapter, or a
receiver bound to `0.0.0.0`, publishes an unauthenticated write endpoint
to the LAN, where a stray or hostile POST to `/log` skews both the totals
and the slack gate that releases real work.

**Decision.** A per-install bearer token, demanded from every peer that is
not loopback:

- Loopback is exempt, so the userscript, the dashboard, and host-side
  `curl` keep working with no setup, and the host keeps its old trust.
- Docker/WSL containers are *not* exempt, even though they were trusted
  before. Once `0.0.0.0` is an option, "came in on a Docker adapter" is not
  a boundary worth defending separately, and one rule — loopback or token —
  is easier to reason about than a list of trusted subnets. The cost is
  that every container's `CLUSAGE_TOKEN` must be set.
- The decision uses the TCP peer address, never a header such as
  `X-Forwarded-For`, which any caller can forge.
- Reads are gated as well as writes: `/slack` and `/consumption` disclose
  usage, and a gate on `/log` alone would leave the dashboard on the LAN.
- The token lives in its own file in the per-user data dir, not in
  `config.yaml`, which may sit in a checkout and is read only at start.
  Rotation from the tray swaps it in memory and on disk at once.
- A failure to load the token fails closed.

**Why a rotation must not drop data.** The uplink skips events the receiver
answers with a 4xx, because a 4xx usually means "this event will never be
accepted" (a skewed `occurred_at`). A `401` or `403` means the opposite —
"this sender is not allowed in" — and would reject every later event the
same way, so treating it as permanent would silently drain the whole
backlog after a rotation. Both hold the cursor instead.

**Why not TLS, HMAC request signing, or per-client tokens.** The threat is
accidental or casual writes from a local network, and a 256-bit bearer
token stops those. Signing would hide the token from a passive sniffer but
not the usage data beside it; hiding both needs TLS, which is the
Cloudflare tunnel's job for anything beyond a trusted network. Per-client
tokens would allow revoking one VM without touching the others, at the cost
of a token registry and a management UI; with one or two clients, rotating
the single token and re-pasting it is cheaper.
