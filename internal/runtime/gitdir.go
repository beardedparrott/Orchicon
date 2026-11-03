package runtime

// gitdir.go — MAKE A HOST-PROVISIONED WORKTREE USABLE INSIDE THE CONTAINER.
//
// THE FAILURE, reported from Windows container runs and first misdiagnosed as "git.exe is absent from the
// runtime PATH". Git is NOT missing: the runtime image installs it (deploy/runtime/Dockerfile), no host git is
// mounted, and the supervisor does not allowlist it. What is broken is the POINTER:
//
//  1. The PLANE (on Windows) provisions the run worktree with `git worktree add <host path>`.
//  2. Git writes a `.git` FILE whose content is `gitdir: C:/projects/weatherapp/.git/worktrees/<run>` — an
//     absolute HOST path, in the host's own dialect.
//  3. The daemon mounts the project into the container at the MAPPED guest path
//     (`C:\\projects\\weatherapp` -> `/mnt/c/projects/weatherapp`, PathModeWSL).
//  4. The container's Linux git opens `.git`, reads `C:/projects/...`, cannot resolve it, and reports
//     "fatal: not a git repository".
//
// So git works and git is present; it cannot follow a pointer written in a namespace that does not exist in
// here. Setting GIT_DIR/GIT_WORK_TREE by hand bypasses the pointer file entirely, which is exactly why that
// workaround worked — and why it looked like a missing binary.
//
// THE FIX IS A REWRITE, applied once at container start, of every `gitdir:` pointer under the run's mounted
// project so it names the GUEST path. It is the same translation mapHostPathFields already performs for
// request fields; this is that translation for content the host wrote into a file.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitdirPrefix is the literal prefix of a worktree pointer file.
const gitdirPrefix = "gitdir: "

// pathMapping is one host->guest prefix pair the supervisor should apply.
type pathMapping struct {
	Host  string
	Guest string
}

// parsePathMappings reads prefix pairs from the environment value the daemon emits, formatted as
// "<host>=<guest>" separated by the platform list separator... which on a Windows plane is ';' and would
// collide with drive letters. So the separator is a literal '|' and the pair separator is ';' only when it
// cannot appear inside a path — it cannot, because a guest path is POSIX and a host path on a WSL plane is
// either a drive path or a POSIX one.
//
// An entry that does not parse is SKIPPED, not fatal: a malformed mapping must not stop a container from
// starting, and the consequence of skipping is the status quo (git unusable in that worktree) rather than a
// dead run.
func parsePathMappings(raw string) []pathMapping {
	var out []pathMapping
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, guest, ok := strings.Cut(entry, "|")
		host, guest = strings.TrimSpace(host), strings.TrimSpace(guest)
		if !ok || host == "" || guest == "" || host == guest {
			continue
		}
		out = append(out, pathMapping{Host: host, Guest: guest})
	}
	return out
}

// rewriteGuestGitdirs repoints every worktree `.git` pointer under root that names a HOST path.
//
// It walks the tree and stops descending at each `.git` it rewrites, so a nested project cannot cost a full
// traversal. Every failure is reported per file and does not abort the walk: one unreadable directory must not
// leave the other worktrees unusable.
func rewriteGuestGitdirs(root string, mappings []pathMapping, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) int {
	if root == "" || len(mappings) == 0 {
		return 0
	}
	rewritten := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip it, keep walking
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() != ".git" {
			return nil
		}
		changed, werr := rewriteGitdirFile(path, mappings)
		if werr != nil {
			log.Warn("gitdir rewrite: could not rewrite a worktree pointer", "file", path, "error", werr)
			return nil
		}
		if changed {
			rewritten++
			log.Info("gitdir rewrite: worktree pointer now names the container's path", "file", path)
			// Do not descend below a worktree: its gitdir lives under the main repo, which has its own walk
			// when it is itself a mount root, and its contents are not ours to traverse.
			return filepath.SkipDir
		}
		return nil
	})
	return rewritten
}

// rewriteGitdirFile repoints ONE `.git` pointer file when it names a host path.
//
// IT IS IDEMPOTENT. A file already naming the guest path matches no host prefix and is left byte-identical, so
// running this on every container start cannot compound. It rewrites only when the CURRENT content starts with
// a mapped host prefix — a `gitdir:` that resolves to something else entirely (a repository outside the
// mounted tree) is left alone rather than guessed at.
func rewriteGitdirFile(path string, mappings []pathMapping) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, gitdirPrefix) {
		return false, nil // not a worktree pointer
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, gitdirPrefix))
	if target == "" {
		return false, nil
	}
	// COMPARE SEPARATOR-NORMALISED. The host wrote whatever its own git wrote — `C:/x` or `C:\x` — and both
	// mean the same place; comparing literally would miss one of them depending on which git produced the file.
	normTarget := strings.ReplaceAll(target, "\\", "/")
	for _, m := range mappings {
		normHost := strings.ReplaceAll(strings.TrimRight(m.Host, "\\/"), "\\", "/")
		if normHost == "" {
			continue
		}
		if !strings.HasPrefix(normTarget, normHost+"/") && normTarget != normHost {
			continue
		}
		rest := strings.TrimPrefix(normTarget, normHost)
		guest := strings.TrimRight(m.Guest, "/") + rest
		changed, werr := writeGitdirFile(path, guest)
		if werr != nil || !changed {
			return changed, werr
		}
		// THE FORWARD POINTER ONLY — never the back-pointer.
		//
		// `<repo>/.git/worktrees/<n>/gitdir` is the REPOSITORY's metadata: the HOST's registry of its own
		// worktrees. Rewriting it to a container path corrupts the host's view of itself. Measured:
		//   fatal: '<branch>' is already used by worktree at
		//          '/mnt/c/projects/weatherapp/.orchicon-worktrees/<run>'
		// on a path that does not exist on Windows — so the registration could never be satisfied, repaired,
		// or pruned by the ordinary route, and EVERY retry of that run failed the same way.
		//
		// It was written "so `git worktree list` works inside the container". That is cosmetic; a host that
		// cannot provision is not.
		return true, nil
	}
	return false, nil
}

// writeGitdirFile replaces the pointer with the guest form, preserving the file's mode.
func writeGitdirFile(path, guestTarget string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	content := gitdirPrefix + guestTarget + "\n"
	if err := os.WriteFile(path, []byte(content), st.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}
