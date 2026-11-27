package antigravity

// permissions_test.go — AC2: the worker permission policy, as a table.
//
// The table IS the acceptance criterion: `rm -rf /` refused, a path outside the project refused, the
// subagent tools refused, and an ordinary `go test ./...` in the worktree allowed. Each row is an agy
// tool call as agy sends it (recorded arg spellings), so the test exercises the real translation from
// agy's vocabulary into the shared rule set rather than a hand-written claude-shaped input.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

// call builds a ToolCallInfo the way agy sends one: args + rawInput both carrying the recorded
// Capitalised keys.
func call(name, kind string, args map[string]any, locations ...string) acp.ToolCallInfo {
	raw, _ := json.Marshal(args)
	c := acp.ToolCallInfo{
		ToolCallID: "call_1",
		Name:       name,
		Kind:       kind,
		Status:     acp.StatusInProgress,
		Args:       raw,
		RawInput:   raw,
	}
	for _, l := range locations {
		c.Locations = append(c.Locations, acp.Location{Path: l})
	}
	return c
}

// newPolicyDir builds a project root with a worktree under it, so the boundary, the carve-outs and the
// worktree-as-cwd case are all exercised on real paths.
func newPolicyDir(t *testing.T) (projectDir, worktreeDir string) {
	t.Helper()
	projectDir = t.TempDir()
	worktreeDir = filepath.Join(projectDir, ".orchicon-worktrees", "run_1")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	return projectDir, worktreeDir
}

