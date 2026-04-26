# Roadmap

Phased build order. Each phase produces something runnable and useful on its own; later
phases enrich earlier ones rather than depending on a big-bang integration.

## Testing posture

Per `AGENTS.md` / `CLAUDE.md`, this project follows TDD. Every phase below is "done"
only when:

- Unit tests cover the logic introduced in that phase, written before or alongside the
  implementation.
- Integration tests exercise any new HTTP endpoint end-to-end (handler → DB → response).
- The full suite (`go test ./...`) passes locally and in CI.
- New schema migrations are tested both forward and on a populated DB.

Per-phase notes below call out the specific things to cover; assume the general
expectation above also applies even when not repeated.

## Phase 0 — Project skeleton

- `go.mod`, basic directory layout per `README.md`.
- A no-op `cmd/trayapp` that opens a tray icon and exits cleanly.
- A no-op `cmd/cli` that prints version.
- CI: `go vet`, `go test`, cross-compile both binaries.

Exit criteria: `trayapp.exe` runs, shows a tray icon, quits cleanly. `clusage-cli ping`
returns `connection refused` against an unreachable host (no panics).

## Phase 1 — Logging path end to end

- SQLite schema + migrations for `usage_events` and `parse_errors`.
- `POST /log` HTTP handler with validation.
- `POST /parse_error` HTTP handler.
- `clusage-cli log` Mode A only (explicit flags).
- Manual smoke test: from a container, post fake events to the host; verify rows in DB.

Exit criteria: a container can register a usage event and the row appears in
`usage.db` on the host.

## Phase 2 — Passive ingestion (both Tier-1 paths)

A shared JSONL parser feeds both the host tailer and the container Stop hook. Both
land in `usage_events` and dedup against `(session_id, message_id)`.

- Internal `transcript-parser` package: takes a path or stream, yields events.
- **Host tailer**: `fsnotify` watcher on `~/.claude/projects/`, offset persistence.
- **Container path**: `clusage-cli log --from-hook` reads stdin JSON, locates
  `transcript_path`, runs the same parser, POSTs each event to `/log`.
- Stop-hook example documented for `~/.claude/settings.json`.

Test focus: a corpus of recorded transcript JSONL files (committed under
`testdata/`) exercises the parser against representative shapes and known schema
quirks; offset persistence is tested by simulating restart mid-file; the dedup
constraint is verified by replaying the same events twice and asserting one row.

Exit criteria: (a) a host Claude Code session produces `usage_events` rows with no
manual posting; (b) a container with the Stop hook installed produces `usage_events`
rows on the host with no `~/.claude` mount.

## Phase 3 — Snapshots and baselines

- `POST /snapshot` HTTP handler.
- `quota_snapshots` table.
- `windows` table population: detect session window starts from first event after a gap,
  set baselines from the closest snapshot.
- Userscript v1: read DOM, post snapshot, debounce.

Exit criteria: opening claude.ai with the userscript installed creates `quota_snapshots`
rows; `windows` rows correctly bracket recent activity.

## Phase 4 — Dashboard

- Static HTML/JS served from trayapp.
- Two burn-down charts (session and weekly).
- `GET /discount` endpoint and the effective-discount widget that consumes it.
- Health/status panel: last snapshot age, parse error count, drift.

Exit criteria: opening `http://localhost:27812` shows charts that match observed reality
within drift tolerance.

## Phase 5 — Slack indicator

- `GET /slack` per `docs/slack-indicator.md`.
- `POST /slack/release` for queues to record consumed slack.
- `clusage-cli slack` subcommand.
- Optional `slack_samples` time-series; `slack_releases` always on.
- Document the queue contract in `docs/slack-indicator.md` (already drafted).

Test focus: table-driven tests for the slack math (clamp boundaries, both windows,
combined `min`); each gate (headroom, priority quiet, freshness) tested in
isolation and in combination; window-not-started returns null fraction, not 0.

Exit criteria: a queue script can poll `slack --format release-bool` and get sane
release decisions in synthetic and real tests.

## Phase 6 — Polish and quality of life

- Tray icon color states.
- Drift alert in tray tooltip.
- `install.ps1` for autostart and config bootstrap.
- Userscript auto-update headers.
- README updated with screenshots.

Exit criteria: a fresh user can install in <10 minutes from the README alone.

## Deferred (v2+)

- **Tier 3 headless scrape.** Only if Tier 1 + Tier 2 prove insufficient in real use.
- **Multi-host / Cloudflare tunnel.** Add a token gate to `/log` and put trayapp behind
  cloudflared. Schema unchanged.
- **Forecasting.** Replace uniform `E(t)` with a learned curve from trailing windows.
- **Job runner.** A built-in queue that consumes the slack signal directly. The current
  design intentionally exposes only the signal; a runner is a separate project.
- **Slack release reconciliation.** `POST /slack/release/<id>/complete` with
  `actual_cost`, plus a `slack_releases.actual_cost` column, so we can compare estimates
  to actuals and tune the per-job budget cap.
- **CLI offset file.** `clusage-cli log --from-hook` would consult an offset file in
  `$XDG_STATE_HOME/clusage` instead of re-POSTing the full transcript every turn.
  Saves bandwidth in long sessions; not needed for v1 since the server dedupes.
- **Per-project breakdown.** Use `project_path` on events to chart cost per project.
- **Export.** CSV/Parquet dump of `usage_events` for external analysis.

## Things explicitly not on the roadmap

- A hosted multi-tenant version.
- Authentication on the local HTTP API. (Trust boundary is the host.)
- Replacing `clusage`. Different goals; `clusage` stays useful for ad-hoc reads.
- Reconciling against Anthropic invoices. Out of scope.
