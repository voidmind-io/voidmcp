package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

func TestTextResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		wantLen int
	}{
		{name: "non-empty text", text: "hello world", wantLen: 1},
		{name: "empty text", text: "", wantLen: 1},
		{name: "multiline text", text: "line1\nline2", wantLen: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := protocol.TextResult(tc.text)
			if got == nil {
				t.Fatal("TextResult() returned nil")
			}
			if got.IsError {
				t.Errorf("TextResult().IsError = true, want false")
			}
			if len(got.Content) != tc.wantLen {
				t.Errorf("len(Content) = %d, want %d", len(got.Content), tc.wantLen)
			}
			if got.Content[0].Type != "text" {
				t.Errorf("Content[0].Type = %q, want %q", got.Content[0].Type, "text")
			}
			if got.Content[0].Text != tc.text {
				t.Errorf("Content[0].Text = %q, want %q", got.Content[0].Text, tc.text)
			}
		})
	}
}

func TestErrorResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  string
	}{
		{name: "error message", msg: "something went wrong"},
		{name: "empty message", msg: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := protocol.ErrorResult(tc.msg)
			if got == nil {
				t.Fatal("ErrorResult() returned nil")
			}
			if !got.IsError {
				t.Errorf("ErrorResult().IsError = false, want true")
			}
			if len(got.Content) != 1 {
				t.Fatalf("len(Content) = %d, want 1", len(got.Content))
			}
			if got.Content[0].Type != "text" {
				t.Errorf("Content[0].Type = %q, want %q", got.Content[0].Type, "text")
			}
			if got.Content[0].Text != tc.msg {
				t.Errorf("Content[0].Text = %q, want %q", got.Content[0].Text, tc.msg)
			}
		})
	}
}

func TestRequestIsNotification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		id             json.RawMessage
		wantNotif      bool
	}{
		{
			name:      "missing ID is notification",
			id:        nil,
			wantNotif: true,
		},
		{
			name:      "null ID is notification",
			id:        json.RawMessage("null"),
			wantNotif: true,
		},
		{
			name:      "numeric ID is not notification",
			id:        json.RawMessage("1"),
			wantNotif: false,
		},
		{
			name:      "string ID is not notification",
			id:        json.RawMessage(`"req-123"`),
			wantNotif: false,
		},
		{
			name:      "zero numeric ID is not notification",
			id:        json.RawMessage("0"),
			wantNotif: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := protocol.Request{
				JSONRPC: "2.0",
				ID:      tc.id,
				Method:  "test/method",
			}
			got := req.IsNotification()
			if got != tc.wantNotif {
				t.Errorf("IsNotification() = %v, want %v", got, tc.wantNotif)
			}
		})
	}
}

func TestRequestJSONRoundTrip(t *testing.T) {
	t.Parallel()

	original := protocol.Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage("42"),
		Method:  "tools/list",
		Params:  json.RawMessage(`{"cursor":null}`),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got protocol.Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.JSONRPC != original.JSONRPC {
		t.Errorf("JSONRPC = %q, want %q", got.JSONRPC, original.JSONRPC)
	}
	if string(got.ID) != string(original.ID) {
		t.Errorf("ID = %s, want %s", got.ID, original.ID)
	}
	if got.Method != original.Method {
		t.Errorf("Method = %q, want %q", got.Method, original.Method)
	}
}

func TestResponseJSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp protocol.Response
	}{
		{
			name: "success response",
			resp: protocol.Response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("1"),
				Result:  map[string]any{"ok": true},
			},
		},
		{
			name: "error response",
			resp: protocol.Response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("2"),
				Error: &protocol.Error{
					Code:    protocol.CodeMethodNotFound,
					Message: "method not found: foo",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tc.resp)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var got protocol.Response
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			if got.JSONRPC != tc.resp.JSONRPC {
				t.Errorf("JSONRPC = %q, want %q", got.JSONRPC, tc.resp.JSONRPC)
			}
			if string(got.ID) != string(tc.resp.ID) {
				t.Errorf("ID = %s, want %s", got.ID, tc.resp.ID)
			}
		})
	}
}

func TestErrorCodes(t *testing.T) {
	t.Parallel()

	if protocol.CodeParseError != -32700 {
		t.Errorf("CodeParseError = %d, want -32700", protocol.CodeParseError)
	}
	if protocol.CodeInvalidRequest != -32600 {
		t.Errorf("CodeInvalidRequest = %d, want -32600", protocol.CodeInvalidRequest)
	}
	if protocol.CodeMethodNotFound != -32601 {
		t.Errorf("CodeMethodNotFound = %d, want -32601", protocol.CodeMethodNotFound)
	}
	if protocol.CodeInvalidParams != -32602 {
		t.Errorf("CodeInvalidParams = %d, want -32602", protocol.CodeInvalidParams)
	}
	if protocol.CodeInternalError != -32603 {
		t.Errorf("CodeInternalError = %d, want -32603", protocol.CodeInternalError)
	}
}
