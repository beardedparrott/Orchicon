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

// HookBinEnv is the operator/deployment override for the absolute path of the
// `orchicon` binary the PreToolUse hook command invokes when the session runs as
// a HOST subprocess (the local transport). It is deliberately NOT consulted on
// the container transport — see HookBinaryFor.
const HookBinEnv = "ORCHICON_CLAUDE_HOOK_BIN"

// HookBinaryContainerPath is where the runtime daemon bind-mounts its own
// executable inside EVERY runtime container (internal/runtime/daemon.go:
// createContainer hard-fails when it cannot — "the orchicon binary is
// bind-mounted into every runtime container, never baked"). It is the same
// convention opencode's runtimeContainerBinaryPath follows for its MCP
// sidecars, and it is the only orchicon path GUARANTEED to exist in the
// container: the control plane's own os.Executable() is a host path.
const HookBinaryContainerPath = "/usr/local/bin/orchicon"

// ProjectDirEnv is the env var the hook reads to learn the worker's project
// boundary. The session sets it on the child's env (childEnv).
const ProjectDirEnv = "ORCHICON_CLAUDE_PROJECT_DIR"

// HookBinFallback is used when neither the env var nor os.Executable() resolves.
const HookBinFallback = "orchicon"

// PreflightHookExecutable proves the PreToolUse hook can actually RUN, and reports the consequence when it
// cannot.
//
// THE HOOK IS THE AUTHORITY FOR EVERY GATED TOOL IN A WORKER SESSION, and nothing checked that it would
// launch. When it does not, the session degrades in a way that looks like a permission WALL rather than a
// broken hook: no allow verdict arrives, so each tool falls through to claude's own permission flow, whose
// ask cannot be answered in a `-p` session and therefore becomes a TERMINAL DENIAL. The operator recorded
// exactly that — a worker reporting that it was refused, waiting for "an approval that no human can give in
// this run", for `git merge --ff-only` among other commands, while this package's own decideBash ALLOWS
// those commands (measured).
//
// So the failure to launch is what must be loud, and it must happen BEFORE the session starts rather than as
// sixty inexplicable refusals inside it. Failing the launch is the honest outcome: a claude worker whose
// boundary cannot be enforced is not a worker that should run.
//
// IN CONTAINER MODE the binary is a path inside the container image, which this process cannot stat — the
// daemon guarantees it by bind-mounting itself there and refusing to create the container otherwise, so only
// the form is checked.
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
	return nil
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

// TodoTrackToolNames are the task/todo-tracking tools a claude worker session
// must be OPTED INTO. Claude Code's task tracking is not on by default for a
// non-interactive (`-p`) session: a tool that carries no allow verdict falls to
// claude's default permission flow, which refuses an unanswerable ask. This
// adapter therefore (a) names the tools in the PreToolUse hook matcher,
// (b) pre-approves them with `permissions.allow` and (c) sets TodoToolsEnv,
// all NON-BYPASS — no
// `--dangerously-*` token is ever emitted (TestNoBypassPermissionFlagIsEverEmitted
// pins that). TodoWrite is the whole-list-replacement tool; TaskCreate/TaskUpdate
// are the cumulative task family (see parse.go). TaskList/TaskGet/TaskOutput are
// read-only members of the same family, named so the family is uniformly allowed.
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
//
// THEY ARE NAMED HERE RATHER THAN ASKED FOR BY THE HOOK, and that distinction is the whole fix. A
// PreToolUse hook's `permissionDecision: "ask"` is NOT routed to `--permission-prompts host` in a
// headless session: MEASURED against the real CLI (2.1.289), it becomes an immediate tool ERROR the
// model reads as "this call needs the operator's approval" — 18 asks produced 0 `can_use_tool`
// control frames and 0 consent cards, which is the operator's "claude adapter ask sessions seem
// blocked by everything". Only the PERMISSION SYSTEM's prompt reaches the host, so the tools that
// need an operator decision belong in this list, and the hook stays silent about them
// (RunHook abstains on an ask verdict).
//
// `mcp__*` USED TO BE THE THIRD-PARTY HALF and has been REMOVED. It read "consent gates each MCP
// call", which is no longer the rule: the operator's MCP servers are allowed outright —
// "if it's added in scope it should just have access" — because attaching a server to a
// conversation or its project IS the approval, made deliberately at configuration time.
//
// NO MCP ENTRY BELONGS HERE AT ALL, for the platform's own surface or anyone else's. An entry in
// this list makes the permission system PROMPT, and a prompt is a card; the hook instead returns an
// explicit ALLOW for the whole `mcp__` class (isMCPTool), which bypasses the permission system.
// Leaving `mcp__*` here would re-introduce the card by the other route, so the two halves move
// together. The MODE boundary remains the governor for an opaque MCP tool — see internal/askmode.
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

