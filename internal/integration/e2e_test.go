// Package integration contains end-to-end tests that exercise the major code
// paths of the dashboard server in-process. Each scenario corresponds to a
// numbered case in testdata/e2e_test.md.
package integration

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/config"
	"github.com/vector76/cc_usage_dashboard/internal/server"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// TestMain pins the package's local time zone to UTC. The server (and the
// SQLite go driver via mattn/go-sqlite3) compares timestamps as text in some
// queries, so a mix of UTC-stored values (e.g. snapshot.observed_at parsed
// from RFC3339) and locally-stamped values (e.g. usage_events.occurred_at
// from time.Now()) yields lexicographically inconsistent comparisons under
// non-UTC zones. Forcing UTC keeps all stored timestamps in a single offset
// so this end-to-end suite runs deterministically on any host.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

// pricesExampleYAML is the path to the example price table relative to this
// package directory. The file lives at the repo root under config/.
const pricesExampleYAML = "../../config/prices.example.yaml"

// testEnv bundles the server under test with the underlying store so test
// bodies can both drive the HTTP API and inspect persisted state.
type testEnv struct {
	srv   *server.Server
	store *store.Store
}

// newTestEnv mirrors the createTestServer pattern from server_test.go: an
// in-memory SQLite store paired with a server constructed via server.New.
// pricingPath is forwarded to cfg.Pricing.TablePath; pass "" to skip pricing.
func newTestEnv(t *testing.T, pricingPath string) *testEnv {
	t.Helper()

	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := &config.Config{}
	cfg.Database.Path = ":memory:"
	cfg.HTTP.Port = 0
	cfg.Pricing.TablePath = pricingPath
	cfg.Subscription.MonthlyUSD = 20.0
	cfg.Subscription.BillingCycleDays = 30
	cfg.Slack.QuietPeriodSeconds = 300
	cfg.Slack.ReleaseThreshold = 0.10
	cfg.Slack.BaselineMaxAgeHours = 48
	cfg.Slack.BaselineDriftThreshold = 0.25

	return &testEnv{srv: server.New(s, cfg), store: s}
}

// do issues an HTTP request directly into the server's mux. Using ServeHTTP
// keeps tests fast and avoids binding a TCP port.
func (e *testEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, req)
	return w
}

// Scenario 1: CLI Mode A posts events, /discount and /slack respond with the
// documented shapes.
func TestE2E_CLIModeA_DiscountAndSlack(t *testing.T) {
	env := newTestEnv(t, "")

	events := []map[string]any{
		{"input_tokens": 100, "output_tokens": 50, "session_id": "s1", "message_id": "m1", "source": "cli", "cost_usd": 0.01},
		{"input_tokens": 200, "output_tokens": 100, "session_id": "s1", "message_id": "m2", "source": "cli", "cost_usd": 0.02},
		{"input_tokens": 150, "output_tokens": 75, "session_id": "s2", "message_id": "m1", "source": "cli", "cost_usd": 0.015},
	}
	for i, ev := range events {
		w := env.do(t, "POST", "/log", ev)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /log #%d: status=%d body=%s", i, w.Code, w.Body.String())
		}
	}

	// /discount: documented top-level fields per docs/discount-calculation.md.
	w := env.do(t, "GET", "/discount?period=24h", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /discount: status=%d body=%s", w.Code, w.Body.String())
	}
	var disc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &disc); err != nil {
		t.Fatalf("decode /discount: %v", err)
	}
	for _, k := range []string{
		"period", "period_start", "period_end",
		"consumed_usd_equivalent", "subscription_cost_prorated_usd",
		"savings_usd", "events_total",
		"events_with_reported_cost", "events_with_computed_cost", "events_without_cost",
		"cost_coverage_pct",
	} {
		if _, ok := disc[k]; !ok {
			t.Errorf("/discount missing field %q: %s", k, w.Body.String())
		}
	}
	if got := disc["events_total"].(float64); got != 3 {
		t.Errorf("events_total=%v, want 3", got)
	}
	if got := disc["events_with_reported_cost"].(float64); got != 3 {
		t.Errorf("events_with_reported_cost=%v, want 3", got)
	}
	if got := disc["events_with_computed_cost"].(float64); got != 0 {
		t.Errorf("events_with_computed_cost=%v, want 0", got)
	}
	if got := disc["events_without_cost"].(float64); got != 0 {
		t.Errorf("events_without_cost=%v, want 0", got)
	}
	consumed, _ := disc["consumed_usd_equivalent"].(float64)
	if math.Abs(consumed-0.045) > 1e-9 {
		t.Errorf("consumed_usd_equivalent=%v, want ~0.045", consumed)
	}

	// /slack: documented top-level keys + gates per docs/slack-indicator.md.
	w = env.do(t, "GET", "/slack", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /slack: status=%d body=%s", w.Code, w.Body.String())
	}
	var slackResp map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &slackResp); err != nil {
		t.Fatalf("decode /slack: %v", err)
	}
	for _, k := range []string{
		"now", "five_hour", "weekly", "slack_combined_fraction",
		"priority_quiet_for_seconds", "paused", "release_recommended", "gates",
	} {
		if _, ok := slackResp[k]; !ok {
			t.Errorf("/slack missing top-level key %q: %s", k, w.Body.String())
		}
	}
	var gates map[string]bool
	if err := json.Unmarshal(slackResp["gates"], &gates); err != nil {
		t.Fatalf("decode gates: %v", err)
	}
	for _, k := range []string{"headroom", "priority_quiet", "baseline_freshness", "not_paused"} {
		if _, ok := gates[k]; !ok {
			t.Errorf("/slack missing gate %q", k)
		}
	}
}

