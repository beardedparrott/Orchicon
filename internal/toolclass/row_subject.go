// Package toolclass — row_subject.go — the ONE-LINE SUBJECT of a tool call's transcript row.
//
// WHY A SHARED FUNCTION RATHER THAN A RENDERER EACH. A tool row has room for one line beside the tool's
// name, and that line is what makes a transcript readable: "read /p/a.go" instead of a JSON dump, "bash
// make test" instead of {"CommandLine":"make test"}. The GUI and the TUI draw the same transcript from
// the same recorded parts, so two renderers would be two answers to "what was this call about" — and the
// drifted one is a bug an operator sees and cannot explain.
//
// THE AGY SPELLINGS ARE THE REASON THIS EXISTS NOW. internal/antigravity canonicalises `run_command` to
// `bash`, `read_url_content` to `webfetch`, `grep_search` to `grep` and so on, but the recorded part's
// input keeps agy's OWN argument keys (`CommandLine`, `TargetFile`, `Url`, `query`). A renderer that read
// only opencode's keys (`command`, `file_path`) therefore showed an EMPTY subject for every agy call —
// the row said "⚙ bash" and nothing else.
//
// PINNED CROSS-CLIENT: row_subject_fixture.json is read VERBATIM here and by the GUI's Vitest suite
// (frontend/src/lib/ask-tool-summary.test.ts), so the two implementations cannot drift.
package toolclass

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// subjectKeys is the ordered list of argument keys that carry a tool call's SUBJECT, per subject kind.
//
// IT IS ORDERED AND CROSS-VENDOR ON PURPOSE: the first non-empty value wins, so a call carrying both
// opencode's `command` and agy's `CommandLine` renders the same line either way, and a future adapter's
// spelling is one entry rather than a new branch.
var (
	commandKeys = []string{"CommandLine", "command_line", "command", "Command", "cmd", "script"}
	pathKeys    = []string{
		"Path", "path", "file_path", "AbsolutePath", "absolute_path",
		"TargetFile", "target_file", "SearchPath", "search_path", "filePath",
		"notebook_path", "DirectoryPath", "directory_path",
	}
	urlKeys   = []string{"Url", "url", "URL", "uri", "href"}
	queryKeys = []string{"query", "Query", "search_query", "searchQuery", "pattern", "Pattern", "q"}
	// patternKeys is checked for read/grep only: a grep's `pattern` is its subject when no path is named,
	// and a `q` on a grep is a pattern, not a free-text question.
)

// RowSubject returns the one-line subject to draw beside a tool call's name, or "" when the input
// carries nothing a human should read.
//
// It is PURE and TOTAL: a nil/empty/malformed input yields "", and it never panics. A name it does not
// know falls back to the first string value it can find, because an unknown adapter's tool is still a
// tool whose arguments are worth one line.
func RowSubject(toolName, inputJSON string) string {
	in := decodeInput(inputJSON)
	if len(in) == 0 {
		return ""
	}
	switch Classify(toolName) {
	case Bash:
		return firstOf(in, commandKeys...)
	case Modify, Read:
		// A read/grep with no path names its pattern instead: `grep TODO` is the subject, `grep` is not.
		if p := firstOf(in, pathKeys...); p != "" {
			return p
		}
		return firstOf(in, queryKeys...)
	case Other:
		// Web tools and everything unclassified: URL, then query, then command, then path.
		for _, keys := range [][]string{urlKeys, queryKeys, commandKeys, pathKeys} {
			if v := firstOf(in, keys...); v != "" {
				return v
			}
		}
		return firstScalar(in)
	}
	return ""
}

// IsWebTool reports whether a canonical tool name is one of the web tools, so a renderer can label the
// subject ("fetch <url>") rather than leaving a bare URL beside a generic name.
func IsWebTool(toolName string) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "webfetch", "web_fetch", "read_url_content", "websearch", "web_search", "search_web":
		return true
	}
	return false
}

// decodeInput parses a tool input. It tolerates the two shapes this platform records: a JSON object
// (the usual case) and a bare JSON string (an adapter that reported its whole input as text).
func decodeInput(inputJSON string) map[string]any {
	s := strings.TrimSpace(inputJSON)
	if s == "" || s == "null" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err == nil {
		return obj
	}
	var bare string
	if err := json.Unmarshal([]byte(s), &bare); err == nil {
		// A bare string IS the subject for a bash call; keep it under the first command key so the
		// per-class order below still applies.
		return map[string]any{commandKeys[0]: bare}
	}
	return nil
}

// firstOf returns the first non-empty STRING value among keys.
func firstOf(in map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := in[k]; ok {
			if s := asString(v); strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// firstScalar returns the first non-empty string-valued entry in a STABLE key order, so an unknown
// tool's subject does not change between two renders of the same call.
func firstScalar(in map[string]any) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s := asString(in[k]); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// asString renders a JSON-decoded value as the string a row can show. Objects and arrays are NOT
// flattened: a row that printed a whole file body would defeat the point of a one-line subject.
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return trimFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// trimFloat renders a JSON number without a trailing ".0" (a `line: 1` should not read `1.0`).
func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// CollapseWhitespace flattens a multi-line subject (a heredoc command) onto one row, so a tool row can
// never break the transcript's row geometry.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
