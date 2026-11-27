package antigravity

// launch.go — HOW THE `agy acp` CHILD IS INVOKED, and the guard that stops a permission-free launch.
//
// TWO THINGS LIVE HERE THAT THE BRIDGE MUST NEVER SHORTCUT:
//
//  1. THE INVOCATION COMES FROM BuildSpawnSpec (procspec.go), and nowhere else. That builder is the one
//     place the tenant's auth method, the GEMINI_API_KEY environment variable and the
//     --disable-telemetry policy are applied, and it is where the key is kept off argv.
//
//  2. THE ARGV IS CHECKED FOR A PERMISSION-BYPASS FLAG BEFORE IT IS SPAWNED. agy accepts
//     `--dangerously-skip-permissions`, and a worker launched that way would never send a
//     `session/request_permission` at all — so this adapter's entire policy (permissions.go) would be
//     silently skipped, and the run would look identical to a gated one in every log line. The claude
//     bridge has the same guard (permissions.go:194-199, bypassLaunchFlags); the check is repeated here
//     rather than shared because the flag SPELLINGS differ per CLI, and a shared list would either miss
//     agy's spelling or carry claude's into agy's argv.

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// antigravityBypassFlags are the agy launch spellings that would discard the permission path.
//
// A LITERAL `--dangerously-skip-permissions` IS THE ONE THE RECORDING PROVES agy accepts; the others are
// the same intent spelled the way other CLIs in this family spell it, listed so a future agy that adopts
// one is refused rather than silently ungated.
var antigravityBypassFlags = []string{
	"--dangerously-skip-permissions",
	"--dangerously-allow-skip-permissions",
	"--allow-dangerously-skip-permissions",
	"--skip-permissions",
	"--yolo",
	"--auto-approve",
	"--always-approve",
	"--never-ask",
}

// antigravityBypassModeFlags are the `--flag value` spellings whose VALUE is what bypasses. They are
// checked as a PAIR because a bare `--approval-mode` is not a bypass — only one of its values is.
var antigravityBypassModeFlags = map[string][]string{
	"--approval-mode":   {"yolo", "never", "auto-approve", "bypass", "none"},
	"--permission-mode": {"bypasspermissions", "yolo", "none"},
}

// assertNoBypass fails when an argv would launch agy without its permission path.
//
// IT PANICS ON NOTHING and it does not sanitise: a spec that reached here with a bypass flag is a bug in
// the caller, and silently stripping the flag would hide it while leaving the argv shape different from
// what the builder produced. The launch FAILS and says which token was the problem.
func assertNoBypass(argv []string) error {
	for i, tok := range argv {
		lower := strings.ToLower(strings.TrimSpace(tok))
		// A `--flag=value` spelling is the same flag; split it so one list covers both.
		name, value, hasValue := strings.Cut(lower, "=")
		for _, bad := range antigravityBypassFlags {
			if name == bad {
				return fmt.Errorf("antigravity: refusing to launch agy with %q — it disables agy's permission "+
					"system entirely, so the worker policy could not gate a single tool call", tok)
			}
		}
		if values, ok := antigravityBypassModeFlags[name]; ok {
			// `--approval-mode yolo` OR `--approval-mode=yolo`.
			arg := value
			if !hasValue && i+1 < len(argv) {
				arg = strings.ToLower(strings.TrimSpace(argv[i+1]))
			}
			for _, v := range values {
				if arg == v {
					return fmt.Errorf("antigravity: refusing to launch agy with %q — it disables agy's permission "+
						"system entirely, so the worker policy could not gate a single tool call", strings.Join([]string{tok, arg}, " "))
				}
			}
		}
	}
	return nil
}

// settingsBypassKeys are the `--settings` JSON keys that would weaken or disable agy's permission
// system from INSIDE the document rather than on argv.
//
// assertNoBypass guards the ARGV SPELLINGS (`--dangerously-skip-permissions`, `--approval-mode yolo`); a
// settings document is a different surface with the same power, and the two must be guarded together or
// the guard has a hole exactly the width of the feature that added it.
var settingsBypassKeys = []string{
	"autoapprove", "auto_approve", "auto-approve",
	"yolo", "alwaysapprove", "always_approve",
	"skip_permissions", "skip-permissions", "skippermissions",
	"dangerously_skip_permissions", "dangerously-skip-permissions",
	"bypass_permissions", "bypass-permissions", "bypasspermissions",
}

