package server

import (
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/oauthusage"
)

func TestSnapshotRequestFromOAuth(t *testing.T) {
	observed := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	sessionEnds := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	weeklyEnds := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	session, weekly, fable := 3.0, 29.0, 1.0
	continuous := true

	got := SnapshotRequestFromOAuth(oauthusage.Snapshot{
		Reading: oauthusage.Reading{
			SessionUsed:       &session,
			SessionWindowEnds: &sessionEnds,
			WeeklyUsed:        &weekly,
			WeeklyWindowEnds:  &weeklyEnds,
			FableWeeklyUsed:   &fable,
		},
		ObservedAt:         observed,
		ContinuousWithPrev: &continuous,
	})

	if got.Source != SourceOAuth {
		t.Errorf("Source = %q, want %q", got.Source, SourceOAuth)
	}
	if !got.ObservedAt.Equal(observed) {
		t.Errorf("ObservedAt = %s, want %s", got.ObservedAt, observed)
	}
	if got.SessionUsed == nil || *got.SessionUsed != 3 {
		t.Errorf("SessionUsed = %v, want 3", got.SessionUsed)
	}
	if got.WeeklyUsed == nil || *got.WeeklyUsed != 29 {
		t.Errorf("WeeklyUsed = %v, want 29", got.WeeklyUsed)
	}
	if got.FableWeeklyUsed == nil || *got.FableWeeklyUsed != 1 {
		t.Errorf("FableWeeklyUsed = %v, want 1", got.FableWeeklyUsed)
	}
	if got.SessionWindowEnds == nil || !got.SessionWindowEnds.Equal(sessionEnds) {
		t.Errorf("SessionWindowEnds = %v, want %s", got.SessionWindowEnds, sessionEnds)
	}
	if got.WeeklyWindowEnds == nil || !got.WeeklyWindowEnds.Equal(weeklyEnds) {
		t.Errorf("WeeklyWindowEnds = %v, want %s", got.WeeklyWindowEnds, weeklyEnds)
	}
	if got.ContinuousWithPrev == nil || !*got.ContinuousWithPrev {
		t.Error("ContinuousWithPrev should carry through as true")
	}
}

// TestSnapshotRequestFromOAuthLeavesActivityUnknown keeps the limbo signal
// honest. The API's is_active does not mean "this window is open" (it read
// false while session utilization climbed 3% -> 7%), so this source has no
// information to offer and must leave the fields absent — which the
// tri-state convention already reads as "unknown". The userscript remains
// the only source that reports limbo.
func TestSnapshotRequestFromOAuthLeavesActivityUnknown(t *testing.T) {
	session := 3.0
	got := SnapshotRequestFromOAuth(oauthusage.Snapshot{
		Reading:    oauthusage.Reading{SessionUsed: &session},
		ObservedAt: time.Now(),
	})
	if got.SessionActive != nil {
		t.Errorf("SessionActive = %v, want nil", *got.SessionActive)
	}
	if got.WeeklyActive != nil {
		t.Errorf("WeeklyActive = %v, want nil", *got.WeeklyActive)
	}
}

// TestSourceOAuthIsDistinctFromUserscript is what makes running both
// sources at once useful rather than confusing: their rows stay separable,
// so they can be compared instead of silently merged. Disagreement is
// data.
func TestSourceOAuthIsDistinctFromUserscript(t *testing.T) {
	if SourceOAuth == "userscript" {
		t.Fatal("the OAuth source must not share the userscript's source tag")
	}
}
