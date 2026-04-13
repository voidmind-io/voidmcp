package executor

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// GenerateTypeDefs produces TypeScript namespace declarations for all tools in
// serverTools. The output is embedded in the execute_code tool description so
// an LLM can infer correct call syntax and argument shapes.
//
// outputSchemas maps server alias to a map of tool name to inferred JSON
// Schema. When a schema is available for a tool its Promise return type is
// replaced with the inferred TypeScript type instead of Promise<any>.
// Pass nil or an empty map when no schemas are available.
//
// Each server name becomes a namespace under the global tools object:
//
//	declare namespace tools.github {
//	  /** Create a GitHub issue */
//	  function create_issue(args: {
//	    title: string;
//	    body?: string;
//	    labels?: any[];
//	  }): Promise<any>;
//	}
//
// Servers and tools are sorted alphabetically for deterministic output.
// Returns an empty string when serverTools is empty.
func GenerateTypeDefs(serverTools map[string][]protocol.Tool, outputSchemas map[string]map[string]json.RawMessage) string {
	if len(serverTools) == 0 {
		return ""
	}

	servers := sortedServerNames(serverTools)

	var sb strings.Builder
	for i, server := range servers {
		tools := serverTools[server]
		if len(tools) == 0 {
			continue
		}
		if i > 0 {
			sb.WriteByte('\n')
		}

		sorted := sortedTools(tools)

		if isValidJSIdent(server) {
			sb.WriteString("declare namespace tools.")
			sb.WriteString(server)
			sb.WriteString(" {\n")
			for _, t := range sorted {
				var outSchema json.RawMessage
				if outputSchemas != nil {
					if schemas, ok := outputSchemas[server]; ok {
						outSchema = schemas[t.Name]
					}
				}
				writeToolDecl(&sb, t, outSchema, server)
			}
			sb.WriteString("}")
		} else {
			// Server alias is not a valid JS identifier — emit all tools as
			// bracket-notation comments so the LLM sees the correct call syntax.
			for _, t := range sorted {
				var outSchema json.RawMessage
				if outputSchemas != nil {
					if schemas, ok := outputSchemas[server]; ok {
						outSchema = schemas[t.Name]
					}
				}
				writeToolDecl(&sb, t, outSchema, server)
			}
		}
	}
	return sb.String()
}

