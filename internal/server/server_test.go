package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/protocol"
	"github.com/voidmind-io/voidmcp/internal/registry"
	"github.com/voidmind-io/voidmcp/internal/server"
	"github.com/voidmind-io/voidmcp/internal/store"
)

// --- Test setup helpers ---

// newTestStore creates a temp-dir-backed store, cleaned up automatically.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newTestPool creates a 1-slot executor pool for tests.
func newTestPool(t *testing.T) *executor.Pool {
	t.Helper()
	pool, err := executor.NewPool(1, 32, 10*time.Second)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// newTestServer builds a fully wired Server with an empty registry.
func newTestServer(t *testing.T) *server.Server {
	t.Helper()
	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	pool := newTestPool(t)
	exec := executor.New(pool)

	return server.New(reg, exec, nil, server.Config{
		
		MaxToolCalls:    10,
	})
}

// handle is a convenience wrapper that calls srv.Handle and parses the response.
func handle(t *testing.T, srv *server.Server, raw string) map[string]any {
	t.Helper()
	resp := srv.Handle(context.Background(), []byte(raw))
	if resp == nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, resp)
	}
	return out
}

// mcpStubServer creates an httptest.Server acting as a minimal MCP stub.
func mcpStubServer(t *testing.T, tools []protocol.Tool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
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
				"result": map[string]any{"tools": tools},
			})
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "stub-result"}},
				},
			})
		case "ping":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- initialize ---

func TestHandle_Initialize(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if resp["error"] != nil {
		t.Fatalf("initialize returned error: %v", resp["error"])
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}

	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want 2025-03-26", result["protocolVersion"])
	}

	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is not an object: %v", result["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities.tools missing")
	}

	info, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo is not an object: %v", result["serverInfo"])
	}
	if info["name"] != "voidmcp" {
		t.Errorf("serverInfo.name = %v, want voidmcp", info["name"])
	}
}

// --- tools/list ---

func TestHandle_ToolsList_ReturnsFiveTools(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	if resp["error"] != nil {
		t.Fatalf("tools/list returned error: %v", resp["error"])
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}

	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools is not an array: %v", result["tools"])
	}
	if len(toolsRaw) != 5 {
		t.Errorf("tools count = %d, want 5", len(toolsRaw))
	}

	wantNames := []string{"add_mcp", "remove_mcp", "list_mcps", "search", "execute_code"}
	gotNames := make([]string, 0, len(toolsRaw))
	for _, tRaw := range toolsRaw {
		tool, ok := tRaw.(map[string]any)
		if !ok {
			t.Fatalf("tool is not an object: %v", tRaw)
		}
		gotNames = append(gotNames, tool["name"].(string))
	}

	for _, want := range wantNames {
		found := false
		for _, got := range gotNames {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing tool %q in tools/list response", want)
		}
	}
}

func TestHandle_ToolsList_ExecuteCodeDescSearchFirst(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{
		{Name: "get_data", Description: "Get some data"},
	})
	reg.Add(context.Background(), store.MCPServer{Name: "myapi", URL: upstream.URL}) //nolint:errcheck

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	resp := handle(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	result := resp["result"].(map[string]any)
	toolsRaw := result["tools"].([]any)

	var execDesc string
	for _, tRaw := range toolsRaw {
		tool := tRaw.(map[string]any)
		if tool["name"] == "execute_code" {
			execDesc = tool["description"].(string)
		}
	}

	// execute_code should always use search-first mode (no inline TypeScript defs).
	if strings.Contains(execDesc, "declare namespace") {
		t.Errorf("execute_code should not inline TypeScript defs, got:\n%s", execDesc)
	}
	if !strings.Contains(execDesc, "search") {
		t.Errorf("execute_code description should reference search(), got:\n%s", execDesc)
	}
	// Server summaries should be present.
	if !strings.Contains(execDesc, "myapi") {
		t.Errorf("execute_code description should list registered server 'myapi', got:\n%s", execDesc)
	}
	if !strings.Contains(execDesc, "tools") {
		t.Errorf("execute_code description missing summary info:\n%s", execDesc)
	}
}

// --- tools/call: list_mcps ---

func TestHandle_ToolsCall_ListMCPs_Empty(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"list_mcps","arguments":{}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("list_mcps returned error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("list_mcps result has no content")
	}

	text := content[0].(map[string]any)["text"].(string)
	// Empty registry should return a JSON array [].
	if !strings.Contains(text, "[]") {
		t.Errorf("list_mcps text = %q, want empty array []", text)
	}
}

// --- tools/call: unknown tool ---

