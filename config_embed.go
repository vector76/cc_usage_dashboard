package ccusage

import _ "embed"

// DefaultConfigYAML is the sample configuration the trayapp materializes as
// config.yaml next to the executable on first run, when no config file is
// found anywhere in the search chain (see config.ResolveConfigPath). Users
// adjust settings — most notably the slack-activation profiles — by editing
// that copy; constructing a config from nothing would require reading the
// Go struct. The sample's active values must stay equivalent to the built-in
// defaults (pinned by config_sample_test.go), so materialization never
// changes behavior.
//
// Like prices.yaml, the file lives at the repo root because go:embed can
// only see files in the embedding package's own directory.
//
//go:embed config.sample.yaml
var DefaultConfigYAML []byte
