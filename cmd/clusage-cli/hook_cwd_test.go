package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The hook reports each turn's real working directory (the transcript line's
// cwd) as project_path, falling back to the lossy encoded directory name only
// for a line that carries no cwd. Project accounting matches folders by path,
// and "C--WebApps-cc-usage-dashboard" cannot be turned back into a path
// reliably.
func TestHookSendsCwdAsProjectPath(t *testing.T) {
	var (
		mu  sync.Mutex
		got = map[string]string{} // message_id -> project_path
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev struct {
			MessageID   string `json:"message_id"`
			ProjectPath string `json:"project_path"`
		}
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		got[ev.MessageID] = ev.ProjectPath
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	transcript := `{"type":"assistant","sessionId":"s","cwd":"/home/user/my_proj","timestamp":"2026-11-17T10:00:00Z","message":{"id":"with-cwd","usage":{"input_tokens":1,"output_tokens":1}}}
{"type":"assistant","sessionId":"s","timestamp":"2026-11-17T10:01:00Z","message":{"id":"no-cwd","usage":{"input_tokens":1,"output_tokens":1}}}
`
	dir := filepath.Join(t.TempDir(), "projects", "-home-user-my-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{"transcript_path": path})
	if err := processHookInput(bytes.NewReader(payload), srv.URL); err != nil {
		t.Fatalf("processHookInput: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got["with-cwd"] != "/home/user/my_proj" {
		t.Errorf("with cwd: project_path = %q, want the cwd", got["with-cwd"])
	}
	if got["no-cwd"] != "-home-user-my-proj" {
		t.Errorf("without cwd: project_path = %q, want the encoded directory name", got["no-cwd"])
	}
}
