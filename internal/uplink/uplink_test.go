package uplink

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// receiver is a stand-in for the host trayapp's POST /log. It records what
// arrived and lets each test dictate the status code.
type receiver struct {
	mu     sync.Mutex
	bodies []map[string]interface{}
	status func(n int) int
	srv    *httptest.Server
}

func newReceiver(t *testing.T, status func(n int) int) *receiver {
	t.Helper()
	r := &receiver{status: status}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/log" {
			t.Errorf("expected POST to /log, got %s", req.URL.Path)
		}
		if ct := req.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("receiver requires application/json, got %q", ct)
		}
		body, _ := io.ReadAll(req.Body)
		var parsed map[string]interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, parsed)
		n := len(r.bodies)
		r.mu.Unlock()

		code := http.StatusOK
		if r.status != nil {
			code = r.status(n)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, `{"id":1}`)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertEvent(t *testing.T, s *store.Store, messageID string) int64 {
	t.Helper()
	cost := 1.23
	id, err := s.InsertUsageEvent(
		time.Now(), "tailer", "sess-1", messageID, "/proj", "claude-opus-5",
		100, 50, 10, 5, &cost, "computed", `{"big":"payload"}`,
	)
	if err != nil {
		t.Fatalf("InsertUsageEvent: %v", err)
	}
	return id
}

func TestForwardOnceDeliversNewEvents(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	insertEvent(t, s, "m2")
	rec := newReceiver(t, nil)

	f := New(rec.srv.URL, s)
	n, err := f.forwardOnce()
	if err != nil {
		t.Fatalf("forwardOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 delivered, got %d", n)
	}
	if rec.count() != 2 {
		t.Errorf("expected receiver to see 2 posts, got %d", rec.count())
	}
}

// Cost and raw_json stay home: the receiver prices from its own table, and
// raw_json can be a whole transcript line.
func TestForwardOnceSendsTokensNotCost(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	rec := newReceiver(t, nil)

	f := New(rec.srv.URL, s)
	if _, err := f.forwardOnce(); err != nil {
		t.Fatalf("forwardOnce: %v", err)
	}

	body := rec.bodies[0]
	if _, present := body["cost_usd"]; present {
		t.Error("cost_usd must not be forwarded — the receiver prices it")
	}
	if _, present := body["raw_json"]; present {
		t.Error("raw_json must not be forwarded")
	}
	for _, key := range []string{"occurred_at", "session_id", "message_id", "model", "input_tokens", "output_tokens"} {
		if _, present := body[key]; !present {
			t.Errorf("payload missing %q", key)
		}
	}
	if got := body["input_tokens"]; got != float64(100) {
		t.Errorf("expected input_tokens 100, got %v", got)
	}
}

