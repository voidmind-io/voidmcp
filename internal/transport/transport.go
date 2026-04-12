// Package transport provides the Transport interface for communicating with
// remote MCP servers over different protocols (HTTP, stdio).
package transport

import (
	"context"
	"encoding/json"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// Transport abstracts the wire protocol used to communicate with an MCP server.
// Implementations must be safe for concurrent use by multiple goroutines.
type Transport interface {
	// ListTools sends an initialize handshake followed by tools/list and
	// returns the server's advertised tool definitions.
	ListTools(ctx context.Context) ([]protocol.Tool, error)

	// CallTool invokes the named tool with the given JSON-encoded arguments
	// and returns the raw JSON result from the server.
	CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

	// Ping sends a JSON-RPC ping and returns an error if the server does not
	// respond with an empty result object within the transport's timeout.
	Ping(ctx context.Context) error

	// Close releases any resources held by the transport (idle connections,
	// child processes, etc.). It is safe to call Close more than once.
	Close()
}
