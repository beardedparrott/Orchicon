// Package workerrestrict is the ONE copy of the rule set that sandboxes an Orchicon worker: the
// command patterns a worker's bash tool may never run.
package workerrestrict

import (
	"path/filepath"
	"strings"

	"github.com/beardedparrott/orchicon/internal/neverallow"
	"github.com/beardedparrott/orchicon/internal/protectedpath"
)

// ScratchDir is the single directory workers may use outside the project for ephemeral scratch
// (screenshots, logs, downloaded files).
const ScratchDir = "/tmp/orchicon"

// RunDirPattern is the carve-out for Orchicon's own run metadata (`.orchicon/<run>/` under the
// project root, and any `.orchicon/worker.recovery` / run summary files).
const RunDirPattern = "**/.orchicon/**"

// RunDirSegment is the path SEGMENT RunDirPattern keys on.
const RunDirSegment = ".orchicon"

// TaskToolDeny is the built-in SUBAGENT tool a worker may never use, denied in every profile.
const TaskToolDeny = "task"

// SubagentToolNames lists every spelling of the built-in subagent tool the two adapters expose, so
// a deny cannot be evaded by the CLI naming its subagent surface differently.
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

// ProjectBoundaryDenyPatterns is the worker's own project-boundary deny list: the destructive `rm`
// targets (absolute system paths, /, ~, $HOME, --no-preserve-root, current-dir wipes.
func ProjectBoundaryDenyPatterns() []string {
	return []string{
		// rm family — target-scoped.
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

// WorkerDenyPatterns is the FULL worker bash deny list, in evaluation order: the shared never-allow
// binary class first, then the project-boundary rules.
func WorkerDenyPatterns() []string {
	return append(neverallow.DenyRules(), ProjectBoundaryDenyPatterns()...)
}

// MatchCommand reports the first rule in WorkerDenyPatterns that matches cmd, and whether any
// matched.
func MatchCommand(cmd string) (rule string, denied bool) {
	for _, p := range WorkerDenyPatterns() {
		if matchGlob(p, cmd) {
			return p, true
		}
	}
	return "", false
}

// MatchCommandAgainst reports whether any of the supplied rules matches cmd and which one — the
// seam a caller with a DIFFERENT copy of the rules (a test.
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
			// No project root known: a relative target cannot be PROVEN inside scope, and the guard's
			// empty-project arm treats every target as outside.
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

// CarveOutDirs returns the two directory carve-outs, as absolute DIRECTORY paths (not globs): the
// scratch dir, and the run-metadata subtree under projectDir.
func CarveOutDirs(projectDir string) []string {
	out := []string{ScratchDir}
	if strings.TrimSpace(projectDir) != "" {
		out = append(out, filepath.Join(projectDir, RunDirSegment))
	}
	return out
}

// ProtectedRoots returns the two protected-path root lists the destructive-path rule uses, computed
// the SAME way for every adapter.
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
	// PER-PLATFORM COMPARISON.
	if pathEqual(child, root) {
		return true
	}
	return pathHasPrefix(child, root+string(filepath.Separator))
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