// assertSettingsNotBypass refuses a `--settings` document that could turn the permission system off.
//
// THIS ADAPTER ONLY EVER EMITS A DENY LIST, so a document naming one of these keys did not come from
// modegate.go — it came from a caller that is trying to widen the gate, and that is a bug worth failing
// the launch over rather than a preference worth honouring.
func assertSettingsNotBypass(settings string) error {
	doc := strings.ToLower(strings.TrimSpace(settings))
	if doc == "" {
		return nil
	}
	for _, key := range settingsBypassKeys {
		if strings.Contains(doc, `"`+key+`"`) {
			return fmt.Errorf("antigravity: refusing a --settings document naming %q — it can disable agy's "+
				"permission system, so the Ask mode boundary would be a claim rather than an enforcement", key)
		}
	}
	return nil
}

// resolveAgyBinary resolves the agy executable.
//
// The resolver is adapter.ResolveBinary, the SAME per-OS/PATH lookup every other adapter uses, so an
// operator's shim and a package install both work. A "" result is a hard failure carrying the shim hint,
// because launching nothing produces an error that names no cause.
func resolveAgyBinary() (string, error) {
	home, _ := os.UserHomeDir()
	bin := strings.TrimSpace(adapter.ResolveBinary(goruntime.GOOS, home, adapter.KindAntigravity, os.Getenv))
	if bin == "" {
		hint := adapter.ShimHint(goruntime.GOOS, home, adapter.KindAntigravity, os.Getenv)
		if hint != "" {
			return "", fmt.Errorf("antigravity: the `agy` CLI could not be resolved on this host — %s", hint)
		}
		return "", fmt.Errorf("antigravity: the `agy` CLI is not on PATH; install Antigravity or put `agy` on PATH " +
			"(Orchicon launches the vendor binary and never bundles it)")
	}
	return bin, nil
}

// isContainer reports whether this execution's child belongs inside the run's runtime container.
//
// The rule is IDENTICAL to claude's (adapter.go:265-282): a container child needs a wired daemon, a run
// id to lease the container by, and a run that is not in local mode. A test override forces the local
// transport, because a fake process cannot live inside a container.
func (b *Bridge) isContainer(m scheduler.ExecutionManifest) bool {
	if b.factoryOverride != nil {
		return false
	}
	return b.rt != nil && m.RuntimeWorkflowID != "" && m.ExecutionMode != "local"
}

// spawnFactory selects the transport: a container child when this execution belongs to a workflow run
// with a daemon wired and is not local mode, otherwise a host subprocess.
func (b *Bridge) spawnFactory() acp.ProcFactory {
	if b.factoryOverride != nil {
		return b.factoryOverride
	}
	if b.rt != nil {
		return acp.ContainerFactory(b.rt)
	}
	return acp.LocalFactory{}.NewProc
}

// askFactory selects the transport for an ASK CONVERSATION's child, and it is NOT spawnFactory().
//
// AN ASK CONVERSATION HAS NO WORKFLOW RUN. spawnFactory() prefers the CONTAINER transport whenever the
// bridge holds a runtime client — correct for a worker, whose child must live in its run's container —
// but a conversation is not a run: there is no run id to lease a container by, and the container factory
// refuses such a spec outright ("acp: no workflow run id for container spawn"). Because EVERY real plane
// wires a runtime client, using spawnFactory() here made every antigravity Ask turn fail on its first
// spawn with `create conversation session: antigravity ask: spawn: acp: no workflow run id for container
// spawn` — the conversation was registered Ask-capable and could not answer a single message. MEASURED
// on a local-mode plane. claude's Ask path has always spawned locally for the same reason
// (internal/claude/ask.go spawnAsk → newLocalProc).
//
// The conversation therefore runs the host's own `agy` — the binary the operator installed and signed
// in, whose credentials the picker's catalogue came from — with the mode gate, the permission handler
// and the OS-level guard shim making up the boundary (ask_turn.go). An explicit factoryOverride (tests)
// still wins, so the fixtures drive this path unchanged.
func (b *Bridge) askFactory() acp.ProcFactory {
	if b.factoryOverride != nil {
		return b.factoryOverride
	}
	return acp.LocalFactory{}.NewProc
}

// childEnv builds the environment the agy child inherits.
//
// THE LOCAL TRANSPORT GETS THE OS-LEVEL GUARD SHIM, exactly as claude's childEnv does: BuildSpawnSpec has
// already decided the auth-method half of the environment, and this adds the PATH shim so the destructive
// commands this process could not otherwise intercept are refused by the OS as well as by the policy. A
// container child takes the same environment WITHOUT the shim — the shim is a host process, and the
// container's own boundary is the run's.
func (b *Bridge) childEnv(manifest scheduler.ExecutionManifest) ([]string, func()) {
	env := os.Environ()
	cleanup := func() {}
	if b.isContainer(manifest) {
		return env, cleanup
	}
	g, err := guard.NewExecutionGuard(manifest.ProjectDir)
	if err != nil {
		b.log.Warn("antigravity: the OS-level execution guard could not be built for the local transport — "+
			"the child runs with the ACP policy only", "execution", manifest.ExecutionID, "error", err)
		return env, cleanup
	}
	return g.Apply(env), g.Close
}

