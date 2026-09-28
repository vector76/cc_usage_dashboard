// Package authtoken holds the shared secret that non-loopback callers must
// present to the trayapp's HTTP server.
//
// The token lives in its own file under the per-user data dir rather than in
// config.yaml: a config.yaml in a build checkout takes precedence over the
// per-user one and sits one `git add` away from being committed, and the
// token must be rotatable at runtime while config.yaml is read only at start.
// See docs/architecture.md "Network and security".
package authtoken

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileName is the token file's name inside the data dir.
const FileName = "auth_token"

// tokenBytes is the entropy per token. 256 bits puts guessing out of reach
// even with no rate limit on failed attempts.
const tokenBytes = 32

// Store holds the current token in memory and persists it to path. The zero
// value holds no token and rejects every Check, so a store that failed to
// load fails closed.
type Store struct {
	path string

	mu    sync.RWMutex
	token string
}

// Load reads the token at path, generating and persisting a fresh one when
// the file is missing or blank. A blank file is regenerated rather than
// honored because an empty token would match a client that sends none.
func Load(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if tok := strings.TrimSpace(string(data)); tok != "" {
			s.token = tok
			return s, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read token file: %w", err)
	}

	if _, err := s.Rotate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the file the token is persisted to.
func (s *Store) Path() string { return s.path }

// Token returns the current token.
func (s *Store) Token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token
}

// Check reports whether presented matches the current token, in constant
// time so response timing cannot be used to recover it byte by byte.
func (s *Store) Check(presented string) bool {
	s.mu.RLock()
	tok := s.token
	s.mu.RUnlock()
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(tok)) == 1
}

// Rotate generates a new token, persists it, and makes it current. Clients
// holding the old token are rejected from the next request on. The file is
// written before the in-memory token changes, so a failed write leaves the
// old token working rather than a new one that would vanish on restart.
func (s *Store) Rotate() (string, error) {
	tok, err := generate()
	if err != nil {
		return "", err
	}
	if err := writeAtomic(s.path, tok); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.token = tok
	s.mu.Unlock()
	return tok, nil
}

func generate() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeAtomic replaces path via a temp file and rename, so a crash mid-write
// cannot leave a truncated token behind. CreateTemp opens the file 0600;
// Windows ignores the mode bits, and there the per-user data dir's ACL is
// what keeps other accounts out.
func writeAtomic(path, tok string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace token file: %w", err)
	}
	return nil
}