// TestBridge_PermissionPolicyTable is AC2. Every row names what agy would send and what the worker
// profile must answer.
func TestBridge_PermissionPolicyTable(t *testing.T) {
	projectDir, worktreeDir := newPolicyDir(t)
	h := &PolicyHandler{
		ProjectDir:  projectDir,
		WorktreeDir: worktreeDir,
		Log:         quiet(),
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")

	cases := []struct {
		name    string
		call    acp.ToolCallInfo
		allow   bool
		rule    string
		comment string
	}{
		{
			name:    "never-allow: rm -rf /",
			call:    call("run_command", "execute", map[string]any{"CommandLine": "rm -rf /"}),
			allow:   false,
			rule:    "rm -rf /",
			comment: "the shared never-allow class, from workerrestrict.MatchCommand — the rule reported is the pattern that matched",
		},
		{
			name:    "never-allow: rm -rf ~",
			call:    call("run_command", "execute", map[string]any{"CommandLine": "rm -rf ~"}),
			allow:   false,
			comment: "the home-wipe spelling",
		},
		{
			name:    "outside the project: write_file to /etc/passwd",
			call:    call("write_file", "edit", map[string]any{"Path": "/etc/passwd", "Content": "x"}),
			allow:   false,
			comment: "a path outside the project and outside every carve-out",
		},
		{
			name:    "outside the project: write via a location only",
			call:    call("write_file", "edit", map[string]any{"Content": "x"}, outside),
			allow:   false,
			comment: "the path arrives only on locations[] — it must still be judged",
		},
		{
			name:    "outside the project: edit_file on an absolute outside path",
			call:    call("edit_file", "edit", map[string]any{"Path": outside, "Target": "a", "Replacement": "b"}),
			allow:   false,
			comment: "edit spellings are judged the same way as writes",
		},
		{
			name:    "subagent: invoke_subagent",
			call:    call("invoke_subagent", "other", map[string]any{"prompt": "do it"}),
			allow:   false,
			rule:    "task",
			comment: "agy's analogue of claude's Task tool",
		},
		{
			name:    "subagent: define_subagent",
			call:    call("define_subagent", "other", map[string]any{"name": "x"}),
			allow:   false,
			rule:    "task",
			comment: "registering a subagent is the same capability one step removed",
		},
		{
			name:    "allowed: go test ./... in the worktree",
			call:    call("run_command", "execute", map[string]any{"CommandLine": "go test ./..."}),
			allow:   true,
			comment: "the ordinary build gate a worker must be able to run",
		},
		{
			name: "allowed: edit_file inside the worktree",
			call: call("edit_file", "edit", map[string]any{
				"Path": filepath.Join(worktreeDir, "main.go"), "Target": "a", "Replacement": "b",
			}),
			allow:   true,
			comment: "a write inside the session cwd",
		},
		{
			name: "allowed: write_file inside the worktree",
			call: call("write_file", "edit", map[string]any{
				"Path": filepath.Join(worktreeDir, "hello.txt"), "Content": "hi",
			}),
			allow:   true,
			comment: "the AC5 live goal's first action",
		},
		{
			name: "allowed: read_file outside the project",
			call: call("read_file", "read", map[string]any{
				"Path": filepath.Join(worktreeDir, "go.mod"),
			}),
			allow:   true,
			comment: "a read inside scope",
		},
		{
			name:    "allowed: write under the .orchicon carve-out",
			call:    call("write_file", "edit", map[string]any{"Path": filepath.Join(projectDir, ".orchicon", "run_1", "notes.md"), "Content": "x"}),
			allow:   true,
			comment: "run metadata is a carve-out, exactly as for claude and opencode",
		},
		{
			name:    "allowed: write under the scratch carve-out",
			call:    call("write_file", "edit", map[string]any{"Path": "/tmp/orchicon/shot.png", "Content": "x"}),
			allow:   true,
			comment: "workerrestrict.ScratchDir",
		},
		{
			name:    "allowed: the platform's own MCP tool",
			call:    call("mcp_orchicon_orchicon_ask_user", "other", map[string]any{"question": "which?"}),
			allow:   true,
			comment: "agy spells MCP tools mcp_<server>_<tool>; an unrecognised name is allowed (claude parity)",
		},
		{
			name:    "allowed: the operator's MCP server",
			call:    call("mcp_echo_echo", "other", map[string]any{"text": "hi"}),
			allow:   true,
			comment: "third-party MCP tools are allowed in the worker profile",
		},
		{
			name:    "allowed: grep_search",
			call:    call("grep_search", "read", map[string]any{"SearchPath": worktreeDir, "Query": "func "}),
			allow:   true,
			comment: "a search inside the worktree",
		},
		{
			name:    "allowed: ask_question",
			call:    call("ask_question", "other", map[string]any{"questions": []any{}}),
			allow:   true,
			comment: "agy renders the question itself; there is no answer channel to gate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := h.Decide(tc.call)
			if v.Allow != tc.allow {
				t.Fatalf("Decide(%s) allow = %v, want %v (rule %q reason %q) — %s",
					tc.call.Name, v.Allow, tc.allow, v.Rule, v.Reason, tc.comment)
			}
			if tc.rule != "" && v.Rule != tc.rule {
				t.Errorf("Decide(%s) rule = %q, want %q (%s)", tc.call.Name, v.Rule, tc.rule, tc.comment)
			}
			if !v.Allow && strings.TrimSpace(v.Reason) == "" {
				t.Errorf("a refusal must carry a reason; Decide(%s) returned none", tc.call.Name)
			}
		})
	}
}

