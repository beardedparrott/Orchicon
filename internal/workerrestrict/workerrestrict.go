// Package workerrestrict is the ONE copy of the rule set that sandboxes an
// Orchicon worker: the command patterns a worker's bash tool may never run, the
// two directory carve-outs that widen the project boundary, the built-in
// subagent tool a worker may never use, and the matchers both adapters call.
//
// WHY IT IS ITS OWN PACKAGE. The restrictions used to live inside
// internal/opencode/config.go as that adapter's private rule list. A second
// worker adapter (the claude bridge) must be constrained by THE SAME
// declarations, and the acceptance criterion is that a destructive command is
// refused IDENTICALLY for a claude worker and an opencode worker. "Two lists
// that agree today" agree only until someone edits one — the argument
// internal/neverallow/share_test.go already makes for the binary class. So the
// list lives here, internal/opencode builds its `permission` block FROM it
// (byte-identical output), and the claude PreToolUse hook calls the SAME
// matchers. Drift is then impossible by construction, not by discipline.
//
// WHY IT IS NOT internal/neverallow. That package is deliberately only the
// "never legitimate, in any session" binary class and says so in its own doc:
// it imports nothing internal and is explicitly NOT the whole worker deny list.
// The project-boundary rules here (destructive `rm` targets, /dev/sd*
// redirection, root-wide chmod/chown, download-and-execute) are a property of
// the worker SANDBOX — an interactive Ask session keeps the class and drops the
// sandbox — so they cannot move into it.
//
// WHY IT IS NOT internal/permpolicy. That is the OPERATOR's own policy store,
// a different layer above this one; a policy deny is an operator decision,
// these are the platform's non-negotiables.
//
// CONSUMED BY: internal/opencode (its worker profile), internal/claude (the
// PreToolUse hook decision core), and proven shared by
// internal/claude/parity_test.go and internal/neverallow/share_test.go.
package workerrestrict

import (
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/protectedpath"
)

// ScratchDir is the single directory workers may use outside the project for
// ephemeral scratch (screenshots, logs, downloaded files). It is the primary
// external_directory carve-out: a precise `/tmp/orchicon/**` allow, so the
// supervisor socket (/tmp/orchicon-agent.sock), the execution guard shims
// (/tmp/orchicon-guard-*), and the opencode-data dirs (/tmp/opencode-data-*,
// which hold the seeded model auth.json copies) stay behind the deny. Workers
// are told to use it in the composite prompt's runtime-environment block.
//
// MOVED HERE VERBATIM from internal/opencode/config.go (which now aliases it),
// so both adapters carve out exactly the same subtree.
const ScratchDir = "/tmp/orchicon"

// RunDirPattern is the carve-out for Orchicon's own run metadata
// (`.orchicon/<run>/` under the project root, and any `.orchicon/worker.recovery`
// / run summary files). Workflow step workers run in an isolated worktree that
// is a SIBLING of the run `.orchicon/` directory, so without this allow they hit
// an external-directory deny on every read the composite prompt explicitly tells
// them to make (summary, facts_learned, issues) — each block burns a full tool
// call + retry. The carve-out is tight: only the `.orchicon/` subtree
// (Orchicon-owned, aliased run metadata, gitignored), never the supervisor socket
// or the auth/data dirs elsewhere on disk.
//
// MOVED HERE VERBATIM from internal/opencode/config.go (which now aliases it).
const RunDirPattern = "**/.orchicon/**"

// RunDirSegment is the path SEGMENT RunDirPattern keys on. The matcher here works
// on a literal path, not on a glob, so the `**/…/**` pattern is realised as "some
// directory component of the path is `.orchicon` and at least one component
// follows it" — which is exactly what `**/.orchicon/**` matches.
const RunDirSegment = ".orchicon"

// TaskToolDeny is the built-in SUBAGENT tool a worker may never use, denied in
// every profile. Orchicon already splits work into focused per-worker steps; a
// subagent that the adapter spawns re-prepends its own system prompt and
// re-carries the parent's history, roughly DOUBLING context on that turn. The
// rule removes the surface entirely.
//
// MOVED HERE VERBATIM from internal/opencode/config.go (which now aliases it).
const TaskToolDeny = "task"

// SubagentToolNames lists every spelling of the built-in subagent tool the two
// adapters expose, so a deny cannot be evaded by the CLI naming its subagent
// surface differently. opencode names it `task` (TaskToolDeny); the claude CLI
// names its built-in subagent tool `Task` (and, in versions that renamed the
// same surface, `Agent`). Both are denied by the claude hook, matching the
// opencode `task` deny exactly.
var SubagentToolNames = []string{"Task", "Agent"}