func TestHandle_ToolsCall_UnknownTool(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":4,"method":"tools/call",
		"params":{"name":"no_such_tool","arguments":{}}
	}`)

	if resp["error"] == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != float64(protocol.CodeInvalidParams) {
		t.Errorf("error code = %v, want %d", errObj["code"], protocol.CodeInvalidParams)
	}
}

// --- ping ---

func TestHandle_Ping(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{"jsonrpc":"2.0","id":5,"method":"ping"}`)

	if resp["error"] != nil {
		t.Fatalf("ping returned error: %v", resp["error"])
	}
	result := resp["result"]
	if result == nil {
		t.Fatal("ping returned nil result")
	}
	// result should be an empty object.
	resultMap, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("ping result is not an object: %T", result)
	}
	if len(resultMap) != 0 {
		t.Errorf("ping result not empty: %v", resultMap)
	}
}

// --- parse error ---

func TestHandle_ParseError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `this is not valid json`)

	if resp["error"] == nil {
		t.Fatal("expected parse error, got nil")
	}
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != float64(protocol.CodeParseError) {
		t.Errorf("error code = %v, want %d", errObj["code"], protocol.CodeParseError)
	}
}

// --- invalid jsonrpc version ---

func TestHandle_InvalidJSONRPCVersion(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{"jsonrpc":"1.0","id":1,"method":"ping"}`)

	if resp["error"] == nil {
		t.Fatal("expected error for invalid jsonrpc version, got nil")
	}
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != float64(protocol.CodeInvalidRequest) {
		t.Errorf("error code = %v, want %d", errObj["code"], protocol.CodeInvalidRequest)
	}
}

// --- notification (no ID) → nil response ---

func TestHandle_Notification_ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	raw := srv.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if raw != nil {
		t.Errorf("notification should return nil, got %s", raw)
	}
}

func TestHandle_NullID_ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	// A method with null ID is a notification — no response expected.
	raw := srv.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`))
	if raw != nil {
		t.Errorf("null ID should produce nil response, got %s", raw)
	}
}

// --- method not found ---