// TestBridge_PermissionOperatorPolicyDeny proves the operator's permpolicy deny list is honoured, and
// that an UNREADABLE policy fails closed rather than silently allowing everything.
func TestBridge_PermissionOperatorPolicyDeny(t *testing.T) {
	projectDir, worktreeDir := newPolicyDir(t)
	policyPath := filepath.Join(t.TempDir(), "permission-policy.yaml")
	denied := filepath.Join(worktreeDir, "secrets.env")
	if err := os.WriteFile(policyPath, []byte("deny:\n  - "+denied+"\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	h := &PolicyHandler{ProjectDir: projectDir, WorktreeDir: worktreeDir, PolicyPath: policyPath, Log: quiet()}

	if v := h.Decide(call("read_file", "read", map[string]any{"Path": denied})); v.Allow {
		t.Fatalf("a path named by the operator's deny list was allowed (rule %q)", v.Rule)
	}

	// A GARBLED POLICY IS A FAIL-CLOSED CONDITION, not "no policy": the operator wrote a deny list and
	// would otherwise be silently unguarded.
	garbled := filepath.Join(t.TempDir(), "garbled.yaml")
	if err := os.WriteFile(garbled, []byte("deny: [unclosed\n"), 0o600); err != nil {
		t.Fatalf("write garbled policy: %v", err)
	}
	hg := &PolicyHandler{ProjectDir: projectDir, WorktreeDir: worktreeDir, PolicyPath: garbled, Log: quiet()}
	v := hg.Decide(call("write_file", "edit", map[string]any{"Path": filepath.Join(worktreeDir, "a.txt"), "Content": "x"}))
	if v.Allow {
		t.Fatalf("an unreadable policy file allowed a call — it must fail closed (rule %q)", v.Rule)
	}
	if !strings.Contains(strings.ToLower(v.Reason), "fail-closed") {
		t.Errorf("the fail-closed refusal must say so; got %q", v.Reason)
	}
}

// TestBridge_PermissionWireAnswers pins the DECISION the handler sends on the wire: an allow answers
// with agy's `allow` optionId, a deny with `deny`, and an agent that offers neither ends up cancelled
// rather than approved.
func TestBridge_PermissionWireAnswers(t *testing.T) {
	projectDir, worktreeDir := newPolicyDir(t)
	h := &PolicyHandler{ProjectDir: projectDir, WorktreeDir: worktreeDir, Log: quiet()}
	opts := []acp.PermissionOption{
		{OptionID: "allow", Name: "Allow", Kind: acp.OptionKindAllowOnce},
		{OptionID: "deny", Name: "Deny", Kind: acp.OptionKindRejectOnce},
	}

	req := acp.PermissionRequest{
		SessionID: "s1",
		ToolCall:  call("run_command", "execute", map[string]any{"CommandLine": "go build ./..."}),
		Options:   opts,
	}
	dec, err := h.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestPermission returned an error (%v) — it must always answer", err)
	}
	if dec.OptionID != "allow" {
		t.Errorf("allowed call answered with %q, want the allow optionId %q", dec.OptionID, "allow")
	}

	req.ToolCall = call("run_command", "execute", map[string]any{"CommandLine": "rm -rf /"})
	dec, err = h.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestPermission returned an error (%v)", err)
	}
	if dec.OptionID != "deny" {
		t.Errorf("refused call answered with %q, want the reject optionId %q", dec.OptionID, "deny")
	}

	// NO OPTIONS AT ALL: answering `allow` here would be inventing an option agy never offered.
	req.Options = nil
	dec, err = h.RequestPermission(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestPermission returned an error (%v)", err)
	}
	if dec.OptionID != "" || !dec.Cancelled {
		t.Errorf("a refused call with no offered options answered %+v, want the cancelled outcome", dec)
	}
	// And an ALLOWED call with no allow option must not be approved either.
	req.ToolCall = call("run_command", "execute", map[string]any{"CommandLine": "go build ./..."})
	dec, _ = h.RequestPermission(context.Background(), req)
	if dec.OptionID != "" || !dec.Cancelled {
		t.Errorf("an allowed call with no allow option answered %+v — it must not be approved", dec)
	}
}

// TestBridge_PermissionOnViolationRecords proves a refusal reaches the session's durable record, since
// agy itself is told nothing but "denied".
func TestBridge_PermissionOnViolationRecords(t *testing.T) {
	projectDir, worktreeDir := newPolicyDir(t)
	var tools []string
	var reasons []string
	h := &PolicyHandler{
		ProjectDir: projectDir, WorktreeDir: worktreeDir, Log: quiet(),
		OnViolation: func(tool, reason string) {
			tools = append(tools, tool)
			reasons = append(reasons, reason)
		},
	}
	_, _ = h.RequestPermission(context.Background(), acp.PermissionRequest{
		ToolCall: call("run_command", "execute", map[string]any{"CommandLine": "rm -rf /"}),
		Options:  []acp.PermissionOption{{OptionID: "deny", Kind: acp.OptionKindRejectOnce}},
	})
	if len(tools) != 1 || tools[0] != "run_command" {
		t.Fatalf("OnViolation tools = %v, want [run_command]", tools)
	}
	if len(reasons) != 1 || strings.TrimSpace(reasons[0]) == "" {
		t.Fatalf("OnViolation reasons = %v, want one non-empty reason", reasons)
	}
}

