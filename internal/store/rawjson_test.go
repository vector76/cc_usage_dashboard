package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A transcript line as Claude Code writes it: the conversation text sits in
// message.content and the tool inputs in wireToolInputs, around the usage
// block and ids that are worth keeping.
const rawLineWithContent = `{"type":"assistant","sessionId":"s","uuid":"u1","timestamp":"2026-10-05T18:00:00Z","cwd":"C:\\x","wireToolInputs":[{"command":"ls -la"}],"message":{"id":"m","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"a long reply"},{"type":"tool_use","input":{"command":"ls -la"}}],"usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":7465,"cache_creation":{"ephemeral_5m_input_tokens":465,"ephemeral_1h_input_tokens":7000}},"stop_reason":"tool_use"}}`

const rawLineTrimmed = `{"type":"assistant","sessionId":"s","uuid":"u1","timestamp":"2026-10-05T18:00:00Z","cwd":"C:\\x","message":{"id":"m","model":"claude-opus-5-5","role":"assistant","usage":{"input_tokens":2,"output_tokens":5,"cache_creation_input_tokens":7465,"cache_creation":{"ephemeral_5m_input_tokens":465,"ephemeral_1h_input_tokens":7000}},"stop_reason":"tool_use"}}`

// sameJSON reports whether two JSON texts decode to equal values, so tests
// don't depend on key order or whitespace.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatalf("unmarshal %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

func TestTrimUsageRawJSONDropsConversationContent(t *testing.T) {
	got := TrimUsageRawJSON(rawLineWithContent)
	if !sameJSON(t, got, rawLineTrimmed) {
		t.Errorf("trimmed = %s\nwant      %s", got, rawLineTrimmed)
	}
}

func TestTrimUsageRawJSONDropsNullContent(t *testing.T) {
	got := TrimUsageRawJSON(`{"message":{"id":"m","content":null},"wireToolInputs":null}`)
	if !sameJSON(t, got, `{"message":{"id":"m"}}`) {
		t.Errorf("trimmed = %s", got)
	}
}

// Anything with nothing to drop comes back byte-for-byte: there is no reason
// to re-encode a payload the trim leaves alone.
func TestTrimUsageRawJSONLeavesOtherPayloadsUnchanged(t *testing.T) {
	for _, raw := range []string{
		"",
		rawLineTrimmed,
		`not json "content"`,
		`[{"message":{"content":"x"}}]`,
		`{"message":"content is a string here"}`,
		`{"type":"assistant", "message": {"id": "m"}}`,
	} {
		if got := TrimUsageRawJSON(raw); got != raw {
			t.Errorf("TrimUsageRawJSON(%q) = %q, want it unchanged", raw, got)
		}
	}
}

