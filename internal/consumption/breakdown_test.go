package consumption

import (
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// eventSpec describes one usage_events row for the breakdown tests. Tokens are
// spelled out per kind because the whole point of the breakdown is that the
// four token columns stay distinguishable.
type eventSpec struct {
	occurred   time.Time
	model      string
	in         int
	out        int
	cacheWrite int
	cacheRead  int
	cost       *float64
	costSource string
}

// insertSpecs writes each spec, minting unique (session_id, message_id) pairs
// so the UNIQUE constraint never fires on rows that share a timestamp.
func insertSpecs(t *testing.T, s *store.Store, specs ...eventSpec) {
	t.Helper()
	for i, sp := range specs {
		id := "e" + time.Duration(i).String()
		_, err := s.InsertUsageEvent(
			sp.occurred, "api",
			"sess-"+id, "msg-"+id,
			"", sp.model,
			sp.in, sp.out, sp.cacheWrite, sp.cacheRead,
			sp.cost, sp.costSource, "{}",
		)
		if err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}
}

// modelRow finds the breakdown row for a model, failing the test when absent.
func modelRow(t *testing.T, b *Breakdown, model string) ModelBreakdown {
	t.Helper()
	for _, m := range b.Models {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("no breakdown row for model %q (got %d rows)", model, len(b.Models))
	return ModelBreakdown{}
}

// The core of the feature: dollars and all four token kinds, grouped per model.
func TestBreakdown_GroupsTokensAndCostByModel(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "claude-opus-4-5", 100, 20, 5, 9000, ptrF(1.50), "computed"},
		eventSpec{now.Add(-2 * time.Hour), "claude-opus-4-5", 200, 30, 7, 1000, ptrF(2.25), "computed"},
		eventSpec{now.Add(-3 * time.Hour), "claude-haiku-4-5", 50, 10, 0, 500, ptrF(0.05), "reported"},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}

	if len(b.Models) != 2 {
		t.Fatalf("want 2 model rows, got %d: %+v", len(b.Models), b.Models)
	}

	opus := modelRow(t, b, "claude-opus-4-5")
	if opus.Events != 2 {
		t.Errorf("opus events = %d, want 2", opus.Events)
	}
	if opus.InputTokens != 300 || opus.OutputTokens != 50 ||
		opus.CacheCreationTokens != 12 || opus.CacheReadTokens != 10000 {
		t.Errorf("opus tokens = in %d / out %d / cw %d / cr %d, want 300 / 50 / 12 / 10000",
			opus.InputTokens, opus.OutputTokens, opus.CacheCreationTokens, opus.CacheReadTokens)
	}
	if !near(opus.CostUSD, 3.75, 1e-9) {
		t.Errorf("opus cost = %v, want 3.75", opus.CostUSD)
	}
	if opus.Family != "opus" {
		t.Errorf("opus family = %q, want %q", opus.Family, "opus")
	}
	if opus.CostSource != "computed" {
		t.Errorf("opus cost source = %q, want %q", opus.CostSource, "computed")
	}

	haiku := modelRow(t, b, "claude-haiku-4-5")
	if haiku.Family != "haiku" || haiku.CostSource != "reported" {
		t.Errorf("haiku family/source = %q/%q, want haiku/reported", haiku.Family, haiku.CostSource)
	}

	if !near(b.TotalCostUSD, 3.80, 1e-9) {
		t.Errorf("total cost = %v, want 3.80", b.TotalCostUSD)
	}
	if b.EventsTotal != 3 {
		t.Errorf("events total = %d, want 3", b.EventsTotal)
	}
}

// Ceiling-priced dollars are a pessimistic guess at rates we invented for a
// model the price table has never heard of. They must not be presented in the
// same number as measured dollars, so they get their own total and their model
// row is flagged.
func TestBreakdown_CeilingPricedModelIsMarkedEstimated(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "claude-sonnet-4-5", 100, 20, 0, 0, ptrF(1.00), "computed"},
		eventSpec{now.Add(-2 * time.Hour), "claude-something-new", 100, 20, 0, 0, ptrF(9.00), "ceiling"},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}

	guess := modelRow(t, b, "claude-something-new")
	if !guess.Estimated {
		t.Error("ceiling-priced model row: Estimated = false, want true")
	}
	if guess.CostSource != "ceiling" {
		t.Errorf("cost source = %q, want %q", guess.CostSource, "ceiling")
	}
	// A ceiling-priced event is a guess whatever its name suggests, matching the
	// gray "other" treatment on the burn-down bars.
	if guess.Family != "other" {
		t.Errorf("family = %q, want %q", guess.Family, "other")
	}

	measured := modelRow(t, b, "claude-sonnet-4-5")
	if measured.Estimated {
		t.Error("computed model row: Estimated = true, want false")
	}

	if !near(b.MeasuredCostUSD, 1.00, 1e-9) {
		t.Errorf("measured cost = %v, want 1.00", b.MeasuredCostUSD)
	}
	if !near(b.EstimatedCostUSD, 9.00, 1e-9) {
		t.Errorf("estimated cost = %v, want 9.00", b.EstimatedCostUSD)
	}
	if !near(b.TotalCostUSD, 10.00, 1e-9) {
		t.Errorf("total cost = %v, want 10.00 (measured + estimated)", b.TotalCostUSD)
	}
}

