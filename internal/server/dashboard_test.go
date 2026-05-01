package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/dashboard"
	"github.com/vector76/cc_usage_dashboard/internal/store"
)

func TestDashboardIndexHTML(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	for _, path := range []string{"/", "/dashboard"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			ct := w.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("expected text/html content type, got %q", ct)
			}
			if w.Body.Len() == 0 {
				t.Error("expected non-empty body")
			}
			body := w.Body.String()
			if !strings.Contains(body, "<html") {
				snippet := body
				if len(snippet) > 120 {
					snippet = snippet[:120]
				}
				t.Errorf("response body does not look like HTML: %q", snippet)
			}
		})
	}
}

func TestDashboardStateJSONShape(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	// Seed a parse error and an event so the state has something to report.
	if _, err := testStore.InsertParseError(time.Now().UTC(), "tailer", "bad", "{}"); err != nil {
		t.Fatalf("insert parse error: %v", err)
	}
	cost := 0.10
	if _, err := testStore.InsertUsageEvent(
		time.Now().UTC(), "api", "session-d1", "msg-d1", "/proj", "model-x",
		1000, 500, 0, 0, &cost, "reported", "{}",
	); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/dashboard/state", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected application/json, got %q", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody=%s", err, w.Body.String())
	}

	for _, key := range []string{
		"now",
		"session",
		"weekly",
		"last_snapshot_age_seconds",
		"parse_errors_24h",
		"paused",
		"slack_release_recommended",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing documented field %q", key)
		}
	}

	// Decode the typed fields we care about.
	var pe int64
	if err := json.Unmarshal(raw["parse_errors_24h"], &pe); err != nil {
		t.Fatalf("parse_errors_24h not an int: %v", err)
	}
	if pe < 1 {
		t.Errorf("expected parse_errors_24h >= 1, got %d", pe)
	}

	var paused bool
	if err := json.Unmarshal(raw["paused"], &paused); err != nil {
		t.Fatalf("paused not a bool: %v", err)
	}
	if paused {
		t.Error("expected paused=false on fresh server")
	}
}

// fetchDashboardState issues GET /api/dashboard/state and decodes the
// response into a dashboard.State. Fails the test on any non-200 or
// decode error.
func fetchDashboardState(t *testing.T, srv *Server) *dashboard.State {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/dashboard/state", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	var s dashboard.State
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode state: %v\nbody=%s", err, w.Body.String())
	}
	return &s
}

// TestDashboardStateNoOpenSessionSynthesizesHypothetical covers properties
// (a)–(d) from the bead: when the windows table has no open session row,
// the response reports session_active=false and a hypothetical WindowState
// pinned to the handler's injected clock, with BaselinePercentUsed=0 and
// pre-window snapshot history loaded into Series for [now-10h, now].
func TestDashboardStateNoOpenSessionSynthesizesHypothetical(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	srv.dashboardHandler.SetNow(func() time.Time { return now })

	// Pre-window history: one snapshot inside the [now-10h, now] window
	// (3h ago) and one well outside it (12h ago) to verify the bounds.
	used := 42.5
	if _, err := testStore.InsertQuotaSnapshot(
		now.Add(-3*time.Hour), now.Add(-3*time.Hour), "userscript",
		&used, nil, nil, nil, nil, nil, nil, "{}",
	); err != nil {
		t.Fatalf("insert in-history snapshot: %v", err)
	}
	out := 10.0
	if _, err := testStore.InsertQuotaSnapshot(
		now.Add(-12*time.Hour), now.Add(-12*time.Hour), "userscript",
		&out, nil, nil, nil, nil, nil, nil, "{}",
	); err != nil {
		t.Fatalf("insert out-of-history snapshot: %v", err)
	}

	state := fetchDashboardState(t, srv)

	// (a) session_active=false; Session present and Hypothetical=true.
	if state.SessionActive {
		t.Errorf("expected session_active=false, got true")
	}
	if state.Session == nil {
		t.Fatal("expected synthesized hypothetical Session, got nil")
	}
	if !state.Session.Hypothetical {
		t.Errorf("expected Session.Hypothetical=true, got false")
	}

	// (b) started_at and ends_at are exactly 5h apart and pinned to now.
	if !state.Session.StartedAt.Equal(now) {
		t.Errorf("StartedAt = %v, want %v", state.Session.StartedAt, now)
	}
	if !state.Session.EndsAt.Equal(now.Add(5 * time.Hour)) {
		t.Errorf("EndsAt = %v, want %v", state.Session.EndsAt, now.Add(5*time.Hour))
	}
	if span := state.Session.EndsAt.Sub(state.Session.StartedAt); span != 5*time.Hour {
		t.Errorf("EndsAt-StartedAt = %v, want 5h", span)
	}

	// (c) BaselinePercentUsed is 0.
	if state.Session.BaselinePercentUsed == nil {
		t.Fatal("expected BaselinePercentUsed pointer, got nil")
	}
	if *state.Session.BaselinePercentUsed != 0 {
		t.Errorf("BaselinePercentUsed = %v, want 0", *state.Session.BaselinePercentUsed)
	}

	// (d) Series loaded from real snapshots in [now-10h, now]: only the
	// 3h-ago point qualifies; the 12h-ago point falls outside the lookback.
	if len(state.Session.Series) != 1 {
		t.Fatalf("Series len = %d, want 1; got %+v", len(state.Session.Series), state.Session.Series)
	}
	pt := state.Session.Series[0]
	if !pt.ObservedAt.Equal(now.Add(-3 * time.Hour)) {
		t.Errorf("Series[0].ObservedAt = %v, want %v", pt.ObservedAt, now.Add(-3*time.Hour))
	}
	if pt.PercentUsed != used {
		t.Errorf("Series[0].PercentUsed = %v, want %v", pt.PercentUsed, used)
	}

	// No points should fall inside the synthesized window itself.
	for _, p := range state.Session.Series {
		if !p.ObservedAt.Before(now) {
			t.Errorf("Series point %v is at or after StartedAt=%v; in-window points must be empty",
				p.ObservedAt, now)
		}
	}
}