func TestForwardOnceAdvancesCursorAndDoesNotResend(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	rec := newReceiver(t, nil)
	f := New(rec.srv.URL, s)

	if _, err := f.forwardOnce(); err != nil {
		t.Fatalf("first forwardOnce: %v", err)
	}
	n, err := f.forwardOnce()
	if err != nil {
		t.Fatalf("second forwardOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("expected nothing left to forward, got %d", n)
	}
	if rec.count() != 1 {
		t.Errorf("event was re-sent: receiver saw %d posts", rec.count())
	}
}

// A receiver that is down or erroring must not consume the backlog. The
// cursor stays put so the next tick retries from the same place.
func TestForwardOnceKeepsCursorOnTransientFailure(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	rec := newReceiver(t, func(n int) int { return http.StatusInternalServerError })

	f := New(rec.srv.URL, s)
	if _, err := f.forwardOnce(); err == nil {
		t.Fatal("expected a transient failure to surface as an error")
	}

	cursor, err := s.GetUplinkCursor(rec.srv.URL)
	if err != nil {
		t.Fatalf("GetUplinkCursor: %v", err)
	}
	if cursor != 0 {
		t.Errorf("cursor must not advance past an undelivered event, got %d", cursor)
	}
}

// The counterpart: a 4xx means the receiver will never accept this event —
// our own occurred_at filter answers 400 for a skewed clock. Holding the
// cursor there would wedge the uplink permanently behind one bad row, so a
// permanent rejection is logged and stepped over.
func TestForwardOnceSkipsPermanentlyRejectedEvent(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	id2 := insertEvent(t, s, "m2")
	rec := newReceiver(t, func(n int) int {
		if n == 1 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})

	f := New(rec.srv.URL, s)
	n, err := f.forwardOnce()
	if err != nil {
		t.Fatalf("a rejected event must not fail the batch: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 delivered (the second), got %d", n)
	}

	cursor, _ := s.GetUplinkCursor(rec.srv.URL)
	if cursor != id2 {
		t.Errorf("cursor should have stepped past the rejection to %d, got %d", id2, cursor)
	}
}

// 429 is a 4xx but means "later", not "never".
func TestForwardOnceTreatsTooManyRequestsAsTransient(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	rec := newReceiver(t, func(n int) int { return http.StatusTooManyRequests })

	f := New(rec.srv.URL, s)
	if _, err := f.forwardOnce(); err == nil {
		t.Fatal("expected 429 to surface as a retryable error")
	}
	if cursor, _ := s.GetUplinkCursor(rec.srv.URL); cursor != 0 {
		t.Errorf("cursor must not advance on 429, got %d", cursor)
	}
}

// The receiver answers 200 with {"duplicate":true} when an event is already
// present. That is success — it is how an interrupted batch reconverges.
func TestForwardOnceTreatsDuplicateAsDelivered(t *testing.T) {
	s := newTestStore(t)
	id := insertEvent(t, s, "m1")
	rec := newReceiver(t, nil)

	f := New(rec.srv.URL, s)
	if _, err := f.forwardOnce(); err != nil {
		t.Fatalf("forwardOnce: %v", err)
	}
	if cursor, _ := s.GetUplinkCursor(rec.srv.URL); cursor != id {
		t.Errorf("expected cursor %d, got %d", id, cursor)
	}
}

// An unreachable peer is the normal state while the host is asleep. It must
// error rather than silently consuming the backlog.
func TestForwardOnceHandlesUnreachablePeer(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")

	f := New("http://127.0.0.1:1", s)
	if _, err := f.forwardOnce(); err == nil {
		t.Fatal("expected an error from an unreachable peer")
	}
	if cursor, _ := s.GetUplinkCursor("http://127.0.0.1:1"); cursor != 0 {
		t.Errorf("cursor must stay at 0 when nothing was delivered, got %d", cursor)
	}
}

func TestStartStopIsClean(t *testing.T) {
	s := newTestStore(t)
	insertEvent(t, s, "m1")
	rec := newReceiver(t, nil)

	f := New(rec.srv.URL, s)
	f.interval = 5 * time.Millisecond
	f.Start()

	deadline := time.Now().Add(2 * time.Second)
	for rec.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	f.Stop()

	if rec.count() == 0 {
		t.Error("expected the loop to forward at least once")
	}
}

// The receiver prices from tokens, so it needs the 1h split to apply the 2x
// cache-write rate.
func TestForwardOnceSendsCacheCreation1hTokens(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.InsertUsageEventRecord(store.UsageEventRecord{
		OccurredAt: time.Now(), Source: "tailer", SessionID: "sess-1", MessageID: "m1",
		InputTokens: 1, CacheCreationTokens: 100, CacheCreation1hTokens: 80,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rec := newReceiver(t, nil)
	if _, err := New(rec.srv.URL, s).forwardOnce(); err != nil {
		t.Fatalf("forwardOnce: %v", err)
	}
	if got := rec.bodies[0]["cache_creation_1h_tokens"]; got != float64(80) {
		t.Errorf("cache_creation_1h_tokens = %v, want 80", got)
	}
}
