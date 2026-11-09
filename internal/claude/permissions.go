package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/workerrestrict"
)

// permissions.go maps the PLATFORM's restrictions onto a claude worker session
// through CLAUDE'S OWN permission mechanism — `--settings <json>` carrying a
// `permissions` block plus a `hooks.PreToolUse` command — and NEVER through a
// permission-bypass flag.
//
// WHY NOT `--dangerously-skip-permissions`. Every other task in this feature
// runs a claude process inside a runtime container, and the platform promises
// the operator that a worker cannot run destructive or out-of-project commands.
// A bypass launch would silently discard the never-allow class, the protected
// paths, the operator's policy AND the project boundary for every claude run —
// a REGRESSION of that promise, worse than the adapter not existing. The
// mechanism below is the only launch shape this adapter may use, and
// TestNoBypassPermissionFlagIsEverEmitted fails if a bypass token appears in
// either the argv or the settings JSON.
//
// TWO LAYERS:
//   - `hooks.PreToolUse` -> `orchicon claude-hook` is the AUTHORITY. It sees the
//     full tool input (the exact bash command string, the target path) and runs
//     the SAME matchers internal/opencode's worker profile encodes, so it can
//     express rules claude's own prefix matcher cannot (the shell-smuggling
//     globs, the project boundary with its two carve-outs, the protected roots).
//   - `permissions.deny` is BELT-AND-SUSPENDERS: `Bash(<binary>:*)` for the
//     shared never-allow class and `Read(...)`/`Edit(...)` for the operator's
//     policy entries, so a hook that fails to launch still cannot run `sudo` or
//     read a denied path. Deliberately NOT a translation of the glob patterns —
//     a second, lossy encoding of the list is exactly what
//     internal/neverallow/share_test.go exists to prevent.

// HookBinEnv is the operator/deployment override for the absolute path of the `orchicon` binary the
// PreToolUse hook command invokes when the session runs as a HOST subprocess (the local.
const HookBinEnv = "ORCHICON_CLAUDE_HOOK_BIN"

// HookBinaryContainerPath is where the runtime daemon bind-mounts its own executable inside EVERY
// runtime container (internal/runtime/daemon.go: createContainer hard-fails when it cannot.
const HookBinaryContainerPath = "/usr/local/bin/orchicon"

// ProjectDirEnv is the env var the hook reads to learn the worker's project
// boundary. The session sets it on the child's env (childEnv).
const ProjectDirEnv = "ORCHICON_CLAUDE_PROJECT_DIR"

// HookBinFallback is used when neither the env var nor os.Executable() resolves.
const HookBinFallback = "orchicon"

// PreflightHookExecutable proves the PreToolUse hook can actually RUN, and reports the consequence
// when it cannot.
func PreflightHookExecutable(isContainer bool) error {
	bin := strings.TrimSpace(HookBinaryFor(isContainer))
	if bin == "" {
		return fmt.Errorf("claude worker: the permission hook binary could not be resolved, so no tool call " +
			"could be gated. Set " + HookBinEnv + " to an absolute orchicon path")
	}
	if isContainer {
		if !strings.HasPrefix(bin, "/") {
			return fmt.Errorf("claude worker: the permission hook path %q is not absolute — inside a runtime "+
				"container the hook must be the daemon's bind-mounted binary (%s)", bin, HookBinaryContainerPath)
		}
		return nil
	}
	// LOCAL MODE: a bare name is resolved on PATH, and an absent one means the hook command will fail every
	// time. LookPath is the same resolution the CLI's hook command performs.
	if !filepath.IsAbs(bin) {
		resolved, err := exec.LookPath(bin)
		if err != nil {
			return fmt.Errorf("claude worker: the permission hook binary %q is not on PATH, so every gated "+
				"tool call would be refused as an unanswerable prompt. Set %s to an absolute orchicon path",
				bin, HookBinEnv)
		}
		bin = resolved
	}
	st, err := os.Stat(bin)
	if err != nil {
		return fmt.Errorf("claude worker: the permission hook binary %s is not readable (%w), so every gated "+
			"tool call would be refused as an unanswerable prompt", bin, err)
	}
	if st.IsDir() {
		return fmt.Errorf("claude worker: the permission hook path %s is a directory, not an executable", bin)
	}
	// THE COMMAND MUST SURVIVE A SHELL, and this is the only place that can say so before every write
	// is refused.
	if err := validateHookCommand(hookCommand(bin)); err != nil {
		return err
	}
	return nil
}

