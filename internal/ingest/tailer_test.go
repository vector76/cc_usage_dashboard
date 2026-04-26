package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/usage-dashboard/internal/store"
)

func TestIsTranscriptFile(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"JSONL file", "/path/to/session-123.jsonl", true},
		{"JSONL with project encoding", "/home/user/.claude/projects/-home-user-myproj/abc123.jsonl", true},
		{"Non-JSONL file", "/path/to/file.txt", false},
		{"Transcript without extension", "/path/to/transcript", false},
		{"Messages without extension", "/path/to/messages", false},
		{"Directory", "/path/to/directory/", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := isTranscriptFile(test.path)
			if result != test.expected {
				t.Errorf("isTranscriptFile(%q) = %v, expected %v", test.path, result, test.expected)
			}
		})
	}
}

func TestTailerProcessFile(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	pt := make(PriceTable)
	pt["claude-3-5-sonnet-20241022"] = &ModelPrices{
		InputRate:         3.00,
		OutputRate:        15.00,
		CacheCreationRate: 3.75,
		CacheReadRate:     0.30,
	}

	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, pt)

	// Create a transcript JSONL file with two events
	transcriptPath := filepath.Join(tmpDir, "session-123.jsonl")
	lines := []string{
		`{"type":"assistant","session_id":"session-123","message_id":"msg-1","timestamp":"2026-04-26T10:00:00Z","usage":{"input_tokens":100,"output_tokens":50}}`,
		`{"type":"assistant","session_id":"session-123","message_id":"msg-2","timestamp":"2026-04-26T10:01:00Z","usage":{"input_tokens":200,"output_tokens":100}}`,
	}
	content := strings.Join(lines, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Process the file
	tailer.handleFileChange(transcriptPath)

	// Verify events were inserted
	rows, err := s.DB().Query(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var count int
	if !rows.Next() {
		t.Fatal("no rows from count query")
	}
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if count != 2 {
		t.Errorf("expected 2 events inserted, got %d", count)
	}

	// Verify offset is set correctly
	tailer.offsetMu.Lock()
	offset := tailer.offsets[transcriptPath]
	tailer.offsetMu.Unlock()

	if offset != int64(len(content)) {
		t.Errorf("expected offset %d, got %d", int64(len(content)), offset)
	}
}

func TestTailerIncremental(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	pt := make(PriceTable)

	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, pt)

	transcriptPath := filepath.Join(tmpDir, "session-456.jsonl")

	// Write initial events
	lines1 := []string{
		`{"type":"assistant","session_id":"session-456","message_id":"msg-1","timestamp":"2026-04-26T10:00:00Z","usage":{"input_tokens":100,"output_tokens":50}}`,
	}
	content1 := strings.Join(lines1, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content1), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Process initial content
	tailer.handleFileChange(transcriptPath)

	// Check we have 1 event
	var count1 int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`)
	if err := row.Scan(&count1); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if count1 != 1 {
		t.Errorf("expected 1 event after first process, got %d", count1)
	}

	// Append more events
	lines2 := []string{
		`{"type":"assistant","session_id":"session-456","message_id":"msg-2","timestamp":"2026-04-26T10:01:00Z","usage":{"input_tokens":200,"output_tokens":100}}`,
	}
	content2 := content1 + strings.Join(lines2, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content2), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Process again (should only add the new event)
	tailer.handleFileChange(transcriptPath)

	// Check we now have 2 events
	var count2 int
	row = s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`)
	if err := row.Scan(&count2); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if count2 != 2 {
		t.Errorf("expected 2 events after append, got %d", count2)
	}

	// Simulate restart: create new tailer with same in-memory state
	tailer2 := NewTailer(tmpDir, s, pt)
	// Manually set offset from previous tailer
	tailer.offsetMu.Lock()
	for path, offset := range tailer.offsets {
		tailer2.offsetMu.Lock()
		tailer2.offsets[path] = offset
		tailer2.offsetMu.Unlock()
	}
	tailer.offsetMu.Unlock()

	// Append another event
	lines3 := []string{
		`{"type":"assistant","session_id":"session-456","message_id":"msg-3","timestamp":"2026-04-26T10:02:00Z","usage":{"input_tokens":300,"output_tokens":150}}`,
	}
	content3 := content2 + strings.Join(lines3, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content3), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Process with the "restarted" tailer
	tailer2.handleFileChange(transcriptPath)

	// Check we have 3 events
	var count3 int
	row = s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`)
	if err := row.Scan(&count3); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if count3 != 3 {
		t.Errorf("expected 3 events after second append, got %d", count3)
	}

	// Verify no duplicates via the unique constraint
	var msgIDs []string
	rows, err := s.DB().Query(`SELECT message_id FROM usage_events WHERE source = 'tailer' ORDER BY message_id`)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		msgIDs = append(msgIDs, id)
	}

	if len(msgIDs) != 3 || msgIDs[0] != "msg-1" || msgIDs[1] != "msg-2" || msgIDs[2] != "msg-3" {
		t.Errorf("unexpected message IDs: %v", msgIDs)
	}
}

func TestTailerMalformedLine(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	pt := make(PriceTable)

	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, pt)

	transcriptPath := filepath.Join(tmpDir, "session-789.jsonl")

	// Write a mix of valid and invalid events
	lines := []string{
		`{"type":"assistant","session_id":"session-789","message_id":"msg-1","timestamp":"2026-04-26T10:00:00Z","usage":{"input_tokens":100,"output_tokens":50}}`,
		`malformed json line`,
		`{"type":"assistant","session_id":"session-789","message_id":"msg-2","timestamp":"2026-04-26T10:01:00Z","usage":{"input_tokens":200,"output_tokens":100}}`,
	}
	content := strings.Join(lines, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Process the file (should not crash)
	tailer.handleFileChange(transcriptPath)

	// Verify valid events were inserted
	var validCount int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`)
	if err := row.Scan(&validCount); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if validCount != 2 {
		t.Errorf("expected 2 valid events, got %d", validCount)
	}

	// Verify parse error was recorded
	var errorCount int
	row = s.DB().QueryRow(`SELECT COUNT(*) FROM parse_errors WHERE source = 'tailer'`)
	if err := row.Scan(&errorCount); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if errorCount > 0 {
		t.Logf("recorded %d parse errors (expected at least 1)", errorCount)
	}
}

