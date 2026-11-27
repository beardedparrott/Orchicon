package antigravity

// toolnames.go — agy's tool vocabulary, and how it maps onto the canonical names the rest of the
// platform reads.
//
// WHY CANONICAL NAMES REACH THE CALLBACK. `OnToolCall`, the diff ledger, the todo parser and the
// execution detail pages are all adapter-neutral: they were built for opencode and claude and today
// key on `bash`/`write`/`edit`/`read`/`todowrite`. Emitting agy's own spellings (`run_command`,
// `write_file`, …) would mean a second parser branch in every one of those readers — the exact drift
// internal/claude/parse.go:619 (canonicalToolName) exists to prevent for claude. So the mapper
// canonicalises, and the RAW agy name is kept alongside the part payload (`agy_name`) and in the
// tool-call input, so nothing is lost for the UI or a human reading the transcript.
//
// BOTH SPELLINGS ARE ACCEPTED. The recorded agy 1.5.13 surface is
// run_command / write_file / edit_file / read_file / ask_question / mcp_<server>_<tool>, while the
// work item and ACP documentation also name write_to_file, replace_file_content,
// multi_replace_file_content, view_file, grep_search, search_web and read_url_content. A table that
// knew only one set would silently classify half its tool calls as "other", and a "write" that is not
// classified as a write never fires OnWrittenFiles or the diff ledger.

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// canonicalToolNames maps every agy spelling onto the platform's canonical name.
//
// The ones with no canonical equivalent (ask_question, MCP tools, invoke_subagent) map onto
// THEMSELVES: an unrecognised name is allowed by the permission policy (claude parity) and shown as
// its own tool in the UI, which is the honest rendering.
var canonicalToolNames = map[string]string{
	// command execution
	"run_command": "bash",
	"bash":        "bash",

	// whole-file writes
	"write_file":    "write",
	"write_to_file": "write",
	"write":         "write",
	"create_file":   "write",

	// in-place edits
	"edit_file":                  "edit",
	"replace_file_content":       "edit",
	"multi_replace_file_content": "edit",
	"apply_patch":                "edit",
	"edit":                       "edit",

	// reads
	"read_file": "read",
	"view_file": "read",
	"read":      "read",
	"open_file": "read",

	// search / navigation
	"grep_search":    "grep",
	"grep":           "grep",
	"list_dir":       "ls",
	"list_directory": "ls",
	"find_by_name":   "glob",
	"glob":           "glob",

	// web
	"search_web":       "websearch",
	"web_search":       "websearch",
	"read_url_content": "webfetch",
	"web_fetch":        "webfetch",
}

// toolKindWrites reports whether an agy tool call's KIND ("edit") makes it a file-writing tool, which
// is what drives OnWrittenFiles and the diff ledger. It is a fallback for a name this table does not
// know: agy sets `kind:"edit"` on an edit tool even when its name is new to us.
func toolKindWrites(kind string) bool { return strings.EqualFold(strings.TrimSpace(kind), "edit") }

// canonicalToolName maps one agy tool name onto the platform's canonical name. An unknown name is
// returned unchanged (trimmed), so an MCP tool (`mcp_orchicon_ask_user`) or a future agy
// built-in still reaches the callbacks and the permission policy as itself.
func canonicalToolName(name string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		return ""
	}
	if c, ok := canonicalToolNames[strings.ToLower(n)]; ok {
		return c
	}
	return n
}

// isMCPTool reports whether an agy tool name belongs to ANY MCP server.
//
// agy's MCP spelling is `mcp_<server>_<tool>` (mcp_tool_call.jsonl: `mcp_echo_echo`), which is a
// PREFIX, not claude's `mcp__<server>__<tool>` double underscore. BOTH are recognised: the agent's own
// spelling is what the wire carries today, and a client-visible alias of claude's form must not slip
// past a policy that reasons about MCP tools.
func isMCPTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(n, "mcp_") || strings.HasPrefix(n, "mcp__")
}

// isOrchiconMCPTool reports whether an agy tool name is one of the PLATFORM's own MCP tools. It has to
// accept both agy's `mcp_orchicon_<tool>` and claude's `mcp__orchicon__<tool>` for the same reason.
func isOrchiconMCPTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(n, "mcp_orchicon_") || strings.HasPrefix(n, "mcp__orchicon__")
}

// isFileEditTool reports whether a canonical name is one of the file-writing built-ins whose edit the
// shared diff pipeline must ledger. It mirrors claude's ledgersFileEdit (parse.go).
func isFileEditTool(canonical string) bool { return canonical == "write" || canonical == "edit" }

// agyInput reads a tool call's arguments into a map, tolerating the three shapes agy uses:
// `rawInput` (the recorded one), `args` (the recorded duplicate) and both being absent.
func agyInput(rawInput, args json.RawMessage) map[string]any {
	for _, raw := range []json.RawMessage{rawInput, args} {
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err == nil && len(m) > 0 {
			return m
		}
	}
	return nil
}

// firstString returns the first non-empty string value among keys, compared case-INSENSITIVELY.
// agy's args are Capitalised (`Path`, `CommandLine`, `Content`) while the ACP-standard tools use
// lower snake; a lookup that knew only one case would extract no path from half the calls, and an
// extracted path is what the ledger, the written-files callback and the boundary check all need.
func firstString(in map[string]any, keys ...string) string {
	if in == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := in[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	for key, v := range in {
		lk := strings.ToLower(key)
		for _, k := range keys {
			if lk != strings.ToLower(k) {
				continue
			}
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// toolInputPath is the filesystem target a tool call names, across agy's spellings. "" means the call
// named no path at all (a command, a web fetch, an MCP tool).
func toolInputPath(in map[string]any) string {
	return firstString(in,
		"Path", "path", "file_path", "AbsolutePath", "absolute_path",
		"TargetFile", "target_file", "SearchPath", "search_path",
		"filePath", "notebook_path", "DirectoryPath", "directory_path",
	)
}

// toolInputCommand is the shell command a run_command call asked for.
func toolInputCommand(in map[string]any) string {
	return firstString(in, "CommandLine", "command_line", "command", "Command", "cmd")
}

// toolInputContent is the whole-file content a write tool asked to write.
func toolInputContent(in map[string]any) string {
	if s := firstString(in, "Content", "content", "CodeContent", "code_content", "file_text"); s != "" {
		return s
	}
	return ""
}

// ledgerPathFor makes an absolute tool path execution-dir-relative, which is the form the shared
// file-edit ledger hook reads (claude's ledgerPath does the same for claude's paths). A path outside
// the execution dir is returned unchanged — the ledger is not the place to reinterpret it.
func ledgerPathFor(execDir, p string) string {
	if p == "" || execDir == "" || !filepath.IsAbs(p) {
		return p
	}
	base := filepath.Clean(execDir)
	rel, err := filepath.Rel(base, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}

// artifactTypeFromPath tags an artifact by its extension. Deliberately identical to opencode's and
// claude's labelling so the same file gets the same badge whichever adapter wrote it.
func artifactTypeFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, ".md"), strings.HasSuffix(path, ".markdown"):
		return "markdown"
	case strings.HasSuffix(path, ".json"):
		return "json"
	case strings.HasSuffix(path, ".yaml"), strings.HasSuffix(path, ".yml"):
		return "yaml"
	case strings.HasSuffix(path, ".html"), strings.HasSuffix(path, ".htm"):
		return "html"
	case strings.HasSuffix(path, ".csv"):
		return "csv"
	case strings.HasSuffix(path, ".xml"):
		return "xml"
	case strings.HasSuffix(path, ".svg"):
		return "svg"
	default:
		return "text"
	}
}
