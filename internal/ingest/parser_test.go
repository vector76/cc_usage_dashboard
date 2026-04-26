package ingest

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParserValidMessage(t *testing.T) {
	jsonl := `{"type":"assistant","session_id":"sess-123","message_id":"msg-456","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":1000,"output_tokens":500},"timestamp":"2026-04-26T10:00:00Z"}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}

	if event.SessionID != "sess-123" {
		t.Errorf("expected session_id 'sess-123', got %s", event.SessionID)
	}
	if event.MessageID != "msg-456" {
		t.Errorf("expected message_id 'msg-456', got %s", event.MessageID)
	}
	if event.InputTokens != 1000 {
		t.Errorf("expected 1000 input tokens, got %d", event.InputTokens)
	}
	if event.OutputTokens != 500 {
		t.Errorf("expected 500 output tokens, got %d", event.OutputTokens)
	}
	if event.Model != "claude-3-5-sonnet-20241022" {
		t.Errorf("expected model 'claude-3-5-sonnet-20241022', got %s", event.Model)
	}
}

func TestParserNonAssistantMessage(t *testing.T) {
	jsonl := `{"type":"user","session_id":"sess-123","content":"hello"}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Error("expected nil for non-assistant message")
	}
}

func TestParserMissingUsage(t *testing.T) {
	jsonl := `{"type":"assistant","session_id":"sess-123"}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Error("expected nil for message without usage")
	}
}

func TestParserMissingRequiredTokens(t *testing.T) {
	jsonl := `{"type":"assistant","session_id":"sess-123","usage":{"input_tokens":1000}}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Error("expected nil for message missing output_tokens")
	}

	// Check that an error was recorded
	errors := parser.Errors()
	if len(errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(errors))
	}
}

func TestParserMalformedJSON(t *testing.T) {
	jsonl := `{"bad": json}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Error("expected nil for malformed JSON")
	}

	// Check that an error was recorded
	errors := parser.Errors()
	if len(errors) != 1 {
		t.Errorf("expected 1 error, got %d", len(errors))
	}
}

func TestParserMultipleMessages(t *testing.T) {
	jsonl := `{"type":"user","content":"hello"}
{"type":"assistant","session_id":"sess-1","message_id":"msg-1","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":100,"output_tokens":50},"timestamp":"2026-04-26T10:00:00Z"}
{"type":"user","content":"more context"}
{"type":"assistant","session_id":"sess-1","message_id":"msg-2","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":200,"output_tokens":150},"timestamp":"2026-04-26T10:01:00Z"}`

	parser := NewParser(strings.NewReader(jsonl))
	events := make([]*ParsedEvent, 0)

	for {
		event, err := parser.ParseNext()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event == nil {
			break
		}
		events = append(events, event)
	}

	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}

	if events[0].MessageID != "msg-1" || events[0].InputTokens != 100 {
		t.Error("first event has wrong data")
	}
	if events[1].MessageID != "msg-2" || events[1].InputTokens != 200 {
		t.Error("second event has wrong data")
	}
}

func TestParserCacheTokens(t *testing.T) {
	jsonl := `{"type":"assistant","session_id":"sess-123","usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":100,"cache_read_input_tokens":50}}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event")
	}

	if event.CacheCreationTokens != 100 {
		t.Errorf("expected 100 cache creation tokens, got %d", event.CacheCreationTokens)
	}
	if event.CacheReadTokens != 50 {
		t.Errorf("expected 50 cache read tokens, got %d", event.CacheReadTokens)
	}
}

