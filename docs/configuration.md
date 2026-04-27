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
# report it. See docs/data-model.md "Cost source".
pricing:
  table_path: "config/prices.example.yaml"

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
