package discount

import (
	"testing"
	"time"

	"github.com/anthropics/usage-dashboard/internal/store"
)

// fixedNow returns a clock anchored at the given time, ignoring the
// monotonic component so durations align with stored timestamps.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func newCalc(t *testing.T, monthly float64, billingDays int, now time.Time) (*Calculator, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	c := NewCalculator(s.DB(), monthly, billingDays)
	c.SetNow(fixedNow(now))
	return c, s
}

func insertEvent(t *testing.T, s *store.Store, occurred time.Time, costUSD *float64, costSource string) {
	t.Helper()
	_, err := s.InsertUsageEvent(
		occurred, "api",
		"sess-"+occurred.Format(time.RFC3339Nano), "msg-"+occurred.Format(time.RFC3339Nano),
		"", "claude-3-5-sonnet-20241022",
		1000, 500, 0, 0,
		costUSD, costSource, "{}",
	)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func ptr(f float64) *float64 { return &f }

func TestCalculate_ResponseShapeMatchesDocs(t *testing.T) {
	now := time.Date(2026, 4, 25, 17, 32, 14, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	// Insert some events within the last 24h.
	insertEvent(t, s, now.Add(-1*time.Hour), ptr(10.00), "reported")
	insertEvent(t, s, now.Add(-2*time.Hour), ptr(20.00), "computed")
	insertEvent(t, s, now.Add(-3*time.Hour), nil, "unknown")
	// One event outside the window must be excluded.
	insertEvent(t, s, now.Add(-48*time.Hour), ptr(99.0), "reported")

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}

	if res.Period != "24h" {
		t.Errorf("Period: got %q, want %q", res.Period, "24h")
	}
	if !res.PeriodEnd.Equal(now) {
		t.Errorf("PeriodEnd: got %v, want %v", res.PeriodEnd, now)
	}
	if !res.PeriodStart.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("PeriodStart: got %v, want %v", res.PeriodStart, now.Add(-24*time.Hour))
	}

	if res.ConsumedUSDEquivalent != 30.00 {
		t.Errorf("ConsumedUSDEquivalent: got %v, want 30.00", res.ConsumedUSDEquivalent)
	}

	wantSub := 200.0 * (1.0 / 30.0)
	if !floatNear(res.SubscriptionCostProratedUSD, wantSub, 1e-9) {
		t.Errorf("SubscriptionCostProratedUSD: got %v, want %v", res.SubscriptionCostProratedUSD, wantSub)
	}

	if res.EventsTotal != 3 {
		t.Errorf("EventsTotal: got %d, want 3", res.EventsTotal)
	}
	if res.EventsWithReportedCost != 1 {
		t.Errorf("EventsWithReportedCost: got %d, want 1", res.EventsWithReportedCost)
	}
	if res.EventsWithComputedCost != 1 {
		t.Errorf("EventsWithComputedCost: got %d, want 1", res.EventsWithComputedCost)
	}
	if res.EventsWithoutCost != 1 {
		t.Errorf("EventsWithoutCost: got %d, want 1", res.EventsWithoutCost)
	}

	wantCoverage := 2.0 / 3.0 * 100
	if !floatNear(res.CostCoveragePct, wantCoverage, 1e-9) {
		t.Errorf("CostCoveragePct: got %v, want %v", res.CostCoveragePct, wantCoverage)
	}

	wantRatio := 30.0 / wantSub
	if res.ValueRatio == nil {
		t.Fatalf("ValueRatio: got nil, want non-nil")
	}
	if !floatNear(*res.ValueRatio, wantRatio, 1e-9) {
		t.Errorf("ValueRatio: got %v, want %v", *res.ValueRatio, wantRatio)
	}
	wantPct := (1 - wantSub/30.0) * 100
	if res.DiscountPct == nil {
		t.Fatalf("DiscountPct: got nil, want non-nil")
	}
	if !floatNear(*res.DiscountPct, wantPct, 1e-9) {
		t.Errorf("DiscountPct: got %v, want %v", *res.DiscountPct, wantPct)
	}
	if !floatNear(res.SavingsUSD, 30.0-wantSub, 1e-9) {
		t.Errorf("SavingsUSD: got %v, want %v", res.SavingsUSD, 30.0-wantSub)
	}
}

