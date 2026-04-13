package executor_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// --- GenerateTypeDefs ---

func TestGenerateTypeDefs_Empty(t *testing.T) {
	t.Parallel()

	got := executor.GenerateTypeDefs(nil, nil)
	if got != "" {
		t.Errorf("GenerateTypeDefs(nil) = %q, want empty string", got)
	}

	got = executor.GenerateTypeDefs(map[string][]protocol.Tool{}, nil)
	if got != "" {
		t.Errorf("GenerateTypeDefs({}) = %q, want empty string", got)
	}
}

func TestGenerateTypeDefs_SingleTool(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"github": {
			{
				Name:        "create_issue",
				Description: "Create a GitHub issue",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"title": {Type: "string", Description: "Issue title"},
						"body":  {Type: "string", Description: "Issue body"},
					},
					Required: []string{"title"},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)

	if !strings.Contains(got, "declare namespace tools.github") {
		t.Errorf("output missing namespace declaration:\n%s", got)
	}
	if !strings.Contains(got, "function create_issue") {
		t.Errorf("output missing function declaration:\n%s", got)
	}
	// title is required — no ? suffix.
	if !strings.Contains(got, "title: string") {
		t.Errorf("output missing required title field:\n%s", got)
	}
	// body is optional — ? suffix.
	if !strings.Contains(got, "body?: string") {
		t.Errorf("output missing optional body? field:\n%s", got)
	}
	if !strings.Contains(got, "Promise<any>") {
		t.Errorf("output missing Promise<any> return type:\n%s", got)
	}
}

func TestGenerateTypeDefs_TypeMappings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		propType string
		want     string
	}{
		{name: "string", propType: "string", want: "string"},
		{name: "number", propType: "number", want: "number"},
		{name: "integer", propType: "integer", want: "number"},
		{name: "boolean", propType: "boolean", want: "boolean"},
		{name: "array no items", propType: "array", want: "any[]"},
		{name: "object", propType: "object", want: "Record<string, any>"},
		{name: "unknown", propType: "null", want: "any"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := map[string][]protocol.Tool{
				"srv": {
					{
						Name: "tool_a",
						InputSchema: protocol.InputSchema{
							Type: "object",
							Properties: map[string]protocol.Property{
								"param": {Type: tc.propType},
							},
							Required: []string{"param"},
						},
					},
				},
			}

			got := executor.GenerateTypeDefs(tools, nil)
			if !strings.Contains(got, "param: "+tc.want) {
				t.Errorf("type %q: output = %q, want to contain %q", tc.propType, got, "param: "+tc.want)
			}
		})
	}
}

func TestGenerateTypeDefs_ArrayWithItemType(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "list_items",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"tags": {
							Type:  "array",
							Items: &protocol.Property{Type: "string"},
						},
					},
					Required: []string{"tags"},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "tags: string[]") {
		t.Errorf("output missing string[] for array with string items:\n%s", got)
	}
}

func TestGenerateTypeDefs_MultipleServersAlphaOrder(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"zebra":  {{Name: "z_tool"}},
		"alpha":  {{Name: "a_tool"}},
		"middle": {{Name: "m_tool"}},
	}

	got := executor.GenerateTypeDefs(tools, nil)

	alphaIdx := strings.Index(got, "tools.alpha")
	middleIdx := strings.Index(got, "tools.middle")
	zebraIdx := strings.Index(got, "tools.zebra")

	if alphaIdx == -1 || middleIdx == -1 || zebraIdx == -1 {
		t.Fatalf("missing namespace in output:\n%s", got)
	}
	if !(alphaIdx < middleIdx && middleIdx < zebraIdx) {
		t.Errorf("servers not in alphabetical order: alpha=%d middle=%d zebra=%d", alphaIdx, middleIdx, zebraIdx)
	}
}

func TestGenerateTypeDefs_InvalidJSIdent_ServerName(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"my-server": {{Name: "my_tool"}},
	}

	got := executor.GenerateTypeDefs(tools, nil)

	// Server with invalid ident should use bracket-notation comment.
	if !strings.Contains(got, `tools["my-server"]`) {
		t.Errorf("output should use bracket notation for invalid ident:\n%s", got)
	}
}

func TestGenerateTypeDefs_InvalidJSIdent_ToolName(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "my-tool"}},
	}

	got := executor.GenerateTypeDefs(tools, nil)

	// Tool with invalid ident should use bracket notation with the actual server name.
	if !strings.Contains(got, `tools["srv"]["my-tool"]`) {
		t.Errorf("output should use bracket notation comment for invalid tool ident:\n%s", got)
	}
}

func TestGenerateTypeDefs_ToolWithNoProperties(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "ping",
				InputSchema: protocol.InputSchema{
					Type: "object",
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "function ping(args: {})") {
		t.Errorf("tool with no properties should have empty args {}:\n%s", got)
	}
}

func TestGenerateTypeDefs_DescriptionTruncatedAtSentence(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name:        "my_tool",
				Description: "Short sentence. This part should be cut off.",
				InputSchema: protocol.InputSchema{Type: "object"},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if strings.Contains(got, "This part should be cut off") {
		t.Errorf("description was not truncated at sentence boundary:\n%s", got)
	}
	if !strings.Contains(got, "Short sentence.") {
		t.Errorf("first sentence missing from description:\n%s", got)
	}
}

