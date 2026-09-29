package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/neverallow"
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
//
// The verdict is THREE-way, not a bool. claude's PreToolUse contract accepts
// "allow" | "deny" | "ask" (verified against the installed binary 2.1.261,
// which also documents the requirement "for 'ask' use
// hookSpecificOutput.permissionDecision in a PreToolUse hook"), and the Ask
// profile needs all three: a destructive command is DENIED, a write or an
// execution is ASKED about, and a read is ALLOWED. A bool would have forced
// "ask" to be expressed as one of the two, which is how a consent card turns
// into either a silent allow or a silent refusal.
type HookVerdict struct {
	Allow bool
	// Ask requests the operator's decision instead of deciding. Only the Ask
	// profile produces it; the worker profile is non-interactive by design, so a
	// worker verdict is always allow or deny.
	Ask    bool
	Reason string
	Rule   string
}

// Decision renders the verdict as claude's wire vocabulary. The zero value is
// DENY: a verdict that set neither flag must never be read as an approval.
func (v HookVerdict) Decision() string {
	switch {
	case v.Allow:
		return DecisionAllow
	case v.Ask:
		return DecisionAsk
	default:
		return DecisionDeny
	}
}

// claude's PreToolUse permissionDecision vocabulary.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
	DecisionAsk   = "ask"
)

// pathToolNames are the tools whose input names a filesystem target.
var pathToolNames = map[string]bool{
	"Read": true, "Write": true, "Edit": true, "MultiEdit": true,
	"NotebookEdit": true, "Glob": true, "Grep": true,
}

func allowVerdict() HookVerdict { return HookVerdict{Allow: true} }

// askVerdict defers the call to the operator (see HookVerdict.Ask).
func askVerdict() HookVerdict { return HookVerdict{Ask: true} }

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

// readOnlyToolNames are the tools that only OBSERVE the filesystem. They are
// ALLOWED in the Ask profile — not merely asked about — because that is what
// opencode's interactive profile does (external_directory "*" -> "allow") and
// because a read is not a decision the operator needs to make: consenting to an
// assistant and then being asked before every file it reads is not consent, it
// is a prompt queue.
var readOnlyToolNames = map[string]bool{
	"Read": true, "Glob": true, "Grep": true,
}

// DecideToolForAsk runs the ASK (interactive) rule set for one tool call, and
// returns allow / deny / ask.
//
// It mirrors opencode's interactive profile (internal/opencode/config.go:
// interactivePermissionRules), which is the reference the platform already
// trusts for an interactive session:
//
//	| concern                        | worker        | Ask               |
//	|--------------------------------|---------------|-------------------|
//	| never-allow class              | deny          | deny              |
//	| built-in subagent tool         | deny          | deny              |
//	| writes / edits                 | deny or allow | ASK               |
//	| an execution (Bash)            | deny or allow | ASK               |
//	| reads                          | boundary      | allow             |
//	| outside the project            | deny          | allow             |
//
// THREE rules stay non-waivable, i.e. denied rather than asked about:
//
//  1. the never-allow class — opencode's interactive profile hard-denies it too,
//     and the OS-level guard shim refuses it regardless. A consent card for a
//     command the shim will refuse anyway is a LIE about what consent can do.
//  2. the built-in subagent tool — denied in every profile.
//  3. a path that would DESTROY the plane's own state
//     (internal/protectedpath). The worker hook treats this as non-waivable and
//     the shim enforces it at OS level, so offering it as a choice would have the
//     same problem as (1). This is deliberately STRICTER than opencode's
//     interactive rules, which carry no protected-path rung at all: that is a gap
//     there, not a behaviour to copy, because the same shim refuses the command
//     underneath it.
//
// The operator's policy deny list is likewise honoured as a deny rather than a
// card: it is a STANDING operator decision, and asking the operator to re-decide
// their own standing decision on every call is noise, not consent.
func DecideToolForAsk(h HookInput, askDir, policyPath string) HookVerdict {
	tool := strings.TrimSpace(h.ToolName)
	if tool == "" {
		// No tool named: nothing to decide on a name this hook cannot read, and
		// the CLI falls back to its own flow. Matching the worker's tolerance.
		return askVerdict()
	}
	if workerrestrict.IsSubagentTool(tool) {
		return denyVerdict(workerrestrict.TaskToolDeny,
			"the built-in subagent tool is denied in every profile: Orchicon already splits the work into focused steps, and a spawned subagent re-carries the parent's context.")
	}
	switch {
	case tool == "Bash":
		return decideBashForAsk(h, askDir, policyPath)
	case pathToolNames[tool]:
		return decidePathForAsk(tool, h, policyPath)
	}
	// An unrecognised tool is ASKED about. The interactive default is consent,
	// and a tool this adapter has never seen is exactly the case where a card is
	// worth more than an assumption.
	return askVerdict()
}

