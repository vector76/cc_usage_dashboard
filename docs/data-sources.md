# Data sources

There are three tiers of Claude usage data. The grouping is by **whether they perturb
the quota**, not by mechanism: any source that piggybacks on the user's normal activity
is "passive" and shares Tier 1, regardless of whether it reaches the trayapp via file
tailing or via a Stop-hook POST. Tier 2 actively reads the dashboard but only when the
user is already there. Tier 3 actively scrapes on a schedule and is reserved for cases
where the cheaper tiers fall short.

## Tier 1 — Passive observation

**Status: primary.** Two paths, same tier. Both ride on activity the user is already
performing, so neither perturbs the 5-hour window or the weekly quota.

### 1a. Session JSONL tailing (host)

Claude Code writes per-session JSONL files to `~/.claude/projects/<encoded-project-path>/`.
Each line is a JSON object representing one event in the conversation. Assistant messages
include a `usage` block with token counts (input, output, cache creation, cache read),
and Claude Code records dollar-equivalent cost when available.

The tailer:

- Uses `fsnotify` (or polling fallback on filesystems where fsnotify is flaky) to detect
  appends.
- Reads new bytes from the last persisted offset for each file.
- Parses each line; ignores lines without a `usage` block.
- Inserts a row into `usage_events` keyed by `(session_id, message_id)` so replays are
  idempotent.
- Persists offsets so a trayapp restart resumes from the right place without rereading
  history.

### 1b. Stop hook + transcript POST (containers without shared `~/.claude`)

When a container does not bind-mount `~/.claude` from the host, the host tailer cannot
see its activity. Instead, a Stop hook installed in the container runs `clusage-cli log
--from-hook` after each turn, reading Claude Code's hook payload from stdin, opening
the transcript file referenced therein, and POSTing each new assistant message's usage
block to the host server. See `docs/container-cli.md` for the wiring.

This path is **also passive in the sense that matters**: it observes work the user
chose to do, never makes a request to provoke a measurement, and never starts a new
5-hour window. It just delivers the same data over a different transport.

The host-side tailer (1a) and the container-side Stop hook (1b) deduplicate against
the same `(session_id, message_id)` key, so both can coexist on a host that runs
some sessions natively and some in unshared containers.

### Pros

- **Zero perturbation.** No requests issued, no wakeups triggered.
- **Authoritative per-message.** The same numbers Claude Code itself reports.

### Container coverage — pick one path per container

For each container, decide between two equally valid options:

- **Bind-mount `~/.claude` from the host** (e.g. `-v ~/.claude:/root/.claude`). The
  host tailer (1a) sees the container's sessions for free. Zero in-container setup.
- **Install the CLI and Stop hook** in the container image. The container reports its
  own usage via 1b. No mount needed.

Both paths land in the same DB and dedup against the same key. A user can use one for
some containers and the other for others. The recommended choice for a container-heavy
workflow is whichever requires less change to existing images.

### Cons / caveats

- **Assumes file format stability.** The JSONL schema is not a public contract. The
  tailer must be defensive and log parse failures loudly.
- **Quota baseline is not in the JSONL.** The files give us *consumption*, not *remaining*.
  We need a snapshot from somewhere else to anchor the baseline.
- **Container path coverage.** See above; the unshared-container case is common enough
  that it shapes the install flow, not just an edge case.

### Schema fragility mitigation

The parser should:

- Treat unknown fields as opaque.
- Require only `usage.input_tokens` and `usage.output_tokens`. Everything else is bonus.
- Surface parse-error rate in the dashboard as a health metric.

## Tier 2 — Userscript snapshots

**Status: secondary, used for baseline anchoring.**

A Tampermonkey/Violentmonkey userscript on `claude.ai/*` reads the dashboard's displayed
quota numbers and POSTs them to the local server. See `docs/userscript.md` for
the implementation plan.

### Pros

- **Authoritative for baselines.** This is what Anthropic's UI shows, by definition.
- **Trivial to install.** A single `.user.js` file, install via userscript manager.
- **Free recalibration.** Fires whenever the user naturally visits the dashboard.

### Cons / caveats

- **Only fires when the page is open.** Cannot be relied on for a fixed cadence.
- **DOM-dependent.** Anthropic can change the page layout at any time. The script must
  fail soft — logging a parse error rather than corrupting the DB.
- **No background acquisition.** If the user goes a week without opening the dashboard,
  baselines drift and we depend entirely on Tier 1 plus the previous baseline.

### When the userscript is the answer

- First-time setup (need any baseline at all).
- Recovering from a multi-day gap in passive data.
- Cross-checking when the slack indicator looks suspicious.

## Tier 3 — Headless browser scrape (deferred)

**Status: not built. Escalation only.**

Use Playwright (or similar) from the trayapp to drive a headless Chrome against
`claude.ai`, log in via a stored profile, and read the same DOM the userscript reads.

### Why deferred

- **Profile locking.** Chrome holds an exclusive lock on the profile directory while it's
  running, so the user cannot have their normal browser open at the same time. Workaround
  is a *copy* of the profile, which then needs periodic refresh as cookies rotate.
- **Login fragility.** Sessions expire. Re-authenticating headlessly is hard if MFA is on.
- **Anti-bot risk.** Anthropic could plausibly fingerprint headless traffic.
- **Maintenance cost.** Selectors break.

### When to escalate to Tier 3

Only if Tier 1 + Tier 2 in practice produce baselines stale enough that the slack
indicator becomes untrustworthy. This will probably not happen for a user who naturally
checks the dashboard a few times a week.

## Tier 0 (rejected) — `clusage` polling

`clusage` works by invoking Claude Code itself to read the current usage figures. This
**starts a 5-hour window** if one is not already active, perturbing the very quantity
being measured. It is therefore unsuitable as an automated data source.

`clusage` remains useful for ad-hoc manual reads where the user wants a definitive number
right now and is willing to start a window to get it.

## Cross-source reconciliation

Because Tier 1 measures consumption and Tier 2 measures remaining, the two should agree:

```
quota_total - sum(tier1_events_in_window) ≈ tier2_remaining_at(now)
```

The trayapp computes both sides on each new snapshot and records the discrepancy. If
discrepancy exceeds a threshold (suggested: 5% of quota or $X dollar-equivalent), the
tray icon shows a warning state and the dashboard highlights the divergence. The user
can then decide whether to trust the snapshot (overwrite baseline) or investigate.

The system never silently averages or "splits the difference." Disagreement is data.
