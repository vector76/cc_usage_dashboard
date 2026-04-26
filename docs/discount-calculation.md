# Effective discount calculation

The Claude Code subscription costs a fixed monthly fee but lets you consume a much larger
dollar-equivalent token budget. The **effective discount** is the gap between those two
numbers, computed over a chosen period.

## Inputs

- `D(period)` = sum of `cost_usd_equivalent` over `usage_events` in the period.
- `S(period)` = prorated subscription cost over the same period.
- `period` = a user-selected interval (last 24h, last week, last month, etc.).

The subscription cost is configured once at install:

```
subscription:
  monthly_usd: 200          # whatever the user actually pays
  billing_cycle_days: 30    # for proration
```

## Output

Two related figures, computed from the same inputs but expressing different things:

```
value_ratio(period) = D / S                    # how many dollars of API value per dollar paid
discount_pct(period) = (1 - S / D) * 100       # conventional "X% off API rates"
savings_usd(period)  = D - S                   # absolute dollars saved
```

- `value_ratio = 1.0` means you broke even.
- `value_ratio = 5.0` means $5 of API value per $1 paid → `discount_pct = 80%`.
- `value_ratio < 1.0` means you would have been better off paying API rates (or
  downgrading the subscription tier). `discount_pct` is negative in that case.

The dashboard surfaces both: `value_ratio` is intuitive for "am I getting my money's
worth?" and `discount_pct` is the conventional framing.

## Worked example

- Subscription: $200/month → ~$6.67/day prorated.
- Last 24h `usage_events.cost_usd_equivalent` sum: $42.10.
- `value_ratio = 42.10 / 6.67 = 6.31` → 6.3× value.
- `discount_pct = (1 - 6.67/42.10) * 100 = 84.2%` off API rates.
- `savings_usd = 42.10 - 6.67 = $35.43` saved that day.

## Why this is interesting

- **Sizing the tier.** If `value_ratio` consistently runs below 1.0, the user is paying
  for capacity they don't use. If it runs very high, the user is leaving money on the
  table by not running more low-priority work in slack windows.
- **Tuning slack thresholds.** A higher `value_ratio` means each released slack job is
  cheaper-than-free. The thresholds in `docs/slack-indicator.md` can be biased more
  aggressively when the user is well into the discount.
- **Negotiating internally.** For users on team plans or company-paid subscriptions,
  the discount number is the answer to "is this expense justified?".

## Caveats

- **Cache tokens skew the comparison.** Anthropic's dollar-equivalent figures may or may
  not weight cached input tokens at their actual API rate. The ratio is meaningful for
  *trend* analysis even if absolute dollars are slightly off.
- **`cost_usd_equivalent` is sometimes missing** when the source did not report it. The
  dashboard should display "X% of events have cost data" alongside the ratio so the user
  knows how much extrapolation is happening.
- **Subscription proration is approximate.** A user who happens to do all their work on
  weekends will see misleadingly high discount ratios on weekday samples. Default to
  computing over windows of at least 7 days for stable ratios; show shorter windows with
  a wider error band.

## API

```
GET /discount?period=7d
```

Response:

```json
{
  "period": "7d",
  "period_start": "2026-04-18T17:32:14Z",
  "period_end":   "2026-04-25T17:32:14Z",
  "consumed_usd_equivalent": 312.40,
  "subscription_cost_prorated_usd": 46.67,
  "value_ratio": 6.69,
  "discount_pct": 85.1,
  "savings_usd": 265.73,
  "events_total": 1283,
  "events_with_cost": 1180,
  "cost_coverage_pct": 92.0
}
```

The dashboard renders this as a single prominent number with the cost coverage shown as
a confidence indicator.
