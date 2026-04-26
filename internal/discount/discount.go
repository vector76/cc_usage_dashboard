// Package discount provides discount calculation functionality.
package discount

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// Result holds the discount calculation result, conforming to
// docs/discount-calculation.md.
type Result struct {
	Period                      string    `json:"period"`
	PeriodStart                 time.Time `json:"period_start"`
	PeriodEnd                   time.Time `json:"period_end"`
	ConsumedUSDEquivalent       float64   `json:"consumed_usd_equivalent"`
	SubscriptionCostProratedUSD float64   `json:"subscription_cost_prorated_usd"`
	ValueRatio                  *float64  `json:"value_ratio"`
	DiscountPct                 *float64  `json:"discount_pct"`
	SavingsUSD                  float64   `json:"savings_usd"`
	EventsTotal                 int64     `json:"events_total"`
	EventsWithReportedCost      int64     `json:"events_with_reported_cost"`
	EventsWithComputedCost      int64     `json:"events_with_computed_cost"`
	EventsWithoutCost           int64     `json:"events_without_cost"`
	CostCoveragePct             float64   `json:"cost_coverage_pct"`
}

// Calculator computes the discount metric.
type Calculator struct {
	db          *sql.DB
	monthlyUSD  float64
	billingDays int
	now         func() time.Time
}

// NewCalculator creates a new discount calculator.
func NewCalculator(db *sql.DB, monthlyUSD float64, billingDays int) *Calculator {
	return &Calculator{
		db:          db,
		monthlyUSD:  monthlyUSD,
		billingDays: billingDays,
		now:         time.Now,
	}
}

// SetNow injects a clock for tests.
func (c *Calculator) SetNow(fn func() time.Time) {
	c.now = fn
}

// Calculate computes the discount for a given time period.
// Period strings can be: 24h, 7d, 30d, 90d, etc.
func (c *Calculator) Calculate(periodStr string) (*Result, error) {
	if periodStr == "" {
		periodStr = "24h"
	}
	duration, err := parsePeriod(periodStr)
	if err != nil {
		return nil, fmt.Errorf("invalid period: %w", err)
	}
	if duration < 0 {
		return nil, fmt.Errorf("invalid period: negative duration %q", periodStr)
	}
	endTime := c.now().UTC()
	startTime := endTime.Add(-duration)

	result := &Result{
		Period:      periodStr,
		PeriodStart: startTime,
		PeriodEnd:   endTime,
	}

	// Single aggregate query: avoids drift between event counts and the
	// consumed sum if a row is inserted between two reads, and ensures the
	// three event buckets sum to events_total. Buckets are mutually
	// exclusive by construction (the WITHOUT bucket is the IS NULL case;
	// the reported/computed buckets require IS NOT NULL).
	err = c.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NOT NULL AND cost_source = 'reported' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NOT NULL AND cost_source = 'computed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_usd_equivalent IS NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(cost_usd_equivalent), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ?
	`, store.FormatTime(startTime), store.FormatTime(endTime)).Scan(
		&result.EventsTotal,
		&result.EventsWithReportedCost,
		&result.EventsWithComputedCost,
		&result.EventsWithoutCost,
		&result.ConsumedUSDEquivalent,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate usage events: %w", err)
	}

	// Cost coverage: events with a known cost / total events.
	if result.EventsTotal > 0 {
		costed := result.EventsWithReportedCost + result.EventsWithComputedCost
		result.CostCoveragePct = float64(costed) / float64(result.EventsTotal) * 100
	} else {
		result.CostCoveragePct = 100
	}

	// Prorated subscription cost: monthly_usd * (period_days / billing_cycle_days).
	periodDays := duration.Hours() / 24
	if c.billingDays > 0 {
		result.SubscriptionCostProratedUSD = c.monthlyUSD * (periodDays / float64(c.billingDays))
	}

	D := result.ConsumedUSDEquivalent
	S := result.SubscriptionCostProratedUSD
	result.SavingsUSD = D - S
	if D > 0 && S > 0 {
		ratio := D / S
		pct := (1 - S/D) * 100
		result.ValueRatio = &ratio
		result.DiscountPct = &pct
	}

	return result, nil
}

// parsePeriod parses a period string like "24h", "7d", "30d".
// Go's time.ParseDuration doesn't accept day units, so a strict "<int>d"
// form is normalized to hours; everything else falls through to
// time.ParseDuration.
func parsePeriod(periodStr string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(periodStr, "d"); ok {
		if days, err := strconv.Atoi(rest); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(periodStr)
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %w", err)
	}
	return d, nil
}
