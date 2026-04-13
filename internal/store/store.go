// Package store provides persistent configuration storage for voidmcp.
// It uses a local SQLite database at ~/.voidmcp/voidmcp.db and an
// AES-256-GCM encryption key at ~/.voidmcp/key to protect auth tokens at rest.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	_ "modernc.org/sqlite" // register "sqlite" driver

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// ErrNotFound is returned when a requested server does not exist in the store.
var ErrNotFound = errors.New("not found")

// schema is the SQL DDL applied on every Open call. CREATE IF NOT EXISTS
// makes it idempotent.
const schema = `
CREATE TABLE IF NOT EXISTS mcp_servers (
    name            TEXT PRIMARY KEY,
    url             TEXT NOT NULL DEFAULT '',
    command         TEXT NOT NULL DEFAULT '',
    auth_type       TEXT NOT NULL DEFAULT 'none',
    auth_header     TEXT NOT NULL DEFAULT '',
    auth_token_enc  TEXT,
    added_at        TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS tool_cache (
    server_name TEXT PRIMARY KEY REFERENCES mcp_servers(name) ON DELETE CASCADE,
    tools_json  TEXT NOT NULL,
    fetched_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS output_schemas (
    server_name TEXT NOT NULL,
    tool_name   TEXT NOT NULL,
    schema_json TEXT NOT NULL,
    inferred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (server_name, tool_name),
    FOREIGN KEY (server_name) REFERENCES mcp_servers(name) ON DELETE CASCADE
);
`

// Store is the voidmcp configuration store. It wraps a SQLite database and an
// AES-256-GCM encryption key used to protect auth tokens at rest.
// Store is safe for concurrent use after Open returns.
type Store struct {
	db     *sql.DB
	encKey []byte
}

// Open opens (or creates) the SQLite database at dbPath and loads (or
// generates) the encryption key from the key file adjacent to it.
// The database schema is applied automatically. The caller must call Close
// when done.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}

	encKey, err := loadOrCreateKey(keyPath(dbPath))
	if err != nil {
		return nil, fmt.Errorf("store: encryption key: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}

	// SQLite works best with a single writer; allow multiple readers.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable foreign keys: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	return &Store{db: db, encKey: encKey}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// MCPServer is a registered MCP server configuration record.
type MCPServer struct {
	// Name is the unique identifier for the server (primary key).
	Name string
	// URL is the HTTP endpoint for Streamable HTTP transport.
	// Mutually exclusive with Command.
	URL string
	// Command is the shell command for stdio transport.
	// Mutually exclusive with URL.
	Command string
	// AuthType is one of "none", "bearer", or "header".
	AuthType string
	// AuthHeader is the header name used when AuthType is "header".
	AuthHeader string
	// AuthToken is the plaintext token (decrypted on read, encrypted on write).
	AuthToken string
	// AddedAt is when the server was registered.
	AddedAt time.Time
}

// AddServer inserts a new MCP server record. AuthToken (if non-empty) is
// encrypted with AES-256-GCM before storage. Returns an error if a server
// with the same name already exists.
func (s *Store) AddServer(ctx context.Context, srv MCPServer) error {
	var tokenEnc *string
	if srv.AuthToken != "" {
		aad := serverAAD(srv.Name)
		enc, err := encrypt(srv.AuthToken, s.encKey, aad)
		if err != nil {
			return fmt.Errorf("store: add server %q: encrypt token: %w", srv.Name, err)
		}
		tokenEnc = &enc
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_servers (name, url, command, auth_type, auth_header, auth_token_enc)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		srv.Name, srv.URL, srv.Command, srv.AuthType, srv.AuthHeader, tokenEnc,
	)
	if err != nil {
		return fmt.Errorf("store: add server %q: %w", srv.Name, err)
	}
	return nil
}

// RemoveServer permanently deletes the server record and its cached tools.
// Returns ErrNotFound if no server with that name exists.
func (s *Store) RemoveServer(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM mcp_servers WHERE name = ?`, name,
	)
	if err != nil {
		return fmt.Errorf("store: remove server %q: %w", name, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: remove server %q: rows affected: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("store: remove server %q: %w", name, ErrNotFound)
	}
	return nil
}

// ListServers returns all registered servers ordered by name. Auth tokens are
// decrypted before returning.
func (s *Store) ListServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, url, command, auth_type, auth_header, auth_token_enc, added_at
		 FROM mcp_servers ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list servers: %w", err)
	}
	defer rows.Close()

	var servers []MCPServer
	for rows.Next() {
		srv, err := s.scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list servers: scan: %w", err)
		}
		servers = append(servers, *srv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list servers: rows: %w", err)
	}
	return servers, nil
}

// GetServer returns the named server. Auth token is decrypted before
// returning. Returns ErrNotFound if no server with that name exists.
func (s *Store) GetServer(ctx context.Context, name string) (*MCPServer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, url, command, auth_type, auth_header, auth_token_enc, added_at
		 FROM mcp_servers WHERE name = ?`, name,
	)
	srv, err := s.scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: get server %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get server %q: %w", name, err)
	}
	return srv, nil
}

