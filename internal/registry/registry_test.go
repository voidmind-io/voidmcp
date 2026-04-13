package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/protocol"
	"github.com/voidmind-io/voidmcp/internal/registry"
	"github.com/voidmind-io/voidmcp/internal/store"
)

// --- Test helpers ---

// newTestStore opens a Store backed by a temp-dir SQLite file. Closed via t.Cleanup.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newTestRegistry creates a Registry backed by a fresh temp-dir store.
func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })
	return reg
}

// mcpStubServer creates an httptest.Server that acts as a minimal MCP stub.
// It responds to initialize, notifications/initialized, tools/list, tools/call,
// and ping. callResults maps tool name to the JSON result to return.
func mcpStubServer(t *testing.T, tools []protocol.Tool, callResults map[string]json.RawMessage) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

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
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p) //nolint:errcheck
			result := json.RawMessage(`{"ok":true}`)
			if callResults != nil {
				if r, ok := callResults[p.Name]; ok {
					result = r
				}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": result,
			})
		case "ping":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- Add ---

func TestRegistry_Add_HappyPath(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	upstream := mcpStubServer(t, []protocol.Tool{
		{Name: "search", Description: "Search things"},
		{Name: "read", Description: "Read file"},
	}, nil)

	gotTools, err := reg.Add(context.Background(), store.MCPServer{
		Name: "test-mcp", URL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(gotTools) != 2 {
		t.Errorf("Add returned %d tools, want 2", len(gotTools))
	}
}

func TestRegistry_Add_PersistsToStore(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "ping"}}, nil)

	if _, err := reg.Add(context.Background(), store.MCPServer{
		Name: "persisted-mcp", URL: upstream.URL,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := st.GetServer(context.Background(), "persisted-mcp")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.Name != "persisted-mcp" {
		t.Errorf("Name = %q, want persisted-mcp", got.Name)
	}
}

func TestRegistry_Add_UnreachableServerReturnsError(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	_, err := reg.Add(context.Background(), store.MCPServer{
		Name: "bad-mcp",
		URL:  "http://127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
}

func TestRegistry_Add_NeitherURLNorCommandReturnsError(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	_, err := reg.Add(context.Background(), store.MCPServer{
		Name: "empty-mcp",
	})
	if err == nil {
		t.Fatal("expected error for server with no URL or command, got nil")
	}
}

// --- Remove ---

func TestRegistry_Remove_HappyPath(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t1"}}, nil)

	if _, err := reg.Add(context.Background(), store.MCPServer{
		Name: "to-remove", URL: upstream.URL,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := reg.Remove(context.Background(), "to-remove"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	for _, s := range reg.List() {
		if s.Name == "to-remove" {
			t.Error("server still in list after Remove")
		}
	}

	_, err := st.GetServer(context.Background(), "to-remove")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound from store after Remove, got %v", err)
	}
}

func TestRegistry_Remove_NotFound(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	err := reg.Remove(context.Background(), "does-not-exist")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// --- List ---

func TestRegistry_List_Empty(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	got := reg.List()
	if len(got) != 0 {
		t.Errorf("List() on empty registry = %d entries, want 0", len(got))
	}
}

func TestRegistry_List_ReturnsAllServers(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}}, nil)
		if _, err := reg.Add(context.Background(), store.MCPServer{
			Name: name, URL: upstream.URL,
		}); err != nil {
			t.Fatalf("Add %q: %v", name, err)
		}
	}

	got := reg.List()
	if len(got) != 3 {
		t.Fatalf("List() = %d entries, want 3", len(got))
	}

	gotNames := make([]string, len(got))
	for i, s := range got {
		gotNames[i] = s.Name
	}
	sort.Strings(gotNames)
	sort.Strings(names)
	for i, want := range names {
		if gotNames[i] != want {
			t.Errorf("server[%d].Name = %q, want %q", i, gotNames[i], want)
		}
	}
}

func TestRegistry_List_StatusConnected(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}}, nil)

	if _, err := reg.Add(context.Background(), store.MCPServer{
		Name: "ok-mcp", URL: upstream.URL,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].Status != "connected" {
		t.Errorf("Status = %q, want connected", servers[0].Status)
	}
}

