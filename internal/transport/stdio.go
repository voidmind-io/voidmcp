package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// stdioTimeout is the maximum time to wait for a response to a single request.
const stdioTimeout = 30 * time.Second

// StdioTransport communicates with a local MCP server process via stdin/stdout
// using newline-delimited JSON-RPC 2.0. It is safe for concurrent use.
type StdioTransport struct {
	command string
	args    []string

	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu     sync.Mutex // guards stdin writes
	idSeq  atomic.Int64
	done   chan struct{}

	pendingMu sync.Mutex
	pending   map[int64]chan []byte
}

// NewStdio starts the given command (split on spaces; no shell expansion) as a
// child process and wires up stdin/stdout for JSON-RPC communication. The
// reader goroutine is started immediately and runs until Close is called or
// the child process exits.
func NewStdio(command string) (*StdioTransport, error) {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return nil, fmt.Errorf("NewStdio: empty command")
	}

	cmd := exec.Command(parts[0], parts[1:]...)

	// Restrict the child's environment to essential variables only.
	// This prevents leaking secrets stored in the parent process environment.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"LANG=" + os.Getenv("LANG"),
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("NewStdio: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("NewStdio: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("NewStdio: start process: %w", err)
	}

	t := &StdioTransport{
		command: parts[0],
		args:    parts[1:],
		cmd:     cmd,
		stdin:   stdinPipe,
		done:    make(chan struct{}),
		pending: make(map[int64]chan []byte),
	}

	go t.readLoop(stdoutPipe)

	return t, nil
}

// readLoop reads newline-delimited JSON from the child process stdout and
// dispatches each response to the waiting caller via the pending map.
// Notifications (responses without an ID) are silently ignored.
// When the scanner exits (EOF or error), done is closed so blocked senders unblock.
func (t *StdioTransport) readLoop(r io.Reader) {
	defer func() {
		select {
		case <-t.done:
			// already closed by Close()
		default:
			close(t.done)
		}
	}()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 10<<20), 10<<20) // 10 MiB

	for scanner.Scan() {
		line := scanner.Bytes()

		// Quick peek to see if there is an ID worth dispatching.
		var peek struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &peek); err != nil {
			continue
		}
		if len(peek.ID) == 0 || string(peek.ID) == "null" {
			// Notification — no caller is waiting.
			continue
		}

		id, err := strconv.ParseInt(string(peek.ID), 10, 64)
		if err != nil {
			continue
		}

		// Make a copy: scanner reuses the underlying buffer.
		buf := make([]byte, len(line))
		copy(buf, line)

		t.pendingMu.Lock()
		ch, ok := t.pending[id]
		t.pendingMu.Unlock()

		if ok {
			select {
			case ch <- buf:
			default:
				// Channel full or closed — drop.
			}
		}
	}
}

// send assigns a unique ID to req, writes it to the child's stdin, and waits
// for the matching response. It returns the raw response bytes.
func (t *StdioTransport) send(ctx context.Context, req protocol.Request) ([]byte, error) {
	id := t.idSeq.Add(1)
	req.ID = json.RawMessage(strconv.FormatInt(id, 10))

	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ch := make(chan []byte, 1)

	t.pendingMu.Lock()
	t.pending[id] = ch
	t.pendingMu.Unlock()

	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
	}()

	t.mu.Lock()
	_, writeErr := fmt.Fprintf(t.stdin, "%s\n", raw)
	t.mu.Unlock()
	if writeErr != nil {
		return nil, fmt.Errorf("write request: %w", writeErr)
	}

	timeout := time.NewTimer(stdioTimeout)
	defer timeout.Stop()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		return nil, fmt.Errorf("transport closed")
	case <-timeout.C:
		return nil, fmt.Errorf("timeout waiting for response to method %q (id %d)", req.Method, id)
	case buf := <-ch:
		return buf, nil
	}
}

// sendNotification writes a JSON-RPC notification (no ID, no response expected)
// to the child's stdin.
func (t *StdioTransport) sendNotification(method string) {
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	})
	t.mu.Lock()
	fmt.Fprintf(t.stdin, "%s\n", raw) //nolint:errcheck — fire-and-forget
	t.mu.Unlock()
}

// ListTools performs the MCP initialize handshake and then retrieves the
// server's tool list.
func (t *StdioTransport) ListTools(ctx context.Context) ([]protocol.Tool, error) {
	initParams, err := json.Marshal(map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "voidmcp", "version": "1.0"},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal initialize params: %w", err)
	}

	_, err = t.send(ctx, protocol.Request{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params:  initParams,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}

	// Notify server that initialization is complete (fire-and-forget).
	t.sendNotification("notifications/initialized")

	listBody, err := t.send(ctx, protocol.Request{
		JSONRPC: "2.0",
		Method:  "tools/list",
	})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}

	var rpcResp struct {
		Result struct {
			Tools []protocol.Tool `json:"tools"`
		} `json:"result"`
		Error *protocol.Error `json:"error"`
	}
	if err := json.Unmarshal(listBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode tools/list response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("tools/list error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result.Tools, nil
}

// CallTool invokes the named tool with the given JSON-encoded arguments.
// It returns the raw JSON of the tools/call result.
func (t *StdioTransport) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	params, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal tools/call params: %w", err)
	}

	body, err := t.send(ctx, protocol.Request{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  params,
	})
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
func (t *StdioTransport) Ping(ctx context.Context) error {
	body, err := t.send(ctx, protocol.Request{
		JSONRPC: "2.0",
		Method:  "ping",
	})
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

// Close shuts down the transport. It closes stdin so the child process sees
// EOF, then waits up to 5 seconds for a clean exit. If the child has not
// exited within 5 seconds it is sent SIGTERM, with an additional 2-second
// grace period before SIGKILL is used.
func (t *StdioTransport) Close() {
	select {
	case <-t.done:
		return // already closed
	default:
		close(t.done)
	}

	t.stdin.Close()

	done := make(chan error, 1)
	go func() { done <- t.cmd.Wait() }()

	select {
	case <-done:
		// exited cleanly
	case <-time.After(5 * time.Second):
		t.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck — best effort
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.cmd.Process.Kill() //nolint:errcheck — best effort
			<-done
		}
	}
}