// --- GenerateServerSummaries ---

func TestGenerateServerSummaries_Empty(t *testing.T) {
	t.Parallel()

	got := executor.GenerateServerSummaries(nil)
	if got != "" {
		t.Errorf("GenerateServerSummaries(nil) = %q, want empty", got)
	}
}

func TestGenerateServerSummaries_SingleServer(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"github": {
			{Name: "create_issue"},
			{Name: "list_repos"},
			{Name: "search_code"},
		},
	}

	got := executor.GenerateServerSummaries(tools)
	if !strings.Contains(got, "github") {
		t.Errorf("output missing server name:\n%s", got)
	}
	if !strings.Contains(got, "3 tools") {
		t.Errorf("output missing tool count:\n%s", got)
	}
	if !strings.Contains(got, "create_issue") {
		t.Errorf("output missing tool name:\n%s", got)
	}
}

func TestGenerateServerSummaries_MoreThan5Tools_Ellipsis(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"bigserver": {
			{Name: "tool_a"},
			{Name: "tool_b"},
			{Name: "tool_c"},
			{Name: "tool_d"},
			{Name: "tool_e"},
			{Name: "tool_f"},
		},
	}

	got := executor.GenerateServerSummaries(tools)
	if !strings.Contains(got, "...") {
		t.Errorf("output with >5 tools should have ellipsis:\n%s", got)
	}
	if !strings.Contains(got, "6 tools") {
		t.Errorf("output missing 6 tools count:\n%s", got)
	}
}

func TestGenerateServerSummaries_SingularTool(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "only_tool"}},
	}

	got := executor.GenerateServerSummaries(tools)
	// "1 tool" not "1 tools".
	if !strings.Contains(got, "1 tool)") {
		t.Errorf("singular 'tool' not used:\n%s", got)
	}
	if strings.Contains(got, "1 tools") {
		t.Errorf("used incorrect plural '1 tools':\n%s", got)
	}
}

func TestGenerateServerSummaries_AlphaOrder(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"zebra": {{Name: "z1"}},
		"alpha": {{Name: "a1"}},
	}

	got := executor.GenerateServerSummaries(tools)
	alphaIdx := strings.Index(got, "alpha")
	zebraIdx := strings.Index(got, "zebra")

	if alphaIdx == -1 || zebraIdx == -1 {
		t.Fatalf("server name missing from summary:\n%s", got)
	}
	if alphaIdx > zebraIdx {
		t.Errorf("servers not sorted alphabetically: alpha at %d, zebra at %d", alphaIdx, zebraIdx)
	}
}

// --- tsTypeFromString coverage (tested indirectly via GenerateTypeDefs) ---

func TestGenerateTypeDefs_ArrayTypeNoItems(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "tool_a",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"tags": {Type: "array"},
					},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "tags?: any[]") {
		t.Errorf("array with no items should produce any[]: got\n%s", got)
	}
}

func TestGenerateTypeDefs_ObjectProperty(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "tool_obj",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"meta": {Type: "object"},
					},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "meta?: Record<string, any>") {
		t.Errorf("object property should produce Record<string, any>: got\n%s", got)
	}
}

func TestGenerateTypeDefs_BooleanProperty(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "toggle",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"enabled": {Type: "boolean"},
					},
					Required: []string{"enabled"},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "enabled: boolean") {
		t.Errorf("boolean property: got\n%s", got)
	}
}

func TestGenerateTypeDefs_NumberProperty(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "count",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"amount": {Type: "number"},
					},
					Required: []string{"amount"},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "amount: number") {
		t.Errorf("number property: got\n%s", got)
	}
}

func TestGenerateTypeDefs_UnknownType(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "weird",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"x": {Type: "null"},
					},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "x?: any") {
		t.Errorf("unknown type should produce any: got\n%s", got)
	}
}

func TestGenerateTypeDefs_ArrayWithIntegerItems(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name: "nums",
				InputSchema: protocol.InputSchema{
					Type: "object",
					Properties: map[string]protocol.Property{
						"values": {
							Type:  "array",
							Items: &protocol.Property{Type: "integer"},
						},
					},
				},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "values?: number[]") {
		t.Errorf("array of integer items should produce number[]: got\n%s", got)
	}
}

func TestGenerateTypeDefs_DescriptionTruncatedAtNewline(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name:        "multi_line",
				Description: "First line\nSecond line should be cut",
				InputSchema: protocol.InputSchema{Type: "object"},
			},
		},
	}

	got := executor.GenerateTypeDefs(tools, nil)
	if strings.Contains(got, "Second line") {
		t.Errorf("description was not truncated at newline:\n%s", got)
	}
	if !strings.Contains(got, "First line") {
		t.Errorf("first line missing from description:\n%s", got)
	}
}

