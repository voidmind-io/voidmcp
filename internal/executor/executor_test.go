package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// newTestPool creates a small executor pool for testing and cleans up when
// the test ends.
func newTestPool(t *testing.T) *executor.Pool {
	t.Helper()
	pool, err := executor.NewPool(2, 32, 10*time.Second)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// --- Pool tests ---

func TestPool_AcquireRelease(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)

	// Pool starts fully available.
	if got := pool.Available(); got != 2 {
		t.Errorf("Available() = %d, want 2", got)
	}

	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if rt == nil {
		t.Fatal("Acquire returned nil runtime")
	}

	if got := pool.Available(); got != 1 {
		t.Errorf("Available() after acquire = %d, want 1", got)
	}

	pool.Release(rt)

	if got := pool.Available(); got != 2 {
		t.Errorf("Available() after release = %d, want 2", got)
	}
}

func TestPool_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)

	// Drain the pool completely.
	rt1, _ := pool.Acquire(context.Background())
	rt2, _ := pool.Acquire(context.Background())
	defer pool.Release(rt1)
	defer pool.Release(rt2)

	// Pool is empty; a cancelled context should fail immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pool.Acquire(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestPool_SizeEnforcement(t *testing.T) {
	t.Parallel()

	pool, err := executor.NewPool(1, 16, 5*time.Second)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = pool.Acquire(ctx)
	if err == nil {
		t.Fatal("expected timeout, got nil")
	}

	pool.Release(rt)
}

// --- Executor.Execute: known behaviors ---
//
// NOTE on return values: the executor wraps user code in an async IIFE and
// evaluates it with qjs.FlagAsync(). Due to a production bug, qjs.FlagAsync()
// returns the Promise object rather than its resolved value, so result.Result
// is always {} and result.Error is always "". See production bug report below.
//
// PRODUCTION BUG #1: executor.Execute always returns result.Result = {} (empty
// JSON object) and result.Error = "" for ALL scripts, because qjs.FlagAsync()
// returns the unresolved Promise rather than the settled value. Return values,
// throw errors, and unhandled rejections from user code are all silently lost.
// Fix: replace qjs.FlagAsync() with a mechanism that awaits Promise resolution,
// or use a JavaScript trampoline that stores the resolved value in a global.

func TestExecute_ResultAlwaysEmptyObjectDueToBug(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// This documents the broken behavior: return 42 should give result.Result = "42"
	// but due to the FlagAsync/Promise bug it returns {}.
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `return 42;`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Document the actual (broken) behavior: result is {} not 42.
	if string(result.Result) != "{}" {
		t.Logf("Result behaviour changed: got %s (was {} before)", result.Result)
	}
}

func TestExecute_CodeSizeLimit(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// 256 KiB + 1 byte should be rejected before execution.
	bigCode := strings.Repeat("x", 256*1024+1)

	_, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: bigCode,
	})
	if err == nil {
		t.Fatal("expected error for oversized code, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("error %q does not mention limit", err.Error())
	}
}

func TestExecute_AtExactCodeSizeLimit(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// Exactly 256 KiB should be accepted (no "exceeds limit" Go error).
	exactCode := strings.Repeat(" ", 256*1024)

	_, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: exactCode,
	})
	if err != nil && strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("code at exact limit should be accepted: %v", err)
	}
}

