// Package registry maintains an in-memory index of registered MCP servers,
// their transports, and their cached tool lists. It is backed by a persistent
// SQLite store for durability across restarts.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/voidmind-io/voidmcp/internal/protocol"
	"github.com/voidmind-io/voidmcp/internal/store"
	"github.com/voidmind-io/voidmcp/internal/transport"
)

// ServerInfo is a snapshot of a registered server and its current status.
type ServerInfo struct {
	// Name is the unique server identifier.
	Name string
	// URL is the HTTP endpoint (non-empty for HTTP transport).
	URL string
	// Command is the shell command (non-empty for stdio transport).
	Command string
	// Status is "connected", "error", or "unknown".
	Status string
	// Tools is the most recently discovered tool list for this server.
	Tools []protocol.Tool
}

// SearchResult holds a single tool match returned by Registry.Search.
type SearchResult struct {
	// ServerName is the name of the server that owns the tool.
	ServerName string
	// Tool is the matching tool.
	Tool protocol.Tool
	// Score is the relevance score (higher is more relevant).
	Score int
}

// serverMeta holds the display metadata for a registered server alongside
// its transport. The transport interface does not expose endpoint details, so
// they are stored separately at registration time.
type serverMeta struct {
	transport transport.Transport
	url       string
	command   string
}

// Registry manages the in-memory index of MCP servers and their tools.
// All exported methods are safe for concurrent use.
type Registry struct {
	store       *store.Store
	servers     map[string]serverMeta
	tools       map[string][]protocol.Tool
	statuses    map[string]string
	mu          sync.RWMutex
	cacheMaxAge time.Duration
}

// New creates a Registry backed by st. cacheMaxAge controls how long a cached
// tool list is considered fresh before a reload is triggered on Load.
func New(st *store.Store, cacheMaxAge time.Duration) *Registry {
	return &Registry{
		store:       st,
		servers:     make(map[string]serverMeta),
		tools:       make(map[string][]protocol.Tool),
		statuses:    make(map[string]string),
		cacheMaxAge: cacheMaxAge,
	}
}

// Load reads all servers from the store and reconnects their transports.
// Cached tool lists are used if they are fresher than cacheMaxAge; otherwise
// tools are re-fetched from the live server.
func (r *Registry) Load(ctx context.Context) error {
	servers, err := r.store.ListServers(ctx)
	if err != nil {
		return fmt.Errorf("registry: load servers: %w", err)
	}

	for _, srv := range servers {
		t, err := newTransport(srv)
		if err != nil {
			// Record the error but continue loading other servers.
			r.mu.Lock()
			r.statuses[srv.Name] = "error"
			r.servers[srv.Name] = serverMeta{url: srv.URL, command: srv.Command}
			r.mu.Unlock()
			continue
		}

		meta := serverMeta{transport: t, url: srv.URL, command: srv.Command}

		tools, fetchedAt, cacheErr := r.store.GetCachedTools(ctx, srv.Name)
		if cacheErr == nil && time.Since(fetchedAt) < r.cacheMaxAge {
			// Cache is fresh — use it without hitting the live server.
			r.mu.Lock()
			r.servers[srv.Name] = meta
			r.tools[srv.Name] = tools
			r.statuses[srv.Name] = "connected"
			r.mu.Unlock()
			continue
		}

		// Cache is stale or missing — fetch live.
		liveTools, listErr := t.ListTools(ctx)
		if listErr != nil {
			// Transport alive but tool fetch failed; use stale cache if any.
			r.mu.Lock()
			r.servers[srv.Name] = meta
			if cacheErr == nil {
				r.tools[srv.Name] = tools
			}
			r.statuses[srv.Name] = "error"
			r.mu.Unlock()
			continue
		}

		// Persist refreshed cache asynchronously — failure is non-fatal.
		_ = r.store.CacheTools(ctx, srv.Name, liveTools)

		r.mu.Lock()
		r.servers[srv.Name] = meta
		r.tools[srv.Name] = liveTools
		r.statuses[srv.Name] = "connected"
		r.mu.Unlock()
	}

	return nil
}

// Add registers a new MCP server: it creates the transport, discovers tools,
// persists both to the store, and registers them in the in-memory index.
// Returns the discovered tool list on success.
func (r *Registry) Add(ctx context.Context, srv store.MCPServer) ([]protocol.Tool, error) {
	t, err := newTransport(srv)
	if err != nil {
		return nil, fmt.Errorf("registry: add %q: create transport: %w", srv.Name, err)
	}

	tools, err := t.ListTools(ctx)
	if err != nil {
		t.Close()
		return nil, fmt.Errorf("registry: add %q: list tools: %w", srv.Name, err)
	}

	if err := r.store.AddServer(ctx, srv); err != nil {
		t.Close()
		return nil, fmt.Errorf("registry: add %q: persist server: %w", srv.Name, err)
	}

	if err := r.store.CacheTools(ctx, srv.Name, tools); err != nil {
		// Cache failure is not fatal — in-memory index is authoritative.
		_ = err
	}

	r.mu.Lock()
	r.servers[srv.Name] = serverMeta{transport: t, url: srv.URL, command: srv.Command}
	r.tools[srv.Name] = tools
	r.statuses[srv.Name] = "connected"
	r.mu.Unlock()

	return tools, nil
}

