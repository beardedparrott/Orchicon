package claude

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The Ask profile is the security boundary an interactive claude session runs under, so every row
// is pinned rather than described.
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
			// THE TABLE HAD NO Glob/Grep ROW, and that absence is the coverage gap this bug shipped through:
			// Read has a MANDATORY file_path so it always resolved a target and always reached the allow.
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

// A read must NOT be gated while an execution must be: if both were asks the profile would be a
// prompt queue, and if both were allows the consent cards would be decorative.
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
// still be delivered on the wire.
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

// NO REGRESSION: with no profile set the hook is the worker sandbox, which is non-interactive — it
// must decide, never defer.
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

// THE MATCHER MUST COVER MCP TOOLS.
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

// The END-TO-END consequence: with the matcher fixed.
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
		// And never an ASK: a worker transport installs no canUseTool handler, so asking is an unanswerable
		// prompt (the same reason the worker profile has no ask arm at all).
		if got.Ask {
			t.Errorf("worker profile asked about %s; a worker session cannot answer a prompt", tool)
		}
	}
}

// THE REPORTED BUG: an Ask session was blocked by EVERYTHING, because the two read-only tools an
// agent uses most — Glob and Grep — raised a consent card on every single call.
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

	// THE ACTION RULE IS UNCHANGED.
	for _, tool := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
		if got := DecideToolForAsk(HookInput{ToolName: tool, ToolInput: map[string]any{}}, askDir, ""); got.Decision() != DecisionAsk {
			t.Errorf("%s with no resolvable target = %s, want ask — the read fix must not turn a write into an allow",
				tool, got.Decision())
		}
	}
}
