package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// captureLogs routes slog's default logger into a buffer of JSON lines for
// the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// slowRequestRecords returns the decoded "slow request" records in buf.
func slowRequestRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("unparseable log line %q: %v", sc.Text(), err)
		}
		if rec["msg"] == "slow request" {
			out = append(out, rec)
		}
	}
	return out
}

func TestSlowRequestThresholdDefaultsToOneSecond(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	if srv.slowRequestThreshold != time.Second {
		t.Errorf("slowRequestThreshold = %v, want 1s", srv.slowRequestThreshold)
	}
}

// A slow reply leaves no trace today: the checker that gave up sees a
// timeout and the trayapp's log stays quiet. The slow-request record says
// which request it was, what it answered, and whether the time went in the
// host/auth gate or in the handler.
func TestSlowRequestIsLoggedWithItsPhases(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	srv.slowRequestThreshold = 20 * time.Millisecond
	srv.mux.HandleFunc("/test/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusTeapot)
	})
	logs := captureLogs(t)

	req := httptest.NewRequest("GET", "/test/slow?x=1", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	srv.ServeHTTP(httptest.NewRecorder(), req)

	recs := slowRequestRecords(t, logs)
	if len(recs) != 1 {
		t.Fatalf("got %d slow-request records, want 1; log:\n%s", len(recs), logs)
	}
	rec := recs[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN so the feedback panel shows it", rec["level"])
	}
	if rec["method"] != "GET" || rec["path"] != "/test/slow" {
		t.Errorf("method/path = %v %v, want GET /test/slow", rec["method"], rec["path"])
	}
	if rec["status"] != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418", rec["status"])
	}
	if rec["remote"] != "127.0.0.1:50000" {
		t.Errorf("remote = %v", rec["remote"])
	}
	took, _ := rec["took_ms"].(float64)
	handler, _ := rec["handler_ms"].(float64)
	gate, ok := rec["gate_ms"].(float64)
	if took < 60 || handler < 60 {
		t.Errorf("took_ms = %v, handler_ms = %v, want both at least 60", took, handler)
	}
	if !ok || gate > took {
		t.Errorf("gate_ms = %v, want present and no more than took_ms %v", rec["gate_ms"], took)
	}
}

func TestFastRequestIsNotLogged(t *testing.T) {
	srv, st := createTestServer(t)
	defer st.Close()
	logs := captureLogs(t)

	srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))

	if recs := slowRequestRecords(t, logs); len(recs) != 0 {
		t.Errorf("fast /healthz logged as slow: %v", recs)
	}
}

// A request the gate turns away is timed too, so a stall in the auth or
// Host check shows up with the status it ended on.
func TestSlowRejectedRequestIsLogged(t *testing.T) {
	srv, _ := authServer(t, "s3cret")
	srv.slowRequestThreshold = 0
	logs := captureLogs(t)

	srv.ServeHTTP(httptest.NewRecorder(), getFrom("192.0.2.7:40000", ""))

	recs := slowRequestRecords(t, logs)
	if len(recs) != 1 || recs[0]["status"] != float64(http.StatusUnauthorized) {
		t.Errorf("records = %v, want one with status 401", recs)
	}
}