// One model can carry events priced by different routes — a transcript that
// reported cost for some messages and not others. The row can't claim a single
// provenance, and it counts as estimated the moment any part of it is a guess.
func TestBreakdown_MixedCostSourcesForOneModel(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "claude-opus-4-5", 10, 1, 0, 0, ptrF(1.00), "reported"},
		eventSpec{now.Add(-2 * time.Hour), "claude-opus-4-5", 10, 1, 0, 0, ptrF(2.00), "computed"},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(b.Models) != 1 {
		t.Fatalf("want the two sources folded into 1 model row, got %d", len(b.Models))
	}
	row := modelRow(t, b, "claude-opus-4-5")
	if row.CostSource != "mixed" {
		t.Errorf("cost source = %q, want %q", row.CostSource, "mixed")
	}
	if row.Estimated {
		t.Error("reported+computed is all measured: Estimated = true, want false")
	}
	if !near(row.CostUSD, 3.00, 1e-9) {
		t.Errorf("cost = %v, want 3.00", row.CostUSD)
	}
}

func TestBreakdown_MixedWithCeilingIsEstimated(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "claude-opus-9", 10, 1, 0, 0, ptrF(1.00), "computed"},
		eventSpec{now.Add(-2 * time.Hour), "claude-opus-9", 10, 1, 0, 0, ptrF(2.00), "ceiling"},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	row := modelRow(t, b, "claude-opus-9")
	if row.CostSource != "mixed" {
		t.Errorf("cost source = %q, want %q", row.CostSource, "mixed")
	}
	if !row.Estimated {
		t.Error("any ceiling contribution taints the row: Estimated = false, want true")
	}
	// Only the ceiling dollars are estimated; the computed ones stay measured.
	if !near(b.MeasuredCostUSD, 1.00, 1e-9) || !near(b.EstimatedCostUSD, 2.00, 1e-9) {
		t.Errorf("measured/estimated = %v/%v, want 1.00/2.00", b.MeasuredCostUSD, b.EstimatedCostUSD)
	}
}

// A NULL cost is "we don't know", not "$0". The tokens are still real and must
// be reported; the event must be visible as uncosted rather than quietly
// contributing zero dollars to a total the reader believes is complete.
func TestBreakdown_NullCostEventsReportTokensAndAreCounted(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "claude-opus-4-5", 100, 20, 0, 0, ptrF(1.00), "computed"},
		eventSpec{now.Add(-2 * time.Hour), "claude-opus-4-5", 400, 80, 0, 0, nil, ""},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	row := modelRow(t, b, "claude-opus-4-5")
	if row.Events != 2 {
		t.Errorf("events = %d, want 2", row.Events)
	}
	if row.EventsWithoutCost != 1 {
		t.Errorf("events without cost = %d, want 1", row.EventsWithoutCost)
	}
	if row.InputTokens != 500 || row.OutputTokens != 100 {
		t.Errorf("tokens = %d in / %d out, want 500 / 100 — uncosted tokens still count",
			row.InputTokens, row.OutputTokens)
	}
	if !near(row.CostUSD, 1.00, 1e-9) {
		t.Errorf("cost = %v, want 1.00", row.CostUSD)
	}
	// The uncosted event contributed no dollars, so the single costed event's
	// provenance is the whole story — not "mixed".
	if row.CostSource != "computed" {
		t.Errorf("cost source = %q, want %q (an absent cost is not a provenance)", row.CostSource, "computed")
	}
	if b.EventsWithoutCost != 1 {
		t.Errorf("top-level events without cost = %d, want 1", b.EventsWithoutCost)
	}
}