func TestCalculate_NoConsumption_RatiosNullSavingsNegative(t *testing.T) {
	now := time.Date(2026, 4, 25, 17, 32, 14, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	// Only an uncostable event in window.
	insertEvent(t, s, now.Add(-1*time.Hour), nil, "unknown")

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}

	if res.ConsumedUSDEquivalent != 0 {
		t.Errorf("ConsumedUSDEquivalent: got %v, want 0", res.ConsumedUSDEquivalent)
	}
	if res.ValueRatio != nil {
		t.Errorf("ValueRatio: got %v, want nil", *res.ValueRatio)
	}
	if res.DiscountPct != nil {
		t.Errorf("DiscountPct: got %v, want nil", *res.DiscountPct)
	}
	wantSavings := -res.SubscriptionCostProratedUSD
	if !floatNear(res.SavingsUSD, wantSavings, 1e-9) {
		t.Errorf("SavingsUSD: got %v, want %v", res.SavingsUSD, wantSavings)
	}
	if res.EventsTotal != 1 || res.EventsWithoutCost != 1 {
		t.Errorf("event counts wrong: total=%d without=%d", res.EventsTotal, res.EventsWithoutCost)
	}
}

func TestCalculate_NoEventsAtAll(t *testing.T) {
	now := time.Date(2026, 4, 25, 17, 32, 14, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.EventsTotal != 0 {
		t.Errorf("EventsTotal: got %d, want 0", res.EventsTotal)
	}
	if res.CostCoveragePct != 100 {
		t.Errorf("CostCoveragePct (no events): got %v, want 100", res.CostCoveragePct)
	}
	if res.ValueRatio != nil || res.DiscountPct != nil {
		t.Errorf("ratios should be nil with no consumption")
	}
}

func TestCalculate_DayPeriod(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	res, err := c.Calculate("7d")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.Period != "7d" {
		t.Errorf("Period: got %q, want %q", res.Period, "7d")
	}
	want := now.Add(-7 * 24 * time.Hour)
	if !res.PeriodStart.Equal(want) {
		t.Errorf("PeriodStart: got %v, want %v", res.PeriodStart, want)
	}
	wantSub := 200.0 * (7.0 / 30.0)
	if !floatNear(res.SubscriptionCostProratedUSD, wantSub, 1e-9) {
		t.Errorf("SubscriptionCostProratedUSD: got %v, want %v", res.SubscriptionCostProratedUSD, wantSub)
	}
}

func TestCalculate_DefaultPeriod(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	res, err := c.Calculate("")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	if res.Period != "24h" {
		t.Errorf("default Period: got %q, want %q", res.Period, "24h")
	}
}

func TestCalculate_InvalidPeriod(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	cases := []string{
		"not-a-duration",
		"5xd", // trailing junk before 'd' must not be accepted as days
		"-1h", // negative durations are nonsensical for "look back N"
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if _, err := c.Calculate(p); err == nil {
				t.Errorf("expected error for period %q", p)
			}
		})
	}
}

// Buckets must be mutually exclusive and sum to events_total even when an
// event has cost_source set but cost_usd_equivalent is NULL (a defensive
// edge case the schema doesn't forbid).
func TestCalculate_BucketsAreMutuallyExclusive(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	// Inject an inconsistent row directly: cost_source='reported' but
	// cost_usd_equivalent IS NULL. It must count once (in without_cost),
	// not twice.
	_, err := s.DB().Exec(`
		INSERT INTO usage_events
			(occurred_at, source, session_id, message_id,
			 input_tokens, output_tokens, cost_usd_equivalent, cost_source, raw_json)
		VALUES (?, 'api', 'sess-x', 'msg-x', 1, 1, NULL, 'reported', '{}')
	`, now.Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}
	sum := res.EventsWithReportedCost + res.EventsWithComputedCost + res.EventsWithoutCost
	if sum != res.EventsTotal {
		t.Errorf("buckets %d+%d+%d=%d != events_total %d",
			res.EventsWithReportedCost, res.EventsWithComputedCost,
			res.EventsWithoutCost, sum, res.EventsTotal)
	}
	if res.EventsWithoutCost != 1 {
		t.Errorf("EventsWithoutCost: got %d, want 1", res.EventsWithoutCost)
	}
	if res.EventsWithReportedCost != 0 {
		t.Errorf("EventsWithReportedCost: got %d, want 0 (cost was NULL)", res.EventsWithReportedCost)
	}
}

func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