// GenerateServerSummaries produces one-line summaries per server suitable for
// use when the total tool count exceeds a schema threshold and full type
// declarations would be too verbose.
//
// Output format:
//
//	- github (12 tools): create_issue, search_code, list_repos, ...
//	- notion (8 tools): create_page, search, update_page, ...
//
// Up to 5 tool names are shown per server; if there are more, "..." is
// appended. Servers and tools are sorted alphabetically.
func GenerateServerSummaries(serverTools map[string][]protocol.Tool) string {
	if len(serverTools) == 0 {
		return ""
	}

	servers := sortedServerNames(serverTools)

	var sb strings.Builder
	for _, server := range servers {
		tools := sortedTools(serverTools[server])
		count := len(tools)

		sb.WriteString("- ")
		sb.WriteString(server)
		sb.WriteString(fmt.Sprintf(" (%d tool", count))
		if count != 1 {
			sb.WriteByte('s')
		}
		sb.WriteString("): ")

		limit := 5
		if count < limit {
			limit = count
		}
		names := make([]string, limit)
		for i := 0; i < limit; i++ {
			names[i] = tools[i].Name
		}
		sb.WriteString(strings.Join(names, ", "))
		if count > 5 {
			sb.WriteString(", ...")
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// writeToolDecl writes a single TypeScript function declaration into sb.
// serverName is the owning server alias; when it is not a valid JS identifier
// all tools for that server are emitted as bracket-notation comments.
// outSchema is an optional inferred JSON Schema for the return type; when nil
// the return type falls back to Promise<any>.
func writeToolDecl(sb *strings.Builder, tool protocol.Tool, outSchema json.RawMessage, serverName string) {
	serverInvalid := !isValidJSIdent(serverName)
	toolInvalid := !isValidJSIdent(tool.Name)

	if serverInvalid || toolInvalid {
		// Emit a bracket-notation comment showing the exact call syntax the
		// WASM proxy supports at runtime.
		retType := "Promise<any>"
		if outSchema != nil {
			retType = "Promise<" + schemaToTypeScript(outSchema) + ">"
		}
		escapedServer := strings.ReplaceAll(serverName, "\"", "\\\"")
		escapedTool := strings.ReplaceAll(tool.Name, "\"", "\\\"")
		sb.WriteString("// tools[\"")
		sb.WriteString(escapedServer)
		sb.WriteString("\"][\"")
		sb.WriteString(escapedTool)
		sb.WriteString("\"](args: {")

		props := tool.InputSchema.Properties
		if len(props) > 0 {
			required := make(map[string]bool, len(tool.InputSchema.Required))
			for _, r := range tool.InputSchema.Required {
				required[r] = true
			}
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			sort.Strings(names)
			parts := make([]string, 0, len(names))
			for _, name := range names {
				prop := props[name]
				opt := ""
				if !required[name] {
					opt = "?"
				}
				parts = append(parts, name+opt+": "+tsType(prop))
			}
			sb.WriteString(" ")
			sb.WriteString(strings.Join(parts, "; "))
			sb.WriteString(" ")
		}

		sb.WriteString("}): ")
		sb.WriteString(retType)
		sb.WriteString(";\n")
		return
	}

	if desc := truncateDescription(tool.Description); desc != "" {
		clean := strings.ReplaceAll(desc, "*/", "")
		clean = strings.ReplaceAll(clean, "\n", " ")
		sb.WriteString("  /** ")
		sb.WriteString(clean)
		sb.WriteString(" */\n")
	}

	sb.WriteString("  function ")
	sb.WriteString(tool.Name)
	sb.WriteString("(args: {")

	props := tool.InputSchema.Properties
	if len(props) > 0 {
		required := make(map[string]bool, len(tool.InputSchema.Required))
		for _, r := range tool.InputSchema.Required {
			required[r] = true
		}

		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)

		sb.WriteByte('\n')
		for _, name := range names {
			prop := props[name]
			sb.WriteString("    ")
			sb.WriteString(name)
			if !required[name] {
				sb.WriteByte('?')
			}
			sb.WriteString(": ")
			sb.WriteString(tsType(prop))
			sb.WriteString(";\n")
		}
		sb.WriteString("  ")
	}

	if outSchema != nil {
		sb.WriteString("}): Promise<" + schemaToTypeScript(outSchema) + ">; // inferred - could depend on previous query\n")
	} else {
		sb.WriteString("}): Promise<any>;\n")
	}
}

// schemaToTypeScript converts an inferred JSON-Schema-like object to a
// TypeScript type string.
func schemaToTypeScript(schema json.RawMessage) string {
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return "any"
	}
	return schemaTypeToTS(s)
}

// schemaTypeToTS recursively maps a parsed JSON Schema map to a TypeScript
// type string.
func schemaTypeToTS(s map[string]any) string {
	typ, _ := s["type"].(string)
	switch typ {
	case "string":
		return "string"
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		items, ok := s["items"].(map[string]any)
		if !ok {
			return "any[]"
		}
		return "Array<" + schemaTypeToTS(items) + ">"
	case "object":
		props, ok := s["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			return "Record<string, any>"
		}
		// Sort keys for deterministic output.
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var sb strings.Builder
		sb.WriteString("{ ")
		for i, k := range keys {
			if i > 0 {
				sb.WriteString("; ")
			}
			propSchema, ok := props[k].(map[string]any)
			if !ok {
				sb.WriteString(k + ": any")
			} else {
				sb.WriteString(k + ": " + schemaTypeToTS(propSchema))
			}
		}
		sb.WriteString(" }")
		return sb.String()
	default:
		return "any"
	}
}

// tsType maps a JSON Schema Property to the corresponding TypeScript type
// string. Array types use the items type when present.
func tsType(p protocol.Property) string {
	switch p.Type {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		if p.Items != nil && p.Items.Type != "" {
			return tsTypeFromString(p.Items.Type) + "[]"
		}
		return "any[]"
	case "object":
		return "Record<string, any>"
	default:
		return "any"
	}
}

// tsTypeFromString maps a raw JSON Schema type string to a TypeScript type.
func tsTypeFromString(t string) string {
	switch t {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "object":
		return "Record<string, any>"
	default:
		return "any"
	}
}

// isValidJSIdent reports whether s is a valid bare JavaScript identifier
// (ASCII letters, digits, or underscore; must not start with a digit).
func isValidJSIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if r >= '0' && r <= '9' && i > 0 {
			continue
		}
		return false
	}
	return true
}

// truncateDescription returns the first sentence of desc. A sentence boundary
// is the first occurrence of ". " (period-space) or a newline. If no boundary
// is found, the entire string is returned.
func truncateDescription(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	if idx := strings.Index(desc, ". "); idx >= 0 {
		return desc[:idx+1]
	}
	if idx := strings.IndexByte(desc, '\n'); idx >= 0 {
		return strings.TrimSpace(desc[:idx])
	}
	return desc
}

// sortedServerNames returns the keys of serverTools sorted alphabetically.
func sortedServerNames(serverTools map[string][]protocol.Tool) []string {
	names := make([]string, 0, len(serverTools))
	for name := range serverTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sortedTools returns a copy of tools sorted by Name.
func sortedTools(tools []protocol.Tool) []protocol.Tool {
	out := make([]protocol.Tool, len(tools))
	copy(out, tools)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}
