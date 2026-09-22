package oauthusage

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeCredentials(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

// expiresAt is unix milliseconds in the file Claude Code writes.
func msSinceEpoch(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }

func TestLoadCredential(t *testing.T) {
	expires := time.Date(2026, 9, 22, 18, 31, 52, 0, time.UTC)
	path := writeCredentials(t, `{
	  "claudeAiOauth": {
	    "accessToken": "test-access-token",
	    "refreshToken": "test-refresh-token",
	    "expiresAt": `+strconv.FormatInt(msSinceEpoch(expires), 10)+`,
	    "subscriptionType": "team"
	  }
	}`)

	cred, err := LoadCredential(path)
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if cred.AccessToken != "test-access-token" {
		t.Errorf("AccessToken = %q", cred.AccessToken)
	}
	if !cred.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %s, want %s", cred.ExpiresAt, expires)
	}
}

// TestLoadCredentialMissingFileIsStale keeps a never-logged-in or
// misconfigured host from being reported as a broken data source. The
// user-visible state is the same as an expired token: this source cannot
// produce a reading right now.
func TestLoadCredentialMissingFileIsStale(t *testing.T) {
	_, err := LoadCredential(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrCredentialStale) {
		t.Errorf("got %v, want ErrCredentialStale", err)
	}
}

func TestLoadCredentialRejectsEmptyToken(t *testing.T) {
	path := writeCredentials(t, `{"claudeAiOauth":{"accessToken":"","expiresAt":1790119912311}}`)
	_, err := LoadCredential(path)
	if !errors.Is(err, ErrCredentialStale) {
		t.Errorf("got %v, want ErrCredentialStale", err)
	}
}

func TestLoadCredentialRejectsMalformedJSON(t *testing.T) {
	path := writeCredentials(t, `{"claudeAiOauth":`)
	if _, err := LoadCredential(path); err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
}

// TestLoadCredentialDoesNotLeakTokenInError matters because these errors
// are logged. A parse or validation failure must describe the problem
// without quoting the file's contents back into the log.
func TestLoadCredentialDoesNotLeakTokenInError(t *testing.T) {
	const secret = "sk-ant-oat01-SUPERSECRET"
	path := writeCredentials(t,
		`{"claudeAiOauth":{"accessToken":"`+secret+`","expiresAt":"not-a-number"}}`)

	_, err := LoadCredential(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the access token: %v", err)
	}
}

// TestCredentialUsableAt pins the pre-flight check. The token lives about
// eight hours and is refreshed only when Claude Code itself runs, so an
// idle host routinely holds an expired one. Detecting that locally turns a
// guaranteed-to-fail request into a skipped tick.
func TestCredentialUsableAt(t *testing.T) {
	expires := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	cred := &Credential{AccessToken: "t", ExpiresAt: expires}

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "well before expiry", now: expires.Add(-4 * time.Hour), want: true},
		{name: "just past expiry", now: expires.Add(1 * time.Second), want: false},
		{name: "long past expiry", now: expires.Add(12 * time.Hour), want: false},
		{
			// Inside the safety margin the token is technically still
			// valid, but a request started now could arrive after it
			// lapses. Skipping is cheaper than a round trip that 401s.
			name: "inside the safety margin",
			now:  expires.Add(-expiryMargin / 2),
			want: false,
		},
		{name: "just outside the safety margin", now: expires.Add(-2 * expiryMargin), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cred.UsableAt(tc.now); got != tc.want {
				t.Errorf("UsableAt(%s) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

// TestCredentialWithoutExpiryIsUsable keeps a credential format that omits
// expiresAt (or carries zero) from being treated as permanently expired —
// a long-lived token from `claude setup-token` would plausibly look like
// this. Let the server be the judge in that case.
func TestCredentialWithoutExpiryIsUsable(t *testing.T) {
	cred := &Credential{AccessToken: "t"}
	if !cred.UsableAt(time.Now()) {
		t.Error("a credential with no expiry should be attempted, not skipped")
	}
}

func TestResolveCredentialsPath(t *testing.T) {
	t.Run("explicit override wins", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join("X", "env"))
		got := ResolveCredentialsPath(filepath.Join("Y", "explicit.json"))
		if got != filepath.Join("Y", "explicit.json") {
			t.Errorf("got %q, want the explicit override", got)
		}
	})

	t.Run("CLAUDE_CONFIG_DIR is honored", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", dir)
		want := filepath.Join(dir, ".credentials.json")
		if got := ResolveCredentialsPath(""); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to the home directory", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		got := ResolveCredentialsPath("")
		if !strings.HasSuffix(got, filepath.Join(".claude", ".credentials.json")) {
			t.Errorf("got %q, want a path ending in .claude/.credentials.json", got)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("got %q, want an absolute path", got)
		}
	})
}