// A model with nothing but uncosted events still belongs in the table — its
// tokens are the only evidence the work happened.
func TestBreakdown_ModelWithOnlyUncostedEvents(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "mystery-model", 100, 20, 0, 0, nil, ""},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	row := modelRow(t, b, "mystery-model")
	if row.CostUSD != 0 {
		t.Errorf("cost = %v, want 0", row.CostUSD)
	}
	if row.CostSource != "none" {
		t.Errorf("cost source = %q, want %q", row.CostSource, "none")
	}
	if row.Estimated {
		t.Error("no cost at all is not an estimate: Estimated = true, want false")
	}
}

// The range is half-open [start, end) so adjacent ranges partition the data
// without double-counting the boundary event.
func TestBreakdown_RangeIsHalfOpen(t *testing.T) {
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	c, s := newCalc(t, end)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{start.Add(-time.Nanosecond), "before", 1, 0, 0, 0, ptrF(1.00), "computed"},
		eventSpec{start, "at-start", 1, 0, 0, 0, ptrF(2.00), "computed"},
		eventSpec{end.Add(-time.Nanosecond), "just-before-end", 1, 0, 0, 0, ptrF(4.00), "computed"},
		eventSpec{end, "at-end", 1, 0, 0, 0, ptrF(8.00), "computed"},
	)

	b, err := c.Breakdown(start, end)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if !near(b.TotalCostUSD, 6.00, 1e-9) {
		t.Errorf("total = %v, want 6.00 (at-start + just-before-end only)", b.TotalCostUSD)
	}
	if b.EventsTotal != 2 {
		t.Errorf("events = %d, want 2", b.EventsTotal)
	}
	for _, m := range b.Models {
		if m.Model == "before" || m.Model == "at-end" {
			t.Errorf("out-of-range model %q leaked into the breakdown", m.Model)
		}
	}

	// The adjacent range picks up exactly the event this one excluded.
	next, err := c.Breakdown(end, end.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Breakdown (next range): %v", err)
	}
	if !near(next.TotalCostUSD, 8.00, 1e-9) {
		t.Errorf("adjacent range total = %v, want 8.00", next.TotalCostUSD)
	}
}

// An empty result must marshal as [] rather than null so the client can iterate
// without a nil guard.
func TestBreakdown_EmptyRangeReturnsEmptySlice(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if b.Models == nil {
		t.Error("Models is nil, want an empty non-nil slice")
	}
	if len(b.Models) != 0 {
		t.Errorf("want 0 model rows, got %d", len(b.Models))
	}
	if b.TotalCostUSD != 0 || b.EventsTotal != 0 {
		t.Errorf("empty range: total = %v, events = %d, want 0 / 0", b.TotalCostUSD, b.EventsTotal)
	}
}

// Events whose model column is empty still carry tokens and cost. They group
// under one row rather than being dropped.
func TestBreakdown_EmptyModelName(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "", 10, 2, 0, 0, ptrF(1.00), "computed"},
		eventSpec{now.Add(-2 * time.Hour), "", 10, 2, 0, 0, ptrF(1.00), "computed"},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(b.Models) != 1 {
		t.Fatalf("want 1 row for the unnamed model, got %d", len(b.Models))
	}
	if b.Models[0].Model != "" {
		t.Errorf("model = %q, want empty string preserved", b.Models[0].Model)
	}
	if b.Models[0].Family != "other" {
		t.Errorf("family = %q, want %q", b.Models[0].Family, "other")
	}
	if b.Models[0].Events != 2 {
		t.Errorf("events = %d, want 2", b.Models[0].Events)
	}
}

