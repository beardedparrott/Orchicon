package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/protectedpath"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// hook.go is the claude worker's PreToolUse decision core plus the wire
// protocol for the `orchicon claude-hook` subcommand.
//
// It is the AUTHORITY half of the pair declared in permissions.go: claude's own
// `permissions.deny` rules are prefix/tool matchers and cannot express a
// shell-smuggling glob, a protected-ancestor relationship, or the project
// boundary with its exact carve-outs. The hook receives the tool name and the
// full tool input as JSON and returns a permission decision, so the whole rule
// set is expressible — and it is the SAME rule set the opencode worker profile
// encodes, because both call internal/workerrestrict and
// internal/protectedpath rather than keeping their own copy.
//
// The function is pure apart from reading the policy file, so the table tests
// exercise every rule with no live session and no Anthropic spend.

// HookInput is the PreToolUse payload claude writes to the hook's stdin. Only
// the fields the decision needs are decoded; unknown fields are ignored so a
// CLI version that adds some does not break the hook.
type HookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	Cwd           string         `json:"cwd"`
}

// HookVerdict is one decision. Reason is the text claude shows the model (and
// the operator), Rule names the declaration that decided it.
type HookVerdict struct {
	Allow  bool
	Reason string
	Rule   string
}

// pathToolNames are the tools whose input names a filesystem target.
var pathToolNames = map[string]bool{
	"Read": true, "Write": true, "Edit": true, "MultiEdit": true,
	"NotebookEdit": true, "Glob": true, "Grep": true,
}

func allowVerdict() HookVerdict { return HookVerdict{Allow: true} }

func denyVerdict(rule, reason string) HookVerdict {
	return HookVerdict{Allow: false, Rule: rule, Reason: reason}
}

// DecideTool runs the whole worker rule set for one tool call. projectDir is the
// worker's project boundary; policyPath is the operator's policy file (empty
// disables the policy rung only).
//
// Order, and why:
//  1. the built-in subagent tool — removed entirely (opencode denies `task`);
//  2. a bash command — the shared deny class first (never-allow + project
//     boundary through workerrestrict.MatchCommand), then the non-waivable
//     protected paths, then the operator's policy;
//  3. a path-carrying tool — protected paths, then policy (including its
//     fail-closed load failure), then the project boundary;
//  4. anything else — ALLOW, deferring to claude's own default. opencode's worker
//     profile deliberately carries no catch-all either, so parity means the same
//     verdict, not a stricter one.
func DecideTool(h HookInput, projectDir, policyPath string) HookVerdict {
	tool := strings.TrimSpace(h.ToolName)
	if tool == "" {
		return allowVerdict()
	}
	if workerrestrict.IsSubagentTool(tool) {
		return denyVerdict(workerrestrict.TaskToolDeny,
			"the built-in subagent tool is denied for every worker execution: Orchicon already splits the work into focused steps, and a spawned subagent re-carries the parent's context.")
	}
	switch {
	case tool == "Bash":
		return decideBash(h, projectDir, policyPath)
	case pathToolNames[tool]:
		return decidePath(tool, h, projectDir, policyPath)
	}
	return allowVerdict()
}

// decideBash applies the command class, the protected roots and the policy to a
// Bash tool call.
func decideBash(h HookInput, projectDir, policyPath string) HookVerdict {
	cmd := stringInput(h.ToolInput, "command")
	if rule, denied := workerrestrict.MatchCommand(cmd); denied {
		return denyVerdict(rule, fmt.Sprintf(
			"refused: the command matches the worker deny rule %q (the same rule internal/neverallow and internal/opencode enforce).", rule))
	}
	targets := commandTargets(cmd)
	// The protected-root rule applies to the PATH-SCOPED binaries only — the same
	// scope the OS-level shim judges — so a plain `cd /some/ancestor` is not
	// refused while `rm -rf` on an ancestor is.
	if isScopedBinary(commandBinary(cmd)) {
		machine, scope := workerrestrict.ProtectedRoots(projectDir, nil)
		for _, t := range targets {
			if root := protectedpath.DestroyedBy(t, machine, scope); root != "" {
				return denyVerdict("protected_path", protectedpath.Refusal(t, root))
			}
		}
	}
	// The operator's policy is consulted for every absolute path argument, the
	// same way permpolicy.HostSuiteGuard consults it for the host tool suite.
	for _, t := range targets {
		if v, decided := policyVerdict(t, policyPath); decided {
			return v
		}
	}
	return allowVerdict()
}

// decidePath applies the protected roots, the policy and the project boundary to
// a path-carrying tool call.
func decidePath(tool string, h HookInput, projectDir, policyPath string) HookVerdict {
	target := pathInput(tool, h.ToolInput)
	if strings.TrimSpace(target) == "" {
		return allowVerdict()
	}
	machine, scope := workerrestrict.ProtectedRoots(projectDir, nil)
	if root := protectedpath.DestroyedBy(target, machine, scope); root != "" {
		return denyVerdict("protected_path", protectedpath.Refusal(target, root))
	}
	if v, decided := policyVerdict(target, policyPath); decided {
		return v
	}
	if allowed, rule := workerrestrict.OutsideProject(target, projectDir); !allowed {
		return denyVerdict(rule, fmt.Sprintf(
			"refused: %s is outside the project and outside every carve-out (%s and .orchicon run metadata) — the same external-directory boundary the opencode worker profile enforces.",
			target, workerrestrict.ScratchDir))
	}
	return allowVerdict()
}

