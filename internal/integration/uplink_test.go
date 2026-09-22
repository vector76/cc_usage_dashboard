package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
	"github.com/vector76/cc_usage_dashboard/internal/uplink"
)

// These exercise a real sender against a real receiver: a store + forwarder
// on one side, the actual server.Server mux behind an httptest listener on
// the other. The forwarder's payload struct is deliberately not the server's
// LogPostRequest type — the contract between them is the JSON — so the unit
// tests on each side would both stay green if a field name drifted. Only an
// end-to-end run catches that.

// senderStore is the VM-side database: a plain store with no server attached.
func senderStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// receiverServer stands up the host-side trayapp's HTTP surface on a real
// port so the forwarder reaches it as it would in production.
func receiverServer(t *testing.T) (*testEnv, string) {
	t.Helper()
	env := newTestEnv(t, pricesExampleYAML)
	ts := httptest.NewServer(env.srv)
	t.Cleanup(ts.Close)
	return env, ts.URL
}

func addSenderEvent(t *testing.T, s *store.Store, messageID string, occurredAt time.Time) int64 {
	t.Helper()
	id, err := s.InsertUsageEvent(
		occurredAt, "tailer", "vm-session", messageID, "/vm/project",
		"claude-opus-5", 1000, 500, 100, 50, nil, "computed", "{}",
	)
	if err != nil {
		t.Fatalf("InsertUsageEvent: %v", err)
	}
	return id
}

// The headline case: events recorded on the VM land in the host's database
// with their token counts and identity intact.
func TestE2E_UplinkDeliversEventsToReceiver(t *testing.T) {
	sender := senderStore(t)
	env, peerURL := receiverServer(t)

	occurred := time.Now().Add(-10 * time.Minute).UTC()
	addSenderEvent(t, sender, "vm-msg-1", occurred)
	addSenderEvent(t, sender, "vm-msg-2", occurred.Add(time.Minute))

	f := uplink.New(peerURL, sender)
	f.Start()
	t.Cleanup(f.Stop)

	waitForEventCount(t, env, 2)

	var sessionID, messageID, model, source string
	var in, out, cacheCreate, cacheRead int
	err := env.store.DB().QueryRow(`
		SELECT session_id, message_id, model, source,
		       input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens
		FROM usage_events WHERE message_id = 'vm-msg-1'
	`).Scan(&sessionID, &messageID, &model, &source, &in, &out, &cacheCreate, &cacheRead)
	if err != nil {
		t.Fatalf("read forwarded event: %v", err)
	}

	if sessionID != "vm-session" || messageID != "vm-msg-1" {
		t.Errorf("identity not preserved: %q / %q", sessionID, messageID)
	}
	if model != "claude-opus-5" {
		t.Errorf("expected model to survive the hop, got %q", model)
	}
	if in != 1000 || out != 500 || cacheCreate != 100 || cacheRead != 50 {
		t.Errorf("token counts mangled: in=%d out=%d cc=%d cr=%d", in, out, cacheCreate, cacheRead)
	}
	if source != "uplink" {
		t.Errorf("expected source 'uplink', got %q", source)
	}
}

// Cost is not forwarded, so the receiver must derive it from its own price
// table. A null cost here would mean the combined dollar figures silently
// undercount every forwarded event.
func TestE2E_UplinkEventsArePricedByReceiver(t *testing.T) {
	sender := senderStore(t)
	env, peerURL := receiverServer(t)
	addSenderEvent(t, sender, "vm-msg-1", time.Now().Add(-time.Minute).UTC())

	f := uplink.New(peerURL, sender)
	f.Start()
	t.Cleanup(f.Stop)
	waitForEventCount(t, env, 1)

	var cost float64
	var costSource string
	err := env.store.DB().QueryRow(`
		SELECT cost_usd_equivalent, cost_source FROM usage_events WHERE message_id = 'vm-msg-1'
	`).Scan(&cost, &costSource)
	if err != nil {
		t.Fatalf("read cost: %v", err)
	}
	if cost <= 0 {
		t.Errorf("expected the receiver to price the event, got %v", cost)
	}
	if costSource != "computed" {
		t.Errorf("expected cost_source 'computed' (receiver's table), got %q", costSource)
	}
}

