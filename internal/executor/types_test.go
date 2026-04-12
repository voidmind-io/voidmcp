package executor_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidmcp/internal/executor"
	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// --- GenerateTypeDefs ---

func TestGenerateTypeDefs_Empty(t *testing.T) {
	t.Parallel()

	got := executor.GenerateTypeDefs(nil)
	if got != "" {
		t.Errorf("GenerateTypeDefs(nil) = %q, want empty string", got)
	}

	got = executor.GenerateTypeDefs(map[string][]protocol.Tool{})
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

	got := executor.GenerateTypeDefs(tools)

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

			got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)

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

	got := executor.GenerateTypeDefs(tools)

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

	got := executor.GenerateTypeDefs(tools)

	// Tool with invalid ident should use comment notation.
	if !strings.Contains(got, `tools["<server>"]["my-tool"]`) {
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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

	got := executor.GenerateTypeDefs(tools)
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
	got := executor.GenerateTypeDefs(tools)
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
	got := executor.GenerateTypeDefs(tools)
	// Should fall through to the bracket-notation path.
	if !strings.Contains(got, `"1invalid"`) {
		t.Errorf("tool starting with digit should use bracket notation:\n%s", got)
	}
}
