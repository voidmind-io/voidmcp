package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fastschema/qjs"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// maxCodeBytes is the maximum allowed size of user-supplied JavaScript source.
// Submissions larger than this are rejected before touching the runtime pool.
const maxCodeBytes = 256 * 1024

// sandboxPreamble removes dangerous global objects exposed by QuickJS's std
// and os modules, replaces console with a log capture buffer, and deletes
// timer APIs before user code executes. Console output is captured into
// __logs and returned in the execution result for debugging.
const sandboxPreamble = "delete globalThis.std;" +
	"delete globalThis.os;" +
	"delete globalThis.bjson;" +
	"delete globalThis.setTimeout;" +
	"delete globalThis.setInterval;" +
	"delete globalThis.clearTimeout;" +
	"delete globalThis.clearInterval;" +
	"delete globalThis.print;" +
	"globalThis.__logs = [];" +
	"globalThis.console = {" +
	"log: (...a) => __logs.push({level:'log',msg:a.map(String).join(' ')})," +
	"warn: (...a) => __logs.push({level:'warn',msg:a.map(String).join(' ')})," +
	"error: (...a) => __logs.push({level:'error',msg:a.map(String).join(' ')})," +
	"info: (...a) => __logs.push({level:'info',msg:a.map(String).join(' ')})," +
	"debug: (...a) => __logs.push({level:'debug',msg:a.map(String).join(' ')})" +
	"};\n"

// ToolCaller executes a single tool call against an MCP server identified by
// serverName. toolName is the tool to invoke and args is the JSON-encoded
// argument object. The raw JSON result from the server is returned.
type ToolCaller func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error)

// ExecuteParams holds the inputs for a single script execution.
type ExecuteParams struct {
	// Code is the JavaScript source to execute.
	Code string
	// ServerTools maps server name to the tools available from that server.
	// Only listed tools are accessible inside the sandbox.
	ServerTools map[string][]protocol.Tool
	// CallTool routes individual tool calls to the named server.
	CallTool ToolCaller
	// MaxToolCalls limits the total number of tool calls allowed per execution.
	// Zero means no limit.
	MaxToolCalls int
	// OnToolResult is an optional hook called after each successful tool call
	// with the unwrapped result. It is invoked in a separate goroutine and must
	// not block. serverName and toolName identify the tool; result is the
	// unwrapped JSON payload.
	OnToolResult func(serverName, toolName string, result json.RawMessage)
}

// ExecuteResult holds the output of a script execution.
type ExecuteResult struct {
	// Result is the return value of the top-level async function, JSON-encoded.
	Result json.RawMessage `json:"result,omitempty"`
	// Logs captures console.log/warn/error/info/debug output.
	Logs []LogEntry `json:"logs"`
	// ToolCalls records every tool invocation made during execution.
	ToolCalls []ToolCallLog `json:"tool_calls"`
	// DurationMS is the total wall-clock execution time in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Error is non-empty when the script failed (syntax error, timeout,
	// OOM, exceeded tool call limit, or unhandled exception).
	Error string `json:"error,omitempty"`
}

// LogEntry is a single console output line captured from an executing script.
type LogEntry struct {
	// Level is the console method called: log, warn, error, info, or debug.
	Level string `json:"level"`
	// Message is the concatenated string representation of all arguments.
	Message string `json:"msg"`
}

// ToolCallLog records a single tool invocation made from within a script.
type ToolCallLog struct {
	// Server is the MCP server name.
	Server string `json:"server"`
	// Tool is the tool name registered on the upstream server.
	Tool string `json:"tool"`
	// DurationMS is the tool call round-trip time in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Status is "success" or "error".
	Status string `json:"status"`
}

// Executor runs JavaScript in sandboxed QuickJS WASM runtimes drawn from Pool.
type Executor struct {
	pool *Pool
}

// New creates an Executor backed by the given runtime pool.
func New(pool *Pool) *Executor {
	return &Executor{pool: pool}
}

