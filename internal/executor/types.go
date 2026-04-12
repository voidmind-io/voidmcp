package executor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/voidmind-io/voidmcp/internal/protocol"
)

// GenerateTypeDefs produces TypeScript namespace declarations for all tools in
// serverTools. The output is embedded in the execute_code tool description so
// an LLM can infer correct call syntax and argument shapes.
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
func GenerateTypeDefs(serverTools map[string][]protocol.Tool) string {
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

		if isValidJSIdent(server) {
			sb.WriteString("declare namespace tools.")
			sb.WriteString(server)
			sb.WriteString(" {\n")
		} else {
			// Fall back to a bracket-notation comment for aliases with
			// special characters that would break the namespace syntax.
			sb.WriteString("// tools[\"")
			sb.WriteString(server)
			sb.WriteString("\"]\ndeclare namespace tools_")
			sb.WriteString(sanitizeIdent(server))
			sb.WriteString(" {\n")
		}

		sorted := sortedTools(tools)
		for _, t := range sorted {
			writeToolDecl(&sb, t)
		}

		sb.WriteString("}")
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
func writeToolDecl(sb *strings.Builder, tool protocol.Tool) {
	if !isValidJSIdent(tool.Name) {
		// Emit a comment — the LLM knows the tool exists but cannot call it
		// via dot-notation syntax.
		sb.WriteString("  // tools[\"<server>\"][\"")
		sb.WriteString(strings.ReplaceAll(tool.Name, "\"", "\\\""))
		sb.WriteString("\"](args)\n")
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

	sb.WriteString("}): Promise<any>;\n")
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

// sanitizeIdent replaces non-alphanumeric, non-underscore characters with
// underscores to produce a valid JS identifier from an arbitrary string.
func sanitizeIdent(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	return sb.String()
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
