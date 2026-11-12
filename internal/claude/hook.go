package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/protectedpath"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// hook.go is the claude worker's PreToolUse decision core plus the wire protocol for the `orchicon
// claude-hook` subcommand.

// HookInput is the PreToolUse payload claude writes to the hook's stdin.
type HookInput struct {
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	Cwd           string         `json:"cwd"`
}

// HookVerdict is one decision.
type HookVerdict struct {
	Allow bool
	// Ask requests the operator's decision instead of deciding.
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

// DecideTool runs the whole worker rule set for one tool call.
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
	// AN UNRECOGNISED NAME IS ALLOWED, and for an MCP tool that is PARITY rather than a concession: the
	// native bridge copies every discovered MCP tool into the worker's tool list with no gating.
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
	// The protected-root rule applies to the PATH-SCOPED binaries only — the same scope the OS-level
	// shim judges — so a plain `cd /some/ancestor` is not refused while `rm -rf` on an ancestor is.
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

// policyVerdict consults the operator's policy for one target.
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

// isScopedBinary reports whether a command invokes one of the PATH-scoped binaries the OS-level
// guard intercepts.
func isScopedBinary(name string) bool {
	for _, n := range guard.ScopedBinaryNames() {
		if name == n {
			return true
		}
	}
	return false
}

// commandTargets extracts the filesystem targets named by a shell command: the absolute tokens,
// plus the home-relative spellings the guard also refuses.
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

// readOnlyToolNames are the tools that only OBSERVE the filesystem.
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
	// EVERY MCP TOOL IS ALLOWED — the platform's own AND the operator's.
	if isMCPTool(tool) {
		return allowVerdict()
	}

	switch {
	case tool == "Bash":
		return decideBashForAsk(h, askDir, policyPath)
	case pathToolNames[tool]:
		return decidePathForAsk(tool, h, policyPath)
	}
	// An unrecognised tool is ASKED about.
	return askVerdict()
}

// orchiconMCPServerName is the server name the built-in Orchicon sidecar is registered under
// (mcpconfig.go), and so the prefix claude gives every tool it provides.
const orchiconMCPServerName = "orchicon"

// isMCPTool reports whether a claude tool name belongs to ANY MCP server — the platform's own
// `mcp__orchicon__<tool>`, or an operator's `mcp__<server>__<tool>`.
func isMCPTool(tool string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool)), "mcp__")
}

// isOrchiconMCPTool reports whether a claude tool name is one of the platform's OWN MCP tools
// (`mcp__orchicon__<tool>`).
func isOrchiconMCPTool(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	return strings.HasPrefix(t, "mcp__"+orchiconMCPServerName+"__")
}

// decideBashForAsk applies the never-allow class, the protected roots and the
// policy to an Ask session's Bash call, then asks.
func decideBashForAsk(h HookInput, askDir, policyPath string) HookVerdict {
	cmd := stringInput(h.ToolInput, "command")
	// NEVER-ALLOW only.
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
	readOnly := readOnlyToolNames[tool]
	target := pathInput(tool, h.ToolInput)
	if strings.TrimSpace(target) == "" {
		// A READ THAT NAMED NO RESOLVABLE TARGET IS STILL A READ, AND THE ORDER HERE IS THE FIX.
		if readOnly {
			return allowVerdict()
		}
		return askVerdict()
	}
	machine, scope := workerrestrict.ProtectedRoots("", nil)
	if root := protectedpath.DestroyedBy(target, machine, scope); root != "" {
		return denyVerdict("protected_path", protectedpath.Refusal(target, root))
	}
	if v, decided := policyVerdict(target, policyPath); decided {
		return v
	}
	if readOnly {
		return allowVerdict()
	}
	return askVerdict()
}

// HookProfileEnv selects the rule set this hook process applies.
const HookProfileEnv = "ORCHICON_CLAUDE_HOOK_PROFILE"