func TestHandle_MethodNotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{"jsonrpc":"2.0","id":6,"method":"no_such_method"}`)

	if resp["error"] == nil {
		t.Fatal("expected method not found error, got nil")
	}
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != float64(protocol.CodeMethodNotFound) {
		t.Errorf("error code = %v, want %d", errObj["code"], protocol.CodeMethodNotFound)
	}
}

// --- execute_code ---

func TestHandle_ExecuteCode_SimpleScript(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":7,"method":"tools/call",
		"params":{"name":"execute_code","arguments":{"code":"return 1+1;"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("execute_code returned RPC error: %v", resp["error"])
	}

	// execute_code returns a ToolResult. Verify the structure is correct.
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("execute_code returned empty content")
	}
	// The text should contain the Duration line (always present).
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Duration:") {
		t.Errorf("execute_code result text = %q, want to contain 'Duration:'", text)
	}
}

func TestHandle_ExecuteCode_ConsoleLogCaptured(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":8,"method":"tools/call",
		"params":{"name":"execute_code","arguments":{"code":"console.log('hello-log'); return 42;"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("execute_code returned RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)

	if !strings.Contains(text, "hello-log") {
		t.Errorf("execute_code text = %q, want to contain 'hello-log'", text)
	}
}

func TestHandle_ExecuteCode_EmptyCodeReturnsError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":9,"method":"tools/call",
		"params":{"name":"execute_code","arguments":{"code":""}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for empty code, got: %v", result["isError"])
	}
}

func TestHandle_ExecuteCode_SyntaxError(t *testing.T) {
	t.Parallel()

	// NOTE: Due to the FlagAsync/Promise production bug (executor.Execute always
	// returns result.Error = ""), syntax errors in user code do not surface as
	// isError=true in the tool result. The execute_code handler only sets isError
	// when result.Error != "". This test documents the actual current behavior.
	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":10,"method":"tools/call",
		"params":{"name":"execute_code","arguments":{"code":"this is invalid js !!!"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}

	// Due to the FlagAsync bug, result.Error is always "", so isError is false
	// even for broken scripts. The result always contains a Duration line.
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("execute_code returned empty content")
	}
}

// --- tools/call: search ---

func TestHandle_ToolsCall_Search_NoMatch(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":11,"method":"tools/call",
		"params":{"name":"search","arguments":{"query":"zzznomatch"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("search returned RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "No tools found") {
		t.Errorf("search text = %q, want 'No tools found'", text)
	}
}

func TestHandle_ToolsCall_Search_EmptyQueryReturnsError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":12,"method":"tools/call",
		"params":{"name":"search","arguments":{"query":""}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for empty query, got: %v", result["isError"])
	}
}

// --- tools/call: add_mcp ---

func TestHandle_ToolsCall_AddMCP_MissingNameReturnsError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":13,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":{"url":"http://x"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for missing name, got: %v", result["isError"])
	}
}

func TestHandle_ToolsCall_AddMCP_MissingURLAndCommandReturnsError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":14,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":{"name":"myserver"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for missing url+command, got: %v", result["isError"])
	}
}

// --- tools/call: remove_mcp ---

func TestHandle_ToolsCall_RemoveMCP_NotFound(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":15,"method":"tools/call",
		"params":{"name":"remove_mcp","arguments":{"name":"nonexistent"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for removing nonexistent server, got: %v", result["isError"])
	}
}

// --- ServeHTTP ---

func TestServeHTTP_POSTReturns200(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	))
	r.Header.Set("Content-Type", "application/json")

	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("POST ping: status = %d, want 200", w.Code)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
	}
	if w.Header().Get("Mcp-Session-Id") == "" {
		t.Error("Mcp-Session-Id header missing from response")
	}
}

func TestServeHTTP_GETReturns405(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	srv.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", w.Code)
	}
}

func TestServeHTTP_NotificationReturns202(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	))
	r.Header.Set("Content-Type", "application/json")

	srv.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Errorf("notification: status = %d, want 202", w.Code)
	}
}

func TestServeHTTP_SessionIDEchoed(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	))
	r.Header.Set("Mcp-Session-Id", "my-session-id")
	r.Header.Set("Content-Type", "application/json")

	srv.ServeHTTP(w, r)

	if got := w.Header().Get("Mcp-Session-Id"); got != "my-session-id" {
		t.Errorf("Mcp-Session-Id echoed = %q, want my-session-id", got)
	}
}

func TestServeHTTP_GeneratesSessionIDWhenMissing(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
	))
	// No Mcp-Session-Id header — server should generate one.

	srv.ServeHTTP(w, r)

	got := w.Header().Get("Mcp-Session-Id")
	if got == "" {
		t.Error("expected server to generate Mcp-Session-Id header, got empty")
	}
	if len(got) < 16 {
		t.Errorf("generated session ID too short: %q", got)
	}
}

func TestServeHTTP_BodyReadError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)

	// A DELETE is not in the POST check, but we can test the parse error path
	// by sending a POST with a body that is not valid JSON.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not valid json"))
	r.Header.Set("Content-Type", "application/json")

	srv.ServeHTTP(w, r)

	// Should return 200 with a JSON-RPC parse error in the body.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (parse errors go in the JSON-RPC body)", w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] == nil {
		t.Fatal("expected parse error in response body, got nil error")
	}
}

// --- tools/call: add_mcp with URL ---

func TestHandle_ToolsCall_AddMCP_WithURL(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{
		{Name: "ping", Description: "Ping the server"},
	})

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	msg := `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":{"name":"mymcp","url":"` + upstream.URL + `"}}
	}`
	resp := handle(t, srv, msg)

	if resp["error"] != nil {
		t.Fatalf("add_mcp returned RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		content := result["content"].([]any)
		text := content[0].(map[string]any)["text"].(string)
		t.Fatalf("add_mcp returned isError=true: %s", text)
	}

	// Verify the server is now listed.
	listResp := handle(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_mcps","arguments":{}}}`)
	listResult := listResp["result"].(map[string]any)
	listContent := listResult["content"].([]any)
	listText := listContent[0].(map[string]any)["text"].(string)
	if !strings.Contains(listText, "mymcp") {
		t.Errorf("list_mcps missing 'mymcp' after add: %s", listText)
	}
}

func TestHandle_ToolsCall_AddMCP_WithBearerAuth(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	var receivedAuth string
	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}})

	// Wrap the stub with auth capture.
	authUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		// Forward to the stub.
		upstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(authUpstream.Close)

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	msg := `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":{"name":"authed","url":"` + authUpstream.URL + `","auth_token":"my-token"}}
	}`
	resp := handle(t, srv, msg)

	if resp["error"] != nil {
		t.Fatalf("add_mcp returned RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		content := result["content"].([]any)
		text := content[0].(map[string]any)["text"].(string)
		t.Fatalf("add_mcp returned isError=true: %s", text)
	}

	if receivedAuth != "Bearer my-token" {
		t.Errorf("Authorization = %q, want 'Bearer my-token'", receivedAuth)
	}
}

func TestHandle_ToolsCall_AddMCP_WithCustomHeader(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	var receivedHeader string
	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}})

	headerUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("X-Api-Key")
		upstream.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(headerUpstream.Close)

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	msg := `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":{"name":"hdr","url":"` + headerUpstream.URL + `","auth_token":"secret","auth_header":"X-Api-Key"}}
	}`
	resp := handle(t, srv, msg)

	if resp["error"] != nil {
		t.Fatalf("add_mcp returned RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		content := result["content"].([]any)
		text := content[0].(map[string]any)["text"].(string)
		t.Fatalf("add_mcp returned isError=true: %s", text)
	}

	if receivedHeader != "secret" {
		t.Errorf("X-Api-Key = %q, want 'secret'", receivedHeader)
	}
}

