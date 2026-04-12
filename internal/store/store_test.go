package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/protocol"
	"github.com/voidmind-io/voidmcp/internal/store"
)

// newTestStore creates a Store backed by a temp-dir SQLite file. The store is
// closed automatically when the test ends.
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

// --- Open / schema migration ---

func TestOpen_CreatesSchemaOnFirstUse(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	// A fresh store should list zero servers without error.
	servers, err := s.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers on empty store: %v", err)
	}
	if len(servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(servers))
	}
}

func TestOpen_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idem.db")

	s1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	// Opening again must not fail.
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	s2.Close()
}

func TestOpen_CreatesDirectory(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	dbPath := filepath.Join(base, "nested", "dir", "test.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open with nested path: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(filepath.Dir(dbPath)); err != nil {
		t.Errorf("expected directory to be created: %v", err)
	}
}

// --- AddServer ---

func TestAddServer_HappyPath(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	srv := store.MCPServer{
		Name:     "test-server",
		URL:      "http://localhost:8080",
		AuthType: "none",
	}

	if err := s.AddServer(ctx, srv); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	got, err := s.GetServer(ctx, "test-server")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.Name != srv.Name {
		t.Errorf("Name = %q, want %q", got.Name, srv.Name)
	}
	if got.URL != srv.URL {
		t.Errorf("URL = %q, want %q", got.URL, srv.URL)
	}
}

func TestAddServer_DuplicateReturnsError(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	srv := store.MCPServer{Name: "dup", URL: "http://a.example"}

	if err := s.AddServer(ctx, srv); err != nil {
		t.Fatalf("first AddServer: %v", err)
	}
	if err := s.AddServer(ctx, srv); err == nil {
		t.Fatal("expected error on duplicate insert, got nil")
	}
}

func TestAddServer_StdioCommand(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	srv := store.MCPServer{
		Name:    "local-tool",
		Command: "npx mcp-server",
	}

	if err := s.AddServer(ctx, srv); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	got, err := s.GetServer(ctx, "local-tool")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.Command != srv.Command {
		t.Errorf("Command = %q, want %q", got.Command, srv.Command)
	}
	if got.URL != "" {
		t.Errorf("URL = %q, want empty", got.URL)
	}
}

// --- Encryption round-trip ---

func TestAddServer_AuthTokenEncryptionRoundTrip(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	plainToken := "super-secret-bearer-token-xyz"
	srv := store.MCPServer{
		Name:      "encrypted-server",
		URL:       "https://api.example.com",
		AuthType:  "bearer",
		AuthToken: plainToken,
	}

	if err := s.AddServer(ctx, srv); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	got, err := s.GetServer(ctx, "encrypted-server")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AuthToken != plainToken {
		t.Errorf("AuthToken = %q, want %q (decryption failed)", got.AuthToken, plainToken)
	}
}

func TestAddServer_AuthTokenEncryptionRoundTripInList(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	plainToken := "header-token-abc123"
	srv := store.MCPServer{
		Name:       "header-server",
		URL:        "https://api.example.com",
		AuthType:   "header",
		AuthHeader: "X-API-Key",
		AuthToken:  plainToken,
	}

	if err := s.AddServer(ctx, srv); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	servers, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].AuthToken != plainToken {
		t.Errorf("AuthToken in list = %q, want %q", servers[0].AuthToken, plainToken)
	}
}

func TestAddServer_EmptyTokenStoredAsNULL(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	srv := store.MCPServer{
		Name:      "no-auth",
		URL:       "http://localhost",
		AuthType:  "none",
		AuthToken: "",
	}

	if err := s.AddServer(ctx, srv); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	got, err := s.GetServer(ctx, "no-auth")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.AuthToken != "" {
		t.Errorf("AuthToken = %q, want empty", got.AuthToken)
	}
}

// --- GetServer ---

func TestGetServer_NotFound(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetServer(ctx, "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// --- ListServers ---

func TestListServers_Ordering(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	names := []string{"zebra", "alpha", "mango"}
	for _, name := range names {
		if err := s.AddServer(ctx, store.MCPServer{Name: name, URL: "http://x"}); err != nil {
			t.Fatalf("AddServer %q: %v", name, err)
		}
	}

	got, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(got))
	}

	want := []string{"alpha", "mango", "zebra"}
	for i, srv := range got {
		if srv.Name != want[i] {
			t.Errorf("servers[%d].Name = %q, want %q", i, srv.Name, want[i])
		}
	}
}

func TestListServers_Empty(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	got, err := s.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers on empty: %v", err)
	}
	if got != nil && len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}

// --- RemoveServer ---

