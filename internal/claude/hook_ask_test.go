package claude

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The Ask profile is the security boundary an interactive claude session runs
// under, so every row is pinned rather than described. The reference is
// opencode's interactive profile (internal/opencode/config.go), which the
// platform already trusts for Ask.
func TestDecideToolForAsk(t *testing.T) {
	const askDir = "/tmp/orchicon-ask-t"

	cases := []struct {
		name   string
		tool   string
		input  map[string]any
		want   string
		reason string
	}{
		{
			name: "never-allow class is DENIED, not offered",
			tool: "Bash",
			// sudo is in the shared never-allow class. A card here would be a lie:
			// the OS-level guard shim refuses this command regardless of consent.
			input:  map[string]any{"command": "sudo rm -rf /tmp/target"},
			want:   DecisionDeny,
			reason: "the never-allow class must stay non-waivable",
		},
		{
			name:   "an ordinary execution is ASKED about",
			tool:   "Bash",
			input:  map[string]any{"command": "rm -rf /tmp/target"},
			want:   DecisionAsk,
			reason: "an execution is the operator's decision in an interactive session",
		},
		{
			name:   "a read-only command is still asked about",
			tool:   "Bash",
			input:  map[string]any{"command": "ls -la /tmp"},
			want:   DecisionAsk,
			reason: "opencode's interactive profile has no catch-all, so unmatched bash stays an ask",
		},
		{
			name:   "the subagent tool is DENIED in every profile",
			tool:   "Task",
			input:  map[string]any{},
			want:   DecisionDeny,
			reason: "opencode denies `task` in the interactive profile too",
		},
		{
			name:   "a read outside the directory is ALLOWED",
			tool:   "Read",
			input:  map[string]any{"file_path": "/tmp/somewhere/else.txt"},
			want:   DecisionAllow,
			reason: "external_directory is allowed in the interactive profile — consent gates the ACTION, not the scope",
		},
		{
			// THE TABLE HAD NO Glob/Grep ROW, and that absence is the coverage gap this bug
			// shipped through: Read has a MANDATORY file_path so it always resolved a target and
			// always reached the allow, while Glob/Grep name theirs optionally and were never
			// exercised. Two of the three read-only tools were therefore untested.
			name:   "a Glob with a relative pattern is ALLOWED — no resolvable target",
			tool:   "Glob",
			input:  map[string]any{"pattern": "**/*.go"},
			want:   DecisionAllow,
			reason: "a read is not a decision, and this pattern names no absolute path for the policy to judge",
		},
		{
			name:   "a Grep with a bare regex is ALLOWED",
			tool:   "Grep",
			input:  map[string]any{"pattern": "func DecideToolForAsk"},
			want:   DecisionAllow,
			reason: "the same shape: the search root is the session's own directory",
		},
		{
			name:   "a Grep that names an absolute path is ALLOWED, but takes the policy rung",
			tool:   "Grep",
			input:  map[string]any{"pattern": "/tmp/somewhere/else"},
			want:   DecisionAllow,
			reason: "external_directory is allowed interactively, and the target IS resolvable so policy/protected-path still run",
		},
		{
			name:   "a write is ASKED about",
			tool:   "Write",
			input:  map[string]any{"file_path": "/tmp/somewhere/else.txt"},
			want:   DecisionAsk,
			reason: "opencode's interactive profile sets edit/write to ask",
		},
		{
			name:   "an edit is ASKED about",
			tool:   "Edit",
			input:  map[string]any{"file_path": "/tmp/somewhere/else.txt"},
			want:   DecisionAsk,
			reason: "opencode's interactive profile sets edit/write to ask",
		},
		{
			name:   "an unrecognised tool is ASKED about, never assumed",
			tool:   "SomeFutureTool",
			input:  map[string]any{},
			want:   DecisionAsk,
			reason: "the interactive default is consent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideToolForAsk(HookInput{ToolName: tc.tool, ToolInput: tc.input}, askDir, "")
			if d := got.Decision(); d != tc.want {
				t.Fatalf("DecideToolForAsk(%s %v) = %s, want %s (%s)",
					tc.tool, tc.input, d, tc.want, tc.reason)
			}
		})
	}
}

