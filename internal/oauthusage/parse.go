// Package oauthusage reads quota figures from Claude Code's OAuth usage
// endpoint (GET https://api.anthropic.com/api/oauth/usage) using the access
// token Claude Code stores locally.
//
// This is a distinct data source from the userscript (docs/userscript.md):
// it reads the same account-scoped quota Anthropic renders on the usage
// page, but as structured JSON rather than scraped DOM, and without needing
// a browser tab open. Like the userscript it is non-perturbing — the
// endpoint reports usage, it does not consume it, and repeated reads were
// observed not to open a 5-hour window — which is what makes it admissible
// as an automated source where invoking Claude Code itself is not (see
// "Tier 0 (rejected)" in docs/data-sources.md).
//
// Only two headers matter to the endpoint: Authorization. A User-Agent and
// the anthropic-beta header were both verified to be unnecessary — five
// header variants sent back to back with the same token, including one with
// no User-Agent at all, returned identical payloads.
package oauthusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors callers switch on with errors.Is.
var (
	// ErrCredentialStale means the token could not be used: expired,
	// revoked, or malformed. It is emphatically NOT a broken-source
	// condition — the access token lives about 8 hours and is refreshed
	// only when Claude Code itself runs, so a host that has been idle
	// overnight hits this routinely and recovers on its own. Callers must
	// report it as "source temporarily unavailable" and must not route it
	// to the parse-error health signal, which exists to mean "the scraper
	// broke, go fix it".
	ErrCredentialStale = errors.New("oauth credential is not usable")

	// ErrNoUsableLimits means the response parsed as JSON but carried
	// neither a session nor a weekly-aggregate row. That is a genuine
	// schema break worth surfacing, unlike a merely absent optional
	// sub-row.
	ErrNoUsableLimits = errors.New("response contained no usable limits")
)

// fableDisplayName is the scope.model.display_name the API uses for the
// weekly sub-row the dashboard tracks separately. Matched exactly: a
// weekly_scoped row for any other model is ignored rather than folded in,
// so a newly-scoped model cannot silently land in the Fable series.
const fableDisplayName = "Fable"

// Reading is the subset of the usage response the dashboard consumes,
// shaped to line up with the fields of a snapshot.
type Reading struct {
	SessionUsed       *float64
	SessionWindowEnds *time.Time
	WeeklyUsed        *float64
	WeeklyWindowEnds  *time.Time

	// FableWeeklyUsed has no window companion: the API reports the same
	// reset boundary for the scoped row as for the weekly aggregate, so
	// the series is anchored on the existing weekly window. Mirrors the
	// userscript's fable_weekly_used.
	FableWeeklyUsed *float64

	// SessionActive and WeeklyActive are always nil, and deliberately so.
	//
	// The API's is_active field does NOT mean "this window is open and
	// accruing": observed live, the session row reported is_active=false
	// while session utilization climbed from 3% to 7%, and the only row
	// set true was the highest-utilization one (weekly_all at 30%). It
	// may mark the currently binding constraint, but that is a guess.
	//
	// The limbo signal these fields feed has a tri-state convention where
	// absent means "unknown" (docs/data-sources.md), so leaving them nil
	// is a correct encoding rather than a stopgap, and the userscript
	// stays the authority on limbo. They are carried here — rather than
	// omitted — so the decision is visible at the point a future mapping
	// would be written, and pinned by
	// TestParseDoesNotInferActivityFromIsActive.
	SessionActive *bool
	WeeklyActive  *bool
}

// apiResponse is intentionally narrow. The live payload also carries a
// couple of dozen top-level keys under internal codenames (tangelo,
// iguana_necktie, nimbus_quill, wattle_ember, ...) which churn as features
// ship; decoding only limits[] keeps that churn from reaching us. limits[]
// is the self-describing part — each entry names its own kind and scope —
// and is what the mapping depends on.
type apiResponse struct {
	Limits []apiLimit `json:"limits"`
}

type apiLimit struct {
	Kind     string     `json:"kind"`
	Percent  *float64   `json:"percent"`
	ResetsAt *time.Time `json:"resets_at"`
	Scope    *apiScope  `json:"scope"`
}

type apiScope struct {
	Model *apiModel `json:"model"`
}

type apiModel struct {
	DisplayName string `json:"display_name"`
}

type apiError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Parse maps a 200 response body to a Reading.
//
// Unrecognised limit kinds and weekly_scoped rows for models other than
// Fable are skipped rather than guessed at. It fails only when neither the
// session nor the weekly-aggregate row is present.
func Parse(body []byte) (*Reading, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding usage response: %w", err)
	}

	var r Reading
	for _, l := range resp.Limits {
		switch l.Kind {
		case "session":
			r.SessionUsed = l.Percent
			r.SessionWindowEnds = normalizeResetsAt(l.ResetsAt)
		case "weekly_all":
			r.WeeklyUsed = l.Percent
			r.WeeklyWindowEnds = normalizeResetsAt(l.ResetsAt)
		case "weekly_scoped":
			if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName == fableDisplayName {
				r.FableWeeklyUsed = l.Percent
			}
		}
	}

	// The Fable sub-row alone is never sufficient — it is optional and
	// plan-dependent, so accepting it would suppress the error that fires
	// when the rows we actually depend on go missing. Same rule the
	// userscript applies to the page (docs/userscript.md, "Endpoint").
	if r.SessionUsed == nil && r.WeeklyUsed == nil {
		return nil, ErrNoUsableLimits
	}

	return &r, nil
}

// normalizeResetsAt converts a reset boundary to UTC and truncates it to
// whole seconds.
//
// The server recomputes resets_at per request: across two reads of the same
// window the whole seconds are stable but the microseconds differ, and they
// differ between entries within a single response too (observed
// .855416/.855447/.855692 in serialization order). Storing the raw value
// would make every poll of an unchanged window look like a new one,
// defeating dedup and breaking any equality-based freshness comparison.
// Truncating to the second is lossless for a boundary that always lands on
// a whole minute in practice. Pinned by TestWindowEndsTruncatedToSecond.
func normalizeResetsAt(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	truncated := t.UTC().Truncate(time.Second)
	return &truncated
}

// ClassifyHTTPError turns a non-2xx response into an error, distinguishing
// a stale credential from every other failure. It returns nil for a 2xx so
// a caller can funnel every response through it.
//
// A 401 is always a credential problem regardless of whether the body
// parses: both observed conditions ("OAuth access token has expired.
// Re-authenticate to continue." and "OAuth access token is invalid.")
// return 401 with error.type == "authentication_error", and an
// intermediary's plain-text 401 means the same thing. The message string is
// never matched on — it is prose and will change.
func ClassifyHTTPError(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}

	var e apiError
	_ = json.Unmarshal(body, &e) // best effort; a non-JSON body is fine

	detail := e.Error.Message
	if detail == "" {
		detail = fmt.Sprintf("HTTP %d", status)
	}

	if status == 401 {
		return fmt.Errorf("%w: %s", ErrCredentialStale, detail)
	}
	return fmt.Errorf("usage endpoint returned HTTP %d: %s", status, detail)
}
