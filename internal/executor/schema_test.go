package executor_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// hookTimeout is how long the test waits for an async OnToolResult call.
const hookTimeout = 3 * time.Second

// waitHook waits on ch until either a value arrives or the deadline is reached.
// Returns true if the hook fired.
func waitHook(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(hookTimeout):
		return false
	}
}

// runToolCallWithHook executes a single JavaScript tool call against a fake
// ToolCaller that returns fakeResponse. The OnToolResult hook is registered so
// callers can observe the unwrapped result. Returns the ExecuteResult and any
// Go-level error.
func runToolCallWithHook(
	t *testing.T,
	pool *executor.Pool,
	fakeResponse json.RawMessage,
	hook func(server, tool string, result json.RawMessage),
) (*executor.ExecuteResult, error) {
	t.Helper()
	exec := executor.New(pool)
	return exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `await tools.srv.my_tool({});`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "my_tool"}},
		},
		CallTool: func(_ context.Context, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
			return fakeResponse, nil
		},
		OnToolResult: hook,
	})
}

// runFailingToolCallWithHook executes a tool call that always errors from the
// server side. The hook must not fire in this case.
func runFailingToolCallWithHook(
	t *testing.T,
	pool *executor.Pool,
	hook func(server, tool string, result json.RawMessage),
) (*executor.ExecuteResult, error) {
	t.Helper()
	exec := executor.New(pool)
	return exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `try { await tools.srv.bad_tool({}); } catch(e) { console.log("caught"); }`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "bad_tool"}},
		},
		CallTool: func(_ context.Context, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
			return nil, errSimulated
		},
		OnToolResult: hook,
	})
}

// errSimulated is a stand-in error returned by the failing tool caller.
var errSimulated = &simError{"simulated server error"}

type simError struct{ msg string }

func (e *simError) Error() string { return e.msg }

// --- InferSchema ---

func TestInferSchema_Primitives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantType string
	}{
		{name: "string", input: `"hello"`, wantType: "string"},
		{name: "number integer", input: `42`, wantType: "number"},
		{name: "number float", input: `3.14`, wantType: "number"},
		{name: "boolean true", input: `true`, wantType: "boolean"},
		{name: "boolean false", input: `false`, wantType: "boolean"},
		{name: "null", input: `null`, wantType: "string"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := executor.InferSchema(json.RawMessage(tc.input))
			if got == nil {
				t.Fatal("InferSchema returned nil for valid JSON")
			}
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if m["type"] != tc.wantType {
				t.Errorf("type = %q, want %q (schema: %s)", m["type"], tc.wantType, string(got))
			}
		})
	}
}

func TestInferSchema_InvalidJSON_ReturnsNil(t *testing.T) {
	t.Parallel()

	got := executor.InferSchema(json.RawMessage(`not-json`))
	if got != nil {
		t.Errorf("expected nil for invalid JSON, got %s", string(got))
	}
}

func TestInferSchema_EmptyArray(t *testing.T) {
	t.Parallel()

	got := executor.InferSchema(json.RawMessage(`[]`))
	if got == nil {
		t.Fatal("InferSchema returned nil")
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "array" {
		t.Errorf("type = %q, want array", m["type"])
	}
	items, ok := m["items"].(map[string]any)
	if !ok {
		t.Fatalf("items is not an object: %v", m["items"])
	}
	// Empty array items schema is {} (empty schema = matches anything in JSON Schema).
	if len(items) != 0 {
		t.Errorf("items should be an empty schema {}, got %v", items)
	}
}

func TestInferSchema_ArrayWithElements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantItemsType string
	}{
		{name: "strings", input: `["a","b","c"]`, wantItemsType: "string"},
		{name: "numbers", input: `[1,2,3]`, wantItemsType: "number"},
		{name: "booleans", input: `[true,false]`, wantItemsType: "boolean"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := executor.InferSchema(json.RawMessage(tc.input))
			if got == nil {
				t.Fatal("InferSchema returned nil")
			}
			var m map[string]any
			if err := json.Unmarshal(got, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m["type"] != "array" {
				t.Errorf("type = %q, want array", m["type"])
			}
			items, ok := m["items"].(map[string]any)
			if !ok {
				t.Fatalf("items is not an object: %v", m["items"])
			}
			if items["type"] != tc.wantItemsType {
				t.Errorf("items.type = %q, want %q", items["type"], tc.wantItemsType)
			}
		})
	}
}