// validateHookCommand checks that a rendered hook command parses into exactly the binary and the
// subcommand, which is what will actually be executed.
func validateHookCommand(cmd string) error {
	tokens := splitHookCommand(cmd)
	if len(tokens) != 2 {
		return fmt.Errorf("claude worker: the permission hook command %q splits into %d arguments, want 2 "+
			"(the binary and `claude-hook`) — a path containing a space must be quoted, or the hook never runs "+
			"and claude blocks every gated tool", cmd, len(tokens))
	}
	if tokens[1] != "claude-hook" {
		return fmt.Errorf("claude worker: the permission hook command %q does not invoke `claude-hook`", cmd)
	}
	if strings.TrimSpace(tokens[0]) == "" {
		return fmt.Errorf("claude worker: the permission hook command %q has an empty binary", cmd)
	}
	return nil
}

// splitHookCommand splits a command the way a POSIX shell would for the simple case that matters: double
// quotes group, and unquoted whitespace separates.
func splitHookCommand(cmd string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
		started bool
	)
	for _, r := range cmd {
		switch {
		case r == '"':
			inQuote = !inQuote
			started = true
		case (r == ' ' || r == '\t') && !inQuote:
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}

// TodoToolsEnv is the ENV TOGGLE that puts claude's todo/task-tracking tool
// family (TodoWrite, TaskCreate/TaskUpdate/TaskList/TaskGet) back into the
// session's tool registry at all.
//
// VERIFIED AGAINST THE INSTALLED NATIVE BINARY (2.1.261), not assumed: the
// binary's own release notes read "Todo/task-tracking tools
// (TaskCreate/Get/Update/List, TodoWrite) are no longer available on Opus 4.8,
// Sonnet 5, Fable 5, Mythos 5, and newer models; set
// CLAUDE_CODE_ENABLE_TODO_TOOLS=1 to bring them back", and the matching gate in
// the minified bundle reads (paraphrased, verified by grep on the installed binary):
//
//	gateModels = [["opus",[4,8]],["sonnet",[5]],["fable",[5]],["mythos",[5]]]
//	todoToolsEnabled() { if (isHeadlessTransport()) return true;
//	  model := currentModel(); if (model === undefined || !isGated(model)) return true;
//	  return env.CLAUDE_CODE_ENABLE_TODO_TOOLS === true; }
//
// so on the models orchicon actually runs claude workers with (Sonnet 5 and
// newer) the family is absent from the tool list unless this is set. A
// permission allow cannot conjure a tool the CLI never offers, so WITHOUT this
// the tool names in HookToolMatcher/permissions.allow are dead letters and NO
// todo ever streams — the parity feature would silently do nothing. It is a
// plain opt-in, NOT a permission bypass: every restriction (the hook authority,
// the deny list, the project boundary) stays in force.
const TodoToolsEnv = "CLAUDE_CODE_ENABLE_TODO_TOOLS"

// TodoTrackToolNames are the task/todo-tracking tools a claude worker session must be OPTED INTO.
var TodoTrackToolNames = []string{
	"TodoWrite",
	"TaskCreate",
	"TaskUpdate",
	"TaskList",
	"TaskGet",
	"TaskOutput",
}

// AskPermissionToolNames are the tools the ASK profile must PROMPT about, expressed as
// `permissions.ask` entries in the settings document.
var AskPermissionToolNames = []string{
	"Write", "Edit", "MultiEdit", "NotebookEdit",
	"Bash",
}

// HookToolMatcher is the PreToolUse matcher: the tools whose input can name a
// command or a path, plus the built-in subagent tool, plus the todo/task-tracking
// family (so the hook's catch-all ALLOW verdict also covers them). Claude matches
// the matcher as a regular expression against the tool name.
//
// `mcp__.*` IS LOAD-BEARING, and its absence was a total failure of the MCP
// surface. A tool the matcher does not name NEVER REACHES THE HOOK, so the hook's
// verdict — including its allow — never happens, and claude falls back to its own
// permission flow. In a `-p` session that flow has nobody to grant, so EVERY MCP
// tool was denied:
//
//	"Claude requested permissions to use mcp__orchicon__list_work_items, but you
//	 haven't granted it yet."
//
// Reproduced against the real CLI with a logging wrapper in place of this
// handler: the hook was never invoked, for either profile. So workers could not
// call an MCP tool either — the entire MCP registration (mcpconfig.go) was
// connected but unusable.
//
// MCP tool names are `mcp__<server>__<tool>` (verified: 87 such tools once the
// sidecar connects), and the pattern is broad on purpose: the PROFILES decide
// what each tool gets. The Ask profile allows the platform's own `mcp__orchicon__*`
// and ASKS about third-party ones; the worker profile defers to claude's default,
// so an operator-configured server is usable.
const HookToolMatcher = "Bash|Read|Write|Edit|MultiEdit|NotebookEdit|Glob|Grep|Task|Agent|TodoWrite|TaskCreate|TaskUpdate|TaskList|TaskGet|TaskOutput|mcp__.*"

// bypassLaunchFlags are the LAUNCH spellings that would discard the restrictions.
var bypassLaunchFlags = []string{
	"--dangerously-skip-permissions",
	"--allow-dangerously-skip-permissions",
	"--dangerously-allow-skip-permissions",
	"--dangerously-skip-permissions-with-sandbox",
}

// bypassPermissionMode is the --permission-mode value that would disable the permission system
// entirely.
const bypassPermissionMode = "bypassPermissions"

// PermissionOptions is the launch-time input to the settings document.
type PermissionOptions struct {
	// ProjectDir is the worker's project root (the boundary an
	// external-directory access may not leave).
	ProjectDir string
	// WorktreeDir is the run's isolated worktree, when provisioned. It is an
	// additional in-scope directory (the session's cwd).
	WorktreeDir string
	// HookBinary is the absolute path of the binary the hook command runs.
	HookBinary string
	// PolicyPath is the operator's permission policy file. Empty disables the
	// policy rung (never the never-allow class).
	PolicyPath string
	// AdditionalDirs widens the in-scope directory set (the carve-outs are
	// always derived from workerrestrict and cannot be removed).
	AdditionalDirs []string
	// Profile selects the hook's rule set (ProfileWorkerEnvValue is the non-interactive sandbox and the
	// ZERO VALUE, so an existing caller is unchanged.
	Profile string
}

// HookBinaryPath resolves the binary the PreToolUse hook invokes: the explicit env override first
// (the runtime container's /usr/local/bin/orchicon).
func HookBinaryPath() string {
	if v := strings.TrimSpace(os.Getenv(HookBinEnv)); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		return exe
	}
	return HookBinFallback
}

