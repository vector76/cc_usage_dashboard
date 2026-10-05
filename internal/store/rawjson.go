package store

import "encoding/json"

// TrimUsageRawJSON drops the conversation from a usage event's raw_json: the
// reply text and tool calls in message.content, and the tool inputs Claude
// Code repeats in wireToolInputs. Kept whole, a transcript line averaged
// about 3.5 KB and made up three quarters of the database; trimmed, it is
// about 1.2 KB, keeping the usage block, the ids and the metadata that a
// replay such as migration v9's needs.
//
// A payload with nothing to drop — empty, not JSON, not an object — comes
// back byte-for-byte. Migration v10 applies the same rule to rows stored
// before it with SQLite's json_remove; the two must stay in step.
func TrimUsageRawJSON(raw string) string {
	var line map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &line) != nil || line == nil {
		return raw
	}

	trimmed := false
	if _, ok := line["wireToolInputs"]; ok {
		delete(line, "wireToolInputs")
		trimmed = true
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(line["message"], &message) == nil && message != nil {
		if _, ok := message["content"]; ok {
			delete(message, "content")
			out, err := json.Marshal(message)
			if err != nil {
				return raw
			}
			line["message"] = out
			trimmed = true
		}
	}
	if !trimmed {
		return raw
	}

	out, err := json.Marshal(line)
	if err != nil {
		return raw
	}
	return string(out)
}
