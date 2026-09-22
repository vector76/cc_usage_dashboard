package consumption

import (
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// The report splits cache writes by TTL because the two bill at different
// rates (1.25x vs 2x input). cache_creation_tokens stays the total; the 5m
// share is what remains after the 1h share. A row with no recorded split
// (NULL 1h, e.g. a hook row from before the split was sent) counts as 5m,
// which is how it was priced.
func TestBreakdown_SplitsCacheWritesBy5mAnd1h(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	c, s := newCalc(t, now)
	defer s.Close()

	for i, r := range []store.UsageEventRecord{
		{CacheCreationTokens: 1000, CacheCreation1hTokens: 900},
		{CacheCreationTokens: 500, CacheCreation1hTokens: 500},
	} {
		r.OccurredAt = now.Add(-time.Hour)
		r.Source = "api"
		r.SessionID = "s"
		r.MessageID = "m" + time.Duration(i).String()
		r.Model = "claude-opus-5-5"
		r.InputTokens = 1
		if _, err := s.InsertUsageEventRecord(r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// A pre-split row: cache_creation_1h_tokens NULL.
	if _, err := s.DB().Exec(`
		INSERT INTO usage_events (occurred_at, source, input_tokens, output_tokens, cache_creation_tokens, model)
		VALUES (?, 'hook', 1, 0, 200, 'claude-opus-5-5')
	`, store.FormatTime(now.Add(-time.Hour))); err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	b, err := c.Breakdown(now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	m := modelRow(t, b, "claude-opus-5-5")
	if m.CacheCreationTokens != 1700 {
		t.Errorf("total cache writes = %d, want 1700", m.CacheCreationTokens)
	}
	if m.CacheCreation1hTokens != 1400 {
		t.Errorf("1h cache writes = %d, want 1400", m.CacheCreation1hTokens)
	}
	if m.CacheCreation5mTokens != 300 {
		t.Errorf("5m cache writes = %d, want 300", m.CacheCreation5mTokens)
	}
}
