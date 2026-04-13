// Package server implements the voidmcp MCP server. It exposes 5 built-in
// tools over JSON-RPC 2.0 and can be served over HTTP or stdio.
package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/protocol"
	"github.com/voidmind-io/voidmcp/internal/registry"
	"github.com/voidmind-io/voidmcp/internal/store"
)

// Version is the server version string, set at build time via -ldflags.
var Version = "dev"

// Config holds tunable parameters for the Server.
type Config struct {
	// SchemaThreshold controls how execute_code describes available tools.
	// -1 = always inline full TypeScript defs.
	//  0 = always use search-first summary mode.
	//  N = inline when total tool count <= N, otherwise summary mode.
	SchemaThreshold int
	// PoolSize is the number of WASM runtimes kept in the executor pool.
	PoolSize int
	// MemoryLimitMB is the per-execution memory limit in megabytes.
	MemoryLimitMB int
	// Timeout is the per-execution wall-clock deadline.
	Timeout time.Duration
	// MaxToolCalls is the maximum number of MCP tool calls allowed per
	// execute_code invocation. Zero means no limit.
	MaxToolCalls int
	// BearerToken, when non-empty, requires all HTTP requests to carry a
	// matching "Authorization: Bearer <token>" header. Requests without a
	// valid token receive 401. Set to empty to disable auth (e.g. behind a
	// trusted reverse proxy or when --no-auth is passed).
	BearerToken string
	// SchemaTTL controls how long an inferred output schema is considered
	// fresh. When a tool's stored schema is older than SchemaTTL the next
	// invocation re-infers and overwrites it. Zero disables re-inference
	// (schemas are inferred once and kept forever). The schema is always
	// shown in TypeScript defs regardless of staleness.
	SchemaTTL time.Duration
}

// Server is the voidmcp MCP server. It is safe for concurrent use.
type Server struct {
	registry *registry.Registry
	executor *executor.Executor
	store    *store.Store
	cfg      Config
}

// New creates a new Server backed by the given Registry, Executor, and Store.
// st may be nil, in which case output schema inference and persistence are
// disabled.
func New(reg *registry.Registry, exec *executor.Executor, st *store.Store, cfg Config) *Server {
	return &Server{
		registry: reg,
		executor: exec,
		store:    st,
		cfg:      cfg,
	}
}

// Handle processes a single JSON-RPC 2.0 message and returns the JSON-encoded
// response. Returns nil for notifications (no response expected by the spec).
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	var req protocol.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return marshalResponse(nil, nil, &protocol.Error{
			Code:    protocol.CodeParseError,
			Message: "parse error",
		})
	}

	if req.JSONRPC != "2.0" {
		return marshalResponse(req.ID, nil, &protocol.Error{
			Code:    protocol.CodeInvalidRequest,
			Message: `jsonrpc must be "2.0"`,
		})
	}

	// Handle notifications BEFORE dispatching — notifications must not
	// execute side effects like adding servers or running code.
	if req.IsNotification() {
		// Only known notification is "notifications/initialized" which is a no-op.
		return nil
	}

	// From here, req is a request that expects a response.
	var result any
	var respErr *protocol.Error

	switch req.Method {
	case "initialize":
		result = s.handleInitialize()
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = s.handleToolsList()
	case "tools/call":
		result, respErr = s.handleToolsCall(ctx, req.Params)
	default:
		respErr = &protocol.Error{
			Code:    protocol.CodeMethodNotFound,
			Message: "method not found: " + req.Method,
		}
	}

	return marshalResponse(req.ID, result, respErr)
}

// ServeHTTP implements http.Handler for Streamable HTTP transport.
// Only POST requests to "/" or "/mcp" are accepted.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.cfg.BearerToken != "" {
		auth := r.Header.Get("Authorization")
		want := "Bearer " + s.cfg.BearerToken
		if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// 512 KB is sufficient for the largest legitimate payload (256 KB code + JSON overhead).
	const maxBody = 512 << 10
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	resp := s.Handle(r.Context(), body)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Session ID is not validated — voidmcp is stateless.
	// Kept for MCP protocol compatibility.
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		sessionID = generateSessionID()
	}
	w.Header().Set("Mcp-Session-Id", sessionID)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// ServeStdio runs the server over stdin/stdout using newline-delimited
// JSON-RPC. It blocks until ctx is cancelled or stdin is closed.
func (s *Server) ServeStdio(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 10<<20), 10<<20)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		resp := s.Handle(ctx, scanner.Bytes())
		if resp != nil {
			_, _ = os.Stdout.Write(resp)
			_, _ = os.Stdout.Write([]byte("\n"))
		}
	}
}

