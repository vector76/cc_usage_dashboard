# Configuration

The trayapp reads a single YAML file at startup, resolved by
`config.ResolveConfigPath` through the same directory chain as the
`prices.yaml` override — in order: the directory containing `trayapp.exe`,
the current working directory, `%APPDATA%\usage_dashboard\`, then
`~/.config/usage-dashboard/`. The schema is defined in
`internal/config/config.go`; the keys below mirror that struct exactly.
Values shown are the defaults `Load` applies when the field is absent —
an empty file produces a fully-functional config.

## First-run materialization

When no `config.yaml` exists anywhere in the chain, the trayapp writes one
next to the executable from the embedded sample (repo-root
`config.sample.yaml`, wired in via `config_embed.go` exactly like the price
table) and loads it. The sample keeps the slack-activation profiles active
— they're the setting users actually tune — and every other key commented
out at its default, so the materialized file is behavior-neutral until
edited. `config_sample_test.go` pins that neutrality: the sample must load
to the same effective config as no file at all. A user's `config.yaml` is
never overwritten or reconciled afterward, and the repo `.gitignore`
excludes it so `git pull` deployments (`pullrun.bat`) never conflict with
local edits. Materialization failure (e.g. exe dir not writable) is
non-fatal: the app logs a warning and runs on built-in defaults, exactly as
it did before the file existed.

```yaml
database:
  path: "usage.db"

http:
  port: 27812
  bind:
    - 127.0.0.1
    # Docker/WSL adapter IPs are auto-detected at startup; add explicit
    # entries here only when the auto-detect misses your topology. There
    # is no 0.0.0.0 fallback — see docs/architecture.md "Network and
    # security" for the rationale.

claude:
  projects_dir: "~/.claude/projects"   # %USERPROFILE%\.claude\projects on Windows
  # Root of the desktop app's Cowork ("local agent mode") session tree.
  # Each Cowork session nests its own private .claude home several levels
  # under this root, so a second tailer walks it recursively. Default
  # resolves to %APPDATA%\Claude\local-agent-mode-sessions on Windows;
  # empty (e.g. APPDATA unset) disables this second tailer.
  cowork_sessions_dir: "%APPDATA%\\Claude\\local-agent-mode-sessions"

# Price table used to compute cost_usd_equivalent when the source did not
# report it. See docs/data-model.md "Cost source" and "Price table
# resolution" below. Default is empty: the embedded built-in table is used
# unless a local prices.yaml override or this explicit path is present.
pricing:
  table_path: ""

tailer:
  poll_interval_ms: 1000

slack:
  baseline_max_age_seconds: 480     # baseline freshness gate (8 min)
  # Slack-activation profiles: [time_pct, remaining_pct] points, see
  # "Slack activation profiles" below. Absent (the default) means
  # "synthesize from the scalar thresholds beneath" — the values shown
  # here are that synthesis, i.e. the same behavior spelled out.
  session_profile:
    - [0, 98]
    - [52, 98]
    - [100, 50]
  weekly_profile:
    - [0, 80]
    - [30, 80]
    - [100, 10]
  session_surplus_threshold: 0.50   # profile-synthesis input (see below)
  weekly_surplus_threshold: 0.10    #   "
  session_absolute_threshold: 0.98  #   "
  weekly_absolute_threshold: 0.80   #   "

retention:
  parse_errors_days: 30
  slack_samples_days: 90

enable_slack_sampling: false        # writes to slack_samples on every /slack hit

logging:
  level: info
  file: ""                          # empty -> stdout; otherwise rotated file path
