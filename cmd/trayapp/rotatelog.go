package main

import (
	"fmt"
	"io"
	"log/slog"
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
	var renameErr error
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
		if err := os.Rename(src, dst); err != nil && i == 1 {
			renameErr = err
		}
	}

	flags := os.O_APPEND | os.O_CREATE | os.O_WRONLY
	var copyErr error
	if renameErr != nil {
		// On Windows any other handle opened without FILE_SHARE_DELETE (a
		// second instance, a log viewer) blocks the rename. Reopening the
		// same file and counting from zero would let it grow without bound,
		// so copy it out and truncate it in place instead.
		copyErr = copyFile(w.path, w.path+".1")
		flags |= os.O_TRUNC
	}

	f, err := os.OpenFile(w.path, flags, 0644)
	if err != nil {
		return fmt.Errorf("reopen log file: %w", err)
	}
	w.f = f
	w.size = 0
	if renameErr != nil {
		// Written straight to the file: logging through slog here would
		// re-enter Write while w.mu is held.
		slog.New(slog.NewJSONHandler(f, nil)).Warn("log rotation: rename failed; copied and truncated in place",
			"err", renameErr, "copy_err", copyErr)
		if info, err := f.Stat(); err == nil {
			w.size = info.Size()
		}
	}
	return nil
}

// copyFile copies src to dst, replacing dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