// policyVerdict consults the operator's policy for one target. decided=false
// means "no policy ruling — keep going".
//
// A LOAD FAILURE IS A REFUSAL, not a pass. permpolicy deliberately distinguishes
// "the file says deny" from "the file could not be read"
// (internal/permpolicy/policy_load_failure_test.go pins that distinction); a
// guard that silently stops guarding is the exact failure the fail-closed arm
// exists to prevent, so an unreadable policy refuses rather than proceeding
// unguarded. This preserves the same contract at the claude layer.
func policyVerdict(target, policyPath string) (HookVerdict, bool) {
	if strings.TrimSpace(policyPath) == "" {
		return allowVerdict(), false
	}
	d, err := permpolicy.NewStore(policyPath).Decide(target, permpolicy.Inputs{})
	if err != nil {
		if permpolicy.IsPolicyLoadFailure(err) {
			return denyVerdict("policy_load_failure", fmt.Sprintf(
				"refused (fail-closed): the permission policy %s could not be read or parsed — %v. Refusing rather than running unguarded.", policyPath, err)), true
		}
		return denyVerdict("policy_error", fmt.Sprintf("refused: the permission policy %s could not be consulted — %v", policyPath, err)), true
	}
	if d.Verdict == permpolicy.VerdictDeny {
		return denyVerdict(d.Entry, fmt.Sprintf(
			"refused: %s is denied by entry %q in %s — a session grant cannot override it.", target, d.Entry, policyPath)), true
	}
	return allowVerdict(), false
}

// commandBinary extracts the invoked binary's basename from a shell command
// string, tolerating leading env assignments (`FOO=1 rm -rf x`).
func commandBinary(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "/") && !strings.HasPrefix(f, "!") {
			continue
		}
		return filepath.Base(strings.Trim(f, `"'`))
	}
	return ""
}

// isScopedBinary reports whether a command invokes one of the PATH-scoped
// binaries the OS-level guard intercepts. Read-only: the list comes from
// internal/guard, so the hook and the shim judge the same commands.
func isScopedBinary(name string) bool {
	for _, n := range guard.ScopedBinaryNames() {
		if name == n {
			return true
		}
	}
	return false
}

// commandTargets extracts the filesystem targets named by a shell command: the
// absolute tokens, plus the home-relative spellings the guard also refuses.
// Flags are skipped. A `key=value` token (a dd if=/of= operand) is not a path.
func commandTargets(cmd string) []string {
	var out []string
	for _, f := range strings.Fields(cmd) {
		t := strings.Trim(f, `"';|&()`)
		if t == "" || strings.HasPrefix(t, "-") {
			continue
		}
		if strings.Contains(t, "=") && !strings.HasPrefix(t, "/") {
			continue
		}
		if strings.HasPrefix(t, "/") ||
			t == "~" || strings.HasPrefix(t, "~/") ||
			strings.HasPrefix(t, "$HOME") || strings.HasPrefix(t, "${HOME}") {
			out = append(out, t)
		}
	}
	return out
}

// pathInput extracts the filesystem target from a path-carrying tool's input.
// Glob/Grep carry a `pattern` whose DIRECTORY part (everything before the last
// segment) is the path actually touched.
func pathInput(tool string, in map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if v := stringInput(in, k); v != "" {
			return v
		}
	}
	if v := stringInput(in, "pattern"); v != "" {
		return dirOfPattern(v)
	}
	return ""
}

// dirOfPattern returns the directory part of a glob/regex pattern: up to the last
// separator, or the pattern itself when it is a bare absolute path.
func dirOfPattern(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

// stringInput reads a string field from a decoded tool input map.
func stringInput(in map[string]any, key string) string {
	if in == nil {
		return ""
	}
	if v, ok := in[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// ---------------------------------------------------------------------
// the wire protocol
// ---------------------------------------------------------------------

// hookOutput is claude's PreToolUse hook contract: a JSON document on stdout
// carrying the decision. permissionDecision is "allow" or "deny"; the reason is
// shown to the model (so it is a tool error it can act on) and to the operator
// (so a wrong rule is checkable).
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// RunHook is the `orchicon claude-hook` entry point: read ONE PreToolUse JSON
// document from in, decide, write the decision document to out, exit 0.
//
// A MALFORMED or EMPTY stdin is exit 0 with NO decision — claude then applies
// its own permission flow. The hook never crashes the worker's tool call: an
// unparseable payload is not a reason to deny every tool, and the launch-time
// `permissions.deny` + the OS-level shim are still in force underneath.
func RunHook(in io.Reader, out io.Writer, getenv func(string) string) int {
	if getenv == nil {
		getenv = os.Getenv
	}
	data, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return 0
	}
	var h HookInput
	if err := json.Unmarshal(data, &h); err != nil {
		return 0
	}
	if strings.TrimSpace(h.ToolName) == "" {
		return 0
	}
	projectDir := strings.TrimSpace(getenv(ProjectDirEnv))
	if projectDir == "" {
		projectDir = strings.TrimSpace(h.Cwd)
	}
	policyPath := strings.TrimSpace(getenv(permpolicy.PolicyEnv))
	if policyPath == "" {
		policyPath = permpolicy.DefaultPath()
	}
	v := DecideTool(h, projectDir, policyPath)
	decision := "allow"
	reason := v.Reason
	if !v.Allow {
		decision = "deny"
		if reason == "" {
			reason = "refused by the Orchicon worker restriction"
		}
	}
	doc := hookOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       decision,
		PermissionDecisionReason: reason,
	}}
	if err := json.NewEncoder(out).Encode(doc); err != nil {
		return 0
	}
	return 0
}
