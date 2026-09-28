package claude

import (
	"encoding/json"
	"os"
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

// HookBinEnv names the absolute path of the `orchicon` binary the PreToolUse
// hook command invokes. The runtime container sets it to the daemon's
// bind-mount (/usr/local/bin/orchicon), which is guaranteed present inside every
// runtime container (same convention as opencode's MCPBinaryPath).
const HookBinEnv = "ORCHICON_CLAUDE_HOOK_BIN"

// ProjectDirEnv is the env var the hook reads to learn the worker's project
// boundary. The session sets it on the child's env (childEnv).
const ProjectDirEnv = "ORCHICON_CLAUDE_PROJECT_DIR"

// HookBinFallback is used when neither the env var nor os.Executable() resolves.
const HookBinFallback = "orchicon"

// HookToolMatcher is the PreToolUse matcher: exactly the tools whose input can
// name a command or a path, plus the built-in subagent tool. Claude matches the
// matcher as a regular expression against the tool name.
const HookToolMatcher = "Bash|Read|Write|Edit|MultiEdit|NotebookEdit|Glob|Grep|Task|Agent"

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
	// Deny the built-in subagent tool by NAME as well as through the hook — the
	// CLI-level deny survives a hook that fails to launch.
	args = append(args, "--disallowedTools", strings.Join(workerrestrict.SubagentToolNames, " "))
	return args, err
}

// BuildSettings marshals the `--settings` document. Empty-string return means
// "no settings" (only possible if marshalling failed, which cannot happen for
// this shape); the error is the policy load failure, if any.
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

	settings := map[string]any{
		"permissions": map[string]any{
			// NOT acceptEdits and NOT bypassPermissions: an unanswerable ask in
			// -p mode is a refusal, which is the honest worker semantic.
			"defaultMode":           "default",
			"deny":                  deny,
			"additionalDirectories": dirs,
			// Pin the bypass mode OFF at the settings layer too, so a later
			// `--permission-mode bypassPermissions` on the command line is refused
			// by claude itself.
			"disableBypassPermissionsMode": "disable",
		},
		"hooks": map[string]any{
			"PreToolUse": []map[string]any{{
				"matcher": HookToolMatcher,
				"hooks": []map[string]any{{
					"type":    "command",
					"command": hookBin + " claude-hook",
				}},
			}},
		},
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
				return strings.TrimSuffix(strings.TrimSpace(h.Command), " claude-hook")
			}
		}
	}
	return ""
}
