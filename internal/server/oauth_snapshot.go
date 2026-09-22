package server

import (
	"github.com/vector76/cc_usage_dashboard/internal/oauthusage"
)

// SourceOAuth tags snapshots read from Claude Code's OAuth usage endpoint,
// as distinct from "userscript" rows scraped off claude.ai.
//
// Keeping them separable is the point: both sources may run at once, and
// the system never averages or splits the difference between sources.
// Disagreement is data — and while the two run side by side, their paired
// rows are also the experiment that will settle what the API's is_active
// field actually means.
const SourceOAuth = "oauth"

// SnapshotRequestFromOAuth adapts a poller reading to the snapshot shape
// every source shares.
//
// SessionActive and WeeklyActive are deliberately left nil: the API's
// is_active does not mean "this window is open and accruing" (observed
// false while session utilization climbed from 3% to 7%), so this source
// has nothing to say about limbo, and absent already means "unknown".
func SnapshotRequestFromOAuth(s oauthusage.Snapshot) SnapshotRequest {
	return SnapshotRequest{
		ObservedAt:         s.ObservedAt,
		Source:             SourceOAuth,
		SessionUsed:        s.SessionUsed,
		SessionWindowEnds:  s.SessionWindowEnds,
		WeeklyUsed:         s.WeeklyUsed,
		WeeklyWindowEnds:   s.WeeklyWindowEnds,
		FableWeeklyUsed:    s.FableWeeklyUsed,
		ContinuousWithPrev: s.ContinuousWithPrev,
	}
}

// OAuthSink returns a Sink that records poller readings through the same
// validate/store/derive path the userscript's POST takes.
func (s *Server) OAuthSink() oauthusage.Sink {
	return func(snap oauthusage.Snapshot) error {
		_, err := s.RecordSnapshot(SnapshotRequestFromOAuth(snap))
		return err
	}
}
