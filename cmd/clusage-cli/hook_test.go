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

func TestInferProjectPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "typical claude code transcript path",
			path: "/home/user/.claude/projects/-home-user-myproj/abc.jsonl",
			want: "-home-user-myproj",
		},
		{
			name: "tilde-style path",
			path: filepath.Join("~", ".claude", "projects", "-home-alice-work", "session-1.jsonl"),
			want: "-home-alice-work",
		},
		{
			name: "path without projects segment",
			path: "/tmp/random/file.jsonl",
			want: "",
		},
		{
			name: "empty path",
			path: "",
			want: "",
		},
		{
			name: "just a filename",
			path: "session.jsonl",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := inferProjectPath(tc.path)
			if got != tc.want {
				t.Errorf("inferProjectPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestProcessHookInputFlow(t *testing.T) {
	// Capture POSTs hitting the stub trayapp.
	type capturedEvent struct {
		SessionID  string `json:"session_id"`
		MessageID  string `json:"message_id"`
		Source     string `json:"source"`
		OccurredAt string `json:"occurred_at"`
	}

	var (
		mu       sync.Mutex
		captured []capturedEvent
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/log" || r.Method != http.MethodPost {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		var ev capturedEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		captured = append(captured, ev)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	// Real Claude Code transcripts have sessionId (camelCase) at the top
	// level and nest model/id/usage under a "message" object. The earlier
	// fixture used a flat snake_case schema that never existed in practice;
	// keeping the test honest is what catches regressions like the one
	// where every line was silently skipped because msgMap["usage"] was nil.
	transcript := `{"type":"assistant","sessionId":"sess-1","timestamp":"2026-04-26T17:04:22.527Z","message":{"id":"msg-1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":50}}}
{"type":"user","sessionId":"sess-1","message":{"content":"hi"}}
{"type":"assistant","sessionId":"sess-1","timestamp":"2026-04-26T17:09:11.012Z","message":{"id":"msg-2","model":"claude-sonnet-4-6","usage":{"input_tokens":200,"output_tokens":120,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}}
`

	// Write the transcript under a projects/<encoded>/<file>.jsonl layout
	// so inferProjectPath produces a non-empty value.
	root := t.TempDir()
	projectDir := filepath.Join(root, "projects", "-home-user-myproj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	transcriptPath := filepath.Join(projectDir, "session-1.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Build the hook payload pointing at the temp transcript.
	payload, err := json.Marshal(map[string]string{
		"transcript_path": transcriptPath,
		"session_id":      "sess-1",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	// Pipe through processHookInput against the stub server.
	processHookInput(bytes.NewReader(payload), srv.URL)

	mu.Lock()
	defer mu.Unlock()

	if len(captured) != 2 {
		t.Fatalf("expected 2 POSTs, got %d (events: %+v)", len(captured), captured)
	}

	// All events should have source=hook.
	for i, ev := range captured {
		if ev.Source != "hook" {
			t.Errorf("event %d: source = %q, want %q", i, ev.Source, "hook")
		}
	}

	// Expected (session_id, message_id, occurred_at) tuples — occurred_at
	// must round-trip from the transcript line's "timestamp" field, not
	// default to the server's wall clock.
	want := map[[2]string]string{
		{"sess-1", "msg-1"}: "2026-04-26T17:04:22.527Z",
		{"sess-1", "msg-2"}: "2026-04-26T17:09:11.012Z",
	}
	seen := map[[2]string]bool{}
	for _, ev := range captured {
		key := [2]string{ev.SessionID, ev.MessageID}
		wantTS, ok := want[key]
		if !ok {
			t.Errorf("unexpected event pair: %v", key)
			continue
		}
		if ev.OccurredAt != wantTS {
			t.Errorf("event %v: occurred_at = %q, want %q", key, ev.OccurredAt, wantTS)
		}
		seen[key] = true
	}
	for pair := range want {
		if !seen[pair] {
			t.Errorf("expected pair %v not seen", pair)
		}
	}
}