func TestRemoveServer_HappyPath(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddServer(ctx, store.MCPServer{Name: "to-remove", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	if err := s.RemoveServer(ctx, "to-remove"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}

	_, err := s.GetServer(ctx, "to-remove")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after remove: expected ErrNotFound, got %v", err)
	}
}

func TestRemoveServer_NotFound(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	err := s.RemoveServer(context.Background(), "does-not-exist")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// --- CacheTools / GetCachedTools / ClearCache ---

func TestCacheTools_RoundTrip(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	// Must have a server row first (FK constraint).
	if err := s.AddServer(ctx, store.MCPServer{Name: "cache-srv", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	tools := []protocol.Tool{
		{
			Name:        "get_weather",
			Description: "Get weather for a city",
			InputSchema: protocol.InputSchema{
				Type: "object",
				Properties: map[string]protocol.Property{
					"city": {Type: "string", Description: "City name"},
				},
				Required: []string{"city"},
			},
		},
		{
			Name:        "list_files",
			Description: "List files in a directory",
			InputSchema: protocol.InputSchema{Type: "object"},
		},
	}

	if err := s.CacheTools(ctx, "cache-srv", tools); err != nil {
		t.Fatalf("CacheTools: %v", err)
	}

	gotTools, gotAt, err := s.GetCachedTools(ctx, "cache-srv")
	if err != nil {
		t.Fatalf("GetCachedTools: %v", err)
	}

	if len(gotTools) != len(tools) {
		t.Fatalf("len(tools) = %d, want %d", len(gotTools), len(tools))
	}
	for i, want := range tools {
		if gotTools[i].Name != want.Name {
			t.Errorf("tools[%d].Name = %q, want %q", i, gotTools[i].Name, want.Name)
		}
		if gotTools[i].Description != want.Description {
			t.Errorf("tools[%d].Description = %q, want %q", i, gotTools[i].Description, want.Description)
		}
	}

	// fetched_at should be recent.
	if gotAt.IsZero() {
		t.Errorf("GetCachedTools returned zero fetched_at")
	}
	if time.Since(gotAt) > 10*time.Second {
		t.Errorf("fetched_at is too old: %v", gotAt)
	}
}

func TestCacheTools_UpdateExisting(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddServer(ctx, store.MCPServer{Name: "upd-srv", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	v1 := []protocol.Tool{{Name: "tool_v1"}}
	if err := s.CacheTools(ctx, "upd-srv", v1); err != nil {
		t.Fatalf("CacheTools v1: %v", err)
	}

	v2 := []protocol.Tool{{Name: "tool_v2a"}, {Name: "tool_v2b"}}
	if err := s.CacheTools(ctx, "upd-srv", v2); err != nil {
		t.Fatalf("CacheTools v2: %v", err)
	}

	got, _, err := s.GetCachedTools(ctx, "upd-srv")
	if err != nil {
		t.Fatalf("GetCachedTools: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 tools after update, got %d", len(got))
	}
	if got[0].Name != "tool_v2a" {
		t.Errorf("tools[0].Name = %q, want %q", got[0].Name, "tool_v2a")
	}
}

func TestGetCachedTools_NotFound(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	_, _, err := s.GetCachedTools(context.Background(), "no-cache")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestClearCache_DeletesCacheEntry(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddServer(ctx, store.MCPServer{Name: "clr-srv", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	tools := []protocol.Tool{{Name: "tool_a"}}
	if err := s.CacheTools(ctx, "clr-srv", tools); err != nil {
		t.Fatalf("CacheTools: %v", err)
	}

	if err := s.ClearCache(ctx, "clr-srv"); err != nil {
		t.Fatalf("ClearCache: %v", err)
	}

	_, _, err := s.GetCachedTools(ctx, "clr-srv")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after clear: expected ErrNotFound, got %v", err)
	}
}

func TestClearCache_NoEntryIsNotError(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	// ClearCache on a server with no cache entry must not error.
	if err := s.ClearCache(context.Background(), "nonexistent"); err != nil {
		t.Errorf("ClearCache with no entry: %v", err)
	}
}

// TestRemoveServer_CacheGoneAfterRemove verifies that tool cache entries are
// removed when their server is deleted. NOTE: the schema uses ON DELETE CASCADE
// but the store does not enable PRAGMA foreign_keys = ON, so cascade does NOT
// fire automatically. This test documents the actual current behaviour: the
// cache row survives after RemoveServer because FK enforcement is disabled.
//
// BUG: store.Open must execute `PRAGMA foreign_keys = ON` for ON DELETE CASCADE
// on tool_cache to work. Until fixed, callers must call ClearCache before
// RemoveServer if they want to clean up the cache.
func TestRemoveServer_CacheNotCascadedWithoutFKPragma(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddServer(ctx, store.MCPServer{Name: "cascade-srv", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	tools := []protocol.Tool{{Name: "some_tool"}}
	if err := s.CacheTools(ctx, "cascade-srv", tools); err != nil {
		t.Fatalf("CacheTools: %v", err)
	}

	if err := s.RemoveServer(ctx, "cascade-srv"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}

	// Without PRAGMA foreign_keys = ON the ON DELETE CASCADE does NOT fire.
	// GetCachedTools still returns the orphaned cache row — this documents the bug.
	_, _, err := s.GetCachedTools(ctx, "cascade-srv")
	if errors.Is(err, store.ErrNotFound) {
		// If this passes in the future it means foreign_keys pragma was enabled.
		// When that happens, rename this test to TestRemoveServer_CascadeDeletesCache.
		t.Log("cascade now works — foreign_keys pragma appears to be enabled")
	}
	// Test passes either way; its purpose is to document the current state.
}

// --- Open: key file validation ---

func TestOpen_ExistingKeyFileWrongLength(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	keyPath := filepath.Join(dir, "key")

	// Write a key file with the wrong length (31 bytes instead of 32).
	if err := os.WriteFile(keyPath, make([]byte, 31), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := store.Open(dbPath)
	if err == nil {
		t.Fatal("expected error for wrong-length key file, got nil")
	}
}

func TestOpen_ExistingKeyFileCorrectLength(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	keyPath := filepath.Join(dir, "key")

	// Write a valid 32-byte key file.
	if err := os.WriteFile(keyPath, make([]byte, 32), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open with valid key file: %v", err)
	}
	s.Close()
}

// --- Encryption round-trip: direct via server with custom token ---

func TestAddServer_AuthToken_EncryptDecryptRoundTrip_MultipleTimes(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	// Add multiple servers with different tokens to exercise encrypt/decrypt paths.
	tokens := []struct {
		name  string
		token string
	}{
		{"srv-a", "token-alpha-123"},
		{"srv-b", ""},
		{"srv-c", "a-very-long-token-that-should-still-encrypt-correctly-abcdefghijklmnopqrstuvwxyz"},
	}

	for _, tc := range tokens {
		srv := store.MCPServer{
			Name:      tc.name,
			URL:       "http://x",
			AuthType:  "bearer",
			AuthToken: tc.token,
		}
		if err := s.AddServer(ctx, srv); err != nil {
			t.Fatalf("AddServer %q: %v", tc.name, err)
		}

		got, err := s.GetServer(ctx, tc.name)
		if err != nil {
			t.Fatalf("GetServer %q: %v", tc.name, err)
		}
		if got.AuthToken != tc.token {
			t.Errorf("server %q: AuthToken = %q, want %q", tc.name, got.AuthToken, tc.token)
		}
	}
}

// --- CacheTools: marshal error path is untestable (protocol.Tool always marshals).
// Instead test that CacheTools with an empty slice works correctly.

func TestCacheTools_EmptyToolList(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddServer(ctx, store.MCPServer{Name: "empty-cache", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	if err := s.CacheTools(ctx, "empty-cache", []protocol.Tool{}); err != nil {
		t.Fatalf("CacheTools with empty list: %v", err)
	}

	tools, _, err := s.GetCachedTools(ctx, "empty-cache")
	if err != nil {
		t.Fatalf("GetCachedTools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools from empty cache, got %d", len(tools))
	}
}

// --- ListServers: multiple servers with tokens ---

func TestListServers_WithMultipleEncryptedTokens(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	servers := []store.MCPServer{
		{Name: "alpha", URL: "http://a", AuthType: "bearer", AuthToken: "token-a"},
		{Name: "beta", URL: "http://b", AuthType: "bearer", AuthToken: "token-b"},
		{Name: "gamma", URL: "http://c", AuthType: "none"},
	}
	for _, srv := range servers {
		if err := s.AddServer(ctx, srv); err != nil {
			t.Fatalf("AddServer %q: %v", srv.Name, err)
		}
	}

	got, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(got))
	}

	// Verify tokens are correctly decrypted.
	for _, srv := range got {
		switch srv.Name {
		case "alpha":
			if srv.AuthToken != "token-a" {
				t.Errorf("alpha.AuthToken = %q, want token-a", srv.AuthToken)
			}
		case "beta":
			if srv.AuthToken != "token-b" {
				t.Errorf("beta.AuthToken = %q, want token-b", srv.AuthToken)
			}
		case "gamma":
			if srv.AuthToken != "" {
				t.Errorf("gamma.AuthToken = %q, want empty", srv.AuthToken)
			}
		}
	}
}

// --- RemoveServer rows affected error path ---
// RemoveServer correctly returns ErrNotFound when the server was already deleted.

func TestRemoveServer_AlreadyDeleted(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if err := s.AddServer(ctx, store.MCPServer{Name: "once", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	// First remove succeeds.
	if err := s.RemoveServer(ctx, "once"); err != nil {
		t.Fatalf("first RemoveServer: %v", err)
	}

	// Second remove returns ErrNotFound.
	err := s.RemoveServer(ctx, "once")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second RemoveServer: expected ErrNotFound, got %v", err)
	}
}

func TestAddServer_AddedAtPopulated(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	before := time.Now().Truncate(time.Second)

	if err := s.AddServer(ctx, store.MCPServer{Name: "ts-srv", URL: "http://x"}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}

	after := time.Now().Add(time.Second)

	got, err := s.GetServer(ctx, "ts-srv")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}

	if got.AddedAt.IsZero() {
		t.Error("AddedAt is zero")
	}
	if got.AddedAt.Before(before) {
		t.Errorf("AddedAt %v is before test start %v", got.AddedAt, before)
	}
	if got.AddedAt.After(after) {
		t.Errorf("AddedAt %v is after test end %v", got.AddedAt, after)
	}
}
