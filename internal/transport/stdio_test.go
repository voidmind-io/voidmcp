package transport_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/protocol"
	"github.com/voidmind-io/voidmcp/internal/transport"
)

// TestMain checks whether this test binary is being re-invoked as a mock MCP
// stdio server. If VOIDMCP_TEST_STDIO_SERVER=1 is set, it runs the server
// instead of the test suite.
func TestMain(m *testing.M) {
	if os.Getenv("VOIDMCP_TEST_STDIO_SERVER") == "1" {
		runMockStdioServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMockStdioServer reads newline-delimited JSON-RPC from stdin and writes
// responses to stdout, mimicking a minimal MCP server. It exits when stdin
// is closed.
func runMockStdioServer() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 10<<20), 10<<20)

	for scanner.Scan() {
		line := scanner.Bytes()

		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		// Notifications have no ID — don't respond.
		if req.IsNotification() {
			continue
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "test-mcp", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []protocol.Tool{
					{
						Name:        "echo",
						Description: "Echo input",
						InputSchema: protocol.InputSchema{
							Type: "object",
							Properties: map[string]protocol.Property{
								"msg": {Type: "string"},
							},
						},
					},
				},
			}
		case "tools/call":
			var call struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &call); err != nil {
				result = protocol.ErrorResult("bad params")
			} else {
				result = protocol.TextResult("echoed: " + string(call.Arguments))
			}
		case "ping":
			result = map[string]any{}
		default:
			out, _ := json.Marshal(protocol.Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &protocol.Error{Code: protocol.CodeMethodNotFound, Message: "method not found: " + req.Method},
			})
			fmt.Fprintln(os.Stdout, string(out))
			continue
		}

		out, _ := json.Marshal(protocol.Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		})
		fmt.Fprintln(os.Stdout, string(out))
	}
}

// testStdioCommand returns the command string to launch this test binary as a
// mock MCP server. It uses the "env" command to set VOIDMCP_TEST_STDIO_SERVER=1
// on the child process and -test.run=^$ so no tests actually execute in it.
func testStdioCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// "env" sets env vars for the child without affecting the current process.
	// -test.run=^$ matches no test names so the child runs only TestMain.
	return fmt.Sprintf("env VOIDMCP_TEST_STDIO_SERVER=1 %s -test.run=^$", exe)
}

func TestNewStdio_EmptyCommandReturnsError(t *testing.T) {
	t.Parallel()

	_, err := transport.NewStdio("")
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

func TestNewStdio_BadCommandReturnsError(t *testing.T) {
	t.Parallel()

	_, err := transport.NewStdio("/this/binary/does/not/exist/at/all")
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
}

func TestStdioTransport_ListTools(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := tr.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Errorf("tools[0].Name = %q, want echo", tools[0].Name)
	}
}

func TestStdioTransport_CallTool(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Must do the initialize handshake first.
	if _, err := tr.ListTools(ctx); err != nil {
		t.Fatalf("ListTools (initialize handshake): %v", err)
	}

	result, err := tr.CallTool(ctx, "echo", json.RawMessage(`{"msg":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result == nil {
		t.Fatal("CallTool returned nil result")
	}
	// Result should contain the echoed arguments.
	resultStr := string(result)
	if resultStr == "" {
		t.Error("CallTool returned empty result")
	}
}

func TestStdioTransport_Ping(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Perform initialize handshake before ping.
	if _, err := tr.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if err := tr.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestStdioTransport_Close_Idempotent(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}

	// Close twice must not panic or deadlock.
	tr.Close()
	tr.Close()
}

func TestStdioTransport_Close_KillsChildProcess(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Establish the server session.
	if _, err := tr.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Close should cleanly terminate the child.
	done := make(chan struct{})
	go func() {
		tr.Close()
		close(done)
	}()

	select {
	case <-done:
		// good — Close returned without hanging
	case <-time.After(5 * time.Second):
		t.Error("Close timed out — child process may be a zombie")
	}
}

func TestStdioTransport_ContextCancellation(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}
	defer tr.Close()

	// Cancel before any call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = tr.ListTools(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestStdioTransport_SendAfterClose(t *testing.T) {
	t.Parallel()

	cmd := testStdioCommand(t)
	tr, err := transport.NewStdio(cmd)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}

	tr.Close()

	// Any call after Close should return an error, not hang.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = tr.ListTools(ctx)
	if err == nil {
		t.Fatal("expected error after Close, got nil")
	}
}
