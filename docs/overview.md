# Overview

## Problem statement

Claude Code subscriptions are billed as a flat monthly fee, but consumption is governed by
two rolling quotas:

- **session window** (the Claude UI's "Current session"): a quota that resets 5 hours after first use following a previous
  reset. The window only begins ticking when the user makes a request, so the start
  time is user-driven, not wall-clock.
- **Weekly quota**: a 7-day quota with a fixed end time exposed by the dashboard.
  Whether Anthropic implements this as a calendar week, a billing-day-aligned week, or
  something else is opaque to us; the snapshot's `weekly_window_ends` tells us where
  the boundary is, and we model it as a fixed `[t0, t1]` window for burn-down purposes.

Anthropic exposes both quotas as **percentages used** ("6% used", "23% used") on
`https://claude.ai/settings/usage` — there is no published per-window dollar quota.
Internally, passive consumption is still tallied in dollar-equivalents from each
event's `cost_usd_equivalent`, but the headline "how much of the plan have you
used?" number is the percent reported in the UI. The actual on-paper subscription
cost is constant.

Two consequences follow:

1. **The subscription's effective value varies by usage.** Heavier consumers extract more
   API-equivalent dollars per subscription dollar; light consumers extract less. The
   tool reports the raw inputs (USD-equivalent consumed, percent of session/weekly
   quota used) and leaves the subscription-cost arithmetic to the user.
2. **Unused budget is forfeit at window boundaries.** Quota not consumed in a session window
   do not roll over. Underutilization is a true economic loss.

Today the user can see a single point-in-time number in the browser dashboard or via
`clusage`. There is no history, no burn-down visualization, no slack signal, and no way to
opportunistically consume slack with low-priority background work.

## Goals

This project aims to:

1. **Record** every Claude Code invocation's token usage and dollar-equivalent cost,
   continuously, with no perturbation of the quota itself.
2. **Visualize** burn-down for both the session and weekly windows, with historical trends.
   The renderer connects observations into a polyline only when each observation's
   `continuous_with_prev` flag is true; a `false` flag breaks the line. Because the
   userscript also dedupes identical observations on the client side, **gaps in the
   burn-down chart now mean a genuine absence of observation** (the page was closed,
   the userscript was uninstalled, the source was offline) rather than a missing
   poll on a stable plateau.
3. **Report** consumption over a period: dollar-equivalent token cost plus
   percent-of-session and percent-of-weekly quota consumed (both can exceed 100%
   over a multi-window period). The user reconciles those numbers against their
   subscription cost, which the tool does not model.
4. **Expose a slack signal** that a queueing system can poll to decide whether to release
   low-priority work — work that would not be worth doing at API rates but is worth doing
   for free.
5. **Remain self-hosted and credential-light.** No online backend, no cloud account, no
   shared secrets beyond what already lives on the user's host.

## Non-goals

- **Multi-user / multi-tenant.** This is a personal tool.
- **Replacing the official dashboard.** Anthropic's UI remains authoritative for current
  quota state; this tool provides history and derived signals.
- **Billing reconciliation against Anthropic invoices.** The dollar-equivalent figures are
  whatever Claude Code reports; we treat them as opaque inputs.
- **Acting as a job runner itself.** The slack signal is a *signal*. A separate queue
  consumes it. The dashboard does not execute user jobs.

## Constraints

- **Windows host** is the primary always-on machine and the only place with a logged-in
  browser session for `claude.ai`.
- **Docker containers** on the same host are where most Claude Code work happens. They
  must be able to register usage cheaply.
- **Polling perturbs the quota.** `clusage` and any wrapper that calls Claude Code to read
  state will start a new session window if one is not active, contaminating the very thing
  being measured. Therefore the primary data source must be passive.
- **Browser snapshots are intermittent.** The dashboard page is only loaded when the user
  chooses to load it. Any architecture that requires authoritative snapshots on a fixed
  cadence is fragile.

## Design principles

- **Passive over active.** Prefer reading state that is already produced as a side
  effect of normal usage (session JSONL on disk; the transcript referenced by Stop
  hooks) over actively polling.
- **One process, one file on Windows.** A single Go `.exe` containing tray UI, HTTP server,
  SQLite DB, log tailer, and dashboard. Easy to autostart, easy to uninstall.
- **Clean HTTP API.** The container CLI, the userscript, and a future Cloudflare tunnel
  all speak the same JSON-over-HTTP protocol. No bespoke transports.
- **Derived state, not stored state.** The session burn-down figure is *computed* from
  baseline snapshots plus passive usage logs. It is not stored as an authoritative number
  that must be kept in sync.
## What "done" looks like for v1

- Tray app runs on logon, shows current session and weekly burn percentages in tooltip.
- Containers can register usage with a one-line Stop hook (`clusage-cli log --from-hook
  || true`); a bind-mount of `~/.claude` works too for hosts that prefer that path.
- Local dashboard at `http://localhost:PORT` shows two burn-down charts and a
  consumption widget (USD + percent-of-session + percent-of-weekly).
- A `/slack` HTTP endpoint returns a numeric slack signal suitable for polling by an
  external queue.
- Userscript posts a snapshot whenever the user loads the claude.ai dashboard.

Anything beyond this — headless scraping, a job runner, multi-machine sync — is explicitly
deferred to v2+.
