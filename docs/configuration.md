# Configuration

The trayapp reads a single YAML file at startup. Default location on Windows
is `%APPDATA%\usage_dashboard\config.yaml`. The schema is defined in
`internal/config/config.go`; the keys below mirror that struct exactly.
Values shown are the defaults `Load` applies when the field is absent —
the file is optional, and an empty file produces a fully-functional config.

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

# Price table used to compute cost_usd_equivalent when the source did not
# report it. See docs/data-model.md "Cost source" and "Price table
# resolution" below. Default is empty: the embedded built-in table is used
# unless a local prices.yaml override or this explicit path is present.
pricing:
  table_path: ""

tailer:
  poll_interval_ms: 1000

slack:
  headroom_threshold: 10.0          # legacy single-window threshold (percent units)
  freshness_threshold_ms: 60000
  baseline_max_age_seconds: 480     # baseline freshness gate (8 min)
  session_surplus_threshold: 0.50   # session headroom gate
  weekly_surplus_threshold: 0.10    # weekly pace-relative gate
  session_absolute_threshold: 0.98  # session absolute-floor gate (percent_used <= 2)
  weekly_absolute_threshold: 0.80   # weekly absolute-floor gate (percent_used <= 20)

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

## Slack absolute thresholds

`session_absolute_threshold` and `weekly_absolute_threshold` parameterise
the absolute-floor leg of each headroom gate. They share a single
convention:

- Units are a **fraction in `[0, 1]`** of the full quota, not a
  percentage. The gate passes when `percent_used <= 100 * (1 - T)`.
  So `0.98` means "release while at most 2% of the window is used,"
  and `0.80` means "release while at most 20% is used."
- **`1.0` disables the absolute branch.** With `T = 1.0` the comparison
  becomes `percent_used <= 0`, which is unreachable in practice — only
  the pace-relative surplus leg can fire the gate. Use this when the
  absolute floor is unwanted (e.g. for tuning experiments where only
  surplus-relative behaviour is being measured).
- `0.0` would mean "release at any usage level," which collapses the
  gate to "always pass" via the absolute leg. This is allowed but
  rarely useful.

Defaults match the values shown in the YAML block above:
`session_absolute_threshold: 0.98` and `weekly_absolute_threshold: 0.80`.
The threshold meanings and the disjunctive structure of the headroom
gates are documented in full in `docs/slack-indicator.md`.

The session headroom gate also has a third disjunct unrelated to this
threshold — "session window absent entirely" — which short-circuits to
true when there is no active session window. That leg is independent of
`session_absolute_threshold`; setting the threshold to `1.0` does not
suppress it. See `docs/no-active-session.md` for the wiring.

## Path placeholders

`%APPDATA%`, `%LOCALAPPDATA%`, `%USERPROFILE%`, and `%HOME%` placeholders
are expanded inside `database.path`, `claude.projects_dir`, and
`pricing.table_path` at load time. When the underlying environment
variable is unset (typical on Linux), the loader falls back to the user's
home directory so cross-platform configs stay testable.

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