// TestBridge_PolicyDefaultPathHonoursEnv pins that the handler consults the SAME policy file claude
// does, including the environment override.
func TestBridge_PolicyDefaultPathHonoursEnv(t *testing.T) {
	t.Setenv(permpolicy.PolicyEnv, "/tmp/orchicon/test-policy.yaml")
	if got := DefaultPolicyPath(); got != "/tmp/orchicon/test-policy.yaml" {
		t.Fatalf("DefaultPolicyPath() = %q, want the %s override", got, permpolicy.PolicyEnv)
	}
}

// TestBridge_PermissionWindowsPaths is AC4 through the ADAPTER: the same inside/outside question asked
// with Windows-spelled paths, decided by the handler the worker actually runs.
//
// WHY THE HANDLER AND NOT JUST workerrestrict. `OutsideProject` is the boundary primitive, but a worker
// is gated by `PolicyHandler.Decide`, which first translates agy's tool name into claude's vocabulary and
// only then asks the boundary. A Windows worker whose edit tool translates to `Edit` takes the claude
// path rule FIRST, so proving the primitive alone would leave the composed decision unproven. These rows
// run the composed decision.
//
// THE BOUNDARY IS SPELLED LIKE A WINDOWS HOST (`C:\repo`), which is what routes `OutsideProject` into its
// Windows arm on this Linux test runner. That is the whole point: `filepath.IsAbs("C:\repo\src\a.go")` is
// false here, so without that arm a Windows path resolves RELATIVE and an external write reads as inside.
func TestBridge_PermissionWindowsPaths(t *testing.T) {
	const projectDir = `C:\repo`
	h := &PolicyHandler{ProjectDir: projectDir, Log: quiet()}

	// A permission policy file that does not exist, so the decision is the WORKER PROFILE's, which is
	// the rule set under test.
	h.PolicyPath = filepath.Join(t.TempDir(), "nonexistent-policy.json")

	cases := []struct {
		name    string
		call    acp.ToolCallInfo
		allow   bool
		comment string
	}{
		{
			name:    "inside: a windows-spelled edit",
			call:    call("edit_file", "edit", map[string]any{"Path": `C:\repo\src\a.go`, "Target": "a", "Replacement": "b"}),
			allow:   true,
			comment: "the plain Windows spelling of a file in the project",
		},
		{
			name:    "inside: the same edit with forward slashes and a lower-case drive",
			call:    call("edit_file", "edit", map[string]any{"Path": `c:/repo/src/a.go`, "Target": "a", "Replacement": "b"}),
			allow:   true,
			comment: "case-insensitive volume, `\\` and `/` treated alike — the SAME file as the row above",
		},
		{
			name:    "outside: a system path",
			call:    call("write_file", "edit", map[string]any{"Path": `C:\Windows\system32\x.dll`, "Content": "x"}),
			allow:   false,
			comment: "on a Windows worker this is the classic external write",
		},
		{
			name:  "outside: a different volume with the same tail",
			call:  call("edit_file", "edit", map[string]any{"Path": `D:\repo\src\a.go`, "Target": "a", "Replacement": "b"}),
			allow: false,
			comment: "a different volume is a different root, even when the remaining path matches the " +
				"project's exactly — stripping the drive letter for comparison would wrongly allow this",
		},
		{
			name:    "outside: a sibling directory sharing the prefix",
			call:    call("edit_file", "edit", map[string]any{"Path": `C:\repo2\src\a.go`, "Target": "a", "Replacement": "b"}),
			allow:   false,
			comment: "separator-bounded: C:\\repo2 is not inside C:\\repo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.Decide(tc.call)
			if got.Allow != tc.allow {
				t.Fatalf("Decide(%s) = allow=%v rule=%q, want allow=%v (%s)", tc.name, got.Allow, got.Rule,
					tc.allow, tc.comment)
			}
			if !tc.allow && strings.TrimSpace(got.Reason) == "" {
				t.Errorf("Decide(%s) refused with no reason — a refusal an operator cannot read is a refusal "+
					"they cannot act on", tc.name)
			}
		})
	}
}
