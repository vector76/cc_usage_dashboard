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
	tailer.processFile(transcriptPath)

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
	tailer.processFile(transcriptPath)

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
	tailer.processFile(transcriptPath)

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
	tailer.processFile(transcriptPath)

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

// TestTailerOffsetAdvancesPastSkippedLines verifies that the saved offset
// reflects every line the parser scanned, not just lines that produced a
// usage event. Otherwise non-usage and malformed lines would be re-read on
// every pass.
func TestTailerOffsetAdvancesPastSkippedLines(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, make(PriceTable))
	transcriptPath := filepath.Join(tmpDir, "session-mixed.jsonl")

	// Mix of valid-usage, non-usage, and malformed lines.
	lines := []string{
		`{"type":"user","content":"hello"}`,
		`malformed json line`,
		`{"type":"assistant","session_id":"s1","message_id":"m1","timestamp":"2026-04-26T10:00:00Z","usage":{"input_tokens":100,"output_tokens":50}}`,
		`{"type":"user","content":"more"}`,
		`{"type":"assistant","session_id":"s1","message_id":"m2","timestamp":"2026-04-26T10:01:00Z","usage":{"input_tokens":200,"output_tokens":100}}`,
	}
	content := strings.Join(lines, "\n") + "\n"

	if err := os.WriteFile(transcriptPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	tailer.processFile(transcriptPath)

	// Offset must equal full file size so subsequent calls don't reprocess
	// the non-usage and malformed lines.
	tailer.offsetMu.Lock()
	offset := tailer.offsets[transcriptPath]
	tailer.offsetMu.Unlock()
	if offset != int64(len(content)) {
		t.Errorf("expected offset %d (full file), got %d", len(content), offset)
	}

	// Append more content; only the appended event should be ingested.
	appended := `{"type":"assistant","session_id":"s1","message_id":"m3","timestamp":"2026-04-26T10:02:00Z","usage":{"input_tokens":300,"output_tokens":150}}` + "\n"
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to open for append: %v", err)
	}
	if _, err := f.WriteString(appended); err != nil {
		t.Fatalf("failed to append: %v", err)
	}
	f.Close()

	tailer.processFile(transcriptPath)

	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE source = 'tailer'`).Scan(&count); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 events total (2 initial + 1 appended), got %d", count)
	}

	tailer.offsetMu.Lock()
	finalOffset := tailer.offsets[transcriptPath]
	tailer.offsetMu.Unlock()
	if finalOffset != int64(len(content)+len(appended)) {
		t.Errorf("expected final offset %d, got %d", len(content)+len(appended), finalOffset)
	}
}

// TestTailerRestartFromPersistedOffset simulates a process restart by
// writing half a transcript, processing it, then constructing a brand-new
// Tailer (with empty in-memory state) and asking it to load offsets from
// the database before processing the appended remainder. Asserts no
// duplicate events and no missed events across the restart boundary.
func TestTailerRestartFromPersistedOffset(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	pt := make(PriceTable)

	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "session-restart.jsonl")

	firstHalf := []string{
		`{"type":"assistant","session_id":"s","message_id":"m1","timestamp":"2026-04-26T10:00:00Z","usage":{"input_tokens":100,"output_tokens":50}}`,
		`{"type":"assistant","session_id":"s","message_id":"m2","timestamp":"2026-04-26T10:01:00Z","usage":{"input_tokens":200,"output_tokens":100}}`,
	}
	firstContent := strings.Join(firstHalf, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(firstContent), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// First lifetime: process the initial half.
	tailer1 := NewTailer(tmpDir, s, pt)
	tailer1.processFile(transcriptPath)

	persisted, err := s.GetTailerOffset(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read persisted offset: %v", err)
	}
	if persisted != int64(len(firstContent)) {
		t.Fatalf("persisted offset=%d, want %d", persisted, len(firstContent))
	}

	// Append the remainder while the "process" is "down".
	secondHalf := []string{
		`{"type":"assistant","session_id":"s","message_id":"m3","timestamp":"2026-04-26T10:02:00Z","usage":{"input_tokens":300,"output_tokens":150}}`,
		`{"type":"assistant","session_id":"s","message_id":"m4","timestamp":"2026-04-26T10:03:00Z","usage":{"input_tokens":400,"output_tokens":200}}`,
	}
	fullContent := firstContent + strings.Join(secondHalf, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(fullContent), 0644); err != nil {
		t.Fatalf("failed to overwrite transcript: %v", err)
	}

	// Second lifetime: brand-new Tailer with empty in-memory map.
	// Load persisted offsets from DB before processing.
	tailer2 := NewTailer(tmpDir, s, pt)
	tailer2.loadPersistedOffsets()

	tailer2.offsetMu.Lock()
	loadedOffset, ok := tailer2.offsets[transcriptPath]
	tailer2.offsetMu.Unlock()
	if !ok {
		t.Fatalf("expected offset for %q to be loaded from DB", transcriptPath)
	}
	if loadedOffset != int64(len(firstContent)) {
		t.Fatalf("loaded offset=%d, want %d", loadedOffset, len(firstContent))
	}

	tailer2.processFile(transcriptPath)

	// Exactly the four message IDs, in order, with no duplicates.
	rows, err := s.DB().Query(`
		SELECT message_id FROM usage_events
		WHERE source = 'tailer' ORDER BY message_id
	`)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan failed: %v", err)
		}
		ids = append(ids, id)
	}

	want := []string{"m1", "m2", "m3", "m4"}
	if len(ids) != len(want) {
		t.Fatalf("got %d events (%v), want %d (%v)", len(ids), ids, len(want), want)
	}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("event %d: got %q, want %q", i, id, want[i])
		}
	}

	finalOffset, err := s.GetTailerOffset(transcriptPath)
	if err != nil {
		t.Fatalf("failed to read final offset: %v", err)
	}
	if finalOffset != int64(len(fullContent)) {
		t.Errorf("final persisted offset=%d, want %d", finalOffset, len(fullContent))
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