// TestDashboardStateNoOpenWeeklySynthesizesHypothetical: when no real
// open weekly window exists (e.g. because the engine refused to mint one
// under weekly limbo), the response carries a hypothetical Weekly window
// spanning [now, now+7d] with BaselinePercentUsed=0.
func TestDashboardStateNoOpenWeeklySynthesizesHypothetical(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	srv.dashboardHandler.SetNow(func() time.Time { return now })

	// Snapshot reports weekly limbo (and session limbo, just to keep the
	// state consistent — though only weekly is exercised here).
	inactive := false
	if _, err := testStore.InsertQuotaSnapshot(
		now, now, "userscript",
		nil, nil, nil, nil,
		&inactive, &inactive, nil, "{}",
	); err != nil {
		t.Fatalf("insert limbo snapshot: %v", err)
	}

	state := fetchDashboardState(t, srv)

	if state.Weekly == nil {
		t.Fatal("expected synthesized hypothetical Weekly, got nil")
	}
	if !state.Weekly.Hypothetical {
		t.Errorf("expected Weekly.Hypothetical=true, got false")
	}
	if !state.Weekly.StartedAt.Equal(now) {
		t.Errorf("Weekly.StartedAt = %v, want %v", state.Weekly.StartedAt, now)
	}
	wantEnds := now.Add(7 * 24 * time.Hour)
	if !state.Weekly.EndsAt.Equal(wantEnds) {
		t.Errorf("Weekly.EndsAt = %v, want %v", state.Weekly.EndsAt, wantEnds)
	}
	if state.Weekly.BaselinePercentUsed == nil || *state.Weekly.BaselinePercentUsed != 0 {
		t.Errorf("expected Weekly.BaselinePercentUsed=0, got %v", state.Weekly.BaselinePercentUsed)
	}
}

// TestDashboardStateOpenSessionUnchanged covers property (e): when a real
// open session window exists, session_active=true, the WindowState is not
// hypothetical, and ID/StartedAt/EndsAt mirror the row in the windows
// table.
func TestDashboardStateOpenSessionUnchanged(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	srv.dashboardHandler.SetNow(func() time.Time { return now })

	startedAt := now.Add(-1 * time.Hour)
	endsAt := startedAt.Add(5 * time.Hour)
	res, err := testStore.DB().Exec(`
		INSERT INTO windows (kind, started_at, ends_at, baseline_percent_used, closed)
		VALUES (?, ?, ?, ?, 0)
	`, "session", store.FormatTime(startedAt), store.FormatTime(endsAt), 12.5)
	if err != nil {
		t.Fatalf("insert open session window: %v", err)
	}
	wantID, _ := res.LastInsertId()

	// Also seed an open weekly window so the response carries no synthesized
	// hypothetical at all — the omitempty assertion below scans the full body.
	weeklyStart := now.Add(-2 * 24 * time.Hour)
	weeklyEnd := weeklyStart.Add(7 * 24 * time.Hour)
	if _, err := testStore.DB().Exec(`
		INSERT INTO windows (kind, started_at, ends_at, baseline_percent_used, closed)
		VALUES (?, ?, ?, ?, 0)
	`, "weekly", store.FormatTime(weeklyStart), store.FormatTime(weeklyEnd), 5.0); err != nil {
		t.Fatalf("insert open weekly window: %v", err)
	}

	state := fetchDashboardState(t, srv)

	if !state.SessionActive {
		t.Errorf("expected session_active=true with a real open window, got false")
	}
	if state.Session == nil {
		t.Fatal("expected Session, got nil")
	}
	if state.Session.Hypothetical {
		t.Errorf("expected Hypothetical=false for a real open window, got true")
	}
	if state.Session.ID != wantID {
		t.Errorf("Session.ID = %d, want %d", state.Session.ID, wantID)
	}
	if !state.Session.StartedAt.Equal(startedAt) {
		t.Errorf("Session.StartedAt = %v, want %v", state.Session.StartedAt, startedAt)
	}
	if !state.Session.EndsAt.Equal(endsAt) {
		t.Errorf("Session.EndsAt = %v, want %v", state.Session.EndsAt, endsAt)
	}
	if state.Session.BaselinePercentUsed == nil || *state.Session.BaselinePercentUsed != 12.5 {
		t.Errorf("BaselinePercentUsed = %v, want 12.5",
			state.Session.BaselinePercentUsed)
	}

	// The JSON should also omit the hypothetical key entirely (omitempty).
	req := httptest.NewRequest("GET", "/api/dashboard/state", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `"hypothetical"`) {
		t.Errorf("expected hypothetical field absent from JSON for real window; body=%s", w.Body.String())
	}
}

