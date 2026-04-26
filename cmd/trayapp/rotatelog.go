package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingWriter is a minimal size-rotating log writer. When the active file
// exceeds maxSize, it is rotated to "<path>.1", existing backups shift up by
// one, and the oldest beyond maxBackups is deleted.
type rotatingWriter struct {
	path       string
	maxSize    int64
	maxBackups int
	mu         sync.Mutex
	f          *os.File
	size       int64
}

func newRotatingWriter(path string, maxSize int64, maxBackups int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	return &rotatingWriter{
		path:       path,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		f:          f,
		size:       info.Size(),
	}, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func (w *rotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil

	// Shift backups: .N-1 -> .N, drop the oldest beyond maxBackups.
	for i := w.maxBackups; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i-1)
		dst := fmt.Sprintf("%s.%d", w.path, i)
		if i == 1 {
			src = w.path
		}
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if i == w.maxBackups {
			_ = os.Remove(dst)
		}
		_ = os.Rename(src, dst)
	}

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("reopen log file: %w", err)
	}
	w.f = f
	w.size = 0
	return nil
}
