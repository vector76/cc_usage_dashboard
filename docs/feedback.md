# Operational feedback panel

The tray app normally runs in the background with no console, so warnings
emitted via `slog` are invisible to the user. The feedback subsystem surfaces
recent operational problems in the dashboard without cluttering the normal UI.

## Sources

Three signals are collected in `internal/feedback` and read by the server's
`GET /api/feedback` handler:

1. **Recent warnings** — a concurrency-safe ring buffer (`feedback.Buffer`,
   capacity `DefaultCapacity` = 200) of warn-and-above log records. It is wired
   as a `slog.Handler` (`feedback.Handler`) that *tees* warn+ records into the
   buffer while passing every record through to the underlying destination
   (console or rotated log file). Because the tee wraps whatever base handler
   the tray app configures, startup warnings such as "price table file not
   found" land in the buffer naturally.

2. **Unknown-model aggregate** — `feedback.UnknownModels` counts usage events
   whose model is missing from the price table, keyed by model name with
   `{count, first_seen, last_seen}` since process start. This is the actionable
   "add it to `prices.yaml`" signal. It is **aggregated, not logged per event**:
   one usage event per message means per-event logging would flood the buffer
   with exactly the model that is missing. Both `ResolveCost` call sites feed
   the same aggregate — the tailer ingest path (`internal/ingest/tailer.go`) and
   the HTTP `POST /log` handler (`internal/server/server.go`). An empty/absent
   model name is a different, already-handled case and is never counted here.

3. **Recent parse errors** — the most recent rows (default 50) from the
   existing `parse_errors` table (`store.RecentParseErrors`), which already
   records tailer/HTTP parse failures with 30-day retention.

## Wiring

The buffer and aggregate are process-wide singletons
(`feedback.DefaultBuffer()`, `feedback.DefaultUnknownModels()`). The tray app
installs the tee handler over its base logging handler at startup; the server
and tailer default to the same singletons so `GET /api/feedback` reads exactly
what the slog tee and both ingest paths write. Tests inject fresh instances
(`Server.SetFeedback`, `Tailer.unknown`) for isolation.

## API and UI

`GET /api/feedback` returns `{warnings, unknown_models, parse_errors, summary}`.
The summary carries `{warnings, unknown_models, unknown_model_events,
parse_errors}` so the dashboard can render a compact badge. The dashboard shows
a collapsible panel ("Feedback"), collapsed by default and visually quiet: the
summary line reads "(none)" when all counts are zero, or e.g.
"(3 warnings · 1 unknown model)" otherwise. When expanded it lists the
unknown-model aggregates first, then recent warnings, then recent parse errors.
The badge refreshes on the page's existing state-refresh cadence so it stays
live even while collapsed.
