package oauthusage

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestReadPlan(t *testing.T) {
	cases := map[string]string{
		`{"claudeAiOauth":{"accessToken":"t","subscriptionType":"enterprise"}}`: "enterprise",
		`{"claudeAiOauth":{"accessToken":"t","subscriptionType":"team"}}`:       "team",
		`{"claudeAiOauth":{"accessToken":"t"}}`:                                 "",
		`{}`:                                                                    "",
	}
	for body, want := range cases {
		if got, err := ReadPlan(writeCredentials(t, body)); err != nil || got != want {
			t.Errorf("ReadPlan(%s) = %q, %v; want %q", body, got, err, want)
		}
	}
}

// The plan does not depend on the token: an expired token, an empty one, or
// none at all must not stop the plan from being read.
func TestReadPlanIgnoresTokenState(t *testing.T) {
	body := `{"claudeAiOauth":{"accessToken":"","expiresAt":1,"subscriptionType":"enterprise"}}`
	if got, err := ReadPlan(writeCredentials(t, body)); err != nil || got != "enterprise" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestReadPlanMissingFileIsNotAnError(t *testing.T) {
	got, err := ReadPlan(t.TempDir() + "/nope.json")
	if err != nil || got != "" {
		t.Errorf("got %q, %v; want empty, nil", got, err)
	}
}

func TestReadPlanMalformedDoesNotLeakContents(t *testing.T) {
	_, err := ReadPlan(writeCredentials(t, `{"claudeAiOauth": "SECRET-VALUE"`))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if strings.Contains(err.Error(), "SECRET-VALUE") {
		t.Errorf("error leaks file contents: %v", err)
	}
}

func TestPlanDetectorEnterprise(t *testing.T) {
	path := writeCredentials(t, `{"claudeAiOauth":{"subscriptionType":"Enterprise"}}`)
	if !NewPlanDetector(path).Enterprise() {
		t.Error("Enterprise() = false for an enterprise credentials file")
	}
	path = writeCredentials(t, `{"claudeAiOauth":{"subscriptionType":"max"}}`)
	if NewPlanDetector(path).Enterprise() {
		t.Error("Enterprise() = true for a max plan")
	}
	if NewPlanDetector(t.TempDir() + "/missing.json").Enterprise() {
		t.Error("Enterprise() = true with no credentials file")
	}
}

// Claude Code rewrites the file on login or plan change; the detector must
// pick that up without a restart.
func TestPlanDetectorFollowsFileChanges(t *testing.T) {
	path := writeCredentials(t, `{"claudeAiOauth":{"subscriptionType":"max"}}`)
	d := NewPlanDetector(path)
	if d.Enterprise() {
		t.Fatal("max read as enterprise")
	}
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"subscriptionType":"enterprise"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mtime in case the filesystem's granularity is coarse.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if !d.Enterprise() {
		t.Error("detector did not notice the rewritten file")
	}
}

// A torn or temporarily unreadable file must not flip the dashboard out of
// enterprise mode: keep the last known answer.
func TestPlanDetectorKeepsLastAnswerOnReadFailure(t *testing.T) {
	path := writeCredentials(t, `{"claudeAiOauth":{"subscriptionType":"enterprise"}}`)
	d := NewPlanDetector(path)
	if !d.Enterprise() {
		t.Fatal("setup: not enterprise")
	}
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth": `), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, later, later)
	if !d.Enterprise() {
		t.Error("a malformed rewrite dropped enterprise mode")
	}
}