// Execute runs JavaScript code in a sandboxed QJS runtime. MCP tools are
// injected as async functions accessible via the tools.<server>.<tool>(args)
// API. The runtime is always discarded after execution to prevent cross-session
// JS global state leakage.
func (e *Executor) Execute(ctx context.Context, params ExecuteParams) (res *ExecuteResult, retErr error) {
	if len(params.Code) > maxCodeBytes {
		return nil, fmt.Errorf("executor: code size %d exceeds limit of %d bytes", len(params.Code), maxCodeBytes)
	}

	rt, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("executor: acquire runtime: %w", err)
	}
	defer e.pool.Release(rt)

	start := time.Now()

	result := &ExecuteResult{
		Logs:      []LogEntry{},
		ToolCalls: []ToolCallLog{},
	}

	var (
		logMu    sync.Mutex
		toolLogs []ToolCallLog
	)

	qctx := rt.Context()

	var toolCallCount atomic.Int64

	// Build a dispatch map keyed by "serverName/toolName". Entries are built
	// up-front so the async callback only needs a map lookup at call time.
	type dispatchEntry struct {
		call   func(json.RawMessage) (json.RawMessage, error)
		server string
		tool   string
	}
	dispatchMap := make(map[string]dispatchEntry)
	for server, tools := range params.ServerTools {
		for _, t := range tools {
			key := server + "/" + t.Name
			capturedServer, capturedTool := server, t.Name
			dispatchMap[key] = dispatchEntry{
				call: func(args json.RawMessage) (json.RawMessage, error) {
					if params.CallTool != nil {
						return params.CallTool(ctx, capturedServer, capturedTool, args)
					}
					return nil, errors.New("no tool caller configured")
				},
				server: capturedServer,
				tool:   capturedTool,
			}
		}
	}

	// Use a randomized bridge name per execution to prevent user code from
	// directly calling the Go bridge by guessing its name.
	bridgeName := fmt.Sprintf("__bridge_%x", rand.Uint64())

	// Register a single async bridge function. The JS Proxy preamble calls
	// bridgeName(serverName, toolName, argsJSON) for every tool invocation.
	qctx.SetAsyncFunc(bridgeName, func(this *qjs.This) {
		if params.MaxToolCalls > 0 && toolCallCount.Add(1) > int64(params.MaxToolCalls) {
			this.Promise().Reject(qctx.ThrowError(
				fmt.Errorf("tool call limit exceeded (max %d)", params.MaxToolCalls),
			))
			return
		}

		args := this.Args()
		if len(args) < 3 {
			for _, a := range args {
				a.Free()
			}
			this.Promise().Reject(qctx.ThrowError(fmt.Errorf("__callTool requires 3 arguments")))
			return
		}
		serverName := args[0].String()
		toolName := args[1].String()
		rawArgs := json.RawMessage(args[2].String())
		for _, a := range args {
			a.Free()
		}
		if len(rawArgs) == 0 {
			rawArgs = json.RawMessage("{}")
		}

		key := serverName + "/" + toolName
		entry, ok := dispatchMap[key]
		if !ok {
			logMu.Lock()
			toolLogs = append(toolLogs, ToolCallLog{Server: serverName, Tool: toolName, Status: "error"})
			logMu.Unlock()
			this.Promise().Reject(qctx.ThrowError(fmt.Errorf("unknown tool: %s/%s", serverName, toolName)))
			return
		}

		callStart := time.Now()
		callResult, callErr := entry.call(rawArgs)
		durationMS := time.Since(callStart).Milliseconds()

		status := "success"
		if callErr != nil {
			status = "error"
		}

		logMu.Lock()
		toolLogs = append(toolLogs, ToolCallLog{
			Server:     entry.server,
			Tool:       entry.tool,
			DurationMS: durationMS,
			Status:     status,
		})
		logMu.Unlock()

		if callErr != nil {
			this.Promise().Reject(qctx.ThrowError(callErr))
			return
		}

		// Unwrap MCP ToolResult wrapper before passing result to JS.
		unwrapped := unwrapToolResult(callResult)

		// Fire schema capture hook asynchronously so it never blocks the JS event loop.
		// Cap captured response size to prevent OOM from huge MCP responses.
		const maxCaptureSize = 1 << 20 // 1 MB
		if params.OnToolResult != nil && len(unwrapped) <= maxCaptureSize {
			captured := make(json.RawMessage, len(unwrapped))
			copy(captured, unwrapped)
			go params.OnToolResult(entry.server, entry.tool, captured)
		}

		this.Promise().Resolve(qctx.ParseJSON(string(unwrapped)))
	})

	proxyPreamble := buildProxyPreamble(params.ServerTools, bridgeName)
	// The trampoline wraps user code in an inner async IIFE so that return
	// statements are valid JS. The outer await (enabled by FlagAsync) drives the
	// event loop to completion before rt.Eval returns. The resolved value of the
	// outer await is always undefined — the actual result and any script error are
	// written to well-known globals (__scriptResult, __scriptError) which are read
	// back via separate synchronous Eval calls. This avoids calling Value.Await()
	// directly, which can corrupt the WASM runtime state after async Go-bridged
	// tool calls have already drained the promise queue.
	fullCode := sandboxPreamble + proxyPreamble +
		"\nglobalThis.__scriptResult = undefined;" +
		"\nglobalThis.__scriptError = null;" +
		"\nawait (async function() {\n" +
		"  try {\n" +
		"    globalThis.__scriptResult = await (async function() {\n" +
		params.Code + "\n" +
		"    })();\n" +
		"  } catch(e) {\n" +
		"    globalThis.__scriptError = (e instanceof Error) ? (e.name + ': ' + e.message) : String(e);\n" +
		"  }\n" +
		"})();"

	// Recover from any panic inside Eval (e.g. WASM OOM) so the caller gets
	// a clean error rather than a crashed process.
	defer func() {
		if r := recover(); r != nil {
			result.DurationMS = time.Since(start).Milliseconds()
			result.ToolCalls = snapshotToolLogs(&logMu, toolLogs)
			result.Error = fmt.Sprintf("runtime panic: %v", r)
			res = result
			retErr = nil
		}
	}()

	evalResult, evalErr := rt.Eval("script.js", qjs.Code(fullCode), qjs.FlagAsync())

	result.DurationMS = time.Since(start).Milliseconds()
	result.ToolCalls = snapshotToolLogs(&logMu, toolLogs)
	result.Logs = extractConsoleLogs(rt)

	// evalResult is the resolved value of the outer await expression, which is
	// always undefined. Free it unconditionally; the actual result is in globals.
	if evalResult != nil {
		evalResult.Free()
	}

	if evalErr != nil {
		result.Error = evalErr.Error()
		return result, nil
	}

	// Read the script error first. If __scriptError is non-null the inner IIFE
	// threw; surface the error string and skip result extraction.
	errVal, errReadErr := rt.Eval("__script_error.js",
		qjs.Code("globalThis.__scriptError"),
	)
	if errReadErr == nil && errVal != nil {
		if !errVal.IsNull() && !errVal.IsUndefined() {
			result.Error = errVal.String()
		}
		errVal.Free()
	}

	if result.Error != "" {
		return result, nil
	}

	// Read __scriptResult and serialise it. undefined means the user code did
	// not return a value; leave result.Result nil in that case.
	resVal, resReadErr := rt.Eval("__script_result.js",
		qjs.Code("JSON.stringify(globalThis.__scriptResult)"),
	)
	if resReadErr != nil || resVal == nil {
		return result, nil
	}
	defer resVal.Free()

	if resVal.IsUndefined() || resVal.IsNull() {
		return result, nil
	}

	// JSON.stringify returns a JS string containing the JSON representation.
	// The string value itself is valid JSON only when __scriptResult is not a
	// plain string; plain strings produce a quoted JSON string value.
	if resVal.IsString() {
		s := resVal.String()
		if json.Valid([]byte(s)) {
			result.Result = json.RawMessage(s)
		}
	}

	return result, nil
}

