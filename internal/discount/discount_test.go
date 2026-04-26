package discount

import (
	"math"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
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

// TestCalculate_WorkedExampleFromDocs reproduces the worked example in
// docs/discount-calculation.md: $200/mo subscription, last 24h, $42.10
// consumed → value_ratio ~6.31, discount_pct ~84.2%, savings ~$35.43.
func TestCalculate_WorkedExampleFromDocs(t *testing.T) {
	now := time.Date(2026, 4, 25, 17, 32, 14, 0, time.UTC)
	c, s := newCalc(t, 200.0, 30, now)
	defer s.Close()

	// Three reported-cost events within the last 24h summing to exactly 42.10.
	insertEvent(t, s, now.Add(-1*time.Hour), ptr(15.00), "reported")
	insertEvent(t, s, now.Add(-6*time.Hour), ptr(20.10), "reported")
	insertEvent(t, s, now.Add(-12*time.Hour), ptr(7.00), "reported")

	res, err := c.Calculate("24h")
	if err != nil {
		t.Fatalf("Calculate: %v", err)
	}

	if !floatNear(res.ConsumedUSDEquivalent, 42.10, 1e-6) {
		t.Errorf("ConsumedUSDEquivalent: got %v, want 42.10", res.ConsumedUSDEquivalent)
	}
	if res.ValueRatio == nil {
		t.Fatal("ValueRatio: got nil, want ~6.31")
	}
	// Doc rounds prorated subscription to $6.67/day; tolerance must cover
	// both that rounded value and the exact 200/30 used by the calculator.
	if math.Abs(*res.ValueRatio-6.31) > 0.01 {
		t.Errorf("ValueRatio: got %v, want ~6.31 (±0.01)", *res.ValueRatio)
	}
	if res.DiscountPct == nil {
		t.Fatal("DiscountPct: got nil, want ~84.2")
	}
	if math.Abs(*res.DiscountPct-84.2) > 0.1 {
		t.Errorf("DiscountPct: got %v, want ~84.2 (±0.1)", *res.DiscountPct)
	}
	if math.Abs(res.SavingsUSD-35.43) > 0.01 {
		t.Errorf("SavingsUSD: got %v, want ~35.43 (±0.01)", res.SavingsUSD)
	}
}

// TestParsePeriod_AcceptedFormats verifies the period strings the docs
// promise (24h, 7d, 30d) all parse to the expected duration, and that
// invalid strings return an error.
func TestParsePeriod_AcceptedFormats(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"banana", 0, true},
		{"7days", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parsePeriod(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parsePeriod(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePeriod(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePeriod(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func floatNear(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