// Remove unregisters a server: closes its transport, removes it from the
// in-memory index, and deletes it (and its cached tools) from the store.
// Returns an error if the server is not registered.
func (r *Registry) Remove(ctx context.Context, name string) error {
	r.mu.Lock()
	meta, ok := r.servers[name]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("registry: remove %q: %w", name, store.ErrNotFound)
	}
	delete(r.servers, name)
	delete(r.tools, name)
	delete(r.statuses, name)
	r.mu.Unlock()

	if meta.transport != nil {
		meta.transport.Close()
	}

	if err := r.store.RemoveServer(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("registry: remove %q: store: %w", name, err)
	}
	return nil
}

// List returns a snapshot of all registered servers and their current status.
func (r *Registry) List() []ServerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ServerInfo, 0, len(r.servers))
	for name, meta := range r.servers {
		tools := r.tools[name]
		if tools == nil {
			tools = []protocol.Tool{}
		}
		status := r.statuses[name]
		if status == "" {
			status = "unknown"
		}
		out = append(out, ServerInfo{
			Name:    name,
			URL:     meta.url,
			Command: meta.command,
			Status:  status,
			Tools:   tools,
		})
	}
	return out
}

// AllTools returns a snapshot of the complete server-to-tools map.
func (r *Registry) AllTools() map[string][]protocol.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string][]protocol.Tool, len(r.tools))
	for name, tools := range r.tools {
		cp := make([]protocol.Tool, len(tools))
		copy(cp, tools)
		out[name] = cp
	}
	return out
}

// TotalToolCount returns the total number of tools across all registered servers.
func (r *Registry) TotalToolCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := 0
	for _, tools := range r.tools {
		n += len(tools)
	}
	return n
}

// Search performs a keyword search across all registered tools. Matching is
// case-insensitive and operates on tool name and description. Results are
// scored and sorted by relevance; at most limit results are returned.
//
// Score tiers:
//   - 100: exact name match
//   - 90:  name has query as prefix
//   - 80:  name contains query
//   - 70:  server name contains query (all tools from that server)
//   - 50:  description contains query / browse-all mode
func (r *Registry) Search(query string, limit int) []SearchResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	var results []SearchResult

	// Empty query or "*" returns all tools (browse mode).
	browseAll := q == "" || q == "*"

	for server, tools := range r.tools {
		serverLower := strings.ToLower(server)
		// If query matches a server name, return ALL tools from that server.
		serverMatch := !browseAll && (serverLower == q || strings.Contains(serverLower, q))

		for _, t := range tools {
			nameLower := strings.ToLower(t.Name)
			descLower := strings.ToLower(t.Description)

			score := 0
			switch {
			case browseAll:
				score = 50 // return everything
			case nameLower == q:
				score = 100
			case strings.HasPrefix(nameLower, q):
				score = 90
			case strings.Contains(nameLower, q):
				score = 80
			case serverMatch:
				score = 70 // server name match — return all tools from this server
			case strings.Contains(descLower, q):
				score = 50
			}

			if score > 0 {
				results = append(results, SearchResult{
					ServerName: server,
					Tool:       t,
					Score:      score,
				})
			}
		}
	}

	// Sort by score descending, then by server+tool name for stability.
	sortSearchResults(results)

	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results
}

// CallTool routes a tool call to the named server. It verifies that the tool
// exists in the cached tool list before forwarding the call to the transport.
func (r *Registry) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
	r.mu.RLock()
	meta, ok := r.servers[serverName]
	tools := r.tools[serverName]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("registry: call tool: unknown server %q", serverName)
	}
	if meta.transport == nil {
		return nil, fmt.Errorf("registry: call tool: server %q has no active transport", serverName)
	}

	if !toolExists(tools, toolName) {
		return nil, fmt.Errorf("registry: call tool: unknown tool %q on server %q", toolName, serverName)
	}

	result, err := meta.transport.CallTool(ctx, toolName, args)
	if err != nil {
		return nil, fmt.Errorf("registry: call tool %q/%q: %w", serverName, toolName, err)
	}
	return result, nil
}

