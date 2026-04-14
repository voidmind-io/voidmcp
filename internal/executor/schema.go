package executor

import "encoding/json"

// InferSchema walks a JSON value and produces a JSON-Schema-like map.
// Max depth 3 — deeper objects become {"type": "object"}.
func InferSchema(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	schema := inferValue(v, 0)
	out, _ := json.Marshal(schema)
	return out
}

func inferValue(v any, depth int) map[string]any {
	if depth > 3 {
		return map[string]any{"type": "object"}
	}
	switch val := v.(type) {
	case nil:
		return map[string]any{"type": "string"} // safe fallback
	case bool:
		return map[string]any{"type": "boolean"}
	case float64:
		return map[string]any{"type": "number"}
	case string:
		return map[string]any{"type": "string"}
	case []any:
		if len(val) == 0 {
			return map[string]any{"type": "array", "items": map[string]any{}}
		}
		return map[string]any{"type": "array", "items": inferValue(val[0], depth+1)}
	case map[string]any:
		if len(val) == 0 {
			return map[string]any{"type": "object"}
		}
		const maxProperties = 64
		if len(val) > maxProperties {
			return map[string]any{"type": "object"}
		}
		props := make(map[string]any, len(val))
		for k, child := range val {
			props[k] = inferValue(child, depth+1)
		}
		return map[string]any{"type": "object", "properties": props}
	default:
		return map[string]any{"type": "any"}
	}
}

// unwrapToolResult extracts the payload from an MCP ToolResult wrapper.
// MCP tools return {content: [{type: "text", text: "..."}]}.
//   - Single text block with valid JSON -> return parsed JSON
//   - Single text block plain text -> return as JSON string
//   - Multiple text blocks -> return JSON array of texts
//   - Not a ToolResult (missing content key, or has unknown top-level keys) -> return raw unchanged
//
// The check for unknown top-level keys guards against treating actual payload
// objects that happen to contain a "content" field as ToolResult wrappers.
// The known ToolResult fields are: content, isError, structuredContent, _meta.
func unwrapToolResult(raw json.RawMessage) json.RawMessage {
	// First pass: inspect top-level keys to confirm the MCP ToolResult shape.
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topLevel); err != nil {
		return raw
	}

	if _, hasContent := topLevel["content"]; !hasContent {
		return raw
	}

	// Only unwrap when every top-level key is one of the known ToolResult fields.
	// Any extra key means this is payload data, not a wrapper.
	knownKeys := 0
	for k := range topLevel {
		switch k {
		case "content", "isError", "structuredContent", "_meta":
			knownKeys++
		}
	}
	if knownKeys != len(topLevel) {
		return raw
	}

	var tr struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil || len(tr.Content) == 0 {
		return raw
	}

	if len(tr.Content) == 1 && tr.Content[0].Type == "text" {
		text := tr.Content[0].Text
		if json.Valid([]byte(text)) {
			return json.RawMessage(text)
		}
		quoted, _ := json.Marshal(text)
		return json.RawMessage(quoted)
	}

	texts := make([]string, 0, len(tr.Content))
	for _, c := range tr.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	out, _ := json.Marshal(texts)
	return json.RawMessage(out)
}
