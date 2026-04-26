# Container CLI

Small Go binary for use inside Linux Docker containers. Posts per-invocation token usage
to the trayapp on the host. Built from the same Go module as the trayapp, different
build target.

## Build

```
GOOS=linux GOARCH=amd64 go build -o clusage-cli ./cmd/cli
```

Static binary, no runtime deps, drop into `/usr/local/bin` in any container image.

## Subcommands

### `clusage-cli log`

POSTs `/log` with token usage. Two invocation modes:

**Mode A — explicit flags** (for ad-hoc tests and non-Claude-Code callers):

```
clusage-cli log \
  --input-tokens 1234 \
  --output-tokens 567 \
  --cache-creation-tokens 0 \
  --cache-read-tokens 8910 \
  --cost-usd 0.0123 \
  --session-id <id> \
  --message-id <id>
```

Flags map to columns in `usage_events`. Only `--input-tokens` and `--output-tokens` are
required; everything else is optional.

**Mode B — `--from-hook`** (for Claude Code Stop hooks):

```
clusage-cli log --from-hook
```

Reads the hook's JSON payload from stdin (Claude Code's standard hook contract — the
payload contains `session_id`, `transcript_path`, etc.), opens the transcript JSONL,
and POSTs every assistant message that has a `usage` block. The trayapp deduplicates
by `(session_id, message_id)` on insert, so the CLI does not need to track state in
the container — it can resend old messages on every hook firing and the server will
discard duplicates. This keeps containers stateless and makes retries automatic.

For long sessions where re-POSTing the full transcript on every turn becomes wasteful,
a future flag may add an offset file in `$XDG_STATE_HOME/clusage`. Not needed for v1.

This mode is the right one for hook wiring because Claude Code does not expose token
counts as environment variables — the authoritative numbers are inside the transcript
file. The hook contract is `JSON-on-stdin`, not env vars.

Exit codes:

- `0` — accepted (HTTP 2xx).
- `2` — usage error (bad flags or unparseable hook payload).
- `3` — host unreachable.
- `4` — host returned 4xx (validation error).
- `5` — host returned 5xx.

Non-zero exit codes must not abort the user's Claude Code session. The hook command
should append `|| true` so the hook exits 0 regardless.

### `clusage-cli slack`

GET `/slack` and print one of:

- `--format json` (default) — full payload from the slack endpoint.
- `--format release-bool` — print `true` or `false` based on `release_recommended`.
- `--format fraction` — print `slack_combined_fraction` as a decimal.

Useful in queue scripts:

```bash
if [[ "$(clusage-cli slack --format release-bool)" == "true" ]]; then
  ./run-low-priority-job.sh
fi
```

### `clusage-cli ping`

GET `/healthz`. Exits `0` if healthy.

## Configuration

Environment variables:

- `CLUSAGE_HOST` — defaults to `host.docker.internal`.
- `CLUSAGE_PORT` — defaults to `27812`.
- `CLUSAGE_TIMEOUT_MS` — defaults to `2000`.

No config file. Containers should be configurable via env, not state.

## Wiring into Claude Code's Stop hook

In a container's `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Stop": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "clusage-cli log --from-hook || true"
          }
        ]
      }
    ]
  }
}
```

Claude Code passes the hook payload as JSON on stdin. The CLI reads it, locates the
transcript via `transcript_path`, and POSTs new events to the host. Verify the exact
hook payload schema against current Claude Code docs at integration time; the schema
is the input contract the CLI must parse.

## Pure-curl fallback

For containers where adding a binary is awkward, you can replicate Mode B as a small
shell script that reads stdin, extracts `transcript_path` with `jq`, and POSTs the last
assistant message's `usage` block. This is more code than just installing the CLI, so
the CLI is the recommended path.
