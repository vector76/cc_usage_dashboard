// Package discount provides discount calculation functionality.
package discount

import (
	"database/sql"
	"fmt"
	"time"
)

// Result holds the discount calculation result.
type Result struct {
	PeriodStart              time.Time `json:"period_start"`
	PeriodEnd                time.Time `json:"period_end"`
	EventsReported           int64     `json:"events_reported"`
	EventsComputed           int64     `json:"events_computed"`
	EventsUncostable         int64     `json:"events_uncostable"`
	CostReported             float64   `json:"cost_reported"`
	CostComputed             float64   `json:"cost_computed"`
	TotalCost                float64   `json:"total_cost"`
	CostCoveragePct          float64   `json:"cost_coverage_percent"`
	SubscriptionCost         float64   `json:"subscription_cost"`
	DiscountUSD              float64   `json:"discount_usd"`
	DiscountPercent          float64   `json:"discount_percent"`
	EffectiveRate            float64   `json:"effective_rate"`
}

// Calculator computes the discount metric.
type Calculator struct {
	db            *sql.DB
	monthlyUSD    float64
	billingDays   int
}

// NewCalculator creates a new discount calculator.
func NewCalculator(db *sql.DB, monthlyUSD float64, billingDays int) *Calculator {
	return &Calculator{
		db:          db,
		monthlyUSD:  monthlyUSD,
		billingDays: billingDays,
	}
}

// Calculate computes the discount for a given time period.
// Period strings can be: 24h, 7d, 30d, 90d, etc.
func (c *Calculator) Calculate(periodStr string) (*Result, error) {
	endTime := time.Now()
	duration, err := parsePeriod(periodStr)
	if err != nil {
		return nil, fmt.Errorf("invalid period: %w", err)
	}
	startTime := endTime.Add(-duration)

	result := &Result{
		PeriodStart: startTime,
		PeriodEnd:   endTime,
	}

	// Get event counts by cost source
	err = c.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN cost_source = 'reported' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_source = 'computed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_source IS NULL THEN 1 ELSE 0 END), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ?
	`, startTime, endTime).Scan(&result.EventsReported, &result.EventsComputed, &result.EventsUncostable)

	if err != nil {
		return nil, fmt.Errorf("failed to query event counts: %w", err)
	}

	// Get costs by source
	err = c.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN cost_source = 'reported' THEN cost_usd_equivalent ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_source = 'computed' THEN cost_usd_equivalent ELSE 0 END), 0)
		FROM usage_events
		WHERE occurred_at >= ? AND occurred_at < ? AND cost_usd_equivalent IS NOT NULL
	`, startTime, endTime).Scan(&result.CostReported, &result.CostComputed)

	if err != nil {
		return nil, fmt.Errorf("failed to query costs: %w", err)
	}

	result.TotalCost = result.CostReported + result.CostComputed

	// Calculate cost coverage percentage
	totalEvents := result.EventsReported + result.EventsComputed + result.EventsUncostable
	if totalEvents > 0 {
		costableEvents := result.EventsReported + result.EventsComputed
		result.CostCoveragePct = float64(costableEvents) / float64(totalEvents) * 100
	} else {
		result.CostCoveragePct = 100 // No events = full coverage
	}

	// Calculate subscription cost for the period (prorated)
	periodDays := duration.Hours() / 24
	result.SubscriptionCost = c.monthlyUSD * (periodDays / float64(c.billingDays))

	// Calculate discount
	result.DiscountUSD = result.SubscriptionCost - result.TotalCost
	if result.SubscriptionCost > 0 {
		result.DiscountPercent = result.DiscountUSD / result.SubscriptionCost * 100
		result.EffectiveRate = result.TotalCost / result.SubscriptionCost
	}

	return result, nil
}

// parsePeriod parses a period string like "24h", "7d", "30d".
func parsePeriod(periodStr string) (time.Duration, error) {
	if periodStr == "" {
		periodStr = "24h" // Default to 24 hours
	}

	d, err := time.ParseDuration(periodStr)
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %w", err)
	}

	return d, nil
}