// buildProxyPreamble generates a JavaScript preamble using ES6 Proxy objects
// to dispatch tool calls. tools.serverName.toolName(args) calls the Go bridge
// function (identified by bridgeName) with (serverName, toolName, JSON.stringify(args || {})).
// The bridge name is randomized per execution to prevent user code from calling
// it directly by guessing a fixed name.
func buildProxyPreamble(serverTools map[string][]protocol.Tool, bridgeName string) string {
	if len(serverTools) == 0 {
		return "const tools = {};\n"
	}

	servers := make([]string, 0, len(serverTools))
	for name := range serverTools {
		servers = append(servers, name)
	}
	sort.Strings(servers)

	var sb strings.Builder
	sb.WriteString("const __servers = new Set([")
	for i, name := range servers {
		if i > 0 {
			sb.WriteByte(',')
		}
		b, _ := json.Marshal(name)
		sb.Write(b)
	}
	sb.WriteString("]);\n")
	sb.WriteString("const tools = new Proxy({}, {\n")
	sb.WriteString("  get(_, alias) {\n")
	sb.WriteString("    if (!__servers.has(alias)) return undefined;\n")
	sb.WriteString("    return new Proxy({}, {\n")
	sb.WriteString("      get(_, toolName) {\n")
	fmt.Fprintf(&sb, "        return (args) => %s(alias, toolName, JSON.stringify(args || {}));\n", bridgeName)
	sb.WriteString("      }\n")
	sb.WriteString("    });\n")
	sb.WriteString("  }\n")
	sb.WriteString("});\n")
	return sb.String()
}

// snapshotToolLogs returns a copy of the accumulated tool call logs under mu.
// Always returns a non-nil slice so JSON serialisation produces [] not null.
func snapshotToolLogs(mu *sync.Mutex, logs []ToolCallLog) []ToolCallLog {
	mu.Lock()
	defer mu.Unlock()
	out := make([]ToolCallLog, len(logs))
	copy(out, logs)
	return out
}

// extractConsoleLogs reads the __logs array from the QJS global scope and
// returns it as a Go slice. The sandbox preamble installs a console replacement
// that pushes {level, msg} objects into __logs.
func extractConsoleLogs(rt *qjs.Runtime) []LogEntry {
	logsVal, err := rt.Eval("__console_logs.js",
		qjs.Code("JSON.stringify(globalThis.__logs || [])"),
		qjs.FlagAsync(),
	)
	if err != nil {
		return []LogEntry{}
	}
	defer logsVal.Free()

	if !logsVal.IsString() {
		return []LogEntry{}
	}

	// The JS struct uses "msg" as the key (matching the preamble) but the Go
	// type names the field Message with json tag "msg".
	var raw []struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(logsVal.String()), &raw); err != nil {
		return []LogEntry{}
	}

	out := make([]LogEntry, len(raw))
	for i, r := range raw {
		out[i] = LogEntry{Level: r.Level, Message: r.Msg}
	}
	return out
}
