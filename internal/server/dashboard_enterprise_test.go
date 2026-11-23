package server

import (
	"testing"
	"time"
)

// Enterprise mode is decided by the injected predicate alone (the credentials
// plan, via cmd/trayapp), not by the OAuth poller: the state carries
// rolling session/weekly windows instead of quota or hypothetical ones.
func TestDashboardStateEnterpriseRollingWindows(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	now := time.Date(2026, 11, 23, 10, 7, 0, 0, time.UTC)
	srv.dashboardHandler.SetNow(func() time.Time { return now })
	srv.SetEnterprise(func() bool { return true })

	state := fetchDashboardState(t, srv)
	if !state.Enterprise {
		t.Fatal("state.Enterprise = false")
	}
	if state.SessionActive {
		t.Error("SessionActive = true in enterprise mode")
	}
	if state.Session == nil || !state.Session.Rolling || state.Session.Hypothetical {
		t.Fatalf("session = %+v, want rolling", state.Session)
	}
	if got := state.Session.EndsAt.Sub(state.Session.StartedAt); got != 15*time.Hour {
		t.Errorf("session span = %v, want 15h", got)
	}
	if state.Weekly == nil || !state.Weekly.Rolling {
		t.Fatalf("weekly = %+v, want rolling", state.Weekly)
	}
	if got := state.Weekly.EndsAt.Sub(state.Weekly.StartedAt); got != 7*24*time.Hour {
		t.Errorf("weekly span = %v, want 7d", got)
	}
}

func TestDashboardStateNonEnterpriseUnchanged(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	srv.SetEnterprise(func() bool { return false })
	state := fetchDashboardState(t, srv)
	if state.Enterprise || (state.Session != nil && state.Session.Rolling) {
		t.Errorf("non-enterprise plan switched on rolling mode: %+v", state)
	}
}