func TestParserReportedCost(t *testing.T) {
	jsonl := `{"type":"assistant","usage":{"input_tokens":1000,"output_tokens":500},"cost_usd":0.05}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event")
	}

	if event.ReportedCost == nil || *event.ReportedCost != 0.05 {
		t.Errorf("expected reported cost 0.05, got %v", event.ReportedCost)
	}
}

func TestParserEmptyLines(t *testing.T) {
	jsonl := `
{"type":"assistant","session_id":"sess-1","usage":{"input_tokens":100,"output_tokens":50}}

{"type":"user","content":"hello"}
{"type":"assistant","session_id":"sess-1","usage":{"input_tokens":200,"output_tokens":100}}`

	parser := NewParser(strings.NewReader(jsonl))
	events := make([]*ParsedEvent, 0)

	for {
		event, err := parser.ParseNext()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event == nil {
			break
		}
		events = append(events, event)
	}

	if len(events) != 2 {
		t.Errorf("expected 2 events (empty lines skipped), got %d", len(events))
	}
}

func TestParserTimestampParsing(t *testing.T) {
	jsonl := `{"type":"assistant","usage":{"input_tokens":100,"output_tokens":50},"timestamp":"2026-04-26T15:30:45Z"}`

	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event")
	}

	// Check that timestamp was parsed (approximately)
	if event.OccurredAt.Year() != 2026 || event.OccurredAt.Month() != 4 || event.OccurredAt.Day() != 26 {
		t.Errorf("timestamp not parsed correctly: %v", event.OccurredAt)
	}
}

func TestParserDefaultTimestamp(t *testing.T) {
	jsonl := `{"type":"assistant","usage":{"input_tokens":100,"output_tokens":50}}`

	before := time.Now()
	parser := NewParser(strings.NewReader(jsonl))
	event, err := parser.ParseNext()
	after := time.Now()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event")
	}

	// Check that timestamp is close to now
	if event.OccurredAt.Before(before) || event.OccurredAt.After(after.Add(1*time.Second)) {
		t.Errorf("default timestamp not set correctly: %v", event.OccurredAt)
	}
}

func TestParserRawJSON(t *testing.T) {
	expectedJSON := `{"type":"assistant","usage":{"input_tokens":100,"output_tokens":50}}`

	parser := NewParser(strings.NewReader(expectedJSON))
	event, err := parser.ParseNext()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event")
	}

	if event.RawJSON != expectedJSON {
		t.Errorf("RawJSON not preserved correctly")
	}
}

func TestParserErrors(t *testing.T) {
	jsonl := `{"bad": json}
{"type":"assistant","usage":{"input_tokens":100}}
{"valid": "json but no usage"}
{"type":"assistant","usage":{"input_tokens":200,"output_tokens":50}}`

	parser := NewParser(strings.NewReader(jsonl))
	events := make([]*ParsedEvent, 0)

	for {
		event, err := parser.ParseNext()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event == nil {
			break
		}
		events = append(events, event)
	}

	// Should have 1 valid event (the last one)
	if len(events) != 1 {
		t.Errorf("expected 1 valid event, got %d", len(events))
	}

	// Should have 2 errors
	errors := parser.Errors()
	if len(errors) != 2 {
		t.Errorf("expected 2 errors, got %d", len(errors))
	}

	// Check error details
	if !strings.Contains(errors[0].Reason, "invalid JSON") {
		t.Errorf("first error should be about invalid JSON")
	}
	if !strings.Contains(errors[1].Reason, "output_tokens") {
		t.Errorf("second error should be about missing output_tokens")
	}
}

func BenchmarkParser(b *testing.B) {
	jsonl := `{"type":"assistant","session_id":"sess-123","message_id":"msg-456","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":1000,"output_tokens":500}}`

	for i := 0; i < b.N; i++ {
		parser := NewParser(strings.NewReader(jsonl))
		parser.ParseNext()
	}
}

func BenchmarkParserLarge(b *testing.B) {
	// Create a large JSONL with many lines
	var buf bytes.Buffer
	for i := 0; i < 100; i++ {
		buf.WriteString(`{"type":"assistant","session_id":"sess-123","message_id":"msg-456","usage":{"input_tokens":1000,"output_tokens":500}}`)
		buf.WriteString("\n")
	}

	jsonl := buf.String()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parser := NewParser(strings.NewReader(jsonl))
		for {
			event, err := parser.ParseNext()
			if err != nil || event == nil {
				break
			}
		}
	}
}
