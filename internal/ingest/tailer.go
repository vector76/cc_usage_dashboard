package ingest

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	go t.run()
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
// including malformed lines whose errors were recorded).
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

	for {
		event, err := parser.ParseNext()
		if err != nil {
			slog.Error("parser error", "path", filePath, "err", err)
			break
		}
		if event == nil {
			break
		}

		cost, costSource := ResolveCost(
			event.ReportedCost,
			event.Model,
			event.InputTokens,
			event.OutputTokens,
			event.CacheCreationTokens,
			event.CacheReadTokens,
			t.priceTable,
		)

		_, err = t.store.InsertUsageEvent(
			event.OccurredAt,
			"tailer",
			event.SessionID,
			event.MessageID,
			event.ProjectPath,
			event.Model,
			event.InputTokens,
			event.OutputTokens,
			event.CacheCreationTokens,
			event.CacheReadTokens,
			cost,
			costSource,
			event.RawJSON,
		)
		if err != nil {
			slog.Error("failed to insert event", "path", filePath, "err", err)
			t.recordParseError("tailer", filePath, fmt.Sprintf("database insert failed: %v", err), event.RawJSON)
		}
	}

	for _, parseErr := range parser.Errors() {
		t.recordParseError("tailer", filePath, parseErr.Reason, parseErr.Line)
	}

	newOffset := offset + parser.BytesConsumed()

	t.offsetMu.Lock()
	t.offsets[filePath] = newOffset
	t.offsetMu.Unlock()

	if err := t.store.SetTailerOffset(filePath, newOffset); err != nil {
		slog.Error("failed to persist tailer offset", "path", filePath, "offset", newOffset, "err", err)
	}
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
