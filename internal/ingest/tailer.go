package ingest

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/usage-dashboard/internal/store"
	"github.com/fsnotify/fsnotify"
)

// Tailer watches the Claude projects directory for transcript changes.
type Tailer struct {
	projectsDir string
	store       *store.Store
	priceTable  PriceTable
	offsets     map[string]int64 // filepath -> byte offset
	offsetMu    sync.Mutex
	stopChan    chan struct{}
	doneChan    chan struct{}
	caughtUp    atomic.Bool
}

// NewTailer creates a new tailer for the given projects directory.
func NewTailer(projectsDir string, s *store.Store, pt PriceTable) *Tailer {
	return &Tailer{
		projectsDir: projectsDir,
		store:       s,
		priceTable:  pt,
		offsets:     make(map[string]int64),
		stopChan:    make(chan struct{}),
		doneChan:    make(chan struct{}),
	}
}

// Start begins watching for transcript changes (runs in a goroutine).
func (t *Tailer) Start() {
	t.loadPersistedOffsets()
	go t.run()
}

// loadPersistedOffsets pre-populates the in-memory offset map from the
// database so previously-tracked files resume at their last persisted
// position rather than being re-read from the beginning.
func (t *Tailer) loadPersistedOffsets() {
	persisted, err := t.store.LoadAllTailerOffsets()
	if err != nil {
		slog.Warn("failed to load persisted tailer offsets", "err", err)
		return
	}
	t.offsetMu.Lock()
	for path, offset := range persisted {
		t.offsets[path] = offset
	}
	t.offsetMu.Unlock()
}

// Stop stops the tailer.
func (t *Tailer) Stop() {
	close(t.stopChan)
	<-t.doneChan
}

// run is the main tailer loop.
func (t *Tailer) run() {
	defer close(t.doneChan)

	// Try to set up fsnotify watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("failed to create fsnotify watcher, falling back to polling", "err", err)
		t.pollLoop()
		return
	}
	defer watcher.Close()

	// Add the projects directory
	if err := watcher.Add(t.projectsDir); err != nil {
		slog.Warn("failed to watch projects directory, falling back to polling", "path", t.projectsDir, "err", err)
		t.pollLoop()
		return
	}

	slog.Info("tailer started", "path", t.projectsDir)

	// Poll initially to catch up
	t.pollOnce()

	// Watch loop
	ticker := time.NewTicker(30 * time.Second) // Periodic poll as backup
	defer ticker.Stop()

	for {
		select {
		case <-t.stopChan:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				t.handleFileChange(event.Name)
				t.refreshCaughtUp()
			}
		case <-ticker.C:
			t.pollOnce()
		}
	}
}

// pollLoop is the fallback polling implementation.
func (t *Tailer) pollLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopChan:
			return
		case <-ticker.C:
			t.pollOnce()
		}
	}
}

// pollOnce scans the projects directory for transcript files and processes them.
func (t *Tailer) pollOnce() {
	filepath.Walk(t.projectsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Continue walking
		}

		// Only look for transcript JSONL files
		if info.IsDir() || !isTranscriptFile(path) {
			return nil
		}

		t.handleFileChange(path)
		return nil
	})
	t.refreshCaughtUp()
}

// CaughtUp reports whether the most recent poll cycle finished with no
// pending bytes for any tracked file. Returns true before any tracked files
// exist (no work pending implies caught up).
func (t *Tailer) CaughtUp() bool {
	return t.caughtUp.Load()
}

// refreshCaughtUp recomputes the caught-up flag by comparing each tracked
// file's persisted offset against its current size.
func (t *Tailer) refreshCaughtUp() {
	t.offsetMu.Lock()
	snapshot := make(map[string]int64, len(t.offsets))
	for f, o := range t.offsets {
		snapshot[f] = o
	}
	t.offsetMu.Unlock()

	for f, off := range snapshot {
		info, err := os.Stat(f)
		if err != nil {
			// Treat unreadable tracked files as caught-up (we can't make
			// progress) so a single missing file doesn't pin the flag false.
			continue
		}
		if info.Size() > off {
			t.caughtUp.Store(false)
			return
		}
	}
	t.caughtUp.Store(true)
}

// handleFileChange processes a changed transcript file.
func (t *Tailer) handleFileChange(filePath string) {
	if !isTranscriptFile(filePath) {
		return
	}
	t.processFile(filePath)
}

