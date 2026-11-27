package toolclass_test

// antigravity_tools_test.go — the antigravity (agy) spelling half of the classifier.
//
// WHY THIS EXISTS SEPARATELY FROM reconcile_test.go. That file pins the classifier against the two EXPORTED
// definitions of the mutating set (askmode's denial table and orchicon.ConsentMutatingTools), which are
// written in the Ask/native registry spelling. agy names the same actions differently, and the antigravity
// adapter emits the RAW agy name on an Ask permission event (that is the AC: `Tool:"run_command"`), so a
// name this package did not know would fall through to Other — "real work we cannot place", which is
// exactly wrong for a file edit.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/toolclass"
)

// TestClassifyAntigravityToolNames pins every agy spelling the adapter can emit.
func TestClassifyAntigravityToolNames(t *testing.T) {
	cases := map[string]toolclass.Class{
		// writes / edits — the recorded set and the ACP-documented set both.
		"write_to_file":              toolclass.Modify,
		"write_file":                 toolclass.Modify,
		"create_file":                toolclass.Modify,
		"replace_file_content":       toolclass.Modify,
		"multi_replace_file_content": toolclass.Modify,
		"edit_file":                  toolclass.Modify,
		"apply_patch":                toolclass.Modify,

		// reads, search and web.
		"view_file":        toolclass.Read,
		"read_file":        toolclass.Read,
		"open_file":        toolclass.Read,
		"grep_search":      toolclass.Read,
		"list_dir":         toolclass.Read,
		"find_by_name":     toolclass.Read,
		"read_url_content": toolclass.Read,
		"search_web":       toolclass.Read,
		"web_fetch":        toolclass.Read,

		// shell.
		"run_command": toolclass.Bash,
	}
	for name, want := range cases {
		if got := toolclass.Classify(name); got != want {
			t.Errorf("Classify(%q) = %v, want %v — an unclassified agy tool reports no activity", name, got, want)
		}
	}
}

// TestClassifyAntigravityAskUserIsACard is the ask-card half: agy spells the platform's own MCP server
// `mcp_orchicon_<tool>`, and a card must contribute nothing to the activity line wherever it is spelled.
func TestClassifyAntigravityAskUserIsACard(t *testing.T) {
	for _, name := range []string{
		"mcp_orchicon_orchicon_ask_user", // agy
		"mcp_orchicon_ask_user",          // agy, unprefixed tool
		"mcp__orchicon__ask_user",        // claude
		"orchicon_ask_user",              // native
		"ask_user",
	} {
		if got := toolclass.Classify(name); got != toolclass.Ignore {
			t.Errorf("Classify(%q) = %v, want Ignore — an ask card is not work", name, got)
		}
	}
	// A PLATFORM tool that is NOT a card still counts as Other (real work), not Ignore: the prefix must not
	// swallow the whole sidecar.
	if got := toolclass.Classify("mcp_orchicon_orchicon_get_work_item"); got != toolclass.Other {
		t.Errorf("Classify(mcp_orchicon_orchicon_get_work_item) = %v, want Other", got)
	}
	// And an OPERATOR's MCP tool is Other, as before.
	if got := toolclass.Classify("mcp_github_create_issue"); got != toolclass.Other {
		t.Errorf("Classify(mcp_github_create_issue) = %v, want Other", got)
	}
}