// Scenario 2: posting the same (session_id, message_id) twice keeps exactly
// one row; the second insert is rejected by the UNIQUE index.
func TestE2E_DuplicateDetection(t *testing.T) {
	env := newTestEnv(t, "")

	payload := map[string]any{
		"input_tokens": 100, "output_tokens": 50,
		"session_id": "s1", "message_id": "m1",
		"source": "cli",
	}

	if w := env.do(t, "POST", "/log", payload); w.Code != http.StatusOK {
		t.Fatalf("first POST /log: status=%d body=%s", w.Code, w.Body.String())
	}

	w := env.do(t, "POST", "/log", payload)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("second POST /log: expected 500 from UNIQUE constraint, got %d (%s)",
			w.Code, w.Body.String())
	}

	var count int
	if err := env.store.DB().QueryRow(
		`SELECT COUNT(*) FROM usage_events WHERE session_id = ? AND message_id = ?`,
		"s1", "m1",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("usage_events row count for (s1,m1)=%d, want 1", count)
	}
}

// Scenario 3: a snapshot creates 5-hour and weekly windows whose
// baseline_total reflects the snapshot's totals.
func TestE2E_SnapshotAndWindowDerivation(t *testing.T) {
	env := newTestEnv(t, "")

	now := time.Now().UTC()
	const fiveHourTotal = 100.0
	const weeklyTotal = 2000.0

	snap := map[string]any{
		"observed_at":           now.Format(time.RFC3339Nano),
		"source":                "userscript",
		"five_hour_remaining":   80.0,
		"five_hour_total":       fiveHourTotal,
		"five_hour_window_ends": now.Add(5 * time.Hour).Format(time.RFC3339Nano),
		"weekly_remaining":      1500.0,
		"weekly_total":          weeklyTotal,
		"weekly_window_ends":    now.Add(48 * time.Hour).Format(time.RFC3339Nano),
	}
	w := env.do(t, "POST", "/snapshot", snap)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /snapshot: status=%d body=%s", w.Code, w.Body.String())
	}

	rows, err := env.store.DB().Query(`SELECT kind, baseline_total FROM windows`)
	if err != nil {
		t.Fatalf("query windows: %v", err)
	}
	defer rows.Close()

	got := map[string]sql.NullFloat64{}
	for rows.Next() {
		var kind string
		var baseline sql.NullFloat64
		if err := rows.Scan(&kind, &baseline); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[kind] = baseline
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	fh, ok := got["five_hour"]
	if !ok {
		t.Fatalf("expected a five_hour window, got rows for kinds=%v", got)
	}
	if !fh.Valid || fh.Float64 != fiveHourTotal {
		t.Errorf("five_hour baseline_total=%v, want %v", fh, fiveHourTotal)
	}

	wk, ok := got["weekly"]
	if !ok {
		t.Fatalf("expected a weekly window, got rows for kinds=%v", got)
	}
	// The weekly window picks the snapshot's weekly_total via the in-window
	// baseline correction pass; this is the post-correction value.
	if !wk.Valid || wk.Float64 != weeklyTotal {
		t.Errorf("weekly baseline_total=%v, want %v", wk, weeklyTotal)
	}
}

