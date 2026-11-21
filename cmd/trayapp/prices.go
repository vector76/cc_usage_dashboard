package main

import (
	"log/slog"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/ingest"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// applyFetchedPrices layers a price list fetched from GitHub under base (a
// custom prices.yaml over the built-in table), so it only adds models base
// does not know, and makes the result take effect: every sink receives it,
// the models it now prices leave the unknown-model prompt, and their stored
// ceiling estimates are re-priced. Returns the new table.
//
// The sinks are swapped before the backfill so events ingested from here on
// are priced with the new rates. An event priced with the old table an
// instant before the swap can still be stored after the backfill's scan; it
// keeps its estimate until the next startup's backfill.
func applyFetchedPrices(base, fetched ingest.PriceTable, db *store.Store,
	unknown *feedback.UnknownModels, sinks ...func(ingest.PriceTable)) ingest.PriceTable {
	effective := ingest.MergePriceTables(base, fetched)
	for _, set := range sinks {
		set(effective)
	}
	unknown.Forget(func(model string) bool { return ingest.HasPrice(effective, model) })
	if _, err := ingest.BackfillCosts(db, effective); err != nil {
		slog.Warn("cost backfill after the GitHub price list update failed", "err", err)
	}
	return effective
}