// runPermissionRequestHook answers a permission prompt by asking the ADAPTER, which raises the
// card.
func runPermissionRequestHook(h HookInput, getenv func(string) string, out io.Writer) int {
	deny := func(msg string) int {
		doc := hookOutput{HookSpecificOutput: hookSpecificOutput{
			HookEventName: "PermissionRequest",
			Decision:      &hookPermissionDecision{Behavior: "deny", Message: msg},
		}}
		_ = json.NewEncoder(out).Encode(doc)
		return 0
	}

	sock := strings.TrimSpace(getenv(ConsentSockEnv))
	if sock == "" {
		return deny("this call was not approved: the session's consent channel is unavailable, so it did not run. Nothing is permanently denied — retry it and it will ask again.")
	}

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.Dial("unix", sock)
	if err != nil {
		return deny("this call was not approved: the session's consent channel could not be reached, so it did not run. Nothing is permanently denied — retry it and it will ask again.")
	}
	defer conn.Close()

	inputJSON, _ := json.Marshal(h.ToolInput)
	req, err := json.Marshal(hookConsentRequest{
		Tool:  h.ToolName,
		Input: inputJSON,
	})
	if err != nil {
		return deny("this call was not approved: its arguments could not be sent to the consent channel.")
	}

	// NO WRITE DEADLINE BEYOND THE DIAL, and NO READ DEADLINE AT ALL: the read IS the operator
	// thinking.
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return deny("this call was not approved: the consent channel closed before the request was sent.")
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return deny("this call was not approved: no decision arrived on the consent channel, so it did not run.")
	}
	var reply hookConsentReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return deny("this call was not approved: the decision on the consent channel could not be read.")
	}
	if strings.EqualFold(strings.TrimSpace(reply.Behavior), "allow") {
		doc := hookOutput{HookSpecificOutput: hookSpecificOutput{
			HookEventName: "PermissionRequest",
			Decision:      &hookPermissionDecision{Behavior: "allow"},
		}}
		_ = json.NewEncoder(out).Encode(doc)
		return 0
	}
	msg := strings.TrimSpace(reply.Message)
	if msg == "" {
		msg = "the operator refused this call, so it did not run."
	}
	return deny(msg)
}

// AskDirEnv is the directory an Ask conversation runs in.
const AskDirEnv = "ORCHICON_CLAUDE_ASK_DIR"

// Permission profile values (the hook's rule set, not claude's own modes).
const (
	// ProfileWorkerEnvValue is the sandboxed, non-interactive rule set.
	ProfileWorkerEnvValue = "worker"
	// ProfileAskEnvValue is the interactive rule set: consent gates the action.
	ProfileAskEnvValue = "ask"
)

// ---------------------------------------------------------------------
// the wire protocol
// ---------------------------------------------------------------------

// hookOutput is claude's PreToolUse hook contract: a JSON document on stdout carrying the decision.
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
	// Decision is the PermissionRequest event's arm (consent_hook.go).
	Decision *hookPermissionDecision `json:"decision,omitempty"`
}

// hookPermissionDecision is a PermissionRequest hook's answer.
type hookPermissionDecision struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

// RunHook is the `orchicon claude-hook` entry point: read ONE PreToolUse JSON document from in,
// decide, write the decision document to out, exit 0.
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

	// The rule set is selected by env, not by a separate binary: the hook command in the settings
	// document is a fixed string, so the PROFILE has to travel on the child's environment.
	var v HookVerdict
	asking := strings.TrimSpace(getenv(HookProfileEnv)) == ProfileAskEnvValue

	// THE PERMISSIONREQUEST EVENT IS A DIFFERENT CONTRACT FROM PreToolUse, and it is the only one that
	// can put a CARD in front of the operator.
	if asking && h.HookEventName == "PermissionRequest" {
		return runPermissionRequestHook(h, getenv, out)
	}

	if asking {
		askDir := strings.TrimSpace(getenv(AskDirEnv))
		if askDir == "" {
			askDir = strings.TrimSpace(h.Cwd)
		}
		modeFile := strings.TrimSpace(getenv(AskModeFileEnv))

		// THE MODE BOUNDARY, FIRST — it is the turn's own rule rather than a per-call judgement, and its
		// message is the one that tells the model what to do (name the mode, ask the user to switch).
		switch denied, reason := askModeDenial(modeFile, h.ToolName); {
		case denied:
			v = denyVerdict("ask_mode", reason)
		case isAskStatePath(pathInput(h.ToolName, h.ToolInput), modeFile):
			// The anti-tamper rule: the ask state directory holds the mode files, so a write to it is a session
			// trying to lift its own restriction — the one thing the refusal text above tells it it cannot do.
			v = denyVerdict("ask_state_tamper", "refused: this path is the platform's own Ask state (the "+
				"conversation's mode boundary). It is not writable by the session, and editing it would not "+
				"change the mode — only the user can do that, from the mode selector.")
		default:
			v = DecideToolForAsk(h, askDir, policyPath)
		}
	} else {
		v = DecideTool(h, projectDir, policyPath)
	}

	decision := v.Decision()
	reason := v.Reason

	// AN ASK IS DELIVERED BY ABSTAINING, NOT BY SAYING "ask" — and this is the fix for "claude adapter
	// ask sessions seem blocked by everything".
	if asking && decision == DecisionAsk {
		return 0
	}
	if reason == "" {
		// WORDING IS PER-PROFILE.
		switch {
		case decision == DecisionDeny && asking:
			reason = "refused by the Orchicon execution restriction"
		case decision == DecisionDeny:
			reason = "refused by the Orchicon worker restriction"
		case decision == DecisionAsk:
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