// handleInitialize returns the server identity and capability advertisement.
func (s *Server) handleInitialize() any {
	return map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "voidmcp",
			"version": Version,
		},
	}
}

// handleToolsList returns the list of the 5 built-in tools. The execute_code
// description is dynamically generated based on current registered servers.
func (s *Server) handleToolsList() any {
	return map[string]any{"tools": s.buildToolList()}
}

// buildToolList constructs the 5 built-in tool definitions. execute_code's
// description adapts to the configured SchemaThreshold and current tool count.
func (s *Server) buildToolList() []protocol.Tool {
	allTools := s.registry.AllTools()
	totalCount := s.registry.TotalToolCount()

	// Load inferred output schemas for all servers so TypeScript defs can show
	// concrete return types instead of Promise<any>.
	outputSchemas := make(map[string]map[string]json.RawMessage)
	if s.store != nil {
		for serverName := range allTools {
			schemas, _, _ := s.store.GetAllOutputSchemas(context.Background(), serverName, s.cfg.SchemaTTL)
			if len(schemas) > 0 {
				outputSchemas[serverName] = schemas
			}
		}
	}

	const codeIntro = "Execute JavaScript that chains multiple MCP tool calls in a single turn. " +
		"Use this instead of calling tools individually - pass output from one tool as input to the next. " +
		"All calls return Promises (use await). Tool results are plain objects you can destructure and pass along.\n\n" +
		"Example:\n" +
		"```js\n" +
		"const results = await tools.server1.search({ query: \"...\" });\n" +
		"const detail = await tools.server1.get_item({ id: results[0].id });\n" +
		"await tools.server2.create({ title: detail.name, content: detail.body });\n" +
		"return { created: true, source: detail.name };\n" +
		"```"

	var codeDesc string
	if s.cfg.SchemaThreshold < 0 || (s.cfg.SchemaThreshold > 0 && totalCount <= s.cfg.SchemaThreshold) {
		typeDefs := executor.GenerateTypeDefs(allTools, outputSchemas)
		codeDesc = codeIntro
		if typeDefs != "" {
			codeDesc += "\n\n" + typeDefs
		}
	} else {
		summaries := executor.GenerateServerSummaries(allTools)
		codeDesc = codeIntro + fmt.Sprintf(
			"\n\n%d tools across %d servers. "+
				"Use search(\"your goal\") to find specific tools first.",
			totalCount, len(allTools),
		)
		if summaries != "" {
			codeDesc += "\n\nAvailable servers:\n" + summaries
		}
	}

	return []protocol.Tool{
		s.addMCPTool(),
		s.removeMCPTool(),
		s.listMCPsTool(),
		s.searchTool(),
		{
			Name:        "execute_code",
			Description: codeDesc,
			InputSchema: protocol.InputSchema{
				Type: "object",
				Properties: map[string]protocol.Property{
					"code": {
						Type:        "string",
						Description: "JavaScript code to execute. Use tools.serverName.toolName(args) to call MCP tools.",
					},
				},
				Required: []string{"code"},
			},
		},
	}
}

// addMCPTool returns the tool definition for add_mcp.
func (s *Server) addMCPTool() protocol.Tool {
	return protocol.Tool{
		Name:        "add_mcp",
		Description: "Register a new HTTP MCP server. Discovers its tools and makes them available in execute_code scripts. For local stdio MCP servers, use the CLI: voidmcp add <name> <command>",
		InputSchema: protocol.InputSchema{
			Type: "object",
			Properties: map[string]protocol.Property{
				"name":        {Type: "string", Description: "Alias for this MCP server (e.g. 'github', 'notion')"},
				"url":         {Type: "string", Description: "MCP server endpoint URL"},
				"auth_token":  {Type: "string", Description: "Optional Bearer token for authentication"},
				"auth_header": {Type: "string", Description: "Optional custom auth header name (default: Authorization)"},
			},
			Required: []string{"name", "url"},
		},
	}
}

// removeMCPTool returns the tool definition for remove_mcp.
func (s *Server) removeMCPTool() protocol.Tool {
	return protocol.Tool{
		Name:        "remove_mcp",
		Description: "Unregister an MCP server and remove it from the tool index.",
		InputSchema: protocol.InputSchema{
			Type: "object",
			Properties: map[string]protocol.Property{
				"name": {Type: "string", Description: "Alias of the MCP server to remove"},
			},
			Required: []string{"name"},
		},
	}
}