// processFile reads new content from a transcript file starting at the saved
// offset, ingests any usage events, and advances the offset by the number of
// bytes the parser fully consumed (complete, newline-terminated lines —
// including malformed lines whose errors were recorded). Event inserts and
// the offset advance are committed in a single SQL transaction so a crash
// mid-batch cannot leave the file half-ingested with the offset already
// moved past the un-inserted events.
func (t *Tailer) processFile(filePath string) {
	t.offsetMu.Lock()
	offset, cached := t.offsets[filePath]
	t.offsetMu.Unlock()

	if !cached {
		dbOffset, err := t.store.GetTailerOffset(filePath)
		if err != nil {
			slog.Warn("failed to load tailer offset from database", "path", filePath, "err", err)
		}
		offset = dbOffset
	}

	file, err := os.Open(filePath)
	if err != nil {
		slog.Error("failed to open transcript file", "path", filePath, "err", err)
		t.recordParseError("tailer", filePath, fmt.Sprintf("failed to open: %v", err), "")
		return
	}
	defer file.Close()

	if offset > 0 {
		if _, err := file.Seek(offset, 0); err != nil {
			slog.Error("failed to seek in transcript file", "path", filePath, "offset", offset, "err", err)
			return
		}
	}

	parser := NewParser(file)
	var events []*ParsedEvent
	for {
		event, err := parser.ParseNext()
		if err != nil {
			slog.Error("parser error", "path", filePath, "err", err)
			break
		}
		if event == nil {
			break
		}
		events = append(events, event)
	}

	parseErrors := parser.Errors()
	newOffset := offset + parser.BytesConsumed()

	if newOffset == offset && len(events) == 0 && len(parseErrors) == 0 {
		return
	}

	tx, err := t.store.DB().Begin()
	if err != nil {
		slog.Error("failed to begin tailer transaction", "path", filePath, "err", err)
		return
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, event := range events {
		cost, costSource := ResolveCost(
			event.ReportedCost,
			event.Model,
			event.InputTokens,
			event.OutputTokens,
			event.CacheCreationTokens,
			event.CacheReadTokens,
			t.priceTable,
		)
		if _, err := tx.Exec(`
			INSERT INTO usage_events (
				occurred_at, source, session_id, message_id, project_path,
				input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
				cost_usd_equivalent, cost_source, model, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, event.OccurredAt, "tailer", event.SessionID, event.MessageID, event.ProjectPath,
			event.InputTokens, event.OutputTokens, event.CacheCreationTokens, event.CacheReadTokens,
			cost, costSource, event.Model, event.RawJSON); err != nil {
			slog.Error("failed to insert event", "path", filePath, "err", err)
			if _, perr := tx.Exec(
				`INSERT INTO parse_errors (occurred_at, source, reason, payload) VALUES (?, ?, ?, ?)`,
				time.Now(), "tailer", fmt.Sprintf("database insert failed: %v", err), event.RawJSON,
			); perr != nil {
				slog.Error("failed to record insert-error", "path", filePath, "err", perr)
			}
		}
	}

	for _, parseErr := range parseErrors {
		if _, err := tx.Exec(
			`INSERT INTO parse_errors (occurred_at, source, reason, payload) VALUES (?, ?, ?, ?)`,
			time.Now(), "tailer", parseErr.Reason, parseErr.Line,
		); err != nil {
			slog.Error("failed to record parse error", "path", filePath, "err", err)
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO tailer_offsets (file_path, byte_offset, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			byte_offset = excluded.byte_offset,
			updated_at = excluded.updated_at
	`, filePath, newOffset, time.Now()); err != nil {
		slog.Error("failed to persist tailer offset", "path", filePath, "offset", newOffset, "err", err)
		return
	}

	if err := tx.Commit(); err != nil {
		slog.Error("failed to commit tailer transaction", "path", filePath, "err", err)
		return
	}
	committed = true

	t.offsetMu.Lock()
	t.offsets[filePath] = newOffset
	t.offsetMu.Unlock()
}

// recordParseError records a parse error in the database.
func (t *Tailer) recordParseError(source, path, reason, payload string) {
	_, err := t.store.InsertParseError(time.Now(), source, reason, payload)
	if err != nil {
		slog.Error("failed to record parse error", "path", path, "err", err)
	}
}

// isTranscriptFile checks if a file is a transcript JSONL.
func isTranscriptFile(path string) bool {
	// Claude Code stores transcripts as JSONL files with the session ID as the filename
	// Layout: ~/.claude/projects/<encoded-project-path>/<session-id>.jsonl
	// We accept any .jsonl file since the tailer only watches the configured projects dir
	return filepath.Ext(path) == ".jsonl"
}