// A read must NOT be gated while an execution must be: if both were asks the
// profile would be a prompt queue, and if both were allows the consent cards
// would be decorative. This pins the distinction that makes the profile useful.
func TestAskProfileSeparatesReadsFromActions(t *testing.T) {
	read := DecideToolForAsk(HookInput{ToolName: "Read", ToolInput: map[string]any{"file_path": "/etc/hosts"}}, "/tmp/ask", "")
	exec := DecideToolForAsk(HookInput{ToolName: "Bash", ToolInput: map[string]any{"command": "ls"}}, "/tmp/ask", "")
	if read.Decision() != DecisionAllow {
		t.Errorf("a read = %s, want allow", read.Decision())
	}
	if exec.Decision() != DecisionAsk {
		t.Errorf("an execution = %s, want ask", exec.Decision())
	}
}

// The zero value of a verdict must never read as an approval. A rule set that
// forgot to set a flag is a bug, and the fail-closed reading of it is deny.
func TestHookVerdictZeroValueIsDeny(t *testing.T) {
	var v HookVerdict
	if got := v.Decision(); got != DecisionDeny {
		t.Fatalf("zero HookVerdict.Decision() = %q, want %q (fail closed)", got, DecisionDeny)
	}
	if got := allowVerdict().Decision(); got != DecisionAllow {
		t.Errorf("allowVerdict().Decision() = %q, want allow", got)
	}
	if got := denyVerdict("r", "why").Decision(); got != DecisionDeny {
		t.Errorf("denyVerdict().Decision() = %q, want deny", got)
	}
	if got := askVerdict().Decision(); got != DecisionAsk {
		t.Errorf("askVerdict().Decision() = %q, want ask", got)
	}
}

// AN ASK IS DELIVERED BY ABSTAINING — THE HOOK MUST EMIT NOTHING.
//
// This test used to require `permissionDecision: "ask"` on the wire, on the belief that "the CLI only
// consults its permission flow ... when the hook says ask rather than allowing the call outright".
// THAT BELIEF WAS WRONG, and it is the bug this change fixes: MEASURED against the real CLI
// (2.1.289) on a real Ask session, a hook's "ask" becomes an immediate TOOL ERROR to the model
// ("this call needs the operator's approval") and NO `can_use_tool` control frame is ever raised —
// 18 asks produced 0 control frames and 0 consent cards. The operator: "claude adapter ask sessions
// seem blocked by everything."
//
// `--permission-prompts host` governs the PERMISSION SYSTEM's prompts, not a hook verdict. So the
// tools that need an operator decision are named in `permissions.ask` (BuildSettings,
// AskPermissionToolNames) and this hook stays SILENT about them — empty output, exit 0, which the CLI
// reads as "no decision" and then applies its own permission flow to.
//
// THE INVARIANT THIS TEST WAS WRITTEN FOR IS UNCHANGED: a Bash call in the Ask profile must reach the
// operator as a card rather than being silently allowed or silently refused. What changed is the
// MECHANISM, and the new assertion is therefore stricter about the failure that actually occurred —
// any output here at all would re-break the card path.
func TestRunHookAbstainsSoThePermissionSystemCanAsk(t *testing.T) {
	env := map[string]string{
		HookProfileEnv: ProfileAskEnvValue,
		AskDirEnv:      "/tmp/orchicon-ask-t",
	}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"touch /tmp/x"},"cwd":"/tmp/orchicon-ask-t"}`

	var out bytes.Buffer
	code := RunHook(strings.NewReader(payload), &out, func(k string) string { return env[k] })
	if code != 0 {
		t.Fatalf("RunHook exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("the hook emitted %q for an ask, want NO output.\n\nAn ask verdict is NOT routed to "+
			"--permission-prompts host: the CLI turns it into a tool error the model reads as "+
			"\"this call needs the operator's approval\" and NO card is raised (measured: 18 asks, 0 "+
			"can_use_tool frames). The card comes from the permission system, which is why this profile "+
			"names its prompt-tools in permissions.ask — see permissions.go, AskPermissionToolNames.\n\n"+
			"Emit a verdict here and the operator is back to a session blocked on every write and every "+
			"command.", got)
	}
}

// AND THE OTHER DIRECTION, so this fix cannot be satisfied by emitting nothing at all: a DENY must
// still be delivered on the wire. The hook is silent only about ASKS; its refusals are the thing the
// settings document cannot express, so they must reach the CLI.
func TestRunHookStillEmitsDenyInTheAskProfile(t *testing.T) {
	env := map[string]string{HookProfileEnv: ProfileAskEnvValue}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"sudo rm -rf /tmp/x"}}`

	var out bytes.Buffer
	RunHook(strings.NewReader(payload), &out, func(k string) string { return env[k] })

	var doc hookOutput
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("a refusal must still be emitted on the wire (abstaining is for asks only): %v (%q)", err, out.String())
	}
	if got := doc.HookSpecificOutput.PermissionDecision; got != DecisionDeny {
		t.Fatalf("permissionDecision = %q, want deny — abstaining must not swallow a refusal", got)
	}
	if doc.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", doc.HookSpecificOutput.HookEventName)
	}
	if doc.HookSpecificOutput.PermissionDecisionReason == "" {
		t.Error("a deny with no reason gives the model nothing to act on")
	}
}