func TestInferSchema_EmptyObject(t *testing.T) {
	t.Parallel()

	got := executor.InferSchema(json.RawMessage(`{}`))
	if got == nil {
		t.Fatal("InferSchema returned nil")
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "object" {
		t.Errorf("type = %q, want object", m["type"])
	}
	if _, hasProps := m["properties"]; hasProps {
		t.Error("empty object should not have a properties key")
	}
}

func TestInferSchema_ObjectWithProperties(t *testing.T) {
	t.Parallel()

	got := executor.InferSchema(json.RawMessage(`{"name":"Alice","age":30,"active":true}`))
	if got == nil {
		t.Fatal("InferSchema returned nil")
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "object" {
		t.Errorf("type = %q, want object", m["type"])
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is not an object: %v", m["properties"])
	}
	checkPropType := func(key, wantType string) {
		t.Helper()
		prop, ok := props[key].(map[string]any)
		if !ok {
			t.Errorf("properties.%s is not an object: %v", key, props[key])
			return
		}
		if prop["type"] != wantType {
			t.Errorf("properties.%s.type = %q, want %q", key, prop["type"], wantType)
		}
	}
	checkPropType("name", "string")
	checkPropType("age", "number")
	checkPropType("active", "boolean")
}

func TestInferSchema_NestedDepthExceedsTruncates(t *testing.T) {
	t.Parallel()

	// Build an object 5 levels deep. InferSchema stops at depth > 3, so
	// the 4th-level child is emitted as {"type":"object"} with no properties.
	// depth 0: root     {"a": ...}
	// depth 1: a        {"b": ...}
	// depth 2: b        {"c": ...}
	// depth 3: c        {"d": ...}   ← depth+1 = 4 > 3, so "d" truncates
	deepJSON := `{"a":{"b":{"c":{"d":{"e":"leaf"}}}}}`

	got := executor.InferSchema(json.RawMessage(deepJSON))
	if got == nil {
		t.Fatal("InferSchema returned nil")
	}

	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatalf("unmarshal root: %v", err)
	}

	// Navigate to "c"'s properties → "d".
	depth := []string{"a", "b", "c"}
	cur := root
	for _, key := range depth {
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			t.Fatalf("missing properties at key before %q", key)
		}
		cur, ok = props[key].(map[string]any)
		if !ok {
			t.Fatalf("missing child %q", key)
		}
	}
	// cur is now the schema for "c". Its properties.d should be truncated.
	cProps, ok := cur["properties"].(map[string]any)
	if !ok {
		t.Fatalf("c.properties is not an object: %v", cur["properties"])
	}
	dSchema, ok := cProps["d"].(map[string]any)
	if !ok {
		t.Fatalf("c.properties.d is not an object: %v", cProps["d"])
	}
	if dSchema["type"] != "object" {
		t.Errorf("truncated d.type = %q, want object", dSchema["type"])
	}
	if _, hasProps := dSchema["properties"]; hasProps {
		t.Error("truncated d should have no properties key")
	}
}

// --- unwrapToolResult (tested via executor.Execute + OnToolResult hook) ---

