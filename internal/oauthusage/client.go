package oauthusage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// DefaultBaseURL is Anthropic's API origin. Overridable for tests.
	DefaultBaseURL = "https://api.anthropic.com"

	usagePath = "/api/oauth/usage"

	// maxResponseBytes bounds what a single reply may cost us. The real
	// payload is a couple of kilobytes; anything approaching this is a
	// misrouted response, not usage data.
	maxResponseBytes = 1 << 20

	// requestTimeout is a per-request ceiling independent of the caller's
	// context, so a stalled connection cannot outlive a poll interval.
	requestTimeout = 15 * time.Second

	// userAgent identifies this client honestly. It deliberately does not
	// impersonate Claude Code: the endpoint was verified not to require
	// any User-Agent, so there is nothing to gain from spoofing one.
	// Pinned by TestClientSendsOnlyAuthorization.
	userAgent = "cc-usage-dashboard"
)

// Client reads quota figures from the OAuth usage endpoint.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client pointed at the live endpoint.
func NewClient() *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: requestTimeout},
	}
}

// Fetch performs one read.
//
// Authorization is the only header the endpoint requires — verified by
// sending five variants back to back with the same token, including one
// with no User-Agent and one with no anthropic-beta, all of which returned
// identical payloads.
//
// A 401 comes back wrapping ErrCredentialStale so the caller can report
// "temporarily unavailable" rather than "the source broke"; see
// ClassifyHTTPError.
func (c *Client) Fetch(ctx context.Context, token string) (*Reading, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+usagePath, nil)
	if err != nil {
		return nil, fmt.Errorf("building usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL is fixed and carries no secrets, but the token is in a
		// header and url.Error never renders headers, so %w is safe here.
		return nil, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so a body sitting exactly at the limit is
	// distinguishable from one that was truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading usage response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("usage response exceeds %d bytes; refusing to parse", maxResponseBytes)
	}

	if err := ClassifyHTTPError(resp.StatusCode, body); err != nil {
		return nil, err
	}
	return Parse(body)
}