// Re-running the whole backlog against a receiver that already has it must
// not double-count. This is what makes at-least-once delivery safe.
func TestE2E_UplinkRedeliveryDoesNotDoubleCount(t *testing.T) {
	sender := senderStore(t)
	env, peerURL := receiverServer(t)
	addSenderEvent(t, sender, "vm-msg-1", time.Now().Add(-time.Minute).UTC())

	f := uplink.New(peerURL, sender)
	f.Start()
	t.Cleanup(f.Stop)
	waitForEventCount(t, env, 1)

	// Rewind the cursor to simulate a sender restored from an older
	// snapshot, or a cursor write that was lost to a crash.
	if err := sender.SetUplinkCursor(peerURL, 0); err != nil {
		t.Fatalf("SetUplinkCursor: %v", err)
	}

	second := uplink.New(peerURL, sender)
	second.Start()
	t.Cleanup(second.Stop)

	// Give the redelivery time to happen, then confirm nothing was added.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countEvents(t, env) != 1 {
			t.Fatalf("redelivery double-counted: %d rows", countEvents(t, env))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The interaction between the two halves of this change: the receiver
// rejects a wildly-skewed timestamp with 400, and the sender must step over
// it rather than wedging the whole backlog behind a row that can never land.
func TestE2E_UplinkSkipsClockRejectedEventAndContinues(t *testing.T) {
	sender := senderStore(t)
	env, peerURL := receiverServer(t)

	// Ordered by insertion id: the poisoned event sits ahead of a good one.
	addSenderEvent(t, sender, "vm-msg-skewed", time.Now().Add(72*time.Hour))
	addSenderEvent(t, sender, "vm-msg-good", time.Now().Add(-time.Minute).UTC())

	f := uplink.New(peerURL, sender)
	f.Start()
	t.Cleanup(f.Stop)

	waitForEventCount(t, env, 1)

	var messageID string
	if err := env.store.DB().QueryRow(
		`SELECT message_id FROM usage_events`,
	).Scan(&messageID); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if messageID != "vm-msg-good" {
		t.Errorf("expected only the good event to land, got %q", messageID)
	}

	// The cursor must have advanced past both, so the rejected event is not
	// retried forever.
	cursor, err := sender.GetUplinkCursor(peerURL)
	if err != nil {
		t.Fatalf("GetUplinkCursor: %v", err)
	}
	if cursor != 2 {
		t.Errorf("expected cursor past both events (2), got %d", cursor)
	}
}

func countEvents(t *testing.T, env *testEnv) int {
	t.Helper()
	var n int
	if err := env.store.DB().QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func waitForEventCount(t *testing.T, env *testEnv, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countEvents(t, env) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d events, receiver has %d", want, countEvents(t, env))
}

// Guards the CSRF mitigation from the receiving side: the forwarder must set
// Content-Type: application/json or every POST is refused.
func TestE2E_UplinkSetsContentTypeAcceptedByReceiver(t *testing.T) {
	sender := senderStore(t)
	env, peerURL := receiverServer(t)
	addSenderEvent(t, sender, "vm-msg-1", time.Now().Add(-time.Minute).UTC())

	f := uplink.New(peerURL, sender)
	f.Start()
	t.Cleanup(f.Stop)

	waitForEventCount(t, env, 1)

	// Sanity: the same body without the header is refused, so the test above
	// is actually exercising the header rather than a lenient server.
	body, err := json.Marshal(map[string]any{
		"input_tokens": 1, "output_tokens": 1,
		"session_id": "x", "message_id": "y",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/log", bytes.NewReader(body))
	w := httptest.NewRecorder()
	env.srv.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Error("expected the receiver to refuse a POST with no Content-Type")
	}
}