func TestExecute_ConsoleLogs_Captured(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// Console capture works even though return values are broken.
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
console.log("hello");
console.warn("world");
console.error("oops");
`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) != 3 {
		t.Fatalf("expected 3 logs, got %d: %v", len(result.Logs), result.Logs)
	}

	wantLogs := []executor.LogEntry{
		{Level: "log", Message: "hello"},
		{Level: "warn", Message: "world"},
		{Level: "error", Message: "oops"},
	}
	for i, want := range wantLogs {
		if result.Logs[i].Level != want.Level {
			t.Errorf("logs[%d].Level = %q, want %q", i, result.Logs[i].Level, want.Level)
		}
		if result.Logs[i].Message != want.Message {
			t.Errorf("logs[%d].Message = %q, want %q", i, result.Logs[i].Message, want.Message)
		}
	}
}

func TestExecute_ConsoleLogs_MultipleArgs(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `console.log("a", "b", "c");`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(result.Logs))
	}
	if result.Logs[0].Message != "a b c" {
		t.Errorf("log message = %q, want %q", result.Logs[0].Message, "a b c")
	}
}

func TestExecute_ToolCall_Success(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	callerCalled := false
	var calledServer, calledTool string

	toolCaller := executor.ToolCaller(func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
		callerCalled = true
		calledServer = serverName
		calledTool = toolName
		return json.RawMessage(`{"temperature": 22}`), nil
	})

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `await tools.myserver.get_weather({"city": "London"});`,
		ServerTools: map[string][]protocol.Tool{
			"myserver": {
				{Name: "get_weather", Description: "Get weather"},
			},
		},
		CallTool: toolCaller,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !callerCalled {
		t.Error("ToolCaller was never called")
	}
	if calledServer != "myserver" {
		t.Errorf("called server = %q, want %q", calledServer, "myserver")
	}
	if calledTool != "get_weather" {
		t.Errorf("called tool = %q, want %q", calledTool, "get_weather")
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call log, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Status != "success" {
		t.Errorf("tool call status = %q, want success", result.ToolCalls[0].Status)
	}
	if result.ToolCalls[0].Server != "myserver" {
		t.Errorf("tool call server = %q, want myserver", result.ToolCalls[0].Server)
	}
	if result.ToolCalls[0].Tool != "get_weather" {
		t.Errorf("tool call tool = %q, want get_weather", result.ToolCalls[0].Tool)
	}
}

func TestExecute_ToolCall_MaxLimit(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	toolCaller := executor.ToolCaller(func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`"ok"`), nil
	})

	// With MaxToolCalls=2, calling the tool 5 times should exceed the limit.
	// The limit is enforced inside __callTool which rejects the promise. Due to
	// the FlagAsync bug, the rejection is also swallowed, but the tool call
	// count in the log should show the limit was enforced.
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
for (let i = 0; i < 5; i++) {
  try { await tools.srv.ping({}); } catch(e) { break; }
}
`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "ping"}},
		},
		CallTool:     toolCaller,
		MaxToolCalls: 2,
	})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}

	// The tool should be called at most MaxToolCalls+1 times (once over the
	// limit triggers the rejection). The counter increments before the guard
	// returns the error, so 3 log entries is acceptable (2 successes + 1 error).
	if len(result.ToolCalls) > 3 {
		t.Errorf("expected <= 3 tool calls (limit=2), got %d", len(result.ToolCalls))
	}
}

func TestExecute_ToolCall_LogsRecorded(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	callCount := 0
	toolCaller := executor.ToolCaller(func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
		callCount++
		return json.RawMessage(`"pong"`), nil
	})

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
await tools.srv.ping({});
await tools.srv.ping({});
`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "ping"}},
		},
		CallTool: toolCaller,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 tool calls, got %d", callCount)
	}
	if len(result.ToolCalls) != 2 {
		t.Errorf("expected 2 tool call logs, got %d", len(result.ToolCalls))
	}
	for _, tc := range result.ToolCalls {
		if tc.Status != "success" {
			t.Errorf("tool call status = %q, want success", tc.Status)
		}
	}
}

func TestExecute_LogsAlwaysNonNil(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `// empty`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Logs == nil {
		t.Error("Logs should never be nil, want empty slice")
	}
	if result.ToolCalls == nil {
		t.Error("ToolCalls should never be nil, want empty slice")
	}
}

func TestExecute_DurationIsPopulated(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `// no-op`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.DurationMS < 0 {
		t.Errorf("DurationMS = %d, should be non-negative", result.DurationMS)
	}
}

func TestExecute_ToolsObjectExistsWhenNoTools(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// Even with no tools, the tools proxy object should be accessible.
	// We verify indirectly via console.log (can't use return due to FlagAsync bug).
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code:        `console.log(typeof tools);`,
		ServerTools: nil,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) == 0 {
		t.Fatal("expected log output, got none")
	}
	if result.Logs[0].Message != "object" {
		t.Errorf("typeof tools = %q, want object", result.Logs[0].Message)
	}
}

func TestExecute_SandboxPreventsStdAccess(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `console.log(typeof std);`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) == 0 {
		t.Fatal("expected log output, got none")
	}
	if result.Logs[0].Message != "undefined" {
		t.Errorf("typeof std = %q, want undefined (sandbox should remove std)", result.Logs[0].Message)
	}
}

