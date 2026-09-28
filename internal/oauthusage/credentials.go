package oauthusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// expiryMargin is how long before the stated expiry a token stops being
// offered. A request started inside this window could arrive after the
// token lapses; skipping the tick is cheaper than a round trip that 401s,
// and it keeps the logs free of a failure we could have predicted.
const expiryMargin = 60 * time.Second

// Credential is the part of Claude Code's .credentials.json this package
// uses. The refresh token is deliberately not read: refreshing it here
// would race Claude Code's own refresh, and if rotation is single-use,
// losing that race logs the user out of their primary tool. The token is
// treated as read-only, produced and rotated by Claude Code alone.
type Credential struct {
	AccessToken string
	// ExpiresAt is zero when the file did not state one. That is treated
	// as "no local opinion", not as "expired" — see UsableAt.
	ExpiresAt time.Time
}

// credentialsFile mirrors the on-disk shape. Only the two fields below are
// decoded; everything else in the file (refreshToken, scopes,
// subscriptionType, rateLimitTier) is intentionally left alone.
type credentialsFile struct {
	ClaudeAIOAuth struct {
		AccessToken string `json:"accessToken"`
		ExpiresAtMs int64  `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// ResolveCredentialsPath locates Claude Code's credentials file.
//
// Precedence: an explicit config override, then $CLAUDE_CONFIG_DIR, then
// ~/.claude. Enumerating other stores is deliberately not attempted — the
// usage endpoint reports account-scoped figures, so any one valid token
// for the account returns the same numbers, and a single well-known
// location is enough.
func ResolveCredentialsPath(override string) string {
	if override != "" {
		return override
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".credentials.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude", ".credentials.json")
	}
	return filepath.Join(".claude", ".credentials.json")
}

// LoadCredential reads the access token fresh from disk.
//
// Callers must call this on every poll rather than caching: Claude Code
// rewrites the file when it rotates the token, and a cached copy goes
// stale within the day.
//
// A missing file or an empty token yields ErrCredentialStale, the same
// state an expired token produces — in every case the source simply
// cannot produce a reading right now, and the distinction does not change
// what the dashboard should show. Errors never quote the file's contents.
//
// A read that fails or does not decode is retried a few times before it is
// reported: a poll that lands while Claude Code is rewriting the file (or,
// on Windows, holding it open) sees a torn or locked file for a moment, and
// that is not the broken source the error would otherwise report.
func LoadCredential(path string) (*Credential, error) {
	var (
		f   credentialsFile
		err error
	)
	for attempt := 1; ; attempt++ {
		f, err = readCredentialsFile(path)
		if err == nil || errors.Is(err, ErrCredentialStale) || attempt == credentialReadAttempts {
			break
		}
		time.Sleep(credentialRetryDelay)
	}
	if err != nil {
		return nil, err
	}

	if f.ClaudeAIOAuth.AccessToken == "" {
		return nil, fmt.Errorf("%w: no access token in %s", ErrCredentialStale, path)
	}

	cred := &Credential{AccessToken: f.ClaudeAIOAuth.AccessToken}
	if f.ClaudeAIOAuth.ExpiresAtMs > 0 {
		cred.ExpiresAt = time.UnixMilli(f.ClaudeAIOAuth.ExpiresAtMs).UTC()
	}
	return cred, nil
}

// credentialReadAttempts and credentialRetryDelay bound LoadCredential's
// retry of a torn or locked read: long enough to outlast a rewrite of a
// file this small, short enough not to hold up the poll.
const (
	credentialReadAttempts = 3
	credentialRetryDelay   = 100 * time.Millisecond
)

func readCredentialsFile(path string) (credentialsFile, error) {
	var f credentialsFile
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f, fmt.Errorf("%w: no credentials file at %s", ErrCredentialStale, path)
		}
		// os.ReadFile's error text is a path and a syscall message, not
		// file contents, so it is safe to wrap.
		return f, fmt.Errorf("reading credentials: %w", err)
	}

	if err := json.Unmarshal(data, &f); err != nil {
		// Deliberately not %w on the decode error: json.Unmarshal's
		// message can quote the offending value, which for this file
		// could be the token itself.
		return f, fmt.Errorf("credentials file at %s is not valid JSON", path)
	}
	return f, nil
}

// UsableAt reports whether the token is worth sending at now.
//
// A zero ExpiresAt means the file stated none, which is treated as usable:
// a long-lived token (from `claude setup-token`, say) would plausibly look
// like that, and refusing to try it would be worse than letting the server
// decide.
func (c *Credential) UsableAt(now time.Time) bool {
	if c.AccessToken == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return now.Before(c.ExpiresAt.Add(-expiryMargin))
}
