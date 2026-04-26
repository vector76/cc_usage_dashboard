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
func processHookInput(stdin io.Reader) {
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
		var msgMap map[string]interface{}
		if err := json.Unmarshal(line, &msgMap); err != nil {
			// Skip malformed lines
			continue
		}

		// Only process assistant messages with usage
		msgType, ok := msgMap["type"].(string)
		if !ok || msgType != "assistant" {
			continue
		}

		usage, ok := msgMap["usage"].(map[string]interface{})
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
		if sessionID, ok := msgMap["session_id"].(string); ok {
			eventPayload["session_id"] = sessionID
		}
		if messageID, ok := msgMap["message_id"].(string); ok {
			eventPayload["message_id"] = messageID
		}
		if model, ok := msgMap["model"].(string); ok {
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
		if payload.SessionID != "" {
			eventPayload["project_path"] = inferProjectPath(payload.TranscriptPath)
		}

		// Post the event
		if postEventPayload(eventPayload) {
			successCount++
		}
	}

	// Always exit 0 to not break the Claude Code session
	// The user's `|| true` will handle our exit code
	if successCount > 0 {
		os.Exit(0)
	}
	// Even if nothing was posted, exit 0 (hook must not fail the session)
	os.Exit(0)
}

// postEventPayload posts an event payload to the trayapp.
func postEventPayload(eventPayload map[string]interface{}) bool {
	body, err := json.Marshal(eventPayload)
	if err != nil {
		return false
	}

	timeout := parseTimeout()
	client := &http.Client{Timeout: timeout}

	resp, err := client.Post(
		fmt.Sprintf("http://%s:%s/log", host, port),
		"application/json",
		bytes.NewReader(body),
	)

	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// inferProjectPath tries to infer the project path from the transcript path.
func inferProjectPath(transcriptPath string) string {
	// Typically Claude Code stores transcripts in:
	// ~/.claude/projects/<project-id>/...
	// Extract the project-id if possible
	parts := filepath.SplitList(transcriptPath)
	for i, part := range parts {
		if part == "projects" && i+1 < len(parts) {
			return filepath.Join(parts[:i+2]...)
		}
	}
	return ""
}
