package oauthusage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestLiveEndpoint is the canary for schema drift. It is skipped unless
// OAUTH_USAGE_LIVE=1, because it makes a real request to
// api.anthropic.com using this machine's Claude Code credentials, which no
// ordinary `go test ./...` should do.
//
// Run it by hand after any change to the mapper, and whenever the
// dashboard's OAuth rows look wrong:
//
//	OAUTH_USAGE_LIVE=1 go test ./internal/oauthusage/ -run Live -v
//
// It asserts only what the dashboard actually depends on. A tightened
// assertion here (exact percentages, a fixed set of limit kinds) would
// fail for ordinary reasons — usage moves — and teach you to ignore it.
//
// A stale credential skips rather than fails: an idle machine holds an
// expired token routinely, and that says nothing about whether the schema
// still matches.
func TestLiveEndpoint(t *testing.T) {
	if os.Getenv("OAUTH_USAGE_LIVE") == "" {
		t.Skip("set OAUTH_USAGE_LIVE=1 to exercise the real endpoint")
	}

	path := ResolveCredentialsPath("")
	cred, err := LoadCredential(path)
	if err != nil {
		if errors.Is(err, ErrCredentialStale) {
			t.Skipf("no usable credential at %s: %v", path, err)
		}
		t.Fatalf("LoadCredential(%s): %v", path, err)
	}
	if !cred.UsableAt(time.Now()) {
		t.Skipf("credential expired at %s; run Claude Code to refresh it", cred.ExpiresAt)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	reading, err := NewClient().Fetch(ctx, cred.AccessToken)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The two rows the dashboard cannot do without. Their absence means
	// limits[] has been reshaped and the mapper needs attention.
	if reading.SessionUsed == nil {
		t.Error("no session row in the live response; limits[] may have been reshaped")
	}
	if reading.WeeklyUsed == nil {
		t.Error("no weekly_all row in the live response; limits[] may have been reshaped")
	}
	if reading.SessionWindowEnds == nil {
		t.Error("no session resets_at in the live response")
	}

	// Truncation is the rule the stored value depends on; verify it holds
	// against a real timestamp, not just the recorded fixtures.
	if e := reading.SessionWindowEnds; e != nil && e.Nanosecond() != 0 {
		t.Errorf("session window ends carries sub-second precision: %s",
			e.Format(time.RFC3339Nano))
	}

	t.Logf("live reading: session=%v weekly=%v fable=%v session_ends=%v weekly_ends=%v",
		deref(reading.SessionUsed), deref(reading.WeeklyUsed), deref(reading.FableWeeklyUsed),
		reading.SessionWindowEnds, reading.WeeklyWindowEnds)
}

func deref(f *float64) any {
	if f == nil {
		return "absent"
	}
	return *f
}