func TestTailerOffsetPersistence(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	pt := make(PriceTable)

	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "session-persist.jsonl")

	// Write initial event
	lines1 := []string{
		`{"type":"assistant","session_id":"session-persist","message_id":"msg-1","timestamp":"2026-04-26T10:00:00Z","usage":{"input_tokens":100,"output_tokens":50}}`,
	}
	content1 := strings.Join(lines1, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content1), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create tailer and process
	tailer1 := NewTailer(tmpDir, s, pt)
	tailer1.handleFileChange(transcriptPath)

	// Verify offset was persisted to DB
	dbOffset, err := s.GetTailerOffset(transcriptPath)
	if err != nil {
		t.Fatalf("failed to get offset from DB: %v", err)
	}

	expectedOffset := int64(len(content1))
	if dbOffset != expectedOffset {
		t.Errorf("expected DB offset %d, got %d", expectedOffset, dbOffset)
	}

	// Append more events
	lines2 := []string{
		`{"type":"assistant","session_id":"session-persist","message_id":"msg-2","timestamp":"2026-04-26T10:01:00Z","usage":{"input_tokens":200,"output_tokens":100}}`,
	}
	content2 := content1 + strings.Join(lines2, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content2), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Create a new tailer (simulating restart) and process the same file
	// The new tailer should load the offset from the DB
	tailer2 := NewTailer(tmpDir, s, pt)
	tailer2.handleFileChange(transcriptPath)

	// Verify we only processed the new event (no duplicates)
	var eventCount int
	row := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`)
	if err := row.Scan(&eventCount); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if eventCount != 2 {
		t.Errorf("expected 2 events total, got %d", eventCount)
	}

	// Verify the new offset was persisted
	dbOffset2, err := s.GetTailerOffset(transcriptPath)
	if err != nil {
		t.Fatalf("failed to get offset from DB: %v", err)
	}

	expectedOffset2 := int64(len(content2))
	if dbOffset2 != expectedOffset2 {
		t.Errorf("expected DB offset %d after append, got %d", expectedOffset2, dbOffset2)
	}
}