func TestRegistry_List_ToolsPopulated(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{
		{Name: "tool_a", Description: "Do A"},
		{Name: "tool_b", Description: "Do B"},
	}
	upstream := mcpStubServer(t, tools, nil)

	if _, err := reg.Add(context.Background(), store.MCPServer{
		Name: "tool-mcp", URL: upstream.URL,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if len(servers[0].Tools) != 2 {
		t.Errorf("Tools = %d, want 2", len(servers[0].Tools))
	}
}

// --- AllTools / TotalToolCount ---

func TestRegistry_AllTools_ReturnsSnapshot(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	upstream := mcpStubServer(t, []protocol.Tool{
		{Name: "tool_x"},
		{Name: "tool_y"},
	}, nil)

	if _, err := reg.Add(context.Background(), store.MCPServer{
		Name: "srv", URL: upstream.URL,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	all := reg.AllTools()
	if len(all) != 1 {
		t.Fatalf("AllTools len = %d, want 1", len(all))
	}
	if len(all["srv"]) != 2 {
		t.Errorf("AllTools[srv] = %d, want 2", len(all["srv"]))
	}
}

func TestRegistry_TotalToolCount(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	if n := reg.TotalToolCount(); n != 0 {
		t.Errorf("TotalToolCount on empty = %d, want 0", n)
	}

	upstream1 := mcpStubServer(t, []protocol.Tool{{Name: "a"}, {Name: "b"}}, nil)
	upstream2 := mcpStubServer(t, []protocol.Tool{{Name: "c"}}, nil)

	if _, err := reg.Add(context.Background(), store.MCPServer{Name: "srv1", URL: upstream1.URL}); err != nil {
		t.Fatalf("Add srv1: %v", err)
	}
	if _, err := reg.Add(context.Background(), store.MCPServer{Name: "srv2", URL: upstream2.URL}); err != nil {
		t.Fatalf("Add srv2: %v", err)
	}

	if n := reg.TotalToolCount(); n != 3 {
		t.Errorf("TotalToolCount = %d, want 3", n)
	}
}

// --- Search ---

func TestRegistry_Search_ExactMatch(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{
		{Name: "get_weather", Description: "Get weather data"},
		{Name: "list_files", Description: "List files in directory"},
	}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("get_weather", 10)
	if len(results) != 1 {
		t.Fatalf("Search exact: expected 1 result, got %d", len(results))
	}
	if results[0].Tool.Name != "get_weather" {
		t.Errorf("Tool.Name = %q, want get_weather", results[0].Tool.Name)
	}
	if results[0].Score != 100 {
		t.Errorf("Score = %d, want 100 for exact match", results[0].Score)
	}
}

func TestRegistry_Search_PrefixMatch(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{
		{Name: "list_files", Description: "List files"},
		{Name: "list_dirs", Description: "List directories"},
		{Name: "get_file", Description: "Get a file"},
	}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("list", 10)
	if len(results) < 2 {
		t.Fatalf("Search prefix: expected >= 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Score != 90 {
			t.Errorf("prefix match score = %d, want 90", r.Score)
		}
	}
}

func TestRegistry_Search_DescriptionMatch(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{
		{Name: "do_thing", Description: "This does something special"},
	}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("special", 10)
	if len(results) != 1 {
		t.Fatalf("Search desc: expected 1 result, got %d", len(results))
	}
	if results[0].Score != 50 {
		t.Errorf("desc match score = %d, want 50", results[0].Score)
	}
}

func TestRegistry_Search_NoMatch(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{{Name: "weather", Description: "Weather tool"}}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("zzznomatch", 10)
	if len(results) != 0 {
		t.Errorf("Search no match: expected 0 results, got %d", len(results))
	}
}

func TestRegistry_Search_Limit(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := make([]protocol.Tool, 5)
	for i := range tools {
		tools[i] = protocol.Tool{Name: "tool_" + string(rune('a'+i))}
	}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("tool", 2)
	if len(results) != 2 {
		t.Errorf("Search with limit 2: got %d results, want 2", len(results))
	}
}

func TestRegistry_Search_CaseInsensitive(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{{Name: "GetWeather", Description: "Weather"}}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("getweather", 10)
	if len(results) != 1 {
		t.Errorf("case-insensitive search: expected 1 result, got %d", len(results))
	}
}

func TestRegistry_Search_SortedByScore(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	tools := []protocol.Tool{
		{Name: "weather_data", Description: "about weather"},
		{Name: "weather", Description: "exact match"},
		{Name: "weather_forecast", Description: "forecast"},
	}
	upstream := mcpStubServer(t, tools, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	results := reg.Search("weather", 10)
	if len(results) < 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted: results[%d].Score=%d > results[%d].Score=%d",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}

	if results[0].Tool.Name != "weather" {
		t.Errorf("top result = %q, want exact match 'weather'", results[0].Tool.Name)
	}
}

// --- CallTool ---

func TestRegistry_CallTool_HappyPath(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	want := json.RawMessage(`{"result":"ok"}`)
	upstream := mcpStubServer(t, []protocol.Tool{{Name: "ping"}}, map[string]json.RawMessage{
		"ping": want,
	})

	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	got, err := reg.CallTool(context.Background(), "srv", "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	// The stub server returns the result as-is; compare raw JSON.
	var gotVal, wantVal any
	json.Unmarshal(got, &gotVal)
	json.Unmarshal(want, &wantVal)
	gotJSON, _ := json.Marshal(gotVal)
	wantJSON, _ := json.Marshal(wantVal)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("CallTool result = %s, want %s", got, want)
	}
}

func TestRegistry_CallTool_UnknownServer(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	_, err := reg.CallTool(context.Background(), "no-such-server", "ping", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown server, got nil")
	}
}

func TestRegistry_CallTool_UnknownTool(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	upstream := mcpStubServer(t, []protocol.Tool{{Name: "existing"}}, nil)
	reg.Add(context.Background(), store.MCPServer{Name: "srv", URL: upstream.URL}) //nolint:errcheck

	_, err := reg.CallTool(context.Background(), "srv", "nonexistent_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
}

// --- Load ---

func TestRegistry_Load_FromStore(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)

	tools := []protocol.Tool{
		{Name: "cached_tool", Description: "A cached tool"},
	}

	upstream := mcpStubServer(t, tools, nil)

	// Add a server and immediately close the first registry.
	reg1 := registry.New(st, time.Hour)
	if _, err := reg1.Add(context.Background(), store.MCPServer{
		Name: "persistent-mcp", URL: upstream.URL,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	reg1.Close()

	// Fresh registry loading from the same store.
	reg2 := registry.New(st, time.Hour)
	t.Cleanup(func() { reg2.Close() })

	if err := reg2.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	servers := reg2.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server after Load, got %d", len(servers))
	}
	if servers[0].Name != "persistent-mcp" {
		t.Errorf("server name = %q, want persistent-mcp", servers[0].Name)
	}
	if len(servers[0].Tools) != 1 {
		t.Errorf("tools count = %d, want 1", len(servers[0].Tools))
	}
}

// --- Load: stale cache path ---

func TestRegistry_Load_StaleCacheRefetchesLive(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)

	tools := []protocol.Tool{{Name: "live_tool", Description: "Live"}}
	upstream := mcpStubServer(t, tools, nil)

	// Pre-populate store with a server and a stale cache (fetched_at very old).
	ctx := context.Background()
	if err := st.AddServer(ctx, store.MCPServer{Name: "stale-mcp", URL: upstream.URL}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	// Cache tools but use a very short max age so it's immediately stale.
	if err := st.CacheTools(ctx, "stale-mcp", tools); err != nil {
		t.Fatalf("CacheTools: %v", err)
	}

	// Use a 0 max-age so the cache is always stale.
	reg := registry.New(st, 0)
	t.Cleanup(func() { reg.Close() })

	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server after Load, got %d", len(servers))
	}
	if servers[0].Status != "connected" {
		t.Errorf("status = %q, want connected", servers[0].Status)
	}
}

func TestRegistry_Load_LiveFetchFailUsesStaleCache(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	ctx := context.Background()

	// Register a server with a reachable URL, cache its tools, then shut down.
	tools := []protocol.Tool{{Name: "cached_tool"}}
	upstream := mcpStubServer(t, tools, nil)

	if err := st.AddServer(ctx, store.MCPServer{Name: "cached-mcp", URL: upstream.URL}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := st.CacheTools(ctx, "cached-mcp", tools); err != nil {
		t.Fatalf("CacheTools: %v", err)
	}

	// Close the upstream server so the live fetch will fail.
	upstream.Close()

	// Use a 0 max-age so cache is stale, forcing a live fetch attempt.
	reg := registry.New(st, 0)
	t.Cleanup(func() { reg.Close() })

	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	// Status should be "error" since live fetch failed.
	if servers[0].Status != "error" {
		t.Errorf("status = %q, want error", servers[0].Status)
	}
	// But stale cache tools should be available.
	if len(servers[0].Tools) != 1 {
		t.Errorf("expected 1 cached tool, got %d", len(servers[0].Tools))
	}
}

func TestRegistry_Load_UnreachableServerNoCache(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	ctx := context.Background()

	// A server that is unreachable and has no cache.
	if err := st.AddServer(ctx, store.MCPServer{Name: "dead-mcp", URL: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	reg := registry.New(st, 0)
	t.Cleanup(func() { reg.Close() })

	// Load should not return an error — it just records the server as errored.
	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].Status != "error" {
		t.Errorf("status = %q, want error", servers[0].Status)
	}
}

func TestRegistry_Load_FreshCacheNoLiveFetch(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	ctx := context.Background()

	tools := []protocol.Tool{{Name: "fresh_tool"}}

	// Server is dead but cache is fresh — Load should use the cache.
	if err := st.AddServer(ctx, store.MCPServer{Name: "cached-srv", URL: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := st.CacheTools(ctx, "cached-srv", tools); err != nil {
		t.Fatalf("CacheTools: %v", err)
	}

	// Very long max-age so the fresh cache is used.
	reg := registry.New(st, 24*time.Hour)
	t.Cleanup(func() { reg.Close() })

	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].Status != "connected" {
		t.Errorf("status = %q, want connected (fresh cache)", servers[0].Status)
	}
	if len(servers[0].Tools) != 1 {
		t.Errorf("expected 1 tool from cache, got %d", len(servers[0].Tools))
	}
}

func TestRegistry_Load_BadCommandReturnsErrorStatus(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	ctx := context.Background()

	// A server with a command that will fail to start (binary not found).
	if err := st.AddServer(ctx, store.MCPServer{
		Name:    "bad-cmd-srv",
		Command: "/this/binary/does/not/exist/anywhere/on/this/system",
	}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	reg := registry.New(st, 0)
	t.Cleanup(func() { reg.Close() })

	// Load should not fail overall — the server is just recorded as errored.
	if err := reg.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	servers := reg.List()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server with error status, got %d", len(servers))
	}
	if servers[0].Status != "error" {
		t.Errorf("status = %q, want error for server with bad command", servers[0].Status)
	}
}

// --- Close ---

func TestRegistry_Close_NoServersPanics(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	// Close on an empty registry must not panic.
	reg.Close()
}

// --- Watch (v0.0.10) ---

// waitFor polls cond every 5ms until it returns true or deadline elapses.
// It calls t.Fatal if the deadline is reached before cond becomes true.
func waitFor(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not satisfied within %v", deadline)
}

// newTestRegistryFromStore creates a Registry from an existing store (shared
// between the watcher under test and the "other writer" that calls the store
// directly).
func newTestRegistryFromStore(t *testing.T, st *store.Store) *registry.Registry {
	t.Helper()
	reg := registry.New(st, time.Hour)
	t.Cleanup(func() { reg.Close() })
	return reg
}

// TestWatch_PicksUpAddFromAnotherWriter simulates a second process calling
// st.AddServer directly (e.g. `voidmcp add`) while Watch is running. The
// watcher must pick up the new server and invoke onChange.
func TestWatch_PicksUpAddFromAnotherWriter(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := newTestRegistryFromStore(t, st)

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "tool_one"}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var callCount int
	var mu sync.Mutex
	onChange := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	go reg.Watch(ctx, 20*time.Millisecond, onChange)

	// Capture callCount before the store write so we can assert it increases.
	mu.Lock()
	countBefore := callCount
	mu.Unlock()

	// Write the server directly to the store — bypassing the registry Add path —
	// to simulate a second CLI process.
	if err := st.AddServer(ctx, store.MCPServer{
		Name: "watcher-add-test",
		URL:  upstream.URL,
	}); err != nil {
		t.Fatalf("st.AddServer: %v", err)
	}

	// Wait for the watcher to pick up the new server.
	waitFor(t, 500*time.Millisecond, func() bool {
		for _, s := range reg.List() {
			if s.Name == "watcher-add-test" {
				return true
			}
		}
		return false
	})

	mu.Lock()
	got := callCount
	mu.Unlock()

	if got <= countBefore {
		t.Errorf("onChange call count did not increase after store add: before=%d after=%d", countBefore, got)
	}
}

// TestWatch_PicksUpRemoveFromAnotherWriter verifies that when a server is
// deleted directly from the store, the watcher removes it from the in-memory
// registry and calls onChange.
func TestWatch_PicksUpRemoveFromAnotherWriter(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := newTestRegistryFromStore(t, st)

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pre-populate via registry.Add so the server is in both the store and
	// the in-memory index.
	if _, err := reg.Add(ctx, store.MCPServer{
		Name: "watcher-remove-test",
		URL:  upstream.URL,
	}); err != nil {
		t.Fatalf("reg.Add: %v", err)
	}

	var callCount int
	var mu sync.Mutex
	onChange := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	go reg.Watch(ctx, 20*time.Millisecond, onChange)

	// Delete directly from the store — simulating another process.
	if err := st.RemoveServer(ctx, "watcher-remove-test"); err != nil {
		t.Fatalf("st.RemoveServer: %v", err)
	}

	// Wait for the watcher to drop the server from the in-memory index.
	waitFor(t, 500*time.Millisecond, func() bool {
		for _, s := range reg.List() {
			if s.Name == "watcher-remove-test" {
				return false
			}
		}
		return true
	})

	mu.Lock()
	got := callCount
	mu.Unlock()

	if got == 0 {
		t.Error("onChange was never called after store remove")
	}
}

// TestWatch_NoDiffNoOnChange asserts that when the store matches the in-memory
// registry, onChange is never called across several tick cycles.
func TestWatch_NoDiffNoOnChange(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := newTestRegistryFromStore(t, st)

	upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Add a server so both in-memory and store are in sync from the start.
	if _, err := reg.Add(ctx, store.MCPServer{
		Name: "no-diff-server",
		URL:  upstream.URL,
	}); err != nil {
		t.Fatalf("reg.Add: %v", err)
	}

	var callCount int
	var mu sync.Mutex
	onChange := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	go reg.Watch(ctx, 20*time.Millisecond, onChange)

	// Allow at least 5 tick cycles (5 * 20ms = 100ms) to elapse.
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	got := callCount
	mu.Unlock()

	if got != 0 {
		t.Errorf("onChange called %d times with no diff, want 0", got)
	}
}

// TestWatch_StopsOnContextCancellation verifies that the Watch goroutine exits
// promptly when its context is cancelled.
func TestWatch_StopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.Watch(ctx, 20*time.Millisecond, nil)
	}()

	// Cancel the context and expect the goroutine to stop within 500ms.
	cancel()

	select {
	case <-done:
		// Goroutine exited cleanly.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Watch goroutine did not stop after context cancellation")
	}
}

// TestWatch_SurvivesTransportErrorForNewServer checks that when a server added
// to the store has an unreachable URL (transport / tool-fetch fails), the
// watcher does not panic and stays alive so that subsequent healthy adds still
// work. Failed attempts are NOT stubbed into the registry — they are retried
// on the next tick (self-healing after transient errors).
func TestWatch_SurvivesTransportErrorForNewServer(t *testing.T) {
	t.Parallel()

	st := newTestStore(t)
	reg := newTestRegistryFromStore(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var changeCount int
	var mu sync.Mutex
	onChange := func() {
		mu.Lock()
		changeCount++
		mu.Unlock()
	}

	go reg.Watch(ctx, 20*time.Millisecond, onChange)

	// Add a server with an unreachable URL directly to the store, bypassing
	// the registry (which would refuse to add an unreachable server).
	if err := st.AddServer(ctx, store.MCPServer{
		Name: "broken-server",
		URL:  "http://127.0.0.1:1", // nothing listening here
	}); err != nil {
		t.Fatalf("st.AddServer broken: %v", err)
	}

	// Give the watcher a few ticks to attempt (and fail) the broken server.
	// The broken server must NOT appear in List() because failed attempts are
	// not stubbed in — they are left for the next tick to retry.
	time.Sleep(120 * time.Millisecond)
	for _, s := range reg.List() {
		if s.Name == "broken-server" {
			t.Errorf("broken-server should not be in List() after transport failure; got status=%q", s.Status)
		}
	}

	// Now add a healthy server to confirm the watcher is still running despite
	// repeated failures on the broken server.
	upstream := mcpStubServer(t, []protocol.Tool{{Name: "healthy_tool"}}, nil)
	if err := st.AddServer(ctx, store.MCPServer{
		Name: "healthy-server",
		URL:  upstream.URL,
	}); err != nil {
		t.Fatalf("st.AddServer healthy: %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		for _, s := range reg.List() {
			if s.Name == "healthy-server" && s.Status == "connected" {
				return true
			}
		}
		return false
	})

	mu.Lock()
	got := changeCount
	mu.Unlock()

	if got < 1 {
		t.Errorf("onChange called %d times, want >= 1 (at least for healthy-server add)", got)
	}
}

// --- Concurrency ---

func TestRegistry_ConcurrentAddSearch(t *testing.T) {
	t.Parallel()

	reg := newTestRegistry(t)
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := "srv-" + string(rune('a'+idx))
			upstream := mcpStubServer(t, []protocol.Tool{{Name: "t"}}, nil)
			reg.Add(context.Background(), store.MCPServer{Name: name, URL: upstream.URL}) //nolint:errcheck
		}(i)
	}

	wg.Wait()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.Search("t", 10)
		}()
	}

	wg.Wait()
}
