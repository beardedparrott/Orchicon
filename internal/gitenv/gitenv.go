// Package gitenv removes git's REPOSITORY POINTERS from a child environment.
//
// THE HAZARD. Orchicon injects GIT_DIR and GIT_WORK_TREE into a worker's child environment on purpose
// (internal/runtime/agent.go: gitEnvFor) so that git inside a per-run container can resolve a worktree's
// `.git` pointer file, whose path in the host namespace does not exist in the container's. That is
// correct for the worker's own repo. It is catastrophic for anything else, because git's environment
// pointers OVERRIDE `-C <dir>` and override ordinary repository discovery:
//
//	priority: GIT_DIR/GIT_WORK_TREE  >  -C <dir>  >  discovery upward from cwd
//
// So `git -C /tmp/t.TempDirXXXX init` run by a test inside a worker does not create a repository in the
// temp dir. It operates on the MAIN repo the pointers name — which is the operator's own checkout. This
// was not theoretical, it was reproduced in three shapes:
//
//	git init            → wrote `worktree = <worker worktree>` and `bare = false` into the MAIN
//	                      repo's .git/config, leaving the intended temp dir EMPTY. That stray
//	                      core.worktree then redirected every later `git` command in the operator's
//	                      checkout to the worker's worktree — which is why `git pull` reported
//	                      conflicts in files the operator had never touched.
//	git config user.*   → wrote `worktree-test@orchicon.dev` into the MAIN repo's config.
//	git status          → writes nothing, and still LIES: GIT_WORK_TREE alone changes what it
//	                      reports, so a clean checkout shows another tree's dirty files.
//
// THE RULE THIS PACKAGE EXISTS TO ENFORCE: anything that is not the worker acting on its OWN repo must
// drop these variables. Orchicon's own repositories (the project dir, the run worktree, a project being
// set up) are reached by an explicit path, never by inheriting an ambient pointer.
//
// WHY NOT FIX THIS AT THE SOURCE. Dropping the variables from the worker's environment entirely would
// break container git, which needs them. The pointers are legitimate for the child; they are simply not
// legitimate for OUR subprocesses. So the decision belongs at each git invocation, and this package makes
// that decision one line instead of a list of names each caller must remember.
package gitenv

import "strings"

// repoPointerKeys are the variables that override repository discovery. Naming them explicitly (rather
// than a GIT_ prefix sweep) keeps GIT_AUTHOR_*, GIT_COMMITTER_*, GIT_TERMINAL_PROMPT and GIT_SSH_COMMAND —
// all of which a caller may legitimately need to pass through.
var repoPointerKeys = []string{
	// The redirection itself: these alone cause every symptom above.
	"GIT_DIR",
	"GIT_WORK_TREE",
	// A displaced index or object store operates on the wrong repository's data even when GIT_DIR is
	// absent (an alternates/object-directory override).
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_PREFIX",
	// Discovery constraints left over from another invocation's context.
	"GIT_CEILING_DIRECTORIES",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_SHALLOW_FILE",
	"GIT_GRAFT_FILE",
}

// RepoPointerKeys returns the variable names Clean removes, for tests and for callers that need to
// assert an environment is clean.
func RepoPointerKeys() []string {
	out := make([]string, len(repoPointerKeys))
	copy(out, repoPointerKeys)
	return out
}

// Clean returns env with every repository pointer removed.
//
// It preserves order and every other entry, so a caller can append to the result and still hand it
// straight to exec.Command.
func Clean(env []string) []string {
	if len(env) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if isRepoPointer(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// isRepoPointer reports whether key is one this package removes. The comparison is case-insensitive
// because Windows environments are: `git_dir` and `GIT_DIR` are the same variable there.
func isRepoPointer(key string) bool {
	for _, k := range repoPointerKeys {
		if strings.EqualFold(key, k) {
			return true
		}
	}
	return false
}

// CleanEnviron is Clean(os.Environ()) — the call site for every git subprocess Orchicon spawns against a
// repository it reaches by path.
func CleanEnviron() []string { return Clean(environ()) }

// environ is a seam so tests can drive CleanEnviron without mutating the real process environment.
var environ = defaultEnviron
