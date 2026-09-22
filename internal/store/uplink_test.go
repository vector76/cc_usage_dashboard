package store

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// An unknown peer starts at 0, which means "forward everything". A fresh
// sender must not silently skip the history it already holds.
func TestGetUplinkCursorDefaultsToZero(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetUplinkCursor("http://host:27812")
	if err != nil {
		t.Fatalf("GetUplinkCursor: %v", err)
	}
	if got != 0 {
		t.Errorf("expected 0 for an unknown peer, got %d", got)
	}
}

func TestSetUplinkCursorRoundTrips(t *testing.T) {
	s := newTestStore(t)
	const url = "http://host:27812"

	if err := s.SetUplinkCursor(url, 42); err != nil {
		t.Fatalf("SetUplinkCursor: %v", err)
	}
	got, err := s.GetUplinkCursor(url)
	if err != nil {
		t.Fatalf("GetUplinkCursor: %v", err)
	}
	if got != 42 {
		t.Errorf("expected cursor 42, got %d", got)
	}

	if err := s.SetUplinkCursor(url, 99); err != nil {
		t.Fatalf("SetUplinkCursor update: %v", err)
	}
	got, _ = s.GetUplinkCursor(url)
	if got != 99 {
		t.Errorf("expected cursor to advance to 99, got %d", got)
	}
}

// The cursor is keyed by peer so retargeting the uplink re-sends the backlog
// to the new receiver, which is correct: the new receiver holds none of it.
func TestUplinkCursorIsPerURL(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetUplinkCursor("http://a:27812", 100); err != nil {
		t.Fatalf("SetUplinkCursor: %v", err)
	}
	got, err := s.GetUplinkCursor("http://b:27812")
	if err != nil {
		t.Fatalf("GetUplinkCursor: %v", err)
	}
	if got != 0 {
		t.Errorf("a different peer must start at 0, got %d", got)
	}
}

func insertEvent(t *testing.T, s *Store, sessionID, messageID string) int64 {
	t.Helper()
	id, err := s.InsertUsageEvent(
		time.Now(), "tailer", sessionID, messageID, "/proj", "claude-opus-5",
		100, 50, 10, 5, nil, "computed", "{}",
	)
	if err != nil {
		t.Fatalf("InsertUsageEvent: %v", err)
	}
	return id
}

func TestForwardableEventsAfterReturnsNewerEventsInIDOrder(t *testing.T) {
	s := newTestStore(t)
	id1 := insertEvent(t, s, "s1", "m1")
	id2 := insertEvent(t, s, "s1", "m2")
	id3 := insertEvent(t, s, "s1", "m3")

	got, err := s.ForwardableEventsAfter(id1, 10)
	if err != nil {
		t.Fatalf("ForwardableEventsAfter: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events after id %d, got %d", id1, len(got))
	}
	if got[0].ID != id2 || got[1].ID != id3 {
		t.Errorf("expected ids [%d %d] in order, got [%d %d]", id2, id3, got[0].ID, got[1].ID)
	}
}

// Only events with both dedup keys are forwardable. Those are the columns
// the receiver's UNIQUE(session_id, message_id) constraint covers, so they
// are the only ones a retry can re-send without double-counting.
func TestForwardableEventsAfterSkipsEventsMissingDedupKeys(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "", "m1")
	insertEvent(t, s, "s1", "")
	wanted := insertEvent(t, s, "s2", "m2")

	got, err := s.ForwardableEventsAfter(0, 10)
	if err != nil {
		t.Fatalf("ForwardableEventsAfter: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected only the fully-keyed event, got %d", len(got))
	}
	if got[0].ID != wanted {
		t.Errorf("expected id %d, got %d", wanted, got[0].ID)
	}
}

func TestForwardableEventsAfterRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		insertEvent(t, s, "s1", string(rune('a'+i)))
	}

	got, err := s.ForwardableEventsAfter(0, 2)
	if err != nil {
		t.Fatalf("ForwardableEventsAfter: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected limit of 2 to be honored, got %d", len(got))
	}
}

func TestForwardableEventsAfterCarriesPayloadFields(t *testing.T) {
	s := newTestStore(t)
	occurred := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	if _, err := s.InsertUsageEvent(
		occurred, "tailer", "sess", "msg", "/some/project", "claude-opus-5",
		111, 222, 333, 444, nil, "computed", "{}",
	); err != nil {
		t.Fatalf("InsertUsageEvent: %v", err)
	}

	got, err := s.ForwardableEventsAfter(0, 10)
	if err != nil {
		t.Fatalf("ForwardableEventsAfter: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e := got[0]
	if e.SessionID != "sess" || e.MessageID != "msg" {
		t.Errorf("dedup keys not carried: %q / %q", e.SessionID, e.MessageID)
	}
	if e.ProjectPath != "/some/project" || e.Model != "claude-opus-5" {
		t.Errorf("project/model not carried: %q / %q", e.ProjectPath, e.Model)
	}
	if e.InputTokens != 111 || e.OutputTokens != 222 ||
		e.CacheCreationTokens != 333 || e.CacheReadTokens != 444 {
		t.Errorf("token counts not carried: %+v", e)
	}
	if !e.OccurredAt.Equal(occurred) {
		t.Errorf("expected occurred_at %v, got %v", occurred, e.OccurredAt)
	}
}