// scanServer scans a single row from the mcp_servers table. The scanner can
// be a *sql.Row or *sql.Rows — both satisfy the interface.
func (s *Store) scanServer(scanner interface{ Scan(...any) error }) (*MCPServer, error) {
	var srv MCPServer
	var tokenEnc *string
	var addedAtStr string

	err := scanner.Scan(
		&srv.Name, &srv.URL, &srv.Command,
		&srv.AuthType, &srv.AuthHeader, &tokenEnc, &addedAtStr,
	)
	if err != nil {
		return nil, err
	}

	if tokenEnc != nil && *tokenEnc != "" {
		aad := serverAAD(srv.Name)
		plain, decErr := decrypt(*tokenEnc, s.encKey, aad)
		if decErr != nil {
			return nil, fmt.Errorf("decrypt token for %q: %w", srv.Name, decErr)
		}
		srv.AuthToken = plain
	}

	// Corrupt timestamp defaults to zero time (non-fatal — AddedAt is informational only).
	srv.AddedAt, _ = time.Parse("2006-01-02 15:04:05", addedAtStr)
	return &srv, nil
}

// CacheTools persists the tool list for the named server, replacing any
// existing cached data.
func (s *Store) CacheTools(ctx context.Context, serverName string, tools []protocol.Tool) error {
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("store: cache tools for %q: marshal: %w", serverName, err)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO tool_cache (server_name, tools_json, fetched_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(server_name) DO UPDATE SET
		     tools_json = excluded.tools_json,
		     fetched_at = excluded.fetched_at`,
		serverName, string(toolsJSON),
	)
	if err != nil {
		return fmt.Errorf("store: cache tools for %q: %w", serverName, err)
	}
	return nil
}

// GetCachedTools returns the cached tool list and fetch time for the named
// server. Returns ErrNotFound if no cache entry exists.
func (s *Store) GetCachedTools(ctx context.Context, serverName string) ([]protocol.Tool, time.Time, error) {
	var toolsJSON string
	var fetchedAtStr string

	err := s.db.QueryRowContext(ctx,
		`SELECT tools_json, fetched_at FROM tool_cache WHERE server_name = ?`,
		serverName,
	).Scan(&toolsJSON, &fetchedAtStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, fmt.Errorf("store: get cached tools %q: %w", serverName, ErrNotFound)
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: get cached tools %q: %w", serverName, err)
	}

	var tools []protocol.Tool
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		return nil, time.Time{}, fmt.Errorf("store: get cached tools %q: unmarshal: %w", serverName, err)
	}

	// Corrupt timestamp defaults to zero time (cache always considered stale).
	fetchedAt, _ := time.Parse("2006-01-02 15:04:05", fetchedAtStr)
	return tools, fetchedAt, nil
}

// ClearCache removes the cached tools for the named server. It is not an
// error if no cache entry exists.
func (s *Store) ClearCache(ctx context.Context, serverName string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM tool_cache WHERE server_name = ?`, serverName,
	)
	if err != nil {
		return fmt.Errorf("store: clear cache for %q: %w", serverName, err)
	}
	return nil
}

