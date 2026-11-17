package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vector76/cc_usage_dashboard/internal/store"
)

// The working directory a turn ran in is the top-level "cwd" of the
// transcript line. It is the folder a project report attributes the turn's
// cost to, so the parser must carry it into ProjectPath rather than leaving it
// buried in raw_json.
func TestParserCapturesCwdAsProjectPath(t *testing.T) {
	line := `{"type":"assistant","sessionId":"s","cwd":"C:\\WebApps\\cc_usage_dashboard","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2}},"timestamp":"2026-11-17T10:00:00Z"}`
	event, err := NewParser(strings.NewReader(line)).ParseNext()
	if err != nil || event == nil {
		t.Fatalf("ParseNext = %v, %v", event, err)
	}
	if want := `C:\WebApps\cc_usage_dashboard`; event.ProjectPath != want {
		t.Errorf("ProjectPath = %q, want %q", event.ProjectPath, want)
	}
}

func TestParserWithoutCwdLeavesProjectPathEmpty(t *testing.T) {
	line := `{"type":"assistant","sessionId":"s","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2}}}`
	event, err := NewParser(strings.NewReader(line)).ParseNext()
	if err != nil || event == nil {
		t.Fatalf("ParseNext = %v, %v", event, err)
	}
	if event.ProjectPath != "" {
		t.Errorf("ProjectPath = %q, want empty", event.ProjectPath)
	}
}

// A non-string cwd is ignored rather than failing the line: the folder is a
// nice-to-have, the tokens are not.
func TestParserIgnoresNonStringCwd(t *testing.T) {
	line := `{"type":"assistant","sessionId":"s","cwd":42,"message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2}}}`
	event, err := NewParser(strings.NewReader(line)).ParseNext()
	if err != nil || event == nil {
		t.Fatalf("ParseNext = %v, %v", event, err)
	}
	if event.ProjectPath != "" {
		t.Errorf("ProjectPath = %q, want empty", event.ProjectPath)
	}
}

// End to end through the tailer: the cwd lands in usage_events.project_path.
func TestTailerStoresCwdAsProjectPath(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	line := `{"type":"assistant","sessionId":"s","cwd":"C:\\OCIS2\\BNA","timestamp":"2026-11-17T10:00:00Z","message":{"id":"m","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	NewTailer(dir, s, PriceTable{}).processFile(path)

	var got string
	if err := s.DB().QueryRow(`SELECT project_path FROM usage_events`).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if want := `C:\OCIS2\BNA`; got != want {
		t.Errorf("project_path = %q, want %q", got, want)
	}
}