// The never-allow class is denied through the WIRE too, not just in the pure
// decision — this is the path a real session takes.
func TestRunHookDeniesNeverAllowInTheAskProfile(t *testing.T) {
	env := map[string]string{HookProfileEnv: ProfileAskEnvValue}
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"sudo rm -rf /tmp/x"}}`

	var out bytes.Buffer
	RunHook(strings.NewReader(payload), &out, func(k string) string { return env[k] })

	var doc hookOutput
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("hook output is not valid JSON: %v", err)
	}
	if got := doc.HookSpecificOutput.PermissionDecision; got != DecisionDeny {
		t.Fatalf("permissionDecision = %q, want deny for the never-allow class", got)
	}
}

// NO REGRESSION: with no profile set the hook is the worker sandbox, which is
// non-interactive — it must decide, never defer. A worker "ask" would be an
// unanswerable prompt (no canUseTool handler is installed on that transport),
// i.e. a hang or a silent refusal where a real verdict was intended.
func TestRunHookDefaultsToTheWorkerProfile(t *testing.T) {
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/project/main.go"},"cwd":"/project"}`

	var out bytes.Buffer
	RunHook(strings.NewReader(payload), &out, func(string) string { return "" })

	var doc hookOutput
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("hook output is not valid JSON: %v", err)
	}
	if got := doc.HookSpecificOutput.PermissionDecision; got == DecisionAsk {
		t.Fatal("the worker profile emitted an ask; that transport installs no handler, so the call would hang or auto-deny")
	}
}

// THE MATCHER MUST COVER MCP TOOLS. A tool the matcher does not name never
// reaches this hook, so its verdict — including the allow that makes the
// platform's own tools usable — never happens, and claude denies the call with
// "requested permissions ... but you haven't granted it yet". Reproduced live
// against the real CLI before this was fixed: the hook was not invoked at all.
func TestHookMatcherCoversMCPTools(t *testing.T) {
	if !strings.Contains(HookToolMatcher, "mcp__") {
		t.Fatalf("HookToolMatcher = %q has no MCP pattern, so no mcp__* tool ever reaches the hook", HookToolMatcher)
	}
	// The matcher is a regex evaluated against the tool name, so assert the
	// SHAPES it must match rather than the literal text.
	re, err := regexp.Compile("^(?:" + HookToolMatcher + ")$")
	if err != nil {
		t.Fatalf("HookToolMatcher is not a valid regex: %v", err)
	}
	for _, name := range []string{
		"mcp__orchicon__list_work_items",
		"mcp__orchicon__get_current_conversation",
		"mcp__github__create_issue",
		"Bash", "Read", "Write", "Task", "TodoWrite",
	} {
		if !re.MatchString(name) {
			t.Errorf("the matcher does not cover %q — the hook would never see it", name)
		}
	}
}

// The END-TO-END consequence: with the matcher fixed, an orchicon MCP tool must
// produce an ALLOW from the Ask profile (not an ask, and never a deny), which is
// what makes list_work_items readable.
func TestAskProfileAllowsOrchiconMCPToolsThroughTheMatcher(t *testing.T) {
	if !regexp.MustCompile("^(?:" + HookToolMatcher + ")$").MatchString("mcp__orchicon__list_work_items") {
		t.Skip("matcher does not cover MCP tools; the test above already failed")
	}
	got := DecideToolForAsk(HookInput{ToolName: "mcp__orchicon__list_work_items", ToolInput: map[string]any{}}, "/tmp/ask", "")
	if d := got.Decision(); d != DecisionAllow {
		t.Fatalf("mcp__orchicon__list_work_items = %s, want allow — a read-only platform tool must not need a card", d)
	}
}

