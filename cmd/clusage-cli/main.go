package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const Version = "0.0.1"

var (
	host      = getenv("CLUSAGE_HOST", "host.docker.internal")
	port      = getenv("CLUSAGE_PORT", "27812")
	timeoutMs = getenv("CLUSAGE_TIMEOUT_MS", "2000")
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "ping":
		cmdPing()
	case "log":
		cmdLog()
	case "slack":
		cmdSlack()
	case "consumption":
		cmdConsumption()
	case "release":
		cmdRelease()
	case "reimport":
		cmdReimport()
	case "--version":
		fmt.Printf("clusage-cli v%s\n", Version)
	case "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `clusage-cli v%s

Usage:
  clusage-cli ping
  clusage-cli log [--from-hook | --input-tokens N --output-tokens N ...]
  clusage-cli slack [--format json|release-bool|fraction]
  clusage-cli consumption [--period 24h] [--format json|summary]
  clusage-cli release --released-at TS --job-tag TAG --estimated-cost N --slack-at-release N [--window-kind session|weekly]
  clusage-cli reimport [--wait] [--wait-timeout 5m]

`, Version)
}

func cmdPing() {
	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	resp, err := client.Get(fmt.Sprintf("http://%s:%s/healthz", host, port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection refused\n")
		os.Exit(3) // Exit code 3: host unreachable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health check failed: %d\n", resp.StatusCode)
		os.Exit(5) // Exit code 5: host returned 5xx (or non-OK)
	}

	fmt.Println("OK")
	os.Exit(0)
}

func cmdLog() {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	fromHook := fs.Bool("from-hook", false, "read hook payload from stdin")
	inputTokens := fs.Int("input-tokens", 0, "number of input tokens")
	outputTokens := fs.Int("output-tokens", 0, "number of output tokens")
	cacheCreationTokens := fs.Int("cache-creation-tokens", 0, "cache creation tokens")
	cacheReadTokens := fs.Int("cache-read-tokens", 0, "cache read tokens")
	costUSD := fs.Float64("cost-usd", 0, "cost in USD")
	sessionID := fs.String("session-id", "", "session ID")
	messageID := fs.String("message-id", "", "message ID")
	model := fs.String("model", "", "model name")
	projectPath := fs.String("project-path", "", "project path")
	source := fs.String("source", "cli", "event source")
	fs.Parse(os.Args[2:])

	if *fromHook {
		// Mode B: process hook payload from stdin
		processHookInput(os.Stdin, hostURL())
		os.Exit(0)
	}

	// Mode A: explicit flags
	if *inputTokens == 0 && *outputTokens == 0 {
		fmt.Fprintf(os.Stderr, "error: --input-tokens and --output-tokens are required\n")
		os.Exit(2)
	}

	payload := map[string]interface{}{
		"input_tokens":          *inputTokens,
		"output_tokens":         *outputTokens,
		"cache_creation_tokens": *cacheCreationTokens,
		"cache_read_tokens":     *cacheReadTokens,
		"source":                *source,
	}

	if *sessionID != "" {
		payload["session_id"] = *sessionID
	}
	if *messageID != "" {
		payload["message_id"] = *messageID
	}
	if *model != "" {
		payload["model"] = *model
	}
	if *projectPath != "" {
		payload["project_path"] = *projectPath
	}
	if *costUSD > 0 {
		payload["cost_usd"] = *costUSD
	}

	postEvent(payload)
}

func cmdSlack() {
	fs := flag.NewFlagSet("slack", flag.ExitOnError)
	format := fs.String("format", "json", "output format: json|release-bool|fraction")
	fs.Parse(os.Args[2:])

	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	resp, err := client.Get(fmt.Sprintf("http://%s:%s/slack", host, port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connection refused\n")
		os.Exit(3)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %d\n", resp.StatusCode)
		os.Exit(5)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to parse response\n")
		os.Exit(5)
	}

	switch *format {
	case "json":
		json.NewEncoder(os.Stdout).Encode(result)
	case "release-bool":
		if release, ok := result["release_recommended"].(bool); ok {
			fmt.Println(release)
		}
	case "fraction":
		if fraction, ok := result["slack_combined_fraction"].(float64); ok {
			fmt.Printf("%.4f\n", fraction)
		}
	}

	os.Exit(0)
}

func postEvent(payload map[string]interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal payload\n")
		os.Exit(2)
	}

	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	resp, err := client.Post(
		fmt.Sprintf("http://%s:%s/log", host, port),
		"application/json",
		bytes.NewReader(body),
	)

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connection refused\n")
		os.Exit(3) // Exit code 3: host unreachable
	}
	defer resp.Body.Close()

	// Read response body
	_, err = io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to read response\n")
		os.Exit(5)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		os.Exit(0) // Success
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		fmt.Fprintf(os.Stderr, "error: %d\n", resp.StatusCode)
		os.Exit(4) // Exit code 4: 4xx error
	default:
		fmt.Fprintf(os.Stderr, "error: %d\n", resp.StatusCode)
		os.Exit(5) // Exit code 5: 5xx error
	}
}

// hostURL returns the base URL of the trayapp.
func hostURL() string {
	return fmt.Sprintf("http://%s:%s", host, port)
}

func cmdConsumption() {
	fs := flag.NewFlagSet("consumption", flag.ExitOnError)
	period := fs.String("period", "24h", "period (e.g. 24h, 7d, 30d)")
	format := fs.String("format", "json", "output format: json|summary")
	fs.Parse(os.Args[2:])

	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	// url.Values.Encode handles characters like &, #, and whitespace in
	// the period flag — fmt.Sprintf would silently produce a malformed
	// URL the server then can't parse.
	q := url.Values{}
	q.Set("period", *period)
	resp, err := client.Get(fmt.Sprintf("%s/consumption?%s", hostURL(), q.Encode()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connection refused\n")
		os.Exit(3)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: %d\n", resp.StatusCode)
		os.Exit(5)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to parse response\n")
		os.Exit(5)
	}

	switch *format {
	case "json":
		json.NewEncoder(os.Stdout).Encode(result)
	case "summary":
		fmt.Printf("period: %v\n", result["period"])
		fmt.Printf("consumed_usd_equivalent: %v\n", result["consumed_usd_equivalent"])
		fmt.Printf("consumed_session_pct: %v\n", result["consumed_session_pct"])
		fmt.Printf("consumed_weekly_pct: %v\n", result["consumed_weekly_pct"])
		fmt.Printf("events_total: %v\n", result["events_total"])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown format %q\n", *format)
		os.Exit(2)
	}

	os.Exit(0)
}

func cmdRelease() {
	fs := flag.NewFlagSet("release", flag.ExitOnError)
	releasedAt := fs.String("released-at", "", "RFC3339 timestamp of release decision (default: now)")
	jobTag := fs.String("job-tag", "", "free-form job identifier (required)")
	estimatedCost := fs.Float64("estimated-cost", 0, "estimated job cost in USD")
	slackAtRelease := fs.Float64("slack-at-release", 0, "slack_fraction (range [-1, +1]) seen at GET /slack at the moment of release")
	windowKind := fs.String("window-kind", "session", "window_kind: session|weekly")
	fs.Parse(os.Args[2:])

	if *jobTag == "" {
		fmt.Fprintf(os.Stderr, "error: --job-tag is required\n")
		os.Exit(2)
	}

	releasedAtTS := *releasedAt
	if releasedAtTS == "" {
		releasedAtTS = time.Now().UTC().Format(time.RFC3339)
	}

	payload := map[string]interface{}{
		"released_at":      releasedAtTS,
		"job_tag":          *jobTag,
		"estimated_cost":   *estimatedCost,
		"slack_at_release": *slackAtRelease,
		"window_kind":      *windowKind,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to marshal payload\n")
		os.Exit(2)
	}

	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	resp, err := client.Post(
		fmt.Sprintf("%s/slack/release", hostURL()),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connection refused\n")
		os.Exit(3)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		fmt.Println(string(respBody))
		os.Exit(0)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		fmt.Fprintf(os.Stderr, "error: %d %s\n", resp.StatusCode, string(respBody))
		os.Exit(4)
	default:
		fmt.Fprintf(os.Stderr, "error: %d\n", resp.StatusCode)
		os.Exit(5)
	}
}

// cmdReimport triggers a full recovery re-walk of every transcript file the
// trayapp's tailer(s) track, bypassing persisted byte offsets entirely.
//
// This is a rare-recovery tool, not routine maintenance: it exists for
// cases like suspected data loss, a parser bug that silently under-counted
// past events, or a restored/corrupted database, where it's unknown which
// specific sessions are affected. It deliberately re-reads every tracked
// transcript byte-for-byte and leans entirely on the server's
// UNIQUE(session_id, message_id) dedup to skip anything already correctly
// recorded — slow and wasteful by design, in exchange for not requiring
// anyone to name the affected files in advance.
func cmdReimport() {
	fs := flag.NewFlagSet("reimport", flag.ExitOnError)
	wait := fs.Bool("wait", false, "block until the tailer reports caught up (or --wait-timeout elapses)")
	waitTimeout := fs.Duration("wait-timeout", 10*time.Minute, "max time to wait with --wait")
	fs.Parse(os.Args[2:])

	client := &http.Client{Timeout: parseTimeout()}
	resp, err := client.Post(fmt.Sprintf("%s/admin/reimport", hostURL()), "application/json", bytes.NewReader(nil))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connection refused\n")
		os.Exit(3)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted:
		fmt.Println(string(body))
	case http.StatusConflict:
		fmt.Fprintf(os.Stderr, "error: %s\n", string(body))
		os.Exit(4)
	case http.StatusServiceUnavailable:
		fmt.Fprintf(os.Stderr, "error: %s\n", string(body))
		os.Exit(5)
	default:
		fmt.Fprintf(os.Stderr, "error: %d %s\n", resp.StatusCode, string(body))
		os.Exit(5)
	}

	if !*wait {
		os.Exit(0)
	}

	fmt.Println("waiting for tailer to catch up...")
	deadline := time.Now().Add(*waitTimeout)
	pollClient := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		hresp, err := pollClient.Get(fmt.Sprintf("%s/healthz", hostURL()))
		if err != nil {
			continue // transient; keep polling until the deadline
		}
		var health struct {
			TailerCaughtUp bool `json:"tailer_caught_up"`
		}
		decodeErr := json.NewDecoder(hresp.Body).Decode(&health)
		hresp.Body.Close()
		if decodeErr != nil {
			continue
		}
		if health.TailerCaughtUp {
			fmt.Println("caught up")
			os.Exit(0)
		}
	}
	fmt.Fprintf(os.Stderr, "error: still not caught up after %s\n", *waitTimeout)
	os.Exit(5)
}

func parseTimeout() time.Duration {
	var ms int64 = 2000
	if timeoutMs != "" {
		fmt.Sscanf(timeoutMs, "%d", &ms)
	}
	return time.Duration(ms) * time.Millisecond
}

func getenv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