// Watch periodically polls the store for server add/remove and applies the
// diff to the in-memory registry. onChange is invoked (from the watcher
// goroutine) after any tick that produced a non-empty diff, so callers can
// broadcast notifications to clients. Returns when ctx is cancelled.
//
// If interval is <= 0, Watch returns immediately without starting a ticker.
func (r *Registry) Watch(ctx context.Context, interval time.Duration, onChange func()) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tickOnce(ctx, onChange)
		}
	}
}

// tickOnce performs a single poll cycle: fetches DB names, diffs against the
// in-memory registry, connects added servers, and removes dropped ones.
func (r *Registry) tickOnce(ctx context.Context, onChange func()) {
	changed := false
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("registry watch: panic: %v", rec)
		}
		if changed && onChange != nil {
			onChange()
		}
	}()

	dbNames, err := r.store.ListServerNames(ctx)
	if err != nil {
		log.Printf("registry watch: list server names: %v", err)
		return
	}

	dbSet := make(map[string]struct{}, len(dbNames))
	for _, n := range dbNames {
		dbSet[n] = struct{}{}
	}

	r.mu.RLock()
	memNames := make(map[string]struct{}, len(r.servers))
	for n := range r.servers {
		memNames[n] = struct{}{}
	}
	r.mu.RUnlock()

	var added, removed []string
	for n := range dbSet {
		if _, ok := memNames[n]; !ok {
			added = append(added, n)
		}
	}
	for n := range memNames {
		if _, ok := dbSet[n]; !ok {
			removed = append(removed, n)
		}
	}

	if ctx.Err() != nil {
		return
	}

	for _, name := range added {
		if ctx.Err() != nil {
			return
		}
		srv, getErr := r.store.GetServer(ctx, name)
		if getErr != nil {
			log.Printf("registry watch: get server %q: %v", name, getErr)
			continue
		}
		t, transErr := newTransport(*srv)
		if transErr != nil {
			// Do not stub into r.servers — leaving the name absent lets the
			// next tick retry automatically (self-healing after transient
			// network/process errors).
			log.Printf("registry watch: new transport for %q: %v", name, transErr)
			continue
		}
		tools, listErr := t.ListTools(ctx)
		if listErr != nil {
			// Same self-healing rationale as above: discard the failed attempt
			// so the next tick re-attempts ListTools.
			log.Printf("registry watch: list tools for %q: %v", name, listErr)
			t.Close()
			continue
		}
		_ = r.store.CacheTools(ctx, name, tools)
		r.mu.Lock()
		// TOCTOU guard: if Add() raced us and already installed a transport,
		// back off and let Add's entry win to avoid leaking the transport we
		// just created.
		if _, exists := r.servers[name]; exists {
			r.mu.Unlock()
			t.Close()
			continue
		}
		r.servers[name] = serverMeta{transport: t, url: srv.URL, command: srv.Command}
		r.tools[name] = tools
		r.statuses[name] = "connected"
		r.mu.Unlock()
		changed = true
	}

	if ctx.Err() != nil {
		return
	}

	for _, name := range removed {
		if ctx.Err() != nil {
			return
		}
		r.mu.Lock()
		meta, ok := r.servers[name]
		if ok {
			delete(r.servers, name)
			delete(r.tools, name)
			delete(r.statuses, name)
		}
		r.mu.Unlock()
		if ok && meta.transport != nil {
			meta.transport.Close()
		}
		if ok {
			changed = true
		}
	}
}

// Close closes all transport connections held by the registry.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, meta := range r.servers {
		if meta.transport != nil {
			meta.transport.Close()
		}
	}
}

// newTransport creates the appropriate Transport implementation based on the
// server configuration. URL takes precedence over Command.
func newTransport(srv store.MCPServer) (transport.Transport, error) {
	if srv.URL != "" {
		return transport.NewHTTP(srv.URL, srv.AuthType, srv.AuthHeader, srv.AuthToken), nil
	}
	if srv.Command != "" {
		return transport.NewStdio(srv.Command)
	}
	return nil, fmt.Errorf("server %q has neither URL nor Command", srv.Name)
}

// toolExists reports whether a tool with the given name exists in the list.
func toolExists(tools []protocol.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// sortSearchResults sorts results by score descending, then by server name
// and tool name ascending for deterministic ordering within the same score.
func sortSearchResults(results []SearchResult) {
	n := len(results)
	for i := 1; i < n; i++ {
		for j := i; j > 0; j-- {
			a, b := results[j-1], results[j]
			less := b.Score > a.Score ||
				(b.Score == a.Score && (b.ServerName < a.ServerName ||
					(b.ServerName == a.ServerName && b.Tool.Name < a.Tool.Name)))
			if !less {
				break
			}
			results[j-1], results[j] = results[j], results[j-1]
		}
	}
}
