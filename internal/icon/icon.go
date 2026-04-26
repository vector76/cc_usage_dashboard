// Package icon owns the canonical app icon. Both the Windows tray UI
// (cmd/trayapp) and the dashboard (internal/dashboard) embed and serve
// the same image, so the bytes live here and are exposed as a single
// variable.
package icon

import _ "embed"

//go:embed claude_clock.png
var PNG []byte
