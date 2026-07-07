// Package ccusage exposes assets embedded into the module at build time.
//
// It lives at the repo root so go:embed can reference prices.yaml (embed can
// only see files in the embedding package's own directory). Keeping the file
// here — rather than buried under internal/ — makes the built-in price table
// trivially editable: change prices.yaml and rebuild.
package ccusage

import _ "embed"

// DefaultPriceTableYAML is the canonical, built-in price table. It is the
// last link in the price-table resolution chain (see
// ingest.ResolvePriceTable): parsed only when no explicit config path and no
// local prices.yaml override is found. This guarantees cost computation works
// out of the box with zero configuration.
//
//go:embed prices.yaml
var DefaultPriceTableYAML []byte
