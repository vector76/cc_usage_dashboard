package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessHookInputSendsCacheCreation1hTokens(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	transcript := `{"type":"assistant","sessionId":"s","timestamp":"2026-09-22T10:00:00Z","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":7465,"cache_creation":{"ephemeral_5m_input_tokens":465,"ephemeral_1h_input_tokens":7000}}}}` + "\n"
	dir := filepath.Join(t.TempDir(), "projects", "-p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{"transcript_path": path})

	processHookInput(bytes.NewReader(payload), srv.URL)

	if got["cache_creation_tokens"] != float64(7465) {
		t.Errorf("cache_creation_tokens = %v, want 7465", got["cache_creation_tokens"])
	}
	if got["cache_creation_1h_tokens"] != float64(7000) {
		t.Errorf("cache_creation_1h_tokens = %v, want 7000", got["cache_creation_1h_tokens"])
	}
}
