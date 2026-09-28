package authtoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCreatesTokenWhenFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "auth_token")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tok := s.Token()
	// 32 random bytes, base64url without padding.
	if len(tok) != 43 {
		t.Errorf("expected a 43-character token, got %d (%q)", len(tok), tok)
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token must be URL-safe and unpadded, got %q", tok)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	if strings.TrimSpace(string(onDisk)) != tok {
		t.Errorf("file holds %q, store holds %q", onDisk, tok)
	}
}

func TestLoadReusesExistingToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth_token")
	first, err := Load(path)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if first.Token() != second.Token() {
		t.Error("a restart must keep the token, or every client breaks on reboot")
	}
}

// A hand-edited file commonly gains a trailing newline; that must not become
// part of the token.
func TestLoadTrimsWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth_token")
	if err := os.WriteFile(path, []byte("  hand-set-token\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Token() != "hand-set-token" {
		t.Errorf("got %q", s.Token())
	}
}

// An empty file must not yield an empty token: Check("") would then pass for
// any client that sends no header value at all.
func TestLoadRegeneratesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth_token")
	if err := os.WriteFile(path, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Token() == "" {
		t.Fatal("empty file produced an empty token")
	}
	onDisk, _ := os.ReadFile(path)
	if strings.TrimSpace(string(onDisk)) != s.Token() {
		t.Error("regenerated token was not persisted")
	}
}

func TestRotateReplacesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth_token")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	old := s.Token()

	fresh, err := s.Rotate()
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if fresh == old {
		t.Fatal("Rotate returned the old token")
	}
	if s.Token() != fresh {
		t.Error("Token() does not reflect the rotation")
	}
	if s.Check(old) {
		t.Error("the old token must stop working immediately")
	}
	if !s.Check(fresh) {
		t.Error("the new token must work immediately")
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Token() != fresh {
		t.Error("rotation was not persisted; a restart would resurrect the old token")
	}
}

func TestCheck(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "auth_token"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tok := s.Token()

	if !s.Check(tok) {
		t.Error("correct token rejected")
	}
	for _, bad := range []string{"", tok[:len(tok)-1], tok + "x", strings.ToUpper(tok)} {
		if s.Check(bad) {
			t.Errorf("Check(%q) passed", bad)
		}
	}
}

// With no token at all (the store failed to load), nothing may pass —
// failing open here would publish the API to the network unauthenticated.
func TestZeroStoreRejectsEverything(t *testing.T) {
	var s Store
	if s.Check("") {
		t.Error("an unloaded store accepted an empty token")
	}
	if s.Check("anything") {
		t.Error("an unloaded store accepted a token")
	}
}
