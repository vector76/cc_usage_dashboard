package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// HookPayload represents the Claude Code Stop hook payload.
type HookPayload struct {
	TranscriptPath string `json:"transcript_path"`
	SessionID      string `json:"session_id,omitempty"`
	// Other fields may be present but we only need transcript_path
}

// processHookInput reads the hook payload from stdin and posts events from the transcript.
// hostURL is the base URL of the trayapp (e.g., "http://127.0.0.1:27812").
func processHookInput(stdin io.Reader, hostURL string) {
	// Decode the hook payload
	var payload HookPayload
	if err := json.NewDecoder(stdin).Decode(&payload); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to parse hook payload: %v\n", err)
		os.Exit(2)
	}

	if err := validateTranscriptPath(payload.TranscriptPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	// Read the transcript file
	transcript, err := os.ReadFile(payload.TranscriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to read transcript: %v\n", err)
		os.Exit(2)
	}

	// Count how many events were posted successfully
	successCount := 0

	// Split by newlines and post each event
	for _, line := range bytes.Split(transcript, []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		// Parse the line as JSON to extract usage information
		var record map[string]interface{}
		if err := json.Unmarshal(line, &record); err != nil {
			// Skip malformed lines
			continue
		}

		// Only process assistant messages.
		msgType, ok := record["type"].(string)
		if !ok || msgType != "assistant" {
			continue
		}

		// Real Claude Code transcripts nest model/id/usage under a top-level
		// "message" object, with "sessionId" (camelCase) as a sibling. The
		// initial parser was written against a mock schema that didn't match
		// any real transcript, so every line was silently skipped.
		msg, ok := record["message"].(map[string]interface{})
		if !ok {
			continue
		}

		usage, ok := msg["usage"].(map[string]interface{})
		if !ok {
			continue
		}

		// Extract token counts
		input, ok := usage["input_tokens"].(float64)
		if !ok {
			continue
		}
		output, ok := usage["output_tokens"].(float64)
		if !ok {
			continue
		}

		// Build event payload
		eventPayload := map[string]interface{}{
			"input_tokens":  int(input),
			"output_tokens": int(output),
			"source":        "hook",
		}

		// Forward the assistant turn's transcript timestamp as occurred_at.
		// Without this, every event in a backfill lands at time.Now(),
		// collapsing an entire session's history to a few seconds.
		if ts, ok := record["timestamp"].(string); ok && ts != "" {
			eventPayload["occurred_at"] = ts
		}

		// Add optional fields if present
		if sessionID, ok := record["sessionId"].(string); ok {
			eventPayload["session_id"] = sessionID
		}
		if messageID, ok := msg["id"].(string); ok {
			eventPayload["message_id"] = messageID
		}
		if model, ok := msg["model"].(string); ok {
			eventPayload["model"] = model
		}

		// Extract cache tokens
		if cacheCreation, ok := usage["cache_creation_input_tokens"].(float64); ok {
			eventPayload["cache_creation_tokens"] = int(cacheCreation)
		}
		if cacheRead, ok := usage["cache_read_input_tokens"].(float64); ok {
			eventPayload["cache_read_tokens"] = int(cacheRead)
		}

		// Add project path if we can infer it
		if projectPath := inferProjectPath(payload.TranscriptPath); projectPath != "" {
			eventPayload["project_path"] = projectPath
		}

		// Post the event
		if postEventPayloadTo(hostURL, eventPayload) {
			successCount++
		}
	}

	// Hooks must not fail the Claude Code session; the caller exits 0
	// regardless of whether any events were posted.
	_ = successCount
}

// postEventPayloadTo posts an event payload to the trayapp at hostURL.
func postEventPayloadTo(hostURL string, eventPayload map[string]interface{}) bool {
	body, err := json.Marshal(eventPayload)
	if err != nil {
		return false
	}

	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	resp, err := client.Post(
		hostURL+"/log",
		"application/json",
		bytes.NewReader(body),
	)

	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// validateTranscriptPath rejects transcript_path values that don't match
// the Claude Code projects layout. The hook payload is structured input
// from Claude Code, but a workspace-level configuration could in principle
// influence it; without this guard the CLI happily ReadFile's whatever
// path the payload names — the file's contents would then be POSTed up to
// the trayapp as raw_json. Two cheap structural checks close the gap:
//
//  1. The file extension must be .jsonl (Claude Code's transcript format).
//  2. The grandparent directory's basename must be exactly "projects",
//     matching ~/.claude/projects/<encoded>/<session>.jsonl.
//
// The checks intentionally don't pin to an absolute prefix because the
// $HOME / projects-dir location varies across containers and CI; the
// shape of the path is what matters.
func validateTranscriptPath(p string) error {
	if p == "" {
		return fmt.Errorf("transcript_path missing from hook payload")
	}
	cleaned := filepath.Clean(p)
	if filepath.Ext(cleaned) != ".jsonl" {
		return fmt.Errorf("transcript_path must be a .jsonl file: %q", p)
	}
	dir := filepath.Dir(cleaned)
	parent := filepath.Base(filepath.Dir(dir))
	if parent != "projects" {
		return fmt.Errorf("transcript_path must live under a 'projects' directory: %q", p)
	}
	return nil
}

// inferProjectPath extracts the encoded project segment from a transcript
// path like ~/.claude/projects/<encoded>/<session>.jsonl. Returns the
// <encoded> directory name (e.g., "-home-user-myproj"), or "" if the path
// does not match the expected layout.
func inferProjectPath(transcriptPath string) string {
	// Walk up: parent of the transcript file is the project segment;
	// its parent's basename should be "projects".
	dir := filepath.Dir(transcriptPath)
	if dir == "" || dir == "." || dir == string(filepath.Separator) {
		return ""
	}
	segment := filepath.Base(dir)
	parent := filepath.Base(filepath.Dir(dir))
	if parent != "projects" {
		return ""
	}
	return segment
}