```

## Price table resolution

The model price table that computes `cost_usd_equivalent` (see
`docs/data-model.md` "Cost source") is resolved at startup by
`ingest.ResolvePriceTable`, which walks a precedence chain so a usable table
is **always** available with zero configuration:

1. **Explicit `pricing.table_path`.** If set and the file **exists**, it is
   loaded and wins. A malformed file here is fatal (the error is surfaced and
   cost computation is disabled) so a broken override is never silently
   masked. If the path is set but the file is **missing**, that is *not*
   fatal: a warning is logged and resolution falls through to steps 2–3. This
   is a deliberate choice — a stale or mistyped config path degrades to the
   built-in default rather than leaving cost uncomputed.
2. **Local `prices.yaml` override.** With no explicit path (the default),
   the first `prices.yaml` found in these directories is loaded — in order:
   the directory containing `trayapp.exe`, the current working directory,
   `%APPDATA%\usage_dashboard\`, then `~/.config/usage-dashboard/`. This is
   the no-rebuild override hook: drop a `prices.yaml` next to the exe to
   change rates.
3. **Embedded default.** If nothing above matches, the canonical `prices.yaml`
   embedded in the binary at build time (repo-root `prices.yaml`, wired in via
   `prices_embed.go`) is parsed. This always succeeds.

The search order is defined by `config.PriceTableSearchDirs`. To update the
built-in rates, edit the repo-root `prices.yaml` and rebuild.

## Slack activation profiles

`session_profile` and `weekly_profile` define when the headroom gates pass,
as piecewise-linear boundaries in the same coordinates as the dashboard
burn-down charts:

- Each point is `[time_pct, remaining_pct]`: at `time_pct` percent of the
  window elapsed, slack is available while the percent of quota remaining
  is **at or above** `remaining_pct`.
- Between points the boundary interpolates linearly; before the first and
  after the last point it extends flat at that point's `remaining_pct`.
- Validation (at load, fatal on failure): every point has exactly two
  values, both in `[0, 100]`, with `time_pct` strictly increasing. An
  explicitly empty list is rejected; omit the key instead.
- The weekly profile also gates the "Fable" weekly sub-row when the usage
  page reports one — the two rows share a window, so they share a boundary.
- The dashboard's green slack zone is drawn from the same server-reported
  points (`/api/dashboard/state` → `slack_profiles`), so the chart and the
  gate cannot disagree.

The implementation is `slack.Profile` in `internal/slack/profile.go`;
gate semantics and the full gate list live in `docs/slack-indicator.md`.

### Legacy scalar thresholds

When a profile key is **absent**, it is synthesized from the four scalar
thresholds (`slack.SynthesizeProfile`), reproducing the original two-leg
gate exactly: pass when `slack_fraction >= *_surplus_threshold` (pace leg)
OR `percent_used <= 100 * (1 - *_absolute_threshold)` (absolute-floor leg).
In boundary form that is a flat floor at `100 * absolute` until the pace
line `100 * (1 + surplus) - elapsed_pct` dips below it, then the pace line
— which is exactly the default profiles shown in the YAML block above.

Scalar conventions (all fractions in `[0, 1]` of the full quota):

- `*_absolute_threshold: 1.0` disables the absolute branch (the floor sits
  at 100% remaining, reachable only at exactly-untouched quota).
- `*_absolute_threshold: 0.0` collapses the gate to "always pass".

A configured profile takes precedence; the scalars are then ignored.

The session headroom gate also has a disjunct independent of any threshold
or profile — "session window absent entirely" — which short-circuits to
true when there is no active session window. See
`docs/no-active-session.md` for the wiring.

## Path placeholders

`%APPDATA%`, `%LOCALAPPDATA%`, `%USERPROFILE%`, and `%HOME%` placeholders
are expanded inside `database.path`, `claude.projects_dir`,
`claude.cowork_sessions_dir`, and `pricing.table_path` at load time. When
the underlying environment variable is unset (typical on Linux), the
loader falls back to the user's home directory so cross-platform configs
stay testable.

`claude.projects_dir` additionally expands a leading `~/` to the user's
home directory.

## Reload semantics

The file is read once at start. Changes require a restart — there is no
`SIGHUP` reload and no in-process reload trigger. This is Windows-first
and the simplification is worth it; the YAML is short and changes are
infrequent.

The slack-signal **pause** flag is not part of this file. Pause is a
transient operator override toggled from the tray menu and held only in
memory; see `docs/design-decisions.md` for the rationale and
`docs/slack-indicator.md` for the gate.

## Sources of truth

- The Go struct in `internal/config/config.go` is authoritative for shape
  and defaults. If this document drifts from it, the struct wins.
- Threshold meanings (the `slack:` block) are documented in
  `docs/slack-indicator.md`.
- Path placeholder expansion lives in `internal/config/config.go`'s
  `expandPlaceholders` / `expandHome`.
