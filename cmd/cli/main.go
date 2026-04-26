package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
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
		processHookInput(os.Stdin)
		return
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

	// Placeholder: will implement in Phase 5
	fmt.Printf("Placeholder: slack --format %s subcommand\n", *format)
	os.Exit(2)
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