func TestUnwrapToolResult_SingleTextBlockWithJSON(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	// ToolResult with a single text block containing valid JSON.
	toolResponse := json.RawMessage(`{"content":[{"type":"text","text":"{\"temperature\":22}"}]}`)

	var hookResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		hookResult = make(json.RawMessage, len(res))
		copy(hookResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	var got map[string]any
	if err := json.Unmarshal(hookResult, &got); err != nil {
		t.Fatalf("hook result is not valid JSON object: %s (%v)", string(hookResult), err)
	}
	if got["temperature"] != float64(22) {
		t.Errorf("temperature = %v, want 22", got["temperature"])
	}
	// Must NOT still be the ToolResult wrapper.
	if _, hasContent := got["content"]; hasContent {
		t.Error("hook received ToolResult wrapper — expected inner JSON")
	}
}

func TestUnwrapToolResult_SingleTextBlockWithPlainText(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	// Single text block with non-JSON text — should become a JSON string.
	toolResponse := json.RawMessage(`{"content":[{"type":"text","text":"hello world"}]}`)

	var hookResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		hookResult = make(json.RawMessage, len(res))
		copy(hookResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	var got string
	if err := json.Unmarshal(hookResult, &got); err != nil {
		t.Fatalf("hook result is not a JSON string: %s (%v)", string(hookResult), err)
	}
	if got != "hello world" {
		t.Errorf("unwrapped text = %q, want %q", got, "hello world")
	}
}

func TestUnwrapToolResult_MultipleTextBlocks(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	// Multiple text blocks → JSON array of texts.
	toolResponse := json.RawMessage(`{"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`)

	var hookResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		hookResult = make(json.RawMessage, len(res))
		copy(hookResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	var got []string
	if err := json.Unmarshal(hookResult, &got); err != nil {
		t.Fatalf("hook result is not a JSON array of strings: %s (%v)", string(hookResult), err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 texts, got %d", len(got))
	}
	if got[0] != "first" || got[1] != "second" {
		t.Errorf("texts = %v, want [first second]", got)
	}
}

func TestUnwrapToolResult_MultipleNonTextBlocks_PassedThrough(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	// Multiple blocks, none of them text — wrapper must pass through so the
	// data (e.g. images, binaries) isn't lost.
	toolResponse := json.RawMessage(`{"content":[{"type":"image","text":""},{"type":"image","text":""}]}`)

	var hookResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		hookResult = make(json.RawMessage, len(res))
		copy(hookResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	// Should be the raw wrapper, not an empty array.
	var got map[string]any
	if err := json.Unmarshal(hookResult, &got); err != nil {
		t.Fatalf("hook result should be the raw wrapper object: %s (%v)", string(hookResult), err)
	}
	if _, ok := got["content"]; !ok {
		t.Errorf("hook result missing content field, got: %s", string(hookResult))
	}
}

func TestUnwrapToolResult_NotAToolResult_PassedThrough(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	// Plain JSON that is not a ToolResult — pass through unchanged.
	toolResponse := json.RawMessage(`{"score":99}`)

	var hookResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		hookResult = make(json.RawMessage, len(res))
		copy(hookResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	var got map[string]any
	if err := json.Unmarshal(hookResult, &got); err != nil {
		t.Fatalf("hook result is not valid JSON: %s", string(hookResult))
	}
	if got["score"] != float64(99) {
		t.Errorf("score = %v, want 99", got["score"])
	}
}

func TestUnwrapToolResult_EmptyContentArray_PassedThrough(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	// content: [] → empty content array, passed through raw unchanged.
	toolResponse := json.RawMessage(`{"content":[]}`)

	var hookResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		hookResult = make(json.RawMessage, len(res))
		copy(hookResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	var got map[string]any
	if err := json.Unmarshal(hookResult, &got); err != nil {
		t.Fatalf("hook result is not valid JSON: %s", string(hookResult))
	}
	// The raw ToolResult structure should be intact since content is empty.
	if _, hasContent := got["content"]; !hasContent {
		t.Error("expected 'content' key in passed-through empty-content ToolResult")
	}
}

// --- OnToolResult hook behaviour ---

func TestOnToolResult_FiresOnSuccess(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	done := make(chan struct{}, 1)

	_, err := runToolCallWithHook(t, pool, json.RawMessage(`{"value":1}`), func(_, _ string, _ json.RawMessage) {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was not called after a successful tool call")
	}
}

func TestOnToolResult_DoesNotFireOnError(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	hookCalled := make(chan struct{}, 1)

	_, err := runFailingToolCallWithHook(t, pool, func(_, _ string, _ json.RawMessage) {
		hookCalled <- struct{}{}
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Give the goroutine time to fire if it erroneously fires.
	select {
	case <-hookCalled:
		t.Error("OnToolResult hook fired after a failed tool call — it must not")
	case <-time.After(200 * time.Millisecond):
		// Correctly not called.
	}
}

func TestOnToolResult_ReceivesServerAndToolName(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)

	var gotServer, gotTool string
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, json.RawMessage(`"ok"`), func(server, tool string, _ json.RawMessage) {
		gotServer = server
		gotTool = tool
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}
	if gotServer != "srv" {
		t.Errorf("hook server = %q, want srv", gotServer)
	}
	if gotTool != "my_tool" {
		t.Errorf("hook tool = %q, want my_tool", gotTool)
	}
}

func TestOnToolResult_ReceivesUnwrappedResult(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)

	// ToolResult containing JSON — hook must receive the inner JSON, not the wrapper.
	toolResponse := json.RawMessage(`{"content":[{"type":"text","text":"{\"key\":\"val\"}"}]}`)

	var gotResult json.RawMessage
	done := make(chan struct{})

	_, err := runToolCallWithHook(t, pool, toolResponse, func(_, _ string, res json.RawMessage) {
		gotResult = make(json.RawMessage, len(res))
		copy(gotResult, res)
		close(done)
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !waitHook(done) {
		t.Fatal("OnToolResult hook was never called")
	}

	var m map[string]any
	if err := json.Unmarshal(gotResult, &m); err != nil {
		t.Fatalf("hook result is not valid JSON: %s", string(gotResult))
	}
	if m["key"] != "val" {
		t.Errorf("key = %v, want val", m["key"])
	}
	if _, hasContent := m["content"]; hasContent {
		t.Error("hook received raw ToolResult wrapper — must receive unwrapped payload")
	}
}

func TestOnToolResult_NilHookDoesNotPanic(t *testing.T) {
	t.Parallel()

	pool := newTestPool(t)
	exec := executor.New(pool)

	// OnToolResult is nil — must not panic.
	_, err := exec.Execute(context.Background(), executor.ExecuteParams{
		Code: `await tools.srv.ping({});`,
		ServerTools: map[string][]protocol.Tool{
			"srv": {{Name: "ping"}},
		},
		CallTool: func(_ context.Context, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`"pong"`), nil
		},
		OnToolResult: nil,
	})
	if err != nil {
		t.Fatalf("Execute with nil hook: %v", err)
	}
}