// listMCPsTool returns the tool definition for list_mcps.
func (s *Server) listMCPsTool() protocol.Tool {
	return protocol.Tool{
		Name:        "list_mcps",
		Description: "List all registered MCP servers and their connection status.",
		InputSchema: protocol.InputSchema{
			Type:       "object",
			Properties: map[string]protocol.Property{},
		},
	}
}

// searchTool returns the tool definition for search.
func (s *Server) searchTool() protocol.Tool {
	return protocol.Tool{
		Name:        "search",
		Description: "Search registered MCP tools by keyword. Returns matching tools with TypeScript definitions.",
		InputSchema: protocol.InputSchema{
			Type: "object",
			Properties: map[string]protocol.Property{
				"query": {Type: "string", Description: "Search terms to match against tool names and descriptions"},
			},
			Required: []string{"query"},
		},
	}
}

// handleToolsCall dispatches a tools/call request to the appropriate built-in
// tool handler.
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *protocol.Error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeInvalidParams, Message: "invalid params: expected {name, arguments}"}
	}

	switch call.Name {
	case "add_mcp":
		return s.handleAddMCP(ctx, call.Arguments), nil
	case "remove_mcp":
		return s.handleRemoveMCP(ctx, call.Arguments), nil
	case "list_mcps":
		return s.handleListMCPs(ctx), nil
	case "search":
		return s.handleSearch(ctx, call.Arguments), nil
	case "execute_code":
		return s.handleExecuteCode(ctx, call.Arguments), nil
	default:
		return nil, &protocol.Error{Code: protocol.CodeInvalidParams, Message: "unknown tool: " + call.Name}
	}
}

