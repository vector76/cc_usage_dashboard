package ingest

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// BackfillNullCosts recomputes the cost of usage events that were unpriceable
// at ingest time (cost_usd_equivalent IS NULL, model non-empty) using the
// current price table. Rows whose model is still missing from the table are
// left untouched; reported and previously computed costs are never
// overwritten (the store-level update is guarded on the cost still being
// NULL). Returns the number of events backfilled.
//
// Run once at startup, after the price table is resolved: the table is loaded
// once per process, so a model that is unknown now stays unknown until the
// next restart — re-running periodically with the same table can never fix
// additional rows.
//
// A successful backfill is logged at Warn rather than Info deliberately: the
// feedback tee only copies warn+ records into the dashboard's feedback panel,
// and a retroactive change to historical dollar totals is exactly the kind of
// event a console-less tray user needs to see.
func BackfillNullCosts(db *store.Store, priceTable PriceTable) (int, error) {
	if len(priceTable) == 0 {
		return 0, nil
	}

	events, err := db.NullCostEvents()
	if err != nil {
		return 0, err
	}

	count := 0
	models := make(map[string]int)
	for _, e := range events {
		cost, source := ResolveCost(nil, e.Model,
			e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens,
			priceTable)
		if source != "computed" || cost == nil {
			continue // model still missing from the table
		}
		updated, err := db.SetComputedCost(e.ID, *cost)
		if err != nil {
			return count, err
		}
		if updated {
			count++
			models[e.Model]++
		}
	}

	if count > 0 {
		names := make([]string, 0, len(models))
		for m := range models {
			names = append(names, m)
		}
		sort.Strings(names)
		slog.Warn("backfilled computed cost for usage events that were unpriced at ingest",
			"events", count, "models", strings.Join(names, ", "))
	}
	return count, nil
}
