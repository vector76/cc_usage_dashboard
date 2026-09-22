package oauthusage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func wantFloat(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil, want %v", label, want)
	}
	if *got != want {
		t.Errorf("%s: got %v, want %v", label, *got, want)
	}
}

func wantTime(t *testing.T, label string, got *time.Time, want time.Time) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil, want %s", label, want)
	}
	if !got.Equal(want) {
		t.Errorf("%s: got %s, want %s", label, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// TestParseGoldenResponse pins the mapping from the real API payload to a
// Reading. The fixture is a verbatim capture of a live response, including
// the internal codename keys (tangelo, iguana_necktie, ...) that must be
// ignored entirely.
func TestParseGoldenResponse(t *testing.T) {
	got, err := Parse(readFixture(t, "usage_response.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	wantFloat(t, "SessionUsed", got.SessionUsed, 3)
	wantFloat(t, "WeeklyUsed", got.WeeklyUsed, 29)
	wantFloat(t, "FableWeeklyUsed", got.FableWeeklyUsed, 1)

	wantTime(t, "SessionWindowEnds", got.SessionWindowEnds,
		time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC))
	wantTime(t, "WeeklyWindowEnds", got.WeeklyWindowEnds,
		time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC))
}

// TestWindowEndsTruncatedToSecond is the landmine test.
//
// The server recomputes resets_at per request: across two reads of the SAME
// window the whole seconds are identical but the microseconds differ, and
// they differ between entries within a single response too. Comparing the
// raw value would report "the window changed" on literally every poll,
// defeating dedup and making every stored row look distinct.
//
// Both fixtures are real captures of the same two windows ~47 minutes apart.
func TestWindowEndsTruncatedToSecond(t *testing.T) {
	first, err := Parse(readFixture(t, "usage_response.json"))
	if err != nil {
		t.Fatalf("Parse first: %v", err)
	}
	second, err := Parse(readFixture(t, "usage_response_jitter.json"))
	if err != nil {
		t.Fatalf("Parse second: %v", err)
	}

	if !first.SessionWindowEnds.Equal(*second.SessionWindowEnds) {
		t.Errorf("session window ends differ across reads of the same window: %s vs %s",
			first.SessionWindowEnds.Format(time.RFC3339Nano),
			second.SessionWindowEnds.Format(time.RFC3339Nano))
	}
	if !first.WeeklyWindowEnds.Equal(*second.WeeklyWindowEnds) {
		t.Errorf("weekly window ends differ across reads of the same window: %s vs %s",
			first.WeeklyWindowEnds.Format(time.RFC3339Nano),
			second.WeeklyWindowEnds.Format(time.RFC3339Nano))
	}

	// The percentages genuinely moved between the two reads; if they had
	// not, the equality above would be trivially satisfied by identical
	// input and the test would prove nothing.
	wantFloat(t, "second SessionUsed", second.SessionUsed, 7)
	wantFloat(t, "second WeeklyUsed", second.WeeklyUsed, 30)
}

// TestWindowEndsAreUTC guards against a parsed offset leaking a non-UTC
// location into the store, where it would render as a wall-clock time in
// the wrong zone.
func TestWindowEndsAreUTC(t *testing.T) {
	got, err := Parse(readFixture(t, "usage_response.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if loc := got.SessionWindowEnds.Location(); loc != time.UTC {
		t.Errorf("SessionWindowEnds location = %v, want UTC", loc)
	}
	if loc := got.WeeklyWindowEnds.Location(); loc != time.UTC {
		t.Errorf("WeeklyWindowEnds location = %v, want UTC", loc)
	}
}

// TestParseIgnoresUnknownKindsAndModels pins forward compatibility. A kind
// we have never seen, and a weekly_scoped row for a model other than Fable,
// must both be skipped rather than guessed at or misattributed to Fable.
func TestParseIgnoresUnknownKindsAndModels(t *testing.T) {
	got, err := Parse(readFixture(t, "usage_response_unknown_kinds.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	wantFloat(t, "SessionUsed", got.SessionUsed, 12)
	wantFloat(t, "WeeklyUsed", got.WeeklyUsed, 44)

	if got.FableWeeklyUsed != nil {
		t.Errorf("FableWeeklyUsed = %v, want nil: a weekly_scoped row for Mythos "+
			"must not be recorded as Fable", *got.FableWeeklyUsed)
	}
}

// TestParseRejectsFableOnly mirrors the rule already documented for the
// userscript: the optional Fable sub-row alone is never sufficient to call a
// read successful. Accepting it would suppress the very error that tells us
// the rows we depend on have gone missing.
func TestParseRejectsFableOnly(t *testing.T) {
	_, err := Parse(readFixture(t, "usage_response_fable_only.json"))
	if err == nil {
		t.Fatal("Parse: got nil error, want a parse failure")
	}
	if !errors.Is(err, ErrNoUsableLimits) {
		t.Errorf("Parse: got %v, want ErrNoUsableLimits", err)
	}
}

// TestParseRejectsMalformedJSON keeps a truncated or HTML error body from
// silently producing an empty Reading.
func TestParseRejectsMalformedJSON(t *testing.T) {
	if _, err := Parse([]byte("<html>502 Bad Gateway</html>")); err == nil {
		t.Fatal("Parse: got nil error, want a parse failure")
	}
}

// TestParseDoesNotInferActivityFromIsActive documents a deliberate decision,
// so that a future refactor cannot quietly "improve" it.
//
// The API's is_active does NOT mean "this window is open and accruing":
// observed live, session showed is_active=false while session utilization
// climbed 3% -> 7%, and only the highest-utilization row (weekly_all) was
// true. Its actual meaning is unconfirmed, so it must not be wired to the
// session_active / weekly_active limbo signal, whose tri-state convention
// already encodes "absent = unknown". The userscript remains the authority
// on limbo until the field's semantics are pinned down against a live page.
func TestParseDoesNotInferActivityFromIsActive(t *testing.T) {
	got, err := Parse(readFixture(t, "usage_response.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.SessionActive != nil {
		t.Errorf("SessionActive = %v, want nil (is_active must not be mapped)", *got.SessionActive)
	}
	if got.WeeklyActive != nil {
		t.Errorf("WeeklyActive = %v, want nil (is_active must not be mapped)", *got.WeeklyActive)
	}
}

// TestClassifyHTTPError keeps a stale credential from being reported as a
// broken data source. Both conditions return 401 with
// error.type == "authentication_error"; only the human-readable message
// differs, so the classifier keys off status and type, never the message.
//
// Misfiling this as a source failure would light up the parse-error health
// signal -- the tray would claim the scraper broke when nothing broke and
// the condition self-heals on the next token refresh.
func TestClassifyHTTPError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      []byte
		wantStale bool
	}{
		{
			name:      "expired token",
			status:    401,
			body:      readFixture(t, "error_expired.json"),
			wantStale: true,
		},
		{
			name:      "invalid token",
			status:    401,
			body:      readFixture(t, "error_invalid.json"),
			wantStale: true,
		},
		{
			name:      "server error is not a credential problem",
			status:    500,
			body:      []byte(`{"type":"error","error":{"type":"api_error","message":"internal"}}`),
			wantStale: false,
		},
		{
			name:      "rate limited is not a credential problem",
			status:    429,
			body:      []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`),
			wantStale: false,
		},
		{
			name:      "401 with an unparseable body is still a credential problem",
			status:    401,
			body:      []byte("Unauthorized"),
			wantStale: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ClassifyHTTPError(tc.status, tc.body)
			if err == nil {
				t.Fatal("ClassifyHTTPError: got nil, want an error")
			}
			if gotStale := errors.Is(err, ErrCredentialStale); gotStale != tc.wantStale {
				t.Errorf("errors.Is(err, ErrCredentialStale) = %v, want %v (err: %v)",
					gotStale, tc.wantStale, err)
			}
		})
	}
}

// TestClassifyHTTPErrorIgnoresSuccess keeps the classifier from inventing an
// error for a 200, which would be an easy mistake in a caller that funnels
// every response through it.
func TestClassifyHTTPErrorIgnoresSuccess(t *testing.T) {
	if err := ClassifyHTTPError(200, readFixture(t, "usage_response.json")); err != nil {
		t.Errorf("ClassifyHTTPError(200, ...) = %v, want nil", err)
	}
}