// Scenario 4: cost resolution across the three sources — reported, computed
// from the price table, and unresolved (NULL).
func TestE2E_CostResolution(t *testing.T) {
	// Resolve the price table relative to the test file so the test is
	// independent of how `go test` is invoked.
	pricePath, err := filepath.Abs(pricesExampleYAML)
	if err != nil {
		t.Fatalf("abs price path: %v", err)
	}

	env := newTestEnv(t, pricePath)

	const model = "claude-3-5-sonnet-20241022"
	reported := 0.05

	// Event 1: reported cost.
	if w := env.do(t, "POST", "/log", map[string]any{
		"input_tokens": 1000, "output_tokens": 500,
		"cost_usd":   reported,
		"model":      model,
		"session_id": "s1", "message_id": "m1",
		"source": "cli",
	}); w.Code != http.StatusOK {
		t.Fatalf("event 1: status=%d body=%s", w.Code, w.Body.String())
	}

	// Event 2: computed via price table.
	if w := env.do(t, "POST", "/log", map[string]any{
		"input_tokens": 1000, "output_tokens": 500,
		"model":      model,
		"session_id": "s1", "message_id": "m2",
		"source": "cli",
	}); w.Code != http.StatusOK {
		t.Fatalf("event 2: status=%d body=%s", w.Code, w.Body.String())
	}

	// Event 3: no cost info, no model.
	if w := env.do(t, "POST", "/log", map[string]any{
		"input_tokens": 1000, "output_tokens": 500,
		"session_id": "s1", "message_id": "m3",
		"source": "cli",
	}); w.Code != http.StatusOK {
		t.Fatalf("event 3: status=%d body=%s", w.Code, w.Body.String())
	}

	rows, err := env.store.DB().Query(`
		SELECT cost_source, cost_usd_equivalent
		FROM usage_events
		ORDER BY id
	`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type costRow struct {
		source string
		cost   sql.NullFloat64
	}
	var observed []costRow
	for rows.Next() {
		var r costRow
		if err := rows.Scan(&r.source, &r.cost); err != nil {
			t.Fatalf("scan: %v", err)
		}
		observed = append(observed, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(observed) != 3 {
		t.Fatalf("expected 3 events, got %d", len(observed))
	}

	if observed[0].source != "reported" {
		t.Errorf("event 1 cost_source=%q, want reported", observed[0].source)
	}
	if !observed[0].cost.Valid || observed[0].cost.Float64 != reported {
		t.Errorf("event 1 cost=%v, want %v", observed[0].cost, reported)
	}

	if observed[1].source != "computed" {
		t.Errorf("event 2 cost_source=%q, want computed", observed[1].source)
	}
	// 1000 input * $3/M + 500 output * $15/M = 0.003 + 0.0075 = 0.0105.
	const expectedComputed = 0.0105
	if !observed[1].cost.Valid || math.Abs(observed[1].cost.Float64-expectedComputed) > 1e-9 {
		t.Errorf("event 2 cost=%v, want ~%v", observed[1].cost, expectedComputed)
	}

	if observed[2].cost.Valid {
		t.Errorf("event 3 cost expected NULL, got %v", observed[2].cost.Float64)
	}
}

// Scenario 5: snapshot creates a 5-hour window; /log records consumption;
// /slack/release records an audit row resolved to that window.
func TestE2E_SlackReleaseFlow(t *testing.T) {
	env := newTestEnv(t, "")

	now := time.Now().UTC()
	snap := map[string]any{
		"observed_at":           now.Format(time.RFC3339Nano),
		"source":                "userscript",
		"five_hour_remaining":   50.0,
		"five_hour_total":       100.0,
		"five_hour_window_ends": now.Add(5 * time.Hour).Format(time.RFC3339Nano),
	}
	if w := env.do(t, "POST", "/snapshot", snap); w.Code != http.StatusOK {
		t.Fatalf("POST /snapshot: status=%d body=%s", w.Code, w.Body.String())
	}

	var wantWindowID int64
	if err := env.store.DB().QueryRow(
		`SELECT id FROM windows WHERE kind = 'five_hour' AND closed = 0`,
	).Scan(&wantWindowID); err != nil {
		t.Fatalf("find seeded window: %v", err)
	}

	if w := env.do(t, "POST", "/log", map[string]any{
		"input_tokens": 100, "output_tokens": 50, "cost_usd": 0.01,
		"session_id": "s1", "message_id": "m1",
		"source": "cli",
	}); w.Code != http.StatusOK {
		t.Fatalf("POST /log: status=%d body=%s", w.Code, w.Body.String())
	}

	// /slack must compute without error and expose the documented field.
	w := env.do(t, "GET", "/slack", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /slack: status=%d body=%s", w.Code, w.Body.String())
	}
	var sl map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &sl); err != nil {
		t.Fatalf("decode /slack: %v", err)
	}
	if _, ok := sl["release_recommended"]; !ok {
		t.Errorf("/slack missing release_recommended: %s", w.Body.String())
	}

	releaseTime := time.Now().UTC()
	estimatedCost := 0.02
	slackAt := 0.49
	rel := map[string]any{
		"released_at":      releaseTime.Format(time.RFC3339Nano),
		"job_tag":          "batch-job-1",
		"estimated_cost":   estimatedCost,
		"slack_at_release": slackAt,
		"window_kind":      "five_hour",
	}
	w = env.do(t, "POST", "/slack/release", rel)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /slack/release: status=%d body=%s", w.Code, w.Body.String())
	}

	var (
		jobTag     string
		dbCost     sql.NullFloat64
		dbSlackAt  sql.NullFloat64
		windowID   int64
		windowKind string
	)
	err := env.store.DB().QueryRow(`
		SELECT sr.job_tag, sr.estimated_cost, sr.slack_at_release, sr.window_id, w.kind
		FROM slack_releases sr
		JOIN windows w ON w.id = sr.window_id
		ORDER BY sr.id DESC
		LIMIT 1
	`).Scan(&jobTag, &dbCost, &dbSlackAt, &windowID, &windowKind)
	if err != nil {
		t.Fatalf("query slack_releases: %v", err)
	}
	if jobTag != "batch-job-1" {
		t.Errorf("job_tag=%q, want batch-job-1", jobTag)
	}
	if !dbCost.Valid || dbCost.Float64 != estimatedCost {
		t.Errorf("estimated_cost=%v, want %v", dbCost, estimatedCost)
	}
	if !dbSlackAt.Valid || dbSlackAt.Float64 != slackAt {
		t.Errorf("slack_at_release=%v, want %v", dbSlackAt, slackAt)
	}
	if windowKind != "five_hour" {
		t.Errorf("release window kind=%q, want five_hour", windowKind)
	}
	if windowID != wantWindowID {
		t.Errorf("release window_id=%d, want %d (snapshot-created window)", windowID, wantWindowID)
	}
}

// Scenario 6: a parse error round-trips through POST /parse_error and
// reappears in the parse_errors table verbatim.
func TestE2E_ParseErrorRoundTrip(t *testing.T) {
	env := newTestEnv(t, "")

	const (
		source  = "tailer"
		reason  = "malformed JSON line"
		payload = "{bad: json}"
	)

	w := env.do(t, "POST", "/parse_error", map[string]any{
		"source":  source,
		"reason":  reason,
		"payload": payload,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /parse_error: status=%d body=%s", w.Code, w.Body.String())
	}

	var (
		gotSource, gotReason, gotPayload string
	)
	err := env.store.DB().QueryRow(
		`SELECT source, reason, payload FROM parse_errors ORDER BY id DESC LIMIT 1`,
	).Scan(&gotSource, &gotReason, &gotPayload)
	if err != nil {
		t.Fatalf("query parse_errors: %v", err)
	}
	if gotSource != source {
		t.Errorf("source=%q, want %q", gotSource, source)
	}
	if gotReason != reason {
		t.Errorf("reason=%q, want %q", gotReason, reason)
	}
	if gotPayload != payload {
		t.Errorf("payload=%q, want %q", gotPayload, payload)
	}
}