// Biggest spender first, so the table answers "where did the money go" without
// the reader scanning. Ties break on model name so the order is deterministic.
func TestBreakdown_SortedByCostDescendingThenModel(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	insertSpecs(t, s,
		eventSpec{now.Add(-1 * time.Hour), "cheap", 1, 0, 0, 0, ptrF(0.10), "computed"},
		eventSpec{now.Add(-2 * time.Hour), "spendy", 1, 0, 0, 0, ptrF(50.00), "computed"},
		eventSpec{now.Add(-3 * time.Hour), "b-tie", 1, 0, 0, 0, ptrF(5.00), "computed"},
		eventSpec{now.Add(-4 * time.Hour), "a-tie", 1, 0, 0, 0, ptrF(5.00), "computed"},
	)

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	got := make([]string, 0, len(b.Models))
	for _, m := range b.Models {
		got = append(got, m.Model)
	}
	want := []string{"spendy", "a-tie", "b-tie", "cheap"}
	if len(got) != len(want) {
		t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestBreakdown_RejectsEndBeforeStart(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	if _, err := c.Breakdown(now, now.Add(-time.Hour)); err == nil {
		t.Error("Breakdown with end before start: err = nil, want an error")
	}
}

// Times are echoed back exactly as requested so the client can label the table
// with the range it actually got rather than the one it asked for.
func TestBreakdown_EchoesRange(t *testing.T) {
	start := time.Date(2026, 7, 1, 6, 30, 0, 0, time.UTC)
	end := time.Date(2026, 7, 8, 6, 30, 0, 0, time.UTC)
	c, s := newCalc(t, end)
	defer s.Close()

	b, err := c.Breakdown(start, end)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if !b.Start.Equal(start) || !b.End.Equal(end) {
		t.Errorf("echoed range = %v..%v, want %v..%v", b.Start, b.End, start, end)
	}
}

// --- ParseRange -----------------------------------------------------------

func TestParseRange_DefaultsToLast24h(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	start, end, err := c.ParseRange("", "", "")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if !end.Equal(now) {
		t.Errorf("end = %v, want now %v", end, now)
	}
	if !start.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("start = %v, want now-24h", start)
	}
}

func TestParseRange_PeriodShorthand(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	// "7d" — Go's ParseDuration has no day unit; parsePeriod handles it.
	start, end, err := c.ParseRange("", "", "7d")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if !end.Equal(now) || !start.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("range = %v..%v, want now-7d..now", start, end)
	}
}

func TestParseRange_ExplicitRFC3339(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	start, end, err := c.ParseRange("2026-07-20T00:00:00Z", "2026-07-21T00:00:00Z", "")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if !start.Equal(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %v", end)
	}
}

// The client sends an absolute instant from a local-time picker, so an offset
// timestamp must normalize to the same UTC instant the events are stored in.
func TestParseRange_OffsetTimestampsNormalizeToUTC(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	start, _, err := c.ParseRange("2026-07-20T00:00:00-07:00", "2026-07-21T00:00:00-07:00", "")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	want := time.Date(2026, 7, 20, 7, 0, 0, 0, time.UTC)
	if !start.Equal(want) {
		t.Errorf("start = %v, want %v", start, want)
	}
	if start.Location() != time.UTC {
		t.Errorf("start location = %v, want UTC", start.Location())
	}
}

func TestParseRange_StartOnlyRunsToNow(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	start, end, err := c.ParseRange("2026-07-20T00:00:00Z", "", "")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if !start.Equal(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(now) {
		t.Errorf("end = %v, want now", end)
	}
}

func TestParseRange_Errors(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	cases := []struct {
		name            string
		start, end, per string
	}{
		{"malformed start", "not-a-time", "", ""},
		{"malformed end", "2026-07-20T00:00:00Z", "nope", ""},
		{"malformed period", "", "", "7 fortnights"},
		{"end without start", "", "2026-07-21T00:00:00Z", ""},
		{"end equals start", "2026-07-20T00:00:00Z", "2026-07-20T00:00:00Z", ""},
		{"end before start", "2026-07-21T00:00:00Z", "2026-07-20T00:00:00Z", ""},
		{"negative period", "", "", "-24h"},
		{"zero period", "", "", "0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := c.ParseRange(tc.start, tc.end, tc.per); err == nil {
				t.Errorf("ParseRange(%q, %q, %q): err = nil, want an error", tc.start, tc.end, tc.per)
			}
		})
	}
}

// Explicit start/end wins over a period, so a client that leaves a stale
// preset in the query string still gets the range its inputs describe.
func TestParseRange_ExplicitRangeWinsOverPeriod(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	start, end, err := c.ParseRange("2026-07-20T00:00:00Z", "2026-07-21T00:00:00Z", "30d")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if !start.Equal(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("range = %v..%v, want the explicit one", start, end)
	}
}

// ModelFamily is the shared classifier the burn-down bars and the report table
// both colour from, so it lives in one place and is exercised directly.
func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-5":           "opus",
		"claude-opus-4-1-20250805":  "opus",
		"claude-sonnet-4-5":         "sonnet",
		"claude-fable-5":            "fable",
		"claude-haiku-4-5-20251001": "haiku",
		"CLAUDE-OPUS-4-5":           "opus",
		"":                          "other",
		"some-other-vendor-model":   "other",
	}
	for in, want := range cases {
		if got := ModelFamily(in); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", in, got, want)
		}
	}
}
