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
