package ingest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ParsedEvent is a usage event extracted from a transcript.
type ParsedEvent struct {
	SessionID              string
	MessageID              string
	OccurredAt             time.Time
	Model                  string
	InputTokens            int
	OutputTokens           int
	CacheCreationTokens    int
	CacheReadTokens        int
	ReportedCost           *float64
	ProjectPath            string
	RawJSON                string
	LineNumber             int64 // For error reporting
}

// ParseError represents a parsing error.
type ParseError struct {
	LineNumber int64
	Line       string
	Reason     string
}

// Parser parses Claude Code JSONL transcripts.
type Parser struct {
	reader   io.Reader
	scanner  *bufio.Scanner
	lineNum  int64
	errors   []ParseError
}

// NewParser creates a new parser for reading from a reader.
func NewParser(r io.Reader) *Parser {
	return &Parser{
		reader:  r,
		scanner: bufio.NewScanner(r),
		errors:  make([]ParseError, 0),
	}
}

// ParseNext reads the next line and returns a parsed event if it contains usage data.
// Returns nil, nil if the line doesn't contain usage or EOF is reached.
// Returns nil, error if parsing fails.
func (p *Parser) ParseNext() (*ParsedEvent, error) {
	for p.scanner.Scan() {
		p.lineNum++
		line := p.scanner.Bytes()

		// Skip empty lines
		if len(line) == 0 {
			continue
		}

		event, err := p.parseLine(line)
		if err != nil {
			p.errors = append(p.errors, ParseError{
				LineNumber: p.lineNum,
				Line:       string(line),
				Reason:     err.Error(),
			})
			continue
		}

		if event != nil {
			return event, nil
		}
	}

	if err := p.scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}

	return nil, nil // EOF
}

// Errors returns all parsing errors encountered.
func (p *Parser) Errors() []ParseError {
	return p.errors
}

// parseLine tries to extract a usage event from a JSON line.
// Returns nil, nil if the line is valid JSON but contains no usage.
func (p *Parser) parseLine(line []byte) (*ParsedEvent, error) {
	var msg map[string]interface{}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// Check if this is an assistant message with usage
	msgType, ok := msg["type"].(string)
	if !ok || msgType != "assistant" {
		return nil, nil
	}

	usage, ok := msg["usage"].(map[string]interface{})
	if !ok {
		return nil, nil // No usage block
	}

	// Extract required fields
	inputTokens, outputTokens, err := extractTokens(usage)
	if err != nil {
		return nil, err
	}

	event := &ParsedEvent{
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		RawJSON:       string(line),
		LineNumber:    p.lineNum,
	}

	// Extract optional fields
	if sessionID, ok := msg["session_id"].(string); ok {
		event.SessionID = sessionID
	}

	if messageID, ok := msg["message_id"].(string); ok {
		event.MessageID = messageID
	}

	if model, ok := msg["model"].(string); ok {
		event.Model = model
	}

	if costStr, ok := msg["cost_usd"].(float64); ok {
		event.ReportedCost = &costStr
	}

	if cacheCreation, ok := usage["cache_creation_input_tokens"].(float64); ok {
		event.CacheCreationTokens = int(cacheCreation)
	}

	if cacheRead, ok := usage["cache_read_input_tokens"].(float64); ok {
		event.CacheReadTokens = int(cacheRead)
	}

	// Try to extract timestamp
	if timestamp, ok := msg["timestamp"].(string); ok {
		t, err := time.Parse(time.RFC3339, timestamp)
		if err == nil {
			event.OccurredAt = t
		}
	}

	// Use current time if no timestamp
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now()
	}

	return event, nil
}

// extractTokens extracts the required token counts from a usage block.
func extractTokens(usage map[string]interface{}) (int, int, error) {
	input, ok := usage["input_tokens"].(float64)
	if !ok {
		return 0, 0, fmt.Errorf("missing or invalid input_tokens")
	}

	output, ok := usage["output_tokens"].(float64)
	if !ok {
		return 0, 0, fmt.Errorf("missing or invalid output_tokens")
	}

	return int(input), int(output), nil
}