// A WORKER may use MCP tools, and this is PARITY, not a concession.
//
// It was worth pinning because the claude path once DENIED every MCP tool to a
// worker — but only because of claude's own matcher gap (the hook never fired, so
// claude's own permission flow denied with nobody to grant). That was a
// claude-only defect; the other two transports have always allowed it:
//
//	NATIVE   — mcpTools.Defs copies every discovered MCP tool into the worker's
//	           tool list verbatim and Execute routes straight to the server: no
//	           askmode check, no permission check, no path check (bridge.go).
//	OPENCODE — the worker permission map carries NO MCP key at all, and its
//	           composite MCP sidecar is "always registered alongside", so the
//	           batch tools a worker depends on are usable (config.go).
//
// So "allow" is what the platform does, and a claude-only restriction here would
// be exactly the adapter drift to avoid. The containment for an MCP tool is the
// SERVER's own scoping plus the operator's choice to configure it — the same
// position both other adapters take, and the reason neither the hook nor the OS
// shim can judge one (an MCP server writes files itself and never invokes a
// PATH-scoped binary).
func TestWorkerProfileAllowsMCPTools(t *testing.T) {
	for _, tool := range []string{
		"mcp__orchicon__list_work_items",
		"mcp__orchicon__create_work_item",
		"mcp__operator_configured__write_file",
	} {
		got := DecideTool(HookInput{ToolName: tool, ToolInput: map[string]any{}}, "", "")
		if d := got.Decision(); d != DecisionAllow {
			t.Errorf("worker profile: %s = %s, want allow — native and opencode both permit MCP tools to a worker", tool, d)
		}
		// And never an ASK: a worker transport installs no canUseTool handler, so
		// asking is an unanswerable prompt (the same reason the worker profile has
		// no ask arm at all).
		if got.Ask {
			t.Errorf("worker profile asked about %s; a worker session cannot answer a prompt", tool)
		}
	}
}

// THE REPORTED BUG: an Ask session was blocked by EVERYTHING, because the two
// read-only tools an agent uses most — Glob and Grep — raised a consent card on
// every single call.
//
// THE MECHANISM IS AN ORDERING BUG, not a wrong rule. decidePathForAsk returned
// askVerdict() the moment no target could be resolved, and that guard sat BEFORE
// the readOnlyToolNames allow — so the allow at the bottom of the function was
// UNREACHABLE for exactly the tools whose input does not name a path.
//
// Glob and Grep take an OPTIONAL `path` plus a `pattern`. The normal call omits
// the path and passes a RELATIVE pattern (`**/*.go`, `func DecideToolForAsk`),
// and dirOfPattern returns "" for anything that is not absolute. So the COMMON
// call was the asking call, and an interactive session became the prompt queue
// this file's own comment says it must never be.
//
// THE DIVERGENCE IS BACKWARDS, which is what makes this a defect rather than a
// policy choice: the WORKER profile — the sandboxed, non-interactive one —
// ALLOWS a path tool whose target does not resolve (decidePath: "empty target ->
// allow"), and its tests pin that shape. The INTERACTIVE profile, whose whole
// premise is that a read is not a decision the operator needs to make, was
// therefore STRICTER THAN THE SANDBOX.
func TestReadOnlyToolsAreAllowedWhenNoTargetResolves(t *testing.T) {
	const askDir = "/tmp/orchicon-ask-t"

	reads := []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{"Glob with a relative pattern (the common call)", "Glob", map[string]any{"pattern": "**/*.go"}},
		{"Grep with a bare regex", "Grep", map[string]any{"pattern": "func DecideToolForAsk"}},
		{"Grep relative with a glob filter", "Grep", map[string]any{"pattern": "TODO", "glob": "*.go"}},
		{"Glob with no input at all", "Glob", map[string]any{}},
	}
	for _, tc := range reads {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideToolForAsk(HookInput{ToolName: tc.tool, ToolInput: tc.input}, askDir, "")
			if d := got.Decision(); d != DecisionAllow {
				t.Errorf("%s %v = %s, want allow.\nA READ CANNOT CHANGE ANYTHING, and this profile's own contract "+
					"is that reads are not gated (readOnlyToolNames: \"consenting to an assistant and then "+
					"being asked before every file it reads is not consent, it is a prompt queue\"). Asking here "+
					"is what blocked an Ask session on every Glob/Grep — the worker profile allows this very "+
					"shape, so the interactive profile must not be the stricter one.", tc.tool, tc.input, d)
			}
		})
	}

	// THE ACTION RULE IS UNCHANGED. A write with no resolvable target is still the
	// operator's decision, so the fix cannot have widened into writes — the same
	// guard, reached with a tool that is NOT read-only.
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
		if got := DecideToolForAsk(HookInput{ToolName: tool, ToolInput: map[string]any{}}, askDir, ""); got.Decision() != DecisionAsk {
			t.Errorf("%s with no resolvable target = %s, want ask — the read fix must not turn a write into an allow",
				tool, got.Decision())
		}
	}
}