// decideBashForAsk applies the never-allow class, the protected roots and the
// policy to an Ask session's Bash call, then asks.
func decideBashForAsk(h HookInput, askDir, policyPath string) HookVerdict {
	cmd := stringInput(h.ToolInput, "command")
	// NEVER-ALLOW only. The worker's MatchCommand also applies the PROJECT
	// BOUNDARY, which an interactive session deliberately does not have — matching
	// against the full worker set here would deny the command class the Ask
	// profile exists to allow.
	if rule, denied := workerrestrict.MatchCommandAgainst(neverallow.DenyRules(), cmd); denied {
		return denyVerdict(rule, fmt.Sprintf(
			"refused: the command matches the never-allow class (%s), which no profile and no approval can waive.", rule))
	}
	targets := commandTargets(cmd)
	if isScopedBinary(commandBinary(cmd)) {
		machine, scope := workerrestrict.ProtectedRoots(askDir, nil)
		for _, t := range targets {
			if root := protectedpath.DestroyedBy(t, machine, scope); root != "" {
				return denyVerdict("protected_path", protectedpath.Refusal(t, root))
			}
		}
	}
	for _, t := range targets {
		if v, decided := policyVerdict(t, policyPath); decided {
			return v
		}
	}
	// Everything else is a real decision for the operator: this is the branch that
	// makes the consent cards meaningful.
	return askVerdict()
}

// decidePathForAsk applies the protected roots and the policy to a path-carrying
// tool call, allows reads, and asks about writes.
func decidePathForAsk(tool string, h HookInput, policyPath string) HookVerdict {
	target := pathInput(tool, h.ToolInput)
	if strings.TrimSpace(target) == "" {
		return askVerdict()
	}
	machine, scope := workerrestrict.ProtectedRoots("", nil)
	if root := protectedpath.DestroyedBy(target, machine, scope); root != "" {
		return denyVerdict("protected_path", protectedpath.Refusal(target, root))
	}
	if v, decided := policyVerdict(target, policyPath); decided {
		return v
	}
	if readOnlyToolNames[tool] {
		return allowVerdict()
	}
	return askVerdict()
}

// HookProfileEnv selects the rule set this hook process applies.
const HookProfileEnv = "ORCHICON_CLAUDE_HOOK_PROFILE"

// AskDirEnv is the directory an Ask conversation runs in. It replaces the
// worker's project boundary: there is no project to stay inside, but the Ask
// directory itself is still protected from destruction.
const AskDirEnv = "ORCHICON_CLAUDE_ASK_DIR"

// Permission profile values (the hook's rule set, not claude's own modes).
const (
	// ProfileWorkerEnvValue is the sandboxed, non-interactive rule set. It is the
	// DEFAULT when HookProfileEnv is unset, so a worker session that never sets it
	// keeps its current behaviour.
	ProfileWorkerEnvValue = "worker"
	// ProfileAskEnvValue is the interactive rule set: consent gates the action.
	ProfileAskEnvValue = "ask"
)

// ---------------------------------------------------------------------
// the wire protocol
// ---------------------------------------------------------------------

// hookOutput is claude's PreToolUse hook contract: a JSON document on stdout
// carrying the decision. permissionDecision is "allow", "deny" or "ask"; the
// reason is shown to the model (so it is a tool error it can act on) and to the
// operator (so a wrong rule is checkable).
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

	// The rule set is selected by env, not by a separate binary: the hook command
	// in the settings document is a fixed string, so the PROFILE has to travel on
	// the child's environment. The default is the worker profile, so a session that
	// never sets it is unchanged.
	var v HookVerdict
	if strings.TrimSpace(getenv(HookProfileEnv)) == ProfileAskEnvValue {
		askDir := strings.TrimSpace(getenv(AskDirEnv))
		if askDir == "" {
			askDir = strings.TrimSpace(h.Cwd)
		}
		v = DecideToolForAsk(h, askDir, policyPath)
	} else {
		v = DecideTool(h, projectDir, policyPath)
	}

	decision := v.Decision()
	reason := v.Reason
	if reason == "" {
		switch decision {
		case DecisionDeny:
			reason = "refused by the Orchicon execution restriction"
		case DecisionAsk:
			reason = "this call needs the operator's approval"
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
