// Package protocol defines the MCP (Model Context Protocol) types for
// JSON-RPC 2.0 messages, tool schemas, and tool results.
package protocol

import "encoding/json"

// JSON-RPC 2.0 standard error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request is a JSON-RPC 2.0 request or notification.
// Notifications have no ID field (or ID is null).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification returns true when the request carries no ID, making it a
// JSON-RPC notification that does not expect a response.
func (r Request) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result or Error will
// be non-nil on a well-formed response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object embedded in a Response.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool describes an MCP tool with its name, description, and input schema.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema defines the JSON Schema for tool input parameters.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

// Property describes a single property in a tool's input schema.
// Items is populated for array-typed properties; Enum lists allowed values.
type Property struct {
	Type        string    `json:"type"`
	Description string    `json:"description,omitempty"`
	Items       *Property `json:"items,omitempty"`
	Enum        []string  `json:"enum,omitempty"`
}

// ToolResult is the result of a tool call. IsError distinguishes tool-level
// errors (problem running the tool) from JSON-RPC protocol errors.
type ToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is a single content block in a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// TextResult creates a successful ToolResult with a single text content block.
func TextResult(text string) *ToolResult {
	return &ToolResult{
		Content: []Content{{Type: "text", Text: text}},
	}
}

// ErrorResult creates a ToolResult that signals a tool-level error.
// Tool errors are NOT JSON-RPC protocol errors — they are surfaced in the
// result with IsError true so the caller can distinguish them from success.
func ErrorResult(msg string) *ToolResult {
	return &ToolResult{
		Content: []Content{{Type: "text", Text: msg}},
		IsError: true,
	}
}
