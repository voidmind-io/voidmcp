package transport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/transport"
)

// mockMCPServer returns an httptest.Server that behaves like a minimal MCP
// server: responds to initialize, notifications/initialized, tools/list, ping,
// and tools/call requests.
func mockMCPServer(t *testing.T, tools []map[string]any) *httptest.Server {
	t.Helper()
	sessionID := "test-session-42"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Mcp-Session-Id", sessionID)
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "mock", "version": "1.0"},
				},
			})
		case "notifications/initialized":
			// Notification — respond with 202.
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"tools": tools},
			})
		case "ping":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{},
			})
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "call result"}},
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error": map[string]any{
					"code":    -32601,
					"message": "method not found",
				},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewHTTP_Constructor(t *testing.T) {
	t.Parallel()

	tr := transport.NewHTTP("http://example.com", "none", "", "")
	if tr == nil {
		t.Fatal("NewHTTP returned nil")
	}
	// Close must not panic.
	tr.Close()
}

func TestHTTPTransport_ListTools(t *testing.T) {
	t.Parallel()

	serverTools := []map[string]any{
		{
			"name":        "weather",
			"description": "Get the weather",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		},
		{
			"name":        "time",
			"description": "Get the current time",
			"inputSchema": map[string]any{"type": "object"},
		},
	}

	upstream := mockMCPServer(t, serverTools)
	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	tools, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "weather" {
		t.Errorf("tools[0].Name = %q, want %q", tools[0].Name, "weather")
	}
	if tools[1].Name != "time" {
		t.Errorf("tools[1].Name = %q, want %q", tools[1].Name, "time")
	}
}

func TestHTTPTransport_ListTools_Empty(t *testing.T) {
	t.Parallel()

	upstream := mockMCPServer(t, []map[string]any{})
	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	tools, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools with empty list: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestHTTPTransport_SessionIDPropagation(t *testing.T) {
	t.Parallel()

	var receivedSessions []string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			receivedSessions = append(receivedSessions, sid)
		}

		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Mcp-Session-Id", "session-abc")
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2025-03-26"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{}},
			})
		}
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	if _, err := tr.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Any request that arrives with a session header must carry the correct ID.
	for _, sid := range receivedSessions {
		if sid != "session-abc" {
			t.Errorf("unexpected session ID sent to server: %q", sid)
		}
	}
}

func TestHTTPTransport_BearerAuth(t *testing.T) {
	t.Parallel()

	var receivedAuth string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")

		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2025-03-26"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{}},
			})
		}
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "bearer", "", "my-secret-token")
	defer tr.Close()

	if _, err := tr.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := "Bearer my-secret-token"
	if receivedAuth != want {
		t.Errorf("Authorization = %q, want %q", receivedAuth, want)
	}
}

func TestHTTPTransport_CustomHeaderAuth(t *testing.T) {
	t.Parallel()

	var receivedHeader string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("X-Api-Key")

		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2025-03-26"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{}},
			})
		}
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "header", "X-Api-Key", "key-value-123")
	defer tr.Close()

	if _, err := tr.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if receivedHeader != "key-value-123" {
		t.Errorf("X-Api-Key = %q, want %q", receivedHeader, "key-value-123")
	}
}

func TestHTTPTransport_Ping(t *testing.T) {
	t.Parallel()

	upstream := mockMCPServer(t, nil)
	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	if err := tr.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestHTTPTransport_CallTool(t *testing.T) {
	t.Parallel()

	upstream := mockMCPServer(t, []map[string]any{
		{"name": "weather", "inputSchema": map[string]any{"type": "object"}},
	})
	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	result, err := tr.CallTool(context.Background(), "weather", json.RawMessage(`{"city":"London"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result == nil {
		t.Fatal("CallTool returned nil result")
	}
}

func TestHTTPTransport_ServerError500(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	_, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention HTTP 500", err.Error())
	}
}

func TestHTTPTransport_SessionExpired404(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	_, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "session") {
		t.Errorf("error %q does not mention session expiry", err.Error())
	}
}

func TestHTTPTransport_ConnectionRefused(t *testing.T) {
	t.Parallel()

	// Port 1 is reserved and nothing should be listening there.
	tr := transport.NewHTTP("http://127.0.0.1:1", "none", "", "")
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := tr.ListTools(ctx)
	if err == nil {
		t.Fatal("expected error for connection refused, got nil")
	}
}

func TestHTTPTransport_SSEResponse(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case "initialize":
			payload, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2025-03-26"},
			})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", payload)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			payload, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{}},
			})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	tools, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestHTTPTransport_SSEDataWithNoSpace(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		switch req.Method {
		case "initialize":
			payload, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2025-03-26"},
			})
			w.Header().Set("Content-Type", "text/event-stream")
			// "data:" without space after colon — transport should still parse.
			fmt.Fprintf(w, "data:%s\n\n", payload)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			payload, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []any{}},
			})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data:%s\n\n", payload)
		}
	}))
	t.Cleanup(upstream.Close)

	tr := transport.NewHTTP(upstream.URL, "none", "", "")
	defer tr.Close()

	tools, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools over SSE (no space): %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestHTTPTransport_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	upstream := mockMCPServer(t, nil)
	tr := transport.NewHTTP(upstream.URL, "none", "", "")

	// Close twice must not panic.
	tr.Close()
	tr.Close()
}