// bypassLaunchFlags are the LAUNCH spellings that would discard the
// restrictions. Their ABSENCE from the argv and the settings document is pinned
// by TestNoBypassPermissionFlagIsEverEmitted.
var bypassLaunchFlags = []string{
	"--dangerously-skip-permissions",
	"--allow-dangerously-skip-permissions",
	"--dangerously-allow-skip-permissions",
	"--dangerously-skip-permissions-with-sandbox",
}

// bypassPermissionMode is the --permission-mode value that would disable the
// permission system entirely. It must never be the mode this adapter launches
// with; the settings document additionally pins it off with
// `disableBypassPermissionsMode: "disable"`.
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
	// Profile selects the hook's rule set (ProfileWorkerEnvValue is the
	// non-interactive sandbox and the ZERO VALUE, so an existing caller is
	// unchanged; ProfileAskEnvValue is the interactive profile where consent
	// gates the action). It also switches on `--permission-prompts host`, which
	// is what makes the CLI raise the ask rather than auto-deny it.
	Profile string
}

// HookBinaryPath resolves the binary the PreToolUse hook invokes: the explicit
// env override first (the runtime container's /usr/local/bin/orchicon), then
// this process's own executable (the control plane IS the orchicon binary), then
// a bare `orchicon` on PATH.
func HookBinaryPath() string {
	if v := strings.TrimSpace(os.Getenv(HookBinEnv)); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		return exe
	}
	return HookBinFallback
}

// HookBinaryFor resolves the binary the PreToolUse hook command invokes for one
// transport.
//
// The container arm is not a nicety. The claude argv — and therefore the
// settings document naming the hook command — is built by the CONTROL PLANE on
// the host and executed by the supervisor inside the run's container, so a host
// path there is a hook that cannot launch: the whole AUTHORITY layer (protected
// paths, the project boundary with its carve-outs, the operator policy) would
// silently disappear for every container run, which is every run this platform
// makes. HookBinEnv is deliberately ignored on this transport: whatever the
// plane's own environment says, it names a host path.
func HookBinaryFor(isContainer bool) string {
	if isContainer {
		return HookBinaryContainerPath
	}
	return HookBinaryPath()
}

// PermissionArgs returns the FULL permission launch argv: the mode flag, the
// inline settings document, and the disallowed-tool list.
//
// The returned error is a POLICY LOAD failure only. It is returned rather than
// swallowed so the caller records it; the launch still happens (the hook fails
// closed on the same file, and `permissions.deny` carries the never-allow class
// regardless), so a broken operator policy degrades to STRICTER, never to
// permissive.
func PermissionArgs(o PermissionOptions) ([]string, error) {
	settings, err := BuildSettings(o)
	args := []string{"--permission-mode", "default"}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	if o.Profile == ProfileAskEnvValue {
		// The Ask session DEPENDS on the CLI asking the host: that request is what
		// raises the can_use_tool control frame the consent cards answer. The CLI's
		// default for this flag is already "host", but it is named EXPLICITLY here
		// because the alternative — "none", nobody answers and anything that would
		// prompt is denied automatically — would silently turn the interactive
		// profile back into the worker sandbox on an operator's screen: every write
		// refused with no card and no explanation.
		args = append(args, "--permission-prompts", "host")
	}
	// Deny the built-in subagent tool by NAME as well as through the hook — the
	// CLI-level deny survives a hook that fails to launch. Denied in EVERY profile
	// (see DecideToolForAsk).
	args = append(args, "--disallowedTools", strings.Join(workerrestrict.SubagentToolNames, " "))
	return args, err
}

