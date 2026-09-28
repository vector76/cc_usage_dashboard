package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Normal rotation: the active file moves to .1 and a fresh one starts.
func TestRotatingWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trayapp.log")
	w, err := newRotatingWriter(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n')
	for i := 0; i < 5; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	if info, err := os.Stat(path); err != nil || info.Size() > 100 {
		t.Errorf("active log size = %v (err %v), want <= 100", info.Size(), err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("no .1 backup after rotation: %v", err)
	}
}

// On Windows another handle without FILE_SHARE_DELETE (a second trayapp, a
// log viewer) makes the rename fail. Rotation used to ignore that, reopen the
// same file and count from zero, so the log grew without bound. The cap must
// hold anyway, and the rotated-out lines should still reach .1.
func TestRotatingWriterStaysBoundedWhenRenameFails(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open handle blocks rename only on Windows")
	}
	path := filepath.Join(t.TempDir(), "trayapp.log")
	const maxSize = 2000
	w, err := newRotatingWriter(path, maxSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Go opens files without FILE_SHARE_DELETE, so this handle blocks the
	// rename exactly as a second instance would.
	holder, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n')
	for i := 0; i < 120; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxSize {
		t.Errorf("active log grew to %d bytes with a %d-byte cap", info.Size(), maxSize)
	}
	if w.size != info.Size() {
		t.Errorf("size counter %d does not match the file's %d bytes", w.size, info.Size())
	}
	if backup, err := os.ReadFile(path + ".1"); err != nil || !bytes.Contains(backup, line) {
		t.Errorf("rotated-out lines missing from .1 (err %v)", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Contains(got, []byte("log rotation")) {
		t.Errorf("active log does not record the failed rename: %q", got)
	}
}
