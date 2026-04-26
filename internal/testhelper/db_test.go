package testhelper

import (
	"os"
	"testing"
)

func TestTempDB(t *testing.T) {
	db, cleanup := TempDB(t)
	defer cleanup()

	if db == nil {
		t.Error("TempDB returned nil")
	}

	// Verify database is working
	var result int
	err := db.QueryRow("SELECT 1").Scan(&result)
	if err != nil {
		t.Fatalf("failed to query database: %v", err)
	}
	if result != 1 {
		t.Errorf("expected 1, got %d", result)
	}
}

func TestTempDir(t *testing.T) {
	dir, cleanup := TempDir(t)
	defer cleanup()

	if dir == "" {
		t.Error("TempDir returned empty string")
	}

	// Verify directory exists
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("failed to stat directory: %v", err)
	}
	if !info.IsDir() {
		t.Error("path is not a directory")
	}
}