// handleAddMCP registers a new HTTP MCP server. stdio MCPs (local commands)
// are only registerable via the CLI to prevent prompt injection attacks from
// triggering arbitrary command execution.
func (s *Server) handleAddMCP(ctx context.Context, raw json.RawMessage) *protocol.ToolResult {
	var args struct {
		Name       string `json:"name"`
		URL        string `json:"url"`
		AuthToken  string `json:"auth_token"`
		AuthHeader string `json:"auth_header"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return protocol.ErrorResult("invalid arguments: " + err.Error())
	}
	if args.Name == "" {
		return protocol.ErrorResult("name is required")
	}
	if args.URL == "" {
		return protocol.ErrorResult("url is required (stdio MCPs can only be added via CLI: voidmcp add <name> <command>)")
	}

	srv := store.MCPServer{
		Name: args.Name,
		URL:  args.URL,
	}
	if args.AuthToken != "" {
		srv.AuthType = "bearer"
		srv.AuthToken = args.AuthToken
		if args.AuthHeader != "" {
			srv.AuthType = "header"
			srv.AuthHeader = args.AuthHeader
		}
	}

	tools, err := s.registry.Add(ctx, srv)
	if err != nil {
		return protocol.ErrorResult(fmt.Sprintf("failed to add server %q: %s", args.Name, err.Error()))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Added %q with %d tools:\n", args.Name, len(tools))
	for _, t := range tools {
		fmt.Fprintf(&sb, "  - %s: %s\n", t.Name, t.Description)
	}
	return protocol.TextResult(strings.TrimRight(sb.String(), "\n"))
}

// handleRemoveMCP unregisters the named MCP server and returns a confirmation.
func (s *Server) handleRemoveMCP(ctx context.Context, raw json.RawMessage) *protocol.ToolResult {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return protocol.ErrorResult("invalid arguments: " + err.Error())
	}
	if args.Name == "" {
		return protocol.ErrorResult("name is required")
	}

	if err := s.registry.Remove(ctx, args.Name); err != nil {
		return protocol.ErrorResult(fmt.Sprintf("failed to remove server %q: %s", args.Name, err.Error()))
	}
	return protocol.TextResult(fmt.Sprintf("Removed server %q.", args.Name))
}

// handleListMCPs returns all registered servers as a JSON text result.
func (s *Server) handleListMCPs(_ context.Context) *protocol.ToolResult {
	servers := s.registry.List()
	type serverSummary struct {
		Name      string `json:"name"`
		URL       string `json:"url,omitempty"`
		Command   string `json:"command,omitempty"`
		Status    string `json:"status"`
		ToolCount int    `json:"tool_count"`
	}
	summaries := make([]serverSummary, len(servers))
	for i, srv := range servers {
		summaries[i] = serverSummary{
			Name:      srv.Name,
			URL:       srv.URL,
			Command:   srv.Command,
			Status:    srv.Status,
			ToolCount: len(srv.Tools),
		}
	}
	out, err := json.MarshalIndent(summaries, "", "  ")
	if err != nil {
		return protocol.ErrorResult("failed to marshal server list: " + err.Error())
	}
	return protocol.TextResult(string(out))
}

// handleSearch searches for tools matching the query and returns matching
// results with TypeScript definitions for each matched tool.
func (s *Server) handleSearch(_ context.Context, raw json.RawMessage) *protocol.ToolResult {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return protocol.ErrorResult("invalid arguments: " + err.Error())
	}
	if args.Query == "" {
		return protocol.ErrorResult("query is required")
	}

	results := s.registry.Search(args.Query, 10)
	if len(results) == 0 {
		return protocol.TextResult(fmt.Sprintf("No tools found matching %q.", args.Query))
	}

	// Build a per-server map of matched tools for GenerateTypeDefs.
	matched := make(map[string][]protocol.Tool)
	for _, r := range results {
		matched[r.ServerName] = append(matched[r.ServerName], r.Tool)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d tool(s) matching %q:\n\n", len(results), args.Query)
	sb.WriteString(executor.GenerateTypeDefs(matched, nil))
	return protocol.TextResult(sb.String())
}

// handleExecuteCode runs the provided JavaScript code in the WASM sandbox,
// injecting all registered MCP tools as callable async functions.
func (s *Server) handleExecuteCode(ctx context.Context, raw json.RawMessage) *protocol.ToolResult {
	var args struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return protocol.ErrorResult("invalid arguments: " + err.Error())
	}
	if args.Code == "" {
		return protocol.ErrorResult("code is required")
	}

	params := executor.ExecuteParams{
		Code:         args.Code,
		ServerTools:  s.registry.AllTools(),
		CallTool:     s.registry.CallTool,
		MaxToolCalls: s.cfg.MaxToolCalls,
	}

	// Wire schema capture hook when a store is available. The hook runs in its
	// own goroutine and never blocks the JS event loop. It only re-infers when
	// the stored schema is missing or older than SchemaTTL.
	if s.store != nil {
		schemaTTL := s.cfg.SchemaTTL
		st := s.store
		params.OnToolResult = func(serverName, toolName string, result json.RawMessage) {
			if st.IsOutputSchemaStale(context.Background(), serverName, toolName, schemaTTL) {
				schema := executor.InferSchema(result)
				if schema != nil {
					_ = st.SaveOutputSchema(context.Background(), serverName, toolName, schema)
				}
			}
		}
	}

	result, err := s.executor.Execute(ctx, params)
	if err != nil {
		return protocol.ErrorResult("executor error: " + err.Error())
	}

	var sb strings.Builder
	if result.Error != "" {
		fmt.Fprintf(&sb, "Error: %s\n", result.Error)
	}
	if result.Result != nil {
		fmt.Fprintf(&sb, "Result: %s\n", string(result.Result))
	}
	if len(result.Logs) > 0 {
		sb.WriteString("\nLogs:\n")
		for _, l := range result.Logs {
			fmt.Fprintf(&sb, "  [%s] %s\n", l.Level, l.Message)
		}
	}
	if len(result.ToolCalls) > 0 {
		fmt.Fprintf(&sb, "\nTool calls: %d (%.0fms total)\n", len(result.ToolCalls), float64(result.DurationMS))
		for _, tc := range result.ToolCalls {
			fmt.Fprintf(&sb, "  - %s/%s: %s (%dms)\n", tc.Server, tc.Tool, tc.Status, tc.DurationMS)
		}
	}
	fmt.Fprintf(&sb, "\nDuration: %dms", result.DurationMS)

	text := strings.TrimSpace(sb.String())
	if result.Error != "" {
		return protocol.ErrorResult(text)
	}
	return protocol.TextResult(text)
}

// marshalResponse encodes a JSON-RPC 2.0 response. If respErr is non-nil it
// takes precedence over result.
func marshalResponse(id json.RawMessage, result any, respErr *protocol.Error) []byte {
	var resp protocol.Response
	resp.JSONRPC = "2.0"
	resp.ID = id
	if respErr != nil {
		resp.Error = respErr
	} else {
		resp.Result = result
	}
	out, _ := json.Marshal(resp)
	return out
}

// generateSessionID returns a random 16-byte hex string for use as an
// Mcp-Session-Id header value.
func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateToken generates a cryptographically random 32-byte bearer token
// returned as a hex string. It is called at startup when HTTP mode is used
// without --no-auth.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
