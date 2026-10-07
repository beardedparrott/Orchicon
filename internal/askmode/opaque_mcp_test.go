package askmode

import "testing"

// --- THE OPAQUE MCP RULE (child 6) -------------------------------------------
//
// THE MODE POLICY'S SECOND HALF. MCP tool names are opaque to the platform: `mcp__github__create_issue`
// reveals nothing to a deny list about whether it acts. So an OPAQUE MCP tool (server != the platform's own
// `orchicon` sidecar) is treated as an ACTION — offered/executed only by a mode that MAY ACT.

// TestIsOpaqueMCPToolOnlyExcludesThePlatformSidecar pins the classifier. The one case that must NOT be opaque
// is the platform's OWN server, whose bare tool name is already in the table (that is why the adapter strips
// the prefix).
func TestIsOpaqueMCPToolOnlyExcludesThePlatformSidecar(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"mcp__github__create_issue", true},
		{"mcp__sentry__list_projects", true},
		{"MCP__GitHub__Create_Issue", true},
		{"mcp__orchicon__create_work_item", false},
		{"mcp__orchicon__get_current_conversation", false},
		{"orchicon_list_projects", false},
		{"create_work_item", false},
		{"read", false},
		{"", false},
		// Not the namespaced shape (no `__<tool>`): the classifier makes no claim.
		{"mcp__github", false},
		{"mcp__github__", false},
	}
	for _, c := range cases {
		if got := IsOpaqueMCPTool(c.name); got != c.want {
			t.Errorf("IsOpaqueMCPTool(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMayActAgreesWithTheDenialSets is the DRIFT GUARD. MayAct is derived from the table conceptually; this ties
// it to the denial sets so the flag can never drift from them. A mode may act exactly when it does not deny the
// work tools.
func TestMayActAgreesWithTheDenialSets(t *testing.T) {
	work := theWorkTools()
	for _, mode := range []string{Brainstorm, Iteration, QuickWork} {
		p, ok := PolicyFor(mode)
		if !ok {
			t.Fatalf("no policy for %q", mode)
		}
		deniesWork := false
		for tool := range work {
			if p.Denied[tool] {
				deniesWork = true
				break
			}
		}
		if want := !deniesWork; p.MayAct != want {
			t.Errorf("%s.MayAct = %v, want %v (denies a work tool = %v) — the flag drifted from the denial sets",
				mode, p.MayAct, want, deniesWork)
		}
	}
	// And the accessor agrees with the field, including the unknown-mode tolerance.
	if MayAct(Iteration) != true {
		t.Errorf("MayAct(iteration) = false, want true — it is the ONE mode that may act")
	}
	for _, mode := range []string{Brainstorm, QuickWork} {
		if MayAct(mode) {
			t.Errorf("MayAct(%s) = true, want false — it does not act", mode)
		}
	}
	for _, mode := range []string{"", "not-a-mode"} {
		if !MayAct(mode) {
			t.Errorf("MayAct(%q) = false — an unknown mode must not disable a session's tools", mode)
		}
	}
}

// TestOpaqueMCPToolsAreActionsInThePolicy is the RULE at the ONE entry point every adapter consults: an opaque MCP
// tool is allowed IFF the mode may act.
func TestOpaqueMCPToolsAreActionsInThePolicy(t *testing.T) {
	const opaque = "mcp__github__create_issue"
	for _, mode := range []string{Brainstorm, QuickWork} {
		if Allows(mode, opaque) {
			t.Errorf("%s may run %q — an opaque MCP tool is an ACTION and this mode may not act", mode, opaque)
		}
	}
	if !Allows(Iteration, opaque) {
		t.Error("iteration may not run an opaque MCP tool — that is the ONE mode that may act")
	}
	// The platform's OWN server stays classified by the table: a planner tool is still refused wherever it
	// already was, and the prefixed spelling now classifies too.
	if Allows(Iteration, "mcp__orchicon__create_work_item") {
		t.Error("iteration may run mcp__orchicon__create_work_item — the prefix stripping is what keeps the table honest")
	}
	for _, mode := range []string{Brainstorm, QuickWork} {
		if !Allows(mode, "mcp__orchicon__create_work_item") {
			t.Errorf("%s may not run the platform's own create_work_item — in-Orchicon work stays available", mode)
		}
	}
}

// TestNormalizeToolNameCollapsesBothPrefixedSpellings keeps the table's vocabulary to ONE definition.
func TestNormalizeToolNameCollapsesBothPrefixedSpellings(t *testing.T) {
	cases := map[string]string{
		"orchicon_create_work_item":       "create_work_item",
		"mcp__orchicon__create_work_item": "create_work_item",
		"create_work_item":                "create_work_item",
		"write":                           "write",
	}
	for in, want := range cases {
		if got := NormalizeToolName(in); got != want {
			t.Errorf("NormalizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