// IsSubagentTool reports whether a tool name is the built-in subagent tool whose
// opencode spelling is TaskToolDeny.
func IsSubagentTool(toolName string) bool {
	for _, n := range SubagentToolNames {
		if toolName == n {
			return true
		}
	}
	return toolName == TaskToolDeny
}

// ProjectBoundaryDenyPatterns is the worker's own project-boundary deny list:
// the destructive `rm` targets (absolute system paths, /, ~, $HOME,
// --no-preserve-root, current-dir wipes — in-project cleanup is allowed), the
// shell-construct smuggling variants, device redirection, root-wide
// chmod/chown, and download-and-execute.
//
// It is the list that used to be inline in internal/opencode/config.go, moved
// here verbatim and in the SAME ORDER so the emitted opencode permission subtree
// stays byte-identical (internal/opencode/testdata/worker_permission_golden.json).
// It is deliberately NOT the never-allow binary class — that stays in
// internal/neverallow and is prepended by WorkerDenyPatterns.
func ProjectBoundaryDenyPatterns() []string {
	return []string{
		// rm family — target-scoped. In-project cleanup (`rm -rf build/`,
		// `node_modules`, `.next`) is legitimate and no longer denied (the
		// denial burned worker tokens on `find -delete`/python workarounds);
		// the OS-level execution guard is the precise backstop (it allows rm
		// only when every path stays inside the project + scratch). What stays
		// denied is the destructive class: absolute system paths, /, ~, $HOME,
		// --no-preserve-root, and the current-dir-wipe variants — the
		// commands that escape the project no matter how they're written.
		"rm -rf /", "rm -r /", "rm -R /", "rm -f /", "rm -fr /", "rm -Rf /",
		"rm -rf /*", "rm -fr /*", "rm -r /*", "rm -R /*", "rm -f /*",
		"rm -rf /home/*", "rm -rf /root/*", "rm -rf /etc/*", "rm -rf /usr/*",
		"rm -rf /var/*", "rm -rf /bin/*", "rm -rf /boot/*",
		"rm -rf ~", "rm -rf ~/*", "rm -rf $HOME", "rm -rf $HOME/*",
		"rm -rf ${HOME}/*", "rm -rf ${HOME}*",
		"rm --no-preserve-root *", "rm -rf --no-preserve-root *",
		"/bin/rm *", "/usr/bin/rm *", "/bin/rm -rf *", "/usr/bin/rm -rf *",
		"rm -rf . /", "rm -rf . ..",
		// shell-construct smuggling variants that hide rm.
		"(*rm*", "{*rm*",
		"* & rm *", "* && rm *", "* ; rm *", "* || rm *", "* | rm *",
		"* > /dev/sd*", "* >> /dev/sd*", ": > /dev/sd*",
		"echo * > /dev/sd*", "echo * >> /dev/sd*",
		"cat * > /dev/sd*", "cat * >> /dev/sd*", "cp * /dev/sd*",
		"mv * /dev/null", "cp -r * /dev/null", "cp -a * /dev/null",
		// root-wide permission changes.
		"chmod -R 777 /*", "chmod -R 777 /", "chmod -R 000 /*", "chmod -R 000 /",
		"chown -R * /*", "chown -R * /", "chmod -R 777 * /",
		// download-and-execute (arbitrary remote code).
		"curl * | sh", "curl * | bash", "curl * | sh -", "curl * | bash -",
		"curl * | zsh", "wget * | sh", "wget * | bash", "wget * | zsh",
	}
}

// WorkerDenyPatterns is the FULL worker bash deny list, in evaluation order:
// the shared never-allow binary class first, then the project-boundary rules.
// It returns a freshly allocated slice, so a caller that appends cannot mutation
// the shared declarations.
//
// THIS IS THE FUNCTION BOTH ADAPTERS BUILD FROM. internal/opencode turns it into
// its `bash` permission map; internal/claude matches a command against it. One
// copy ⇒ identical verdicts.
func WorkerDenyPatterns() []string {
	return append(neverallow.DenyRules(), ProjectBoundaryDenyPatterns()...)
}

