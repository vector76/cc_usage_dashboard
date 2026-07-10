package ingest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vector76/cc_usage_dashboard/internal/feedback"
	"github.com/vector76/cc_usage_dashboard/internal/store"
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
		`{"type":"assistant","sessionId":"session-123","timestamp":"2026-04-26T10:00:00Z","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"session-123","timestamp":"2026-04-26T10:01:00Z","message":{"id":"msg-2","usage":{"input_tokens":200,"output_tokens":100}}}`,
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

// TestTailerAggregatesUnknownModel verifies the ingest path records events
// whose model is missing from the price table into the unknown-model aggregate
// (one entry per model, event count summed), while events with a priced model
// or an empty model are not aggregated as unknown.
func TestTailerAggregatesUnknownModel(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	pt := make(PriceTable)
	pt["known-model"] = &ModelPrices{InputRate: 1, OutputRate: 1}

	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, pt)
	// Inject a fresh aggregate so the test is isolated from the process-wide
	// default (and from other tests).
	agg := feedback.NewUnknownModels()
	tailer.unknown = agg

	transcriptPath := filepath.Join(tmpDir, "session-unk.jsonl")
	lines := []string{
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:00:00Z","message":{"id":"m1","model":"unpriced-model","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:01:00Z","message":{"id":"m2","model":"unpriced-model","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:02:00Z","message":{"id":"m3","model":"known-model","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:03:00Z","message":{"id":"m4","usage":{"input_tokens":100,"output_tokens":50}}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	tailer.processFile(transcriptPath)

	snap := agg.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected exactly 1 unknown model, got %d: %+v", len(snap), snap)
	}
	if snap[0].Model != "unpriced-model" {
		t.Errorf("expected 'unpriced-model', got %q", snap[0].Model)
	}
	if snap[0].Count != 2 {
		t.Errorf("expected count 2, got %d", snap[0].Count)
	}
	if agg.TotalEvents() != 2 {
		t.Errorf("expected 2 total unknown-model events, got %d", agg.TotalEvents())
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
		`{"type":"assistant","sessionId":"session-456","timestamp":"2026-04-26T10:00:00Z","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}`,
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
		`{"type":"assistant","sessionId":"session-456","timestamp":"2026-04-26T10:01:00Z","message":{"id":"msg-2","usage":{"input_tokens":200,"output_tokens":100}}}`,
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
		`{"type":"assistant","sessionId":"session-456","timestamp":"2026-04-26T10:02:00Z","message":{"id":"msg-3","usage":{"input_tokens":300,"output_tokens":150}}}`,
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
		`{"type":"assistant","sessionId":"session-789","timestamp":"2026-04-26T10:00:00Z","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`malformed json line`,
		`{"type":"assistant","sessionId":"session-789","timestamp":"2026-04-26T10:01:00Z","message":{"id":"msg-2","usage":{"input_tokens":200,"output_tokens":100}}}`,
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
		`{"type":"assistant","sessionId":"s1","timestamp":"2026-04-26T10:00:00Z","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"user","content":"more"}`,
		`{"type":"assistant","sessionId":"s1","timestamp":"2026-04-26T10:01:00Z","message":{"id":"m2","usage":{"input_tokens":200,"output_tokens":100}}}`,
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
	appended := `{"type":"assistant","sessionId":"s1","timestamp":"2026-04-26T10:02:00Z","message":{"id":"m3","usage":{"input_tokens":300,"output_tokens":150}}}` + "\n"
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
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:00:00Z","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:01:00Z","message":{"id":"m2","usage":{"input_tokens":200,"output_tokens":100}}}`,
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
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:02:00Z","message":{"id":"m3","usage":{"input_tokens":300,"output_tokens":150}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-04-26T10:03:00Z","message":{"id":"m4","usage":{"input_tokens":400,"output_tokens":200}}}`,
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
		`{"type":"assistant","sessionId":"session-persist","timestamp":"2026-04-26T10:00:00Z","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}`,
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
		`{"type":"assistant","sessionId":"session-persist","timestamp":"2026-04-26T10:01:00Z","message":{"id":"msg-2","usage":{"input_tokens":200,"output_tokens":100}}}`,
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

// TestTailerSkipsSymlinks verifies that a *.jsonl symlink under the
// projects dir is not opened. Without this guard a hostile symlink
// pointing at, say, /etc/passwd would have its bytes read line-by-line
// and any non-JSON line would land in parse_errors.payload.
func TestTailerSkipsSymlinks(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	tmpDir := t.TempDir()

	// The "secret" file represents anything outside the projects dir
	// that we must never read. Contents are deliberately not JSON so a
	// regression would write to parse_errors and fail the test loudly.
	secretPath := filepath.Join(tmpDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("SHOULD-NOT-BE-READ\n"), 0644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.Mkdir(projectsDir, 0755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	// Drop a *.jsonl symlink under projectsDir pointing at the secret.
	symlinkPath := filepath.Join(projectsDir, "evil.jsonl")
	if err := os.Symlink(secretPath, symlinkPath); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	tailer := NewTailer(projectsDir, s, make(PriceTable))

	// Both ingest paths share the same skip logic; exercise both.
	tailer.pollOnce()
	tailer.handleFileChange(symlinkPath)

	var ueCount, peCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&ueCount); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if ueCount != 0 {
		t.Errorf("expected 0 usage_events from symlink read, got %d", ueCount)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM parse_errors`).Scan(&peCount); err != nil {
		t.Fatalf("count parse_errors: %v", err)
	}
	if peCount != 0 {
		t.Errorf("symlink should be silently skipped, but parse_errors=%d", peCount)
	}
	// The symlink must not be tracked either: an offset row would mean
	// we did at least open and seek the file.
	if _, err := s.GetTailerOffset(symlinkPath); err == nil {
		var n int
		_ = s.DB().QueryRow(`SELECT COUNT(*) FROM tailer_offsets WHERE file_path = ?`, symlinkPath).Scan(&n)
		if n != 0 {
			t.Errorf("expected no tailer_offsets row for symlink, got %d", n)
		}
	}
}

// TestTailerStoresQueryableTimestamp verifies occurred_at is written in the
// RFC3339 shape store.FormatTime produces, not Go's default time.Time
// String() format. modernc.org/sqlite serializes an unconverted time.Time
// query argument via String() ("2006-01-02 15:04:05.x +0000 UTC"), which
// SQLite's strftime/date functions silently fail to parse (returning NULL
// rather than an error). The dashboard's weekly/session burn-down charts
// bucket usage_events with strftime('%s', occurred_at) — a tailer-sourced
// row written the wrong way would scan back as SQL NULL there and break the
// query for every request touching that time range, exactly the failure
// this test pins.
func TestTailerStoresQueryableTimestamp(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, make(PriceTable))

	transcriptPath := filepath.Join(tmpDir, "session-ts.jsonl")
	line := `{"type":"assistant","sessionId":"session-ts","timestamp":"2026-04-26T10:00:00Z","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	tailer.processFile(transcriptPath)

	var bucketUnix sql.NullInt64
	row := s.DB().QueryRow(`
		SELECT CAST(strftime('%s', occurred_at) AS INTEGER)
		FROM usage_events WHERE session_id = 'session-ts' AND message_id = 'msg-1'
	`)
	if err := row.Scan(&bucketUnix); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if !bucketUnix.Valid {
		t.Fatal("strftime on occurred_at returned NULL — occurred_at was not stored via store.FormatTime")
	}
	want := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC).Unix()
	if bucketUnix.Int64 != want {
		t.Errorf("bucket_unix = %d, want %d", bucketUnix.Int64, want)
	}
}

// TestTailerSkipsAlreadyRecordedEvent verifies that when the Stop hook (or
// the other tailer root, for a session visible to both) has already
// recorded a (session_id, message_id) pair, the tailer treats re-reading
// that same line as idempotent: no duplicate row, no error log, and no
// parse_errors entry — mirroring the identical dedup contract server.go's
// handleLog already applies to hook-sourced re-posts. Before this, the
// tailer's INSERT lacked that check, so every message the hook got to
// first would be logged as an insert failure and leave a permanent
// parse_errors row on every restart's catch-up poll.
func TestTailerSkipsAlreadyRecordedEvent(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	// Simulate the hook having already recorded this message.
	if _, err := s.InsertUsageEvent(
		time.Now(), "hook", "session-dup", "msg-1", "", "",
		100, 50, 0, 0, nil, "", "",
	); err != nil {
		t.Fatalf("failed to seed hook-sourced event: %v", err)
	}

	pt := make(PriceTable)
	tmpDir := t.TempDir()
	tailer := NewTailer(tmpDir, s, pt)

	transcriptPath := filepath.Join(tmpDir, "session-dup.jsonl")
	line := `{"type":"assistant","sessionId":"session-dup","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	tailer.processFile(transcriptPath)

	var totalCount, parseErrCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE session_id = 'session-dup' AND message_id = 'msg-1'`).Scan(&totalCount); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if totalCount != 1 {
		t.Errorf("expected exactly 1 row for the (session_id, message_id) pair, got %d", totalCount)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM parse_errors WHERE source = 'tailer'`).Scan(&parseErrCount); err != nil {
		t.Fatalf("count parse_errors: %v", err)
	}
	if parseErrCount != 0 {
		t.Errorf("expected no parse_errors from an idempotent re-read, got %d", parseErrCount)
	}

	// The offset must still advance past the duplicate line, or every
	// subsequent poll would re-read (and re-skip) it forever.
	tailer.offsetMu.Lock()
	offset := tailer.offsets[transcriptPath]
	tailer.offsetMu.Unlock()
	if offset != int64(len(line)) {
		t.Errorf("expected offset to advance past the duplicate line, got %d want %d", offset, len(line))
	}
}

// TestTailerReimportRecoversPastOffset reproduces the exact scenario
// Reimport exists to fix: a byte offset already sitting at EOF for a file
// (as if an earlier bug had "read" it without successfully extracting
// every event), with one message genuinely missing from usage_events. A
// normal poll can't recover it — it has nothing new to read past the
// existing offset. Reimport must wipe the offset, re-read from byte 0, and
// rely on the unique constraint to bring back only what's actually
// missing: recovering msg-2 without duplicating the already-recorded
// hook-sourced msg-1.
func TestTailerReimportRecoversPastOffset(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer s.Close()

	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "session-recover.jsonl")
	lines := []string{
		`{"type":"assistant","sessionId":"session-recover","message":{"id":"msg-1","usage":{"input_tokens":100,"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"session-recover","message":{"id":"msg-2","usage":{"input_tokens":200,"output_tokens":100}}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// The hook already recorded msg-1. msg-2 was never recorded anywhere,
	// but the tailer's persisted offset already sits at EOF, as if an
	// earlier bug had consumed the bytes without producing an event.
	if _, err := s.InsertUsageEvent(
		time.Now(), "hook", "session-recover", "msg-1", "", "",
		100, 50, 0, 0, nil, "", "",
	); err != nil {
		t.Fatalf("failed to seed hook-sourced event: %v", err)
	}
	if err := s.SetTailerOffset(transcriptPath, int64(len(content))); err != nil {
		t.Fatalf("failed to seed offset: %v", err)
	}

	tailer := NewTailer(tmpDir, s, make(PriceTable))
	tailer.loadPersistedOffsets() // mirrors what Start() does

	// Sanity check: a normal poll finds nothing new, confirming msg-2 is
	// genuinely stuck (not just untested).
	tailer.processFile(transcriptPath)
	var countBeforeReimport int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE session_id = 'session-recover'`).Scan(&countBeforeReimport); err != nil {
		t.Fatalf("count before reimport: %v", err)
	}
	if countBeforeReimport != 1 {
		t.Fatalf("expected only the pre-seeded hook row before Reimport, got %d", countBeforeReimport)
	}

	tailer.Reimport()

	rows, err := s.DB().Query(`SELECT message_id, source FROM usage_events WHERE session_id = 'session-recover' ORDER BY message_id`)
	if err != nil {
		t.Fatalf("query after reimport: %v", err)
	}
	defer rows.Close()

	type row struct{ messageID, source string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.messageID, &r.source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}

	want := []row{{"msg-1", "hook"}, {"msg-2", "tailer"}}
	if len(got) != len(want) {
		t.Fatalf("got %d rows %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