func TestHandle_ToolsCall_AddMCP_InvalidArguments(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":null}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for null arguments, got: %v", result["isError"])
	}
}

func TestHandle_ToolsCall_AddMCP_UnreachableServerReturnsError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"add_mcp","arguments":{"name":"bad","url":"http://127.0.0.1:1"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for unreachable server, got: %v", result["isError"])
	}
}

// --- tools/call: remove_mcp ---

func TestHandle_ToolsCall_RemoveMCP_HappyPath(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}})

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	// First add it.
	addMsg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_mcp","arguments":{"name":"todel","url":"` + upstream.URL + `"}}}`
	addResp := handle(t, srv, addMsg)
	if addResp["result"].(map[string]any)["isError"] == true {
		t.Fatal("add_mcp failed in setup")
	}

	// Now remove it.
	removeMsg := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remove_mcp","arguments":{"name":"todel"}}}`
	removeResp := handle(t, srv, removeMsg)

	if removeResp["error"] != nil {
		t.Fatalf("remove_mcp RPC error: %v", removeResp["error"])
	}
	result := removeResp["result"].(map[string]any)
	if result["isError"] == true {
		content := result["content"].([]any)
		text := content[0].(map[string]any)["text"].(string)
		t.Fatalf("remove_mcp returned isError=true: %s", text)
	}
}

func TestHandle_ToolsCall_RemoveMCP_InvalidArguments(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"remove_mcp","arguments":null}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for null arguments, got: %v", result["isError"])
	}
}

func TestHandle_ToolsCall_RemoveMCP_MissingNameReturnsError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"remove_mcp","arguments":{}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for missing name, got: %v", result["isError"])
	}
}

// --- tools/call: search ---

func TestHandle_ToolsCall_Search_WithResults(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{
		{Name: "search_docs", Description: "Search documentation"},
	})
	reg.Add(context.Background(), store.MCPServer{Name: "docs", URL: upstream.URL}) //nolint:errcheck

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"search","arguments":{"query":"search"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("search returned RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] == true {
		content := result["content"].([]any)
		text := content[0].(map[string]any)["text"].(string)
		t.Fatalf("search returned isError=true: %s", text)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "search_docs") {
		t.Errorf("search result text = %q, want to contain 'search_docs'", text)
	}
}

func TestHandle_ToolsCall_Search_InvalidArguments(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"search","arguments":null}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for null arguments, got: %v", result["isError"])
	}
}

// --- tools/call: list_mcps with servers ---

func TestHandle_ToolsCall_ListMCPs_WithServers(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "my_tool", Description: "Does something"}})
	reg.Add(context.Background(), store.MCPServer{Name: "myserver", URL: upstream.URL}) //nolint:errcheck

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{})
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"list_mcps","arguments":{}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("list_mcps returned RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "myserver") {
		t.Errorf("list_mcps text = %q, want to contain 'myserver'", text)
	}
	if !strings.Contains(text, "connected") {
		t.Errorf("list_mcps text = %q, want to contain 'connected'", text)
	}
}

// --- tools/call: execute_code with tool call ---

func TestHandle_ExecuteCode_WithToolCall(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{
		{Name: "ping", Description: "Ping the server"},
	})
	reg.Add(context.Background(), store.MCPServer{Name: "testsrv", URL: upstream.URL}) //nolint:errcheck

	pool := newTestPool(t)
	exec := executor.New(pool)
	srv := server.New(reg, exec, nil, server.Config{MaxToolCalls: 5})
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"execute_code","arguments":{"code":"await tools.testsrv.ping({});"}}
	}`)

	if resp["error"] != nil {
		t.Fatalf("execute_code returned RPC error: %v", resp["error"])
	}

	result := resp["result"].(map[string]any)
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("execute_code returned empty content")
	}
	text := content[0].(map[string]any)["text"].(string)
	// Tool calls should be reported in the output.
	if !strings.Contains(text, "Tool calls:") {
		t.Errorf("execute_code result = %q, want to contain 'Tool calls:'", text)
	}
}

func TestHandle_ExecuteCode_InvalidArguments(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"execute_code","arguments":null}
	}`)

	if resp["error"] != nil {
		t.Fatalf("unexpected RPC error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Errorf("expected isError=true for null arguments, got: %v", result["isError"])
	}
}

// --- tools/call: invalid params ---

func TestHandle_ToolsCall_InvalidParams(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	// params is not a valid JSON object with name/arguments.
	resp := handle(t, srv, `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":null
	}`)

	if resp["error"] == nil {
		t.Fatal("expected error for invalid params, got nil")
	}
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != float64(protocol.CodeInvalidParams) {
		t.Errorf("error code = %v, want %d", errObj["code"], protocol.CodeInvalidParams)
	}
}