func TestInsertUsageEventRecordTrimsRawJSON(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := s.InsertUsageEventRecord(UsageEventRecord{
		OccurredAt: time.Now(), Source: "hook", SessionID: "s", MessageID: "m",
		InputTokens: 2, OutputTokens: 5, RawJSON: rawLineWithContent,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var stored string
	if err := s.DB().QueryRow(`SELECT raw_json FROM usage_events`).Scan(&stored); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !sameJSON(t, stored, rawLineTrimmed) {
		t.Errorf("stored raw_json = %s\nwant           %s", stored, rawLineTrimmed)
	}
}

// insertRawUsageEvent writes a usage_events row directly, the way rows looked
// before migration v10.
func insertRawUsageEvent(t *testing.T, db *sql.DB, msgID, raw string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO usage_events (occurred_at, source, session_id, message_id,
			input_tokens, output_tokens, model, raw_json)
		VALUES (?, 'tailer', 's', ?, 2, 5, 'claude-opus-5-5', ?)
	`, FormatTime(time.Now()), msgID, raw); err != nil {
		t.Fatalf("insert %s: %v", msgID, err)
	}
}

func TestMigrateFromV9TrimsStoredRawJSON(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open memory DB: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:9]
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v1-v9 migrations: %v", err)
	}

	rows := map[string]string{
		"with-content":     rawLineWithContent,
		"already-trimmed":  rawLineTrimmed,
		"null-content":     `{"message":{"id":"m","content":null},"wireToolInputs":null}`,
		"hook-row":         "",
		"garbage":          `not json "content" wireToolInputs`,
		"array":            `[{"message":{"content":"x"}}]`,
		"message-a-string": `{"message":"content"}`,
	}
	for id, raw := range rows {
		insertRawUsageEvent(t, db, id, raw)
	}
	// quota_snapshots.raw_json is the userscript's payload, not a transcript
	// line, and must be left alone.
	snapshotRaw := `{"content":"keep me","wireToolInputs":1}`
	if _, err := db.Exec(`INSERT INTO quota_snapshots (observed_at, received_at, source, raw_json)
		VALUES (?, ?, 'userscript', ?)`, FormatTime(time.Now()), FormatTime(time.Now()), snapshotRaw); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	migrations = saved
	if err := ApplyMigrations(db); err != nil {
		t.Fatalf("failed to apply v10 migration: %v", err)
	}

	// The SQL migration and the Go trim must agree, or rows written before
	// and after the upgrade would differ in what they keep.
	for id, raw := range rows {
		var stored string
		if err := db.QueryRow(`SELECT raw_json FROM usage_events WHERE message_id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("select %s: %v", id, err)
		}
		want := TrimUsageRawJSON(raw)
		if json.Valid([]byte(want)) && json.Valid([]byte(stored)) {
			if !sameJSON(t, stored, want) {
				t.Errorf("%s: migrated raw_json = %s, want %s", id, stored, want)
			}
		} else if stored != want {
			t.Errorf("%s: migrated raw_json = %q, want %q", id, stored, want)
		}
	}

	// What v9 read back out of raw_json must still be there.
	var oneH int
	if err := db.QueryRow(`SELECT json_extract(raw_json, '$.message.usage.cache_creation.ephemeral_1h_input_tokens')
		FROM usage_events WHERE message_id = 'with-content'`).Scan(&oneH); err != nil {
		t.Fatalf("extract 1h tokens: %v", err)
	}
	if oneH != 7000 {
		t.Errorf("1h tokens after trim = %d, want 7000", oneH)
	}

	var gotSnapshot string
	if err := db.QueryRow(`SELECT raw_json FROM quota_snapshots`).Scan(&gotSnapshot); err != nil {
		t.Fatalf("select snapshot: %v", err)
	}
	if gotSnapshot != snapshotRaw {
		t.Errorf("quota snapshot raw_json changed to %q", gotSnapshot)
	}
}

// The trim only frees pages; without a VACUUM the file would stay the same
// size. The migration that trims must also give the space back, and leave
// no WAL as large as the database behind it.
func TestMigrateFromV9ShrinksDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")

	saved := migrations
	defer func() { migrations = saved }()
	migrations = saved[:9]
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open at v9: %v", err)
	}
	reply := strings.Repeat("x", 8000)
	for i := 0; i < 500; i++ {
		raw := fmt.Sprintf(`{"type":"assistant","sessionId":"s","message":{"id":"m%d","content":[{"type":"text","text":%q}],"usage":{"input_tokens":2,"output_tokens":5}}}`, i, reply)
		insertRawUsageEvent(t, s.DB(), fmt.Sprintf("m%d", i), raw)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	s.Close()
	before := fileSize(t, path)

	migrations = saved
	s, err = Open(path)
	if err != nil {
		t.Fatalf("open at v10: %v", err)
	}
	defer s.Close()

	after := fileSize(t, path)
	if after > before/4 {
		t.Errorf("database file %d bytes after trim, want under a quarter of %d", after, before)
	}
	if wal := fileSize(t, path+"-wal"); wal > before/4 {
		t.Errorf("WAL is %d bytes after the vacuum, want it checkpointed away", wal)
	}

	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE raw_json LIKE '%"content"%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows still carry message.content", n)
	}
}

// fileSize returns a file's size, or 0 when it does not exist.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