// MatchCommand reports the first rule in WorkerDenyPatterns that matches cmd, and
// whether any matched. It is the exact matcher semantic opencode applies to the
// command string ("opencode matches each rule against the command string"), so a
// verdict here IS opencode's verdict.
//
// Glob semantic: `*` matches any run of characters INCLUDING spaces and `/` (a
// shell command is one flat string, so a pattern like `* && sudo *` must span the
// whole command). Everything else is literal. A pattern with no `*` is an exact
// match.
func MatchCommand(cmd string) (rule string, denied bool) {
	for _, p := range WorkerDenyPatterns() {
		if matchGlob(p, cmd) {
			return p, true
		}
	}
	return "", false
}

// MatchCommandAgainst reports whether any of the supplied rules matches cmd and
// which one — the seam a caller with a DIFFERENT copy of the rules (a test, or a
// writer that materialised the opencode map) uses to prove both copies agree.
func MatchCommandAgainst(rules []string, cmd string) (rule string, denied bool) {
	for _, p := range rules {
		if matchGlob(p, cmd) {
			return p, true
		}
	}
	return "", false
}

// matchGlob is the wildcard matcher (`*` = any run). Iterative backtracking, so
// it is linear in practice and has no recursion depth limit on a long command.
func matchGlob(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			mark = si
			pi++
			continue
		}
		if pi < len(pattern) && pattern[pi] == s[si] {
			pi++
			si++
			continue
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// OutsideProject reports whether a path target escapes the worker's project
// boundary, and the rule that refuses it. It is the port of opencode's
// `external_directory` rule (deny `*`, allow ScratchDir+"/**", allow
// RunDirPattern) onto a LITERAL path, so the claude hook reaches the same verdict
// the opencode profile reaches.
//
// A target is ALLOWED when:
//   - it resolves inside projectDir, or
//   - it is ScratchDir itself or inside it (the scratch carve-out), or
//   - some path component is `.orchicon` with at least one component after it
//     (the run-metadata carve-out, `**/.orchicon/**`).
//
// A RELATIVE target is resolved against projectDir first, so `../../etc/passwd`
// is correctly refused rather than trivially allowed as "relative = in project".
func OutsideProject(target, projectDir string) (allowed bool, rule string) {
	t := strings.TrimSpace(target)
	if t == "" {
		// No path at all: nothing to bound (the caller's tool does not name a
		// path). Not an external access.
		return true, ""
	}
	// A glob-style tool input carries a trailing wildcard, not a literal path.
	t = strings.TrimSuffix(t, "/**")
	p := t
	if !filepath.IsAbs(p) {
		if strings.TrimSpace(projectDir) == "" {
			// No project root known: a relative target cannot be PROVEN inside
			// scope, and the guard's empty-project arm treats every target as
			// outside. Fail closed.
			return false, "*"
		}
		p = filepath.Join(projectDir, p)
	}
	p = filepath.Clean(p)
	if insideDir(p, projectDir) {
		return true, ""
	}
	if p == ScratchDir || insideDir(p, ScratchDir) {
		return true, ""
	}
	if hasRunDirSegment(p) {
		return true, ""
	}
	return false, "*"
}

// CarveOutDirs returns the two directory carve-outs, as absolute DIRECTORY
// paths (not globs): the scratch dir, and the run-metadata subtree under
// projectDir. Both adapters derive their allow list from here, so a carve-out
// cannot be widened in one adapter only.
func CarveOutDirs(projectDir string) []string {
	out := []string{ScratchDir}
	if strings.TrimSpace(projectDir) != "" {
		out = append(out, filepath.Join(projectDir, RunDirSegment))
	}
	return out
}

// ProtectedRoots returns the two protected-path root lists the destructive-path
// rule uses, computed the SAME way for every adapter: the machine roots (refused
// on equals or contains) and the work scope (refused on contains only — a proper
// ancestor). It is a thin wrapper over internal/protectedpath so both adapters
// are provably reading one declaration.
func ProtectedRoots(projectDir string, grants []string) (machine, scope []string) {
	return protectedpath.Roots(""), protectedpath.ScopeRoots(projectDir, grants)
}

// insideDir reports whether child is root itself or below it. The separator
// matters: without it /home2 would look like it is inside /home.
func insideDir(child, root string) bool {
	root = strings.TrimSpace(root)
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	child = filepath.Clean(child)
	if child == root {
		return true
	}
	return strings.HasPrefix(child, root+string(filepath.Separator))
}

// hasRunDirSegment reports whether some component of p is `.orchicon` with at
// least one component after it — the literal-path form of `**/.orchicon/**`.
func hasRunDirSegment(p string) bool {
	segs := strings.Split(filepath.Clean(p), string(filepath.Separator))
	for i, s := range segs {
		if s == RunDirSegment && i < len(segs)-1 {
			return true
		}
	}
	return false
}
