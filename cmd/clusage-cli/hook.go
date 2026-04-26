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

	if payload.TranscriptPath == "" {
		fmt.Fprintf(os.Stderr, "error: transcript_path not found in hook payload\n")
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