// containerStdioOpener is the daemon surface acp.ContainerFactory needs. *runtime.Client satisfies it
// directly (acp.StdioOpener); this function exists so the assertion is checked at COMPILE time rather
// than only where a container spawn happens to be exercised.
func containerStdioOpener(rt *runtime.Client) acp.StdioOpener {
	var opener acp.StdioOpener = rt
	return opener
}

// executionDir resolves the worker's working directory: the run's worktree when provisioned, else the
// project dir (the same rule claude and opencode use).
func executionDir(m scheduler.ExecutionManifest) string {
	if strings.TrimSpace(m.WorktreePath) != "" {
		return m.WorktreePath
	}
	return m.ProjectDir
}

// composeGoalPrompt is the FIRST prompt's text.
//
// IT CARRIES ONLY THE GOAL AND THE ACCEPTANCE CRITERIA. The system prompt — worker prompt, project
// context files and skills — already travelled as `_meta.antigravity.systemPrompt.append` on
// `session/new`, so repeating it here would double every token of it in the model's context and, worse,
// place it where a slash-command-looking line in the context could be interpreted as one.
func composeGoalPrompt(m scheduler.ExecutionManifest) string {
	var b strings.Builder
	if g := strings.TrimSpace(m.Goal); g != "" {
		b.WriteString("=== GOAL ===\n")
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	if a := strings.TrimSpace(m.AcceptanceCriteria); a != "" {
		b.WriteString("=== ACCEPTANCE CRITERIA ===\n")
		b.WriteString(a)
		b.WriteString("\n")
	}
	out := b.String()
	if strings.TrimSpace(out) == "" {
		return "Proceed with the task."
	}
	return out
}

// CONTEXT FILES: agy's OWN repo customizations LOAD, and that is deliberate parity with claude.
//
// The claude bridge launches with no `--setting-sources` override, so claude's own `CLAUDE.md` in the
// worktree is loaded by the CLI alongside the system prompt Orchicon composes. This bridge does the same
// thing by NOT DOING ANYTHING: `agy acp --help` lists `--include-default-customizations` with a default of
// **true**, so agy's own `AGENTS.md` and `.agents/` in the worktree load the way the CLI loads them by
// default. The flag is NOT passed explicitly — the default is already the behaviour we want, and passing it
// would pin a vendor default into our argv where a later agy could change it. A worker that finds its own
// project's `AGENTS.md` should see it, exactly as it sees `CLAUDE.md` on the claude adapter.
//
// THE COMPOSED PROMPT DOES NOT RELY ON THAT PATH. The worker prompt, the project context files and the
// skills travel as `_meta.antigravity.systemPrompt.append` on `session/new`/`session/resume`
// (see composeGoalPrompt and sessionrun.go), which is the platform's own channel and the one a resume
// preserves. `AGENTS.md` is therefore ADDRESSED BY BOTH mechanisms — the same overlap claude already
// has, and preferable to the alternative: suppressing the repo's own customizations would make a worker
// behave differently under Orchicon than under the CLI for no gain.
//
// If a future agy adds a flag that DISABLES them (`--no-include-default-customizations` or a
// `=false` spelling), it must NOT be added to the launch argv without revisiting this decision —
// TestBridge_LaunchArgvKeepsDefaultCustomizations asserts the argv carries no such flag.

// additionalDirectories is the `additionalDirectories` list for `session/new` and `session/resume`: the
// SAME two carve-outs claude passes as --add-dir (workerrestrict.CarveOutDirs), so a worker can write to
// the scratch dir and its own run metadata and nowhere else.
//
// IT IS COMPUTED FROM ProjectDir, NOT FROM THE CWD. The cwd is the worktree; the .orchicon run metadata
// lives under the PROJECT root, which is the directory both claude and opencode treat as the carve-out
// source.
func additionalDirectories(m scheduler.ExecutionManifest) []string {
	dirs := workerCarveOuts(m.ProjectDir)
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if strings.TrimSpace(d) != "" {
			out = append(out, filepath.Clean(d))
		}
	}
	return out
}