// BuildSettings marshals the `--settings` document. Empty-string return means
// "no settings" (only possible if marshalling failed, which cannot happen for
// this shape); the error is the policy load failure, if any.
// hookCommand renders the shell command claude runs for the PreToolUse hook.
//
// THE PATH MUST BE QUOTED, and this is a Windows defect that reads as a permissions defect. The command is
// executed by a SHELL, so an unquoted path is split at its first space: `C:\Program Files\Orchicon\orchicon.exe
// claude-hook` becomes `C:\Program` plus arguments, the hook never runs, and claude treats a FAILING
// PreToolUse hook as a BLOCK. Every gated tool is then refused — reported as "Write/Edit permission was
// denied for all project files", which points at the permission POLICY rather than at a broken command.
//
// It is silent in exactly the way that matters: PreflightHookExecutable checks that the FILE exists and is
// executable, and a path with spaces satisfies that. The breakage only appears at shell-split time.
//
// Quoting is applied unconditionally rather than only when a space is present: it changes nothing for a path
// without spaces, and a conditional would leave the next odd path (a quote character, a unicode space)
// uncovered. The path never contains a double quote itself on either platform — an executable name cannot.
func hookCommand(hookBin string) string {
	return `"` + hookBin + `" claude-hook`
}

func BuildSettings(o PermissionOptions) (string, error) {
	policyDeny, policyErr := policyDenyEntries(o.PolicyPath)

	deny := make([]string, 0, len(neverallow.Shimmed())*2+len(policyDeny)*2+len(workerrestrict.SubagentToolNames))
	// The shared never-allow class, as claude's own Bash rule syntax. The
	// binaries come from the CLASS (neverallow.Shimmed()), not from a second
	// hand-written list.
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
	//
	// Only a permission-system prompt reaches the host (`--permission-prompts host`), and that host
	// prompt is the `can_use_tool` frame the consent cards answer. A hook verdict of "ask" never gets
	// there — see AskPermissionToolNames. The worker profile gets NO ask list: it is non-interactive
	// by design, so a prompt there would be an unanswerable refusal.
	perms := map[string]any{
		// NOT acceptEdits and NOT bypassPermissions: an unanswerable ask in
		// -p mode is a refusal, which is the honest worker semantic.
		"defaultMode": "default",
		"deny":        deny,
		// The task/todo-tracking family is PRE-APPROVED so it streams at all
		// in a non-interactive session. This is an explicit opts-in, not a
		// bypass: every other restriction (the hook authority, the deny list,
		// the project boundary) stays in force, and these tools only write
		// the in-session todo list.
		"allow":                 append([]string(nil), TodoTrackToolNames...),
		"additionalDirectories": dirs,
		// Pin the bypass mode OFF at the settings layer too, so a later
		// `--permission-mode bypassPermissions` on the command line is refused
		// by claude itself.
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
	// THE CARD'S ONLY DELIVERY PATH. A `PreToolUse` verdict cannot ask the operator in this launch shape
	// (a hook's "ask" is a tool error, and the permission system's ask is terminal — both MEASURED against
	// 2.1.289), but a `PermissionRequest` hook FIRES and its allow/deny IS honoured. So the Ask profile
	// registers one: it connects to the session's consent socket, raises the card through the ordinary
	// consent path, blocks until the operator answers, and returns the decision.
	//
	// THE TIMEOUT EXCEEDS THE WAIT (consent_hook.go: 960s vs 15m) so OUR side expires first and the hook
	// still exits cleanly with a deny, rather than being killed mid-write while the operator is reading.
	//
	// THE MATCHER IS DERIVED FROM AskPermissionToolNames, so the tools that prompt and the tools that card
	// cannot diverge: a name in one and not the other is a call the permission system asks about with no
	// hook to answer it — a terminal denial, which is the whole defect.
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
// An ABSENT policy is a nil error and no entries ("no policy" is a legitimate
// state, exactly as permpolicy.Load documents); a MALFORMED one is returned as
// its PolicyLoadError so the caller can record it — it is never silently
// dropped, and the hook fails closed on the same file.
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
				// Unquote as well as untrim: hookCommand always quotes the binary path (a Windows install
				// path contains spaces), so the raw command is `"<path>" claude-hook` and returning it with
				// the quotes still on would break every caller that compares it to a path.
				bin := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(h.Command), " claude-hook"))
				// SHELL quoting, not Go quoting: strip one surrounding pair of double quotes. strconv.Unquote
				// is the wrong tool here — a Windows path's backslashes are not valid Go escapes, so it
				// fails and the quotes would survive.
				bin = strings.TrimSuffix(strings.TrimPrefix(bin, `"`), `"`)
				return bin
			}
		}
	}
	return ""
}