// HookBinaryFor resolves the binary the PreToolUse hook command invokes for one transport.
func HookBinaryFor(isContainer bool) string {
	if isContainer {
		return HookBinaryContainerPath
	}
	return HookBinaryPath()
}

// PermissionArgs returns the FULL permission launch argv: the mode flag, the inline settings
// document, and the disallowed-tool list.
func PermissionArgs(o PermissionOptions) ([]string, error) {
	settings, err := BuildSettings(o)
	args := []string{"--permission-mode", "default"}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	if o.Profile == ProfileAskEnvValue {
		// The Ask session DEPENDS on the CLI asking the host: that request is what raises the can_use_tool
		// control frame the consent cards answer.
		args = append(args, "--permission-prompts", "host")
	}
	// Deny the built-in subagent tool by NAME as well as through the hook — the CLI-level deny survives
	// a hook that fails to launch.
	args = append(args, "--disallowedTools", strings.Join(workerrestrict.SubagentToolNames, " "))
	return args, err
}

// BuildSettings marshals the `--settings` document. THE PATH MUST BE QUOTED, and this is a Windows
// defect that reads as a permissions defect.
func hookCommand(hookBin string) string {
	return `"` + hookBin + `" claude-hook`
}

func BuildSettings(o PermissionOptions) (string, error) {
	policyDeny, policyErr := policyDenyEntries(o.PolicyPath)

	deny := make([]string, 0, len(neverallow.Shimmed())*2+len(policyDeny)*2+len(workerrestrict.SubagentToolNames))
	// The shared never-allow class, as claude's own Bash rule syntax.
	for _, b := range neverallow.Shimmed() {
		deny = append(deny, "Bash("+b+":*)")
	}
	// The built-in subagent tool, denied by name (the opencode profile's `task`
	// deny, expressed in claude's vocabulary).
	deny = append(deny, workerrestrict.SubagentToolNames...)
	// The operator's policy denies, as read and edit rules.
	for _, e := range policyDeny {
		deny = append(deny, "Read("+e+")", "Edit("+e+")")
	}

	// The carve-outs come from workerrestrict, so they are the SAME two subtrees
	// the opencode worker profile allows — never a wider approximation.
	dirs := append([]string(nil), workerrestrict.CarveOutDirs(o.ProjectDir)...)
	if strings.TrimSpace(o.WorktreeDir) != "" {
		dirs = append(dirs, o.WorktreeDir)
	}
	for _, d := range o.AdditionalDirs {
		if strings.TrimSpace(d) != "" {
			dirs = append(dirs, d)
		}
	}

	hookBin := strings.TrimSpace(o.HookBinary)
	if hookBin == "" {
		hookBin = HookBinaryPath()
	}

	// THE ASK PROFILE'S PROMPTS COME FROM THE PERMISSION SYSTEM, NOT FROM THE HOOK.
	perms := map[string]any{
		// NOT acceptEdits and NOT bypassPermissions: an unanswerable ask in
		// -p mode is a refusal, which is the honest worker semantic.
		"defaultMode": "default",
		"deny":        deny,
		// The task/todo-tracking family is PRE-APPROVED so it streams at all in a non-interactive session.
		"allow":                 append([]string(nil), TodoTrackToolNames...),
		"additionalDirectories": dirs,
		// Pin the bypass mode OFF at the settings layer too, so a later `--permission-mode
		// bypassPermissions` on the command line is refused by claude itself.
		"disableBypassPermissionsMode": "disable",
	}
	if o.Profile == ProfileAskEnvValue {
		perms["ask"] = append([]string(nil), AskPermissionToolNames...)
	}

	hooks := map[string]any{
		"PreToolUse": []map[string]any{{
			"matcher": HookToolMatcher,
			"hooks": []map[string]any{{
				"type":    "command",
				"command": hookCommand(hookBin),
			}},
		}},
	}
	// THE CARD'S ONLY DELIVERY PATH.
	if o.Profile == ProfileAskEnvValue {
		hooks["PermissionRequest"] = []map[string]any{{
			"matcher": askHookToolMatcher(),
			"hooks": []map[string]any{{
				"type":    "command",
				"command": hookCommand(hookBin),
				"timeout": AskHookTimeoutSeconds,
			}},
		}}
	}

	settings := map[string]any{
		"permissions": perms,
		"hooks":       hooks,
	}

	b, err := json.Marshal(settings)
	if err != nil {
		return "", policyErr
	}
	return string(b), policyErr
}

// policyDenyEntries reads the operator's policy and returns its deny patterns.
func policyDenyEntries(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	p, err := permpolicy.Load(path)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), p.Deny...), nil
}

// SettingsHookBinary extracts the hook command's binary from a settings document
// (test/observability seam).
func SettingsHookBinary(settings string) string {
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &doc); err != nil {
		return ""
	}
	for _, m := range doc.Hooks.PreToolUse {
		for _, h := range m.Hooks {
			if h.Command != "" {
				// Unquote as well as untrim: hookCommand always quotes the binary path (a Windows install path
				// contains spaces), so the raw command is `"<path>" claude-hook` and returning it with the quotes.
				bin := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(h.Command), " claude-hook"))
				// SHELL quoting, not Go quoting: strip one surrounding pair of double quotes.
				bin = strings.TrimSuffix(strings.TrimPrefix(bin, `"`), `"`)
				return bin
			}
		}
	}
	return ""
}
