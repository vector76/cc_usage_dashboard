package ingest

import (
	"log/slog"
	"sort"
	"strings"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// BackfillCosts repairs usage events whose stored cost is not (yet) a real one,
// using the current price table: rows left NULL at ingest, and rows carrying a
// `ceiling` estimate that the table can now price properly. Reported and
// computed costs are never overwritten, and a ceiling estimate is never
// replaced by another ceiling estimate — both guards live in the store-level
// update. Returns the number of events changed.
//
// The ceiling upgrade is what makes adding a model to prices.yaml retroactive.
// A ceiling estimate uses the table's maximum rates and so can overstate by
// several times; without this pass, editing prices.yaml would correct new
// events while leaving all the historical ones inflated.
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
func BackfillCosts(db *store.Store, priceTable PriceTable) (int, error) {
	if len(priceTable) == 0 {
		return 0, nil
	}

	events, err := db.NullCostEvents()
	if err != nil {
		return 0, err
	}

	count := 0
	models := make(map[string]int)
	ceilingModels := make(map[string]int)
	for _, e := range events {
		cost, source := ResolveCost(nil, e.Model,
			e.InputTokens, e.OutputTokens, e.CacheCreationTokens, e.CacheReadTokens,
			priceTable)
		if cost == nil || source == "" {
			continue // nothing to price against (no model name)
		}
		updated, err := db.SetComputedCost(e.ID, *cost, source)
		if err != nil {
			return count, err
		}
		if updated {
			count++
			models[e.Model]++
			if source == "ceiling" {
				ceilingModels[e.Model]++
			}
		}
	}

	if count > 0 {
		slog.Warn("backfilled cost for usage events that were unpriced at ingest",
			"events", count, "models", strings.Join(sortedKeys(models), ", "))
	}
	// Ceiling estimates get their own line: the dollars are deliberately
	// overstated, so the reader needs to know which models are guesses and
	// that adding them to prices.yaml will move historical totals down.
	if len(ceilingModels) > 0 {
		n := 0
		for _, c := range ceilingModels {
			n += c
		}
		slog.Warn("priced unrecognized models at the price table ceiling; add them to prices.yaml for real rates",
			"events", n, "models", strings.Join(sortedKeys(ceilingModels), ", "))
	}
	return count, nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
