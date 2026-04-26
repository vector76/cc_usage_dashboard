package testhelper

import (
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"
)

// TempDB creates a temporary in-memory SQLite database for testing.
// It returns a *sql.DB and a cleanup function.
func TempDB(t *testing.T) (*sql.DB, func()) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open temp DB: %v", err)
	}

	cleanup := func() {
		db.Close()
	}

	return db, cleanup
}

// TempDir creates a temporary directory for testing.
// It returns the directory path and a cleanup function.
func TempDir(t *testing.T) (string, func()) {
	dir, err := os.MkdirTemp("", "usage-dashboard-test-*")
	if err != nil {
		t.Fatalf("failed to create temp directory: %v", err)
	}

	cleanup := func() {
		os.RemoveAll(dir)
	}

	return dir, cleanup
}
