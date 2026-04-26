# Development Conventions

This document establishes coding conventions used throughout the project.

## Logging

The project uses the stdlib `log/slog` package for structured logging. All packages should use
the global logger or accept a logger as a dependency.

**Conventions:**
- Use `slog.Info()` for normal operational events.
- Use `slog.Warn()` for recoverable issues (e.g., a parse error that gets recorded but doesn't stop processing).
- Use `slog.Error()` for unrecoverable failures or errors requiring operator intervention.
- Use `slog.Debug()` sparingly; it's off by default in production.
- Always include context: pass relevant IDs, file paths, error details as attributes.

**Example:**
```go
slog.Error("failed to parse transcript", "path", path, "offset", offset, "err", err)
```

## Error handling and wrapping

Use the stdlib `fmt.Errorf` with the `%w` verb to wrap errors so the error chain is
preserved for forensic debugging:

```go
if err := someFunc(); err != nil {
    return fmt.Errorf("operation X failed: %w", err)
}
```

Always wrap at the point where you can add context; do not re-wrap the same error
at multiple levels.

## Configuration

Configuration is loaded once at startup from a YAML file. See `internal/config` (Phase 2+)
for the schema. Restart is required for config changes; no hot-reload.

## Build tags for Windows-specific code

Use `//go:build windows` and `// +build windows` (for Go 1.16 compatibility) for
platform-specific code. Stub implementations must exist for non-Windows platforms
so the entire codebase compiles on Linux without platform-specific dependencies.

Example:
- `internal/tray/tray_windows.go` — Windows implementation
- `internal/tray/tray_linux.go` — Linux stub (no-op)

## Testing

Tests are written before or alongside implementation. See `docs/roadmap.md` for
the testing strategy per phase.

- Unit tests: table-driven tests for logic, mocked I/O where needed.
- Integration tests: use `internal/testhelper` to spin up temp DB and HTTP servers.
- No external test fixtures; all test data is generated or embedded in code.
- Use `testing.T` and `testing.B` only; no external test frameworks.
