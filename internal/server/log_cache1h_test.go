package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Stop hook and a peer's uplink both deliver the 1-hour cache-write split
// through /log; it must land in its own column so pricing and the report can
// tell the 2x writes from the 1.25x ones.
func TestHandleLogStoresCacheCreation1hTokens(t *testing.T) {
	srv, testStore := createTestServer(t)
	defer testStore.Close()

	body, _ := json.Marshal(LogPostRequest{
		InputTokens:           10,
		OutputTokens:          5,
		CacheCreationTokens:   1000,
		CacheCreation1hTokens: 900,
		SessionID:             "s",
		MessageID:             "m",
		Model:                 "claude-3-5-sonnet-20241022",
	})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, jsonPOST("/log", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	var total, oneH int
	if err := testStore.DB().QueryRow(`SELECT cache_creation_tokens, cache_creation_1h_tokens FROM usage_events`).
		Scan(&total, &oneH); err != nil {
		t.Fatalf("select: %v", err)
	}
	if total != 1000 || oneH != 900 {
		t.Errorf("total=%d 1h=%d, want 1000/900", total, oneH)
	}
}
