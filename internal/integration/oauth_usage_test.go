package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/oauthusage"
	"github.com/vector76/cc_usage_dashboard/internal/server"
)

// TestOAuthPollerWritesSnapshotToStore closes the last gap the unit tests
// leave open: that a reading actually lands in the database, through the
// same path the userscript's POST takes, tagged as its own source.
//
// Everything below the stub endpoint is real — real Store, real Server,
// real RecordSnapshot, real window derivation.
func TestOAuthPollerWritesSnapshotToStore(t *testing.T) {
	env := newTestEnv(t, "")

	// A live capture, with the reset boundaries rebased onto now so the
	// server's timestamp validation sees a plausible current reading
	// rather than a fixture from the past.
	now := time.Now().UTC()
	sessionEnds := now.Add(2 * time.Hour).Truncate(time.Second)
	weeklyEnds := now.Add(30 * time.Hour).Truncate(time.Second)
	body := `{"limits":[
	  {"kind":"session","percent":11,"resets_at":"` + sessionEnds.Format(time.RFC3339Nano) + `","scope":null,"is_active":false},
	  {"kind":"weekly_all","percent":30,"resets_at":"` + weeklyEnds.Format(time.RFC3339Nano) + `","scope":null,"is_active":true},
	  {"kind":"weekly_scoped","percent":1,"resets_at":"` + weeklyEnds.Format(time.RFC3339Nano) + `",
	   "scope":{"model":{"display_name":"Fable"}},"is_active":false}
	]}`

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(api.Close)

	credPath := filepath.Join(t.TempDir(), ".credentials.json")
	expires := now.Add(8 * time.Hour).UnixMilli()
	cred := `{"claudeAiOauth":{"accessToken":"tok","expiresAt":` + strconv.FormatInt(expires, 10) + `}}`
	if err := os.WriteFile(credPath, []byte(cred), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	client := oauthusage.NewClient()
	client.BaseURL = api.URL

	poller := oauthusage.NewPoller(oauthusage.PollerConfig{
		Interval:        time.Minute,
		CredentialsPath: credPath,
		Client:          client,
		Sink:            env.srv.OAuthSink(),
	})

	if err := pollOnceForTest(t, poller); err != nil {
		t.Fatalf("poll: %v", err)
	}

	var (
		source      string
		sessionUsed float64
		weeklyUsed  float64
		fableUsed   *float64
		sessActive  *bool
		weekActive  *bool
		continuous  *bool
	)
	row := env.store.DB().QueryRow(`
		SELECT source, session_used, weekly_used, fable_weekly_used,
		       session_active, weekly_active, continuous_with_prev
		FROM quota_snapshots ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&source, &sessionUsed, &weeklyUsed, &fableUsed,
		&sessActive, &weekActive, &continuous); err != nil {
		t.Fatalf("no snapshot row was written: %v", err)
	}

	if source != server.SourceOAuth {
		t.Errorf("source = %q, want %q", source, server.SourceOAuth)
	}
	if sessionUsed != 11 {
		t.Errorf("session_used = %v, want 11", sessionUsed)
	}
	if weeklyUsed != 30 {
		t.Errorf("weekly_used = %v, want 30", weeklyUsed)
	}
	if fableUsed == nil || *fableUsed != 1 {
		t.Errorf("fable_weekly_used = %v, want 1", fableUsed)
	}
	// is_active was present on every row of the response and must not have
	// been mapped onto the limbo columns.
	if sessActive != nil {
		t.Errorf("session_active = %v, want NULL (unknown)", *sessActive)
	}
	if weekActive != nil {
		t.Errorf("weekly_active = %v, want NULL (unknown)", *weekActive)
	}
	// Cold start: this is the first reading, so it begins a segment.
	if continuous == nil || *continuous {
		t.Error("continuous_with_prev should be false on the first reading")
	}
}

// pollOnceForTest drives exactly one poll. Start would work too, but a
// single deterministic poll keeps the assertion from racing the ticker.
func pollOnceForTest(t *testing.T, p *oauthusage.Poller) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.PollOnce(ctx)
}