func TestGenerateTypeDefs_EmptyDescription(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{
				Name:        "nodesc",
				Description: "",
				InputSchema: protocol.InputSchema{Type: "object"},
			},
		},
	}

	// Empty description should not cause a panic or empty comment line.
	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "function nodesc") {
		t.Errorf("tool missing from output:\n%s", got)
	}
}

func TestGenerateTypeDefs_StartsWithDigit(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{Name: "1invalid"},
		},
	}

	// A tool name starting with a digit is not a valid JS ident.
	got := executor.GenerateTypeDefs(tools, nil)
	// Should fall through to the bracket-notation path.
	if !strings.Contains(got, `"1invalid"`) {
		t.Errorf("tool starting with digit should use bracket notation:\n%s", got)
	}
}

// --- schemaToTypeScript (exercised via GenerateTypeDefs with outputSchemas) ---

func TestGenerateTypeDefs_OutputSchema_StringType(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "get_name", InputSchema: protocol.InputSchema{Type: "object"}}},
	}
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"get_name": json.RawMessage(`{"type":"string"}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	if !strings.Contains(got, "Promise<string>") {
		t.Errorf("expected Promise<string> in output, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_NumberType(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "get_count", InputSchema: protocol.InputSchema{Type: "object"}}},
	}
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"get_count": json.RawMessage(`{"type":"number"}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	if !strings.Contains(got, "Promise<number>") {
		t.Errorf("expected Promise<number> in output, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_BooleanType(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "is_active", InputSchema: protocol.InputSchema{Type: "object"}}},
	}
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"is_active": json.RawMessage(`{"type":"boolean"}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	if !strings.Contains(got, "Promise<boolean>") {
		t.Errorf("expected Promise<boolean> in output, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_ArrayOfStrings(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "list_tags", InputSchema: protocol.InputSchema{Type: "object"}}},
	}
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"list_tags": json.RawMessage(`{"type":"array","items":{"type":"string"}}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	if !strings.Contains(got, "Promise<Array<string>>") {
		t.Errorf("expected Promise<Array<string>> in output, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_ObjectWithProperties_SortedKeys(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "get_user", InputSchema: protocol.InputSchema{Type: "object"}}},
	}
	// Properties with multiple keys — output must be sorted alphabetically.
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"get_user": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"number"}}}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	// Expect "{ age: number; name: string }" — keys sorted.
	if !strings.Contains(got, "Promise<{ age: number; name: string }>") {
		t.Errorf("expected sorted object type in Promise<...>, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_EmptyOrInvalid_FallsBackToAny(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema json.RawMessage
	}{
		{name: "empty bytes", schema: json.RawMessage(``)},
		{name: "invalid json", schema: json.RawMessage(`not-json`)},
		{name: "unknown type", schema: json.RawMessage(`{"type":"unknown"}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := map[string][]protocol.Tool{
				"srv": {{Name: "do_thing", InputSchema: protocol.InputSchema{Type: "object"}}},
			}
			schemas := map[string]map[string]json.RawMessage{
				"srv": {"do_thing": tc.schema},
			}

			got := executor.GenerateTypeDefs(tools, schemas)
			// Empty/invalid schema must fall back to Promise<any>.
			if !strings.Contains(got, "Promise<any>") {
				t.Errorf("expected Promise<any> fallback for schema %q, got:\n%s", string(tc.schema), got)
			}
		})
	}
}

func TestGenerateTypeDefs_OutputSchema_NoSchemaForTool_UsesAny(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {
			{Name: "tool_with_schema", InputSchema: protocol.InputSchema{Type: "object"}},
			{Name: "tool_without_schema", InputSchema: protocol.InputSchema{Type: "object"}},
		},
	}
	// Only provide a schema for one of the two tools.
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"tool_with_schema": json.RawMessage(`{"type":"string"}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	if !strings.Contains(got, "Promise<string>") {
		t.Errorf("tool_with_schema: expected Promise<string>, got:\n%s", got)
	}
	// The tool without a schema must still use Promise<any>.
	if !strings.Contains(got, "Promise<any>") {
		t.Errorf("tool_without_schema: expected Promise<any>, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_ObjectNoProperties_RecordFallback(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "get_meta", InputSchema: protocol.InputSchema{Type: "object"}}},
	}
	// Object with no properties → Record<string, any>.
	schemas := map[string]map[string]json.RawMessage{
		"srv": {"get_meta": json.RawMessage(`{"type":"object"}`)},
	}

	got := executor.GenerateTypeDefs(tools, schemas)
	if !strings.Contains(got, "Promise<Record<string, any>>") {
		t.Errorf("expected Promise<Record<string, any>> for object without properties, got:\n%s", got)
	}
}

func TestGenerateTypeDefs_OutputSchema_NilOutputSchemas_FallsBackToAny(t *testing.T) {
	t.Parallel()

	tools := map[string][]protocol.Tool{
		"srv": {{Name: "my_fn", InputSchema: protocol.InputSchema{Type: "object"}}},
	}

	// nil outputSchemas → all tools use Promise<any>.
	got := executor.GenerateTypeDefs(tools, nil)
	if !strings.Contains(got, "Promise<any>") {
		t.Errorf("nil outputSchemas: expected Promise<any>, got:\n%s", got)
	}
}
