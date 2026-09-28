package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain lets a test re-run this binary as clusage-cli itself, so the
// subcommands' os.Exit paths can be observed from outside.
func TestMain(m *testing.M) {
	if os.Getenv("CLUSAGE_CLI_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type cliResult struct {
	code           int
	stdout, stderr string
}

// runCLI runs clusage-cli with args against srv (nil for no host), with
// extra environment entries and stdin.
func runCLI(t *testing.T, srv *httptest.Server, env []string, stdin string, args ...string) cliResult {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "CLUSAGE_CLI_TEST_MAIN=1", "CLUSAGE_TOKEN=", "CLUSAGE_TIMEOUT_MS=")
	if srv != nil {
		u, _ := url.Parse(srv.URL)
		cmd.Env = append(cmd.Env, "CLUSAGE_HOST="+u.Hostname(), "CLUSAGE_PORT="+u.Port())
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return cliResult{code, stdout.String(), stderr.String()}
}

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// For a Claude Code Stop hook, exit 2 means "block stopping and make Claude
// continue", which re-fires the same failing hook. Hook-mode failures must
// exit with a non-blocking code.
func TestHookModeFailureDoesNotExit2(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
	}{
		{"unparseable payload", "not json"},
		{"path outside the projects layout", `{"transcript_path":"/tmp/random/abc.jsonl"}`},
		{"unreadable transcript", `{"transcript_path":"/nonexistent/projects/-p/missing.jsonl"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCLI(t, nil, nil, tc.stdin, "log", "--from-hook")
			if r.code != 1 {
				t.Errorf("exit code = %d, want 1 (non-blocking); stderr %q", r.code, r.stderr)
			}
			if !strings.Contains(r.stderr, "error:") {
				t.Errorf("expected the failure on stderr, got %q", r.stderr)
			}
		})
	}
}

// Undeliverable events are routine (host asleep, token rotated) and must not
// fail the hook at all.
func TestHookModeUndeliveredEventsExit0(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	r := runCLI(t, srv, nil, string(writeHookTranscript(t, 1)), "log", "--from-hook")
	if r.code != 0 {
		t.Errorf("exit code = %d, want 0; stderr %q", r.code, r.stderr)
	}
}

// The token is pasted from the tray menu into env files that may have been
// written on Windows; a trailing CR would make every request unsendable.
func TestTokenSurroundingWhitespaceIsTrimmed(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	r := runCLI(t, srv, []string{"CLUSAGE_TOKEN= s3cret\r"}, "", "ping")
	if r.code != 0 {
		t.Fatalf("ping exit code = %d, want 0; stderr %q", r.code, r.stderr)
	}
	if got != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer s3cret")
	}
}

// A timeout, a DNS failure and a refused connection need different fixes,
// so the message must carry the underlying error rather than a fixed
// "connection refused".
func TestTransportErrorIsReportedAsItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	r := runCLI(t, srv, []string{"CLUSAGE_TIMEOUT_MS=100"}, "", "ping")
	if r.code != 3 {
		t.Errorf("exit code = %d, want 3", r.code)
	}
	if strings.Contains(r.stderr, "connection refused") || !strings.Contains(r.stderr, "Timeout") {
		t.Errorf("expected stderr to report the timeout, got %q", r.stderr)
	}
}

func TestSlackFractionPrintsValue(t *testing.T) {
	srv := jsonServer(t, `{"release_recommended":true,"slack_combined_fraction":0.25}`)
	r := runCLI(t, srv, nil, "", "slack", "--format", "fraction")
	if r.code != 0 || r.stdout != "0.2500\n" {
		t.Errorf("got exit %d stdout %q, want 0 and %q", r.code, r.stdout, "0.2500\n")
	}
}

// Queue scripts gate work on this output; empty output with exit 0 reads as
// success.
func TestSlackMissingFieldFails(t *testing.T) {
	cases := []struct {
		name, body, format, field string
	}{
		{"null fraction", `{"release_recommended":false,"slack_combined_fraction":null}`, "fraction", "slack_combined_fraction"},
		{"missing release flag", `{}`, "release-bool", "release_recommended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := jsonServer(t, tc.body)
			r := runCLI(t, srv, nil, "", "slack", "--format", tc.format)
			if r.code != 5 {
				t.Errorf("exit code = %d, want 5", r.code)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
			if !strings.Contains(r.stderr, tc.field) {
				t.Errorf("expected stderr to name %s, got %q", tc.field, r.stderr)
			}
		})
	}
}

func TestSlackUnknownFormatIsUsageError(t *testing.T) {
	srv := jsonServer(t, `{"release_recommended":true,"slack_combined_fraction":0.25}`)
	r := runCLI(t, srv, nil, "", "slack", "--format", "release_bool")
	if r.code != 2 {
		t.Errorf("exit code = %d, want 2; stdout %q", r.code, r.stdout)
	}
}

// 0 or a negative value would disable http.Client's timeout entirely, and a
// value Sscanf only half-reads ("1.5" as 1 ms) would time every request out.
func TestParseTimeoutRejectsUnusableValues(t *testing.T) {
	old := timeoutMs
	t.Cleanup(func() { timeoutMs = old })

	cases := []struct {
		in   string
		want time.Duration
	}{
		{"500", 500 * time.Millisecond},
		{"", 2 * time.Second},
		{"0", 2 * time.Second},
		{"-5", 2 * time.Second},
		{"abc", 2 * time.Second},
		{"1.5", 2 * time.Second},
		{"9300000000000", maxTimeout},
		{"99999999999999999999", 2 * time.Second},
	}
	for _, tc := range cases {
		timeoutMs = tc.in
		var got time.Duration
		captureStderr(t, func() { got = parseTimeout() })
		if got != tc.want {
			t.Errorf("parseTimeout(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
