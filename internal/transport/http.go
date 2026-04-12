package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// defaultTimeout is applied to the HTTP client when no explicit timeout is given.
const defaultTimeout = 30 * time.Second

// bodyLimit caps the number of bytes read from any upstream response to prevent
// OOM from a misbehaving server.
const bodyLimit = 10 << 20 // 10 MiB

// ErrSessionExpired is returned by send when the upstream MCP server responds
// with HTTP 404, indicating that the session ID is no longer valid.
var ErrSessionExpired = errors.New("MCP session expired")

// HTTPTransport proxies JSON-RPC requests to a remote MCP server over HTTP
// using the Streamable HTTP transport (MCP spec 2025-03-26).
// It is safe for concurrent use after construction.
type HTTPTransport struct {
	endpoint   string
	authType   string // "none", "bearer", or "header"
	authHeader string // header name when authType is "header"
	authToken  string // plaintext token value

	mu        sync.Mutex
	sessionID string

	client *http.Client
}

// NewHTTP creates an HTTPTransport for the given endpoint.
// authType must be one of "none", "bearer", or "header".
// When authType is "bearer", authToken is sent as a Bearer token.
// When authType is "header", authToken is sent under the authHeader header name.
func NewHTTP(endpoint, authType, authHeader, authToken string) *HTTPTransport {
	return &HTTPTransport{
		endpoint:   endpoint,
		authType:   authType,
		authHeader: authHeader,
		authToken:  authToken,
		client: &http.Client{
			Timeout: defaultTimeout,
			// Never follow redirects — POST bodies must not be silently re-sent
			// to a different URL, and MCP servers should not redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// send issues a single JSON-RPC request to the upstream server.
// It attaches the current session ID (if any) and updates it from the response.
// Returns nil body on HTTP 202 (notification acknowledged).
// Returns ErrSessionExpired on HTTP 404.
func (t *HTTPTransport) send(ctx context.Context, raw []byte) ([]byte, error) {
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	t.applyAuth(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	// Capture and store any session ID the server returns.
	if newSID := resp.Header.Get("Mcp-Session-Id"); newSID != "" {
		t.mu.Lock()
		t.sessionID = newSID
		t.mu.Unlock()
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, ErrSessionExpired
	case http.StatusAccepted:
		// Notification acknowledged — no body expected.
		return nil, nil
	case http.StatusOK:
		// Continue to body processing below.
	default:
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	// If the server responded with SSE, extract the JSON payload from the
	// first data: line. This handles MCP servers that prefer text/event-stream.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return extractSSEData(body), nil
	}

	return body, nil
}

// applyAuth sets the appropriate Authorization or custom header on req.
func (t *HTTPTransport) applyAuth(req *http.Request) {
	switch t.authType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	case "header":
		if t.authHeader != "" {
			req.Header.Set(t.authHeader, t.authToken)
		}
	}
}

// ListTools performs the MCP initialize handshake and then retrieves the
// server's tool list. It stores the session ID for subsequent calls.
func (t *HTTPTransport) ListTools(ctx context.Context) ([]protocol.Tool, error) {
	// Step 1: initialize — establish the session.
	initReq, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "voidmcp", "version": "1.0"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal initialize: %w", err)
	}

	if _, err = t.send(ctx, initReq); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// Step 2: notifications/initialized — fire-and-forget.
	notifyReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	_, _ = t.send(ctx, notifyReq) // notification; ignore error

	// Step 3: tools/list.
	listReq, err := json.Marshal(protocol.Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/list: %w", err)
	}

	body, err := t.send(ctx, listReq)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	if body == nil {
		return nil, nil
	}

	var rpcResp struct {
		Result struct {
			Tools []protocol.Tool `json:"tools"`
		} `json:"result"`
		Error *protocol.Error `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode tools/list response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("tools/list error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result.Tools, nil
}

// CallTool invokes the named tool with the given JSON-encoded arguments.
// It returns the raw JSON of the tools/call result.
func (t *HTTPTransport) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/call: %w", err)
	}

	body, err := t.send(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("tools/call %s: %w", name, err)
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *protocol.Error `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode tools/call response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("tools/call %s error %d: %s", name, rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result, nil
}

// Ping sends a JSON-RPC ping and expects an empty result object in response.
func (t *HTTPTransport) Ping(ctx context.Context) error {
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "ping",
	})
	if err != nil {
		return fmt.Errorf("marshal ping: %w", err)
	}

	body, err := t.send(ctx, raw)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result"`
		Error  *protocol.Error `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return fmt.Errorf("decode ping response: %w", err)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("ping error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return nil
}

// Close releases idle connections held by the underlying HTTP client.
func (t *HTTPTransport) Close() {
	t.client.CloseIdleConnections()
}

// extractSSEData pulls the first data: line from a buffered SSE response body.
func extractSSEData(body []byte) []byte {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data: ")) {
			return bytes.TrimPrefix(line, []byte("data: "))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimPrefix(line, []byte("data:"))
		}
	}
	return body // fallback: return as-is
}