// SaveOutputSchema stores or updates the inferred output schema for a tool.
func (s *Store) SaveOutputSchema(ctx context.Context, serverName, toolName string, schema json.RawMessage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO output_schemas (server_name, tool_name, schema_json, inferred_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(server_name, tool_name) DO UPDATE SET
		     schema_json = excluded.schema_json,
		     inferred_at = excluded.inferred_at`,
		serverName, toolName, string(schema),
	)
	if err != nil {
		return fmt.Errorf("store: save output schema %q/%q: %w", serverName, toolName, err)
	}
	return nil
}

// GetAllOutputSchemas returns all output schemas for a server.
// Always returns whatever is in the DB (never hides expired schemas).
// The staleTools slice lists tools whose inferred_at is older than maxAge.
// When maxAge is zero, no tools are considered stale.
func (s *Store) GetAllOutputSchemas(ctx context.Context, serverName string, maxAge time.Duration) (schemas map[string]json.RawMessage, staleTools []string, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool_name, schema_json, inferred_at FROM output_schemas WHERE server_name = ?`,
		serverName,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: get output schemas for %q: %w", serverName, err)
	}
	defer rows.Close()

	schemas = make(map[string]json.RawMessage)
	now := time.Now()
	for rows.Next() {
		var toolName, schemaJSON, inferredAtStr string
		if err := rows.Scan(&toolName, &schemaJSON, &inferredAtStr); err != nil {
			return nil, nil, fmt.Errorf("store: get output schemas for %q: scan: %w", serverName, err)
		}
		schemas[toolName] = json.RawMessage(schemaJSON)
		inferredAt, _ := time.Parse("2006-01-02 15:04:05", inferredAtStr)
		if maxAge > 0 && now.Sub(inferredAt) > maxAge {
			staleTools = append(staleTools, toolName)
		}
	}
	return schemas, staleTools, rows.Err()
}

// IsOutputSchemaStale reports whether the stored schema for the named tool is
// missing or older than maxAge. When maxAge is zero it always returns false.
func (s *Store) IsOutputSchemaStale(ctx context.Context, serverName, toolName string, maxAge time.Duration) bool {
	if maxAge == 0 {
		return false
	}
	var inferredAtStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT inferred_at FROM output_schemas WHERE server_name = ? AND tool_name = ?`,
		serverName, toolName,
	).Scan(&inferredAtStr)
	if err != nil {
		return true // missing = stale
	}
	inferredAt, _ := time.Parse("2006-01-02 15:04:05", inferredAtStr)
	return time.Since(inferredAt) > maxAge
}

// serverAAD returns the additional authenticated data bound to a server's
// encrypted token. Binding the AAD to the server name prevents a ciphertext
// from one row being replayed against a different row.
func serverAAD(name string) []byte {
	return []byte("server:" + name)
}

// keyPath derives the encryption key file path from the database path.
// Both files live in the same directory (~/.voidmcp/).
func keyPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "key")
}

// loadOrCreateKey reads the 32-byte AES-256 key from path, or generates and
// writes a new one if the file does not exist. The key file is written with
// mode 0600 (owner read/write only). Returns an error if the existing file has
// group- or world-readable/writable permissions.
func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		// On Unix, reject group/world-readable key files. Windows does not
		// use POSIX permission bits, so the check is skipped there.
		if runtime.GOOS != "windows" {
			info, statErr := os.Stat(path)
			if statErr == nil && info.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("key file %s has unsafe permissions %04o, expected 0600", path, info.Mode().Perm())
			}
		}
		if len(data) != 32 {
			return nil, fmt.Errorf("key file %q has unexpected length %d (want 32)", path, len(data))
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read key file %q: %w", path, err)
	}

	// Generate a fresh 32-byte key.
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}

	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file %q: %w", path, err)
	}
	return key, nil
}

// encrypt encrypts plaintext with AES-256-GCM using the provided key and AAD.
// The output is standard base64-encoded [ nonce | ciphertext+tag ].
func encrypt(plaintext string, key, aad []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("encrypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encrypt: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encrypt: generate nonce: %w", err)
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), aad)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// decrypt decodes a base64 ciphertext produced by encrypt and decrypts it.
func decrypt(ciphertext string, key, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt: decode base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("decrypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt: new GCM: %w", err)
	}
	ns := gcm.NonceSize()
	if len(raw) < ns+gcm.Overhead() {
		return "", fmt.Errorf("decrypt: ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], aad)
	if err != nil {
		return "", fmt.Errorf("decrypt: authentication failed: %w", err)
	}
	return string(plain), nil
}