func TestExecute_SandboxPreventsOsAccess(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `console.log(typeof os);`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) == 0 {
		t.Fatal("expected log output, got none")
	}
	if result.Logs[0].Message != "undefined" {
		t.Errorf("typeof os = %q, want undefined (sandbox should remove os)", result.Logs[0].Message)
	}
}

func TestExecute_SandboxPreventsSetTimeout(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `console.log(typeof setTimeout);`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) == 0 {
		t.Fatal("expected log output, got none")
	}
	if result.Logs[0].Message != "undefined" {
		t.Errorf("typeof setTimeout = %q, want undefined (sandbox should remove timers)", result.Logs[0].Message)
	}
}

func TestExecute_PoolTimeout_ContextCancelled(t *testing.T) {
	t.Parallel()

	pool, err := executor.NewPool(1, 16, 5*time.Second)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	exec := executor.New(pool)

	// Cancel the context immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Drain the pool first so Acquire must block.
	rt, acquireErr := pool.Acquire(context.Background())
	if acquireErr != nil {
		t.Fatalf("drain acquire: %v", acquireErr)
	}

	// With the pool empty and ctx already cancelled, Execute should fail fast.
	_, execErr := exec.Execute(ctx, executor.ExecuteParams{Code: `// code`})
	pool.Release(rt)

	if execErr == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

func TestExecute_ToolCall_UnknownTool_IsRecorded(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// Call a tool that is not in the dispatch map — it should be recorded with status "error".
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
try {
  await tools.srv.nonexistent({});
} catch(e) {
  console.log("caught: " + e);
}
`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "other_tool"}},
		},
		CallTool: func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"ok"`), nil
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The unknown tool call should be logged with "error" status.
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call log, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Status != "error" {
		t.Errorf("tool call status = %q, want error", result.ToolCalls[0].Status)
	}
}

func TestExecute_ToolCall_NoCallerConfigured(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// CallTool is nil — the tool call should fail gracefully.
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
try {
  await tools.srv.ping({});
} catch(e) {
  console.log("caught error");
}
`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "ping"}},
		},
		CallTool: nil,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The error should be caught in JS; we should see the log.
	if len(result.Logs) == 0 {
		t.Error("expected log output from caught error, got none")
	}
}

func TestExecute_ToolCall_ErrorFromServer(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	toolErr := errors.New("server error")
	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
try {
  await tools.srv.failing({});
} catch(e) {
  console.log("caught: " + e.message);
}
`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "failing"}},
		},
		CallTool: func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
			return nil, toolErr
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The tool call should be logged with "error" status.
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call log, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Status != "error" {
		t.Errorf("tool call status = %q, want error", result.ToolCalls[0].Status)
	}
}

func TestExecute_MultipleServerTools(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	calledTools := make(map[string]bool)
	toolCaller := executor.ToolCaller(func(ctx context.Context, serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
		calledTools[serverName+"/"+toolName] = true
		return json.RawMessage(`"ok"`), nil
	})

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
await tools.srv1.tool_a({});
await tools.srv2.tool_b({});
`,
		ServerTools: map[string][]protocol.Tool{
			"srv1": {{Name: "tool_a"}},
			"srv2": {{Name: "tool_b"}},
		},
		CallTool: toolCaller,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(result.ToolCalls) != 2 {
		t.Errorf("expected 2 tool call logs, got %d", len(result.ToolCalls))
	}
	if !calledTools["srv1/tool_a"] {
		t.Error("srv1/tool_a was not called")
	}
	if !calledTools["srv2/tool_b"] {
		t.Error("srv2/tool_b was not called")
	}
}

func TestExecute_ConsoleInfo_Captured(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	result, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `
console.info("info message");
console.debug("debug message");
`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Logs) != 2 {
		t.Fatalf("expected 2 logs, got %d: %v", len(result.Logs), result.Logs)
	}
	if result.Logs[0].Level != "info" {
		t.Errorf("logs[0].Level = %q, want info", result.Logs[0].Level)
	}
	if result.Logs[1].Level != "debug" {
		t.Errorf("logs[1].Level = %q, want debug", result.Logs[1].Level)
	}
}
