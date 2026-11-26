package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

// envGitConfigRW, when set to 1/true, lets containers WRITE the project's own .git/config (the pre-guard
// behaviour). Off by default; see repoConfigGuardMount.
const envGitConfigRW = "ORCHICON_RUNTIME_GIT_CONFIG_RW"

// repoConfigGuardMount returns a READ-ONLY bind of the project's own .git/config, or false when there is nothing
// to protect (no checkout, no config file) or the operator opted out.
//
// WHY. The project directory is mounted read-write at the same path in every runtime container, and a linked
// worktree's git state lives inside the project's .git — including the ONE config file shared by the operator's
// checkout and every worktree. A process in a container that runs `git init` or `git config` under the
// GIT_DIR/GIT_WORK_TREE the supervisor injects (internal/runtime/agent.go gitEnvFor) writes into that shared
// file. It has recorded `core.worktree = <a run's worktree>` there three times, and each time the operator's
// checkout silently turned into a view of a worker's files (internal/gitenv, internal/repoguard).
//
// With the file read-only the write fails, loudly, at the offending command: `fatal: could not set
// 'core.repositoryformatversion'`, `could not write config file ...: Device or resource busy`. Measured against
// a platform-shaped repository in a container (podman, keep-id): commit, push, `push -u`, fetch, `checkout -b`
// and `worktree list` all still work — `push -u` still pushes and exits 0, and prints a config-write error
// because it cannot record upstream tracking — while the poisoning `git init` fails and the file's hash is
// unchanged. `git remote add` and `git config` writes are refused, which is the point.
//
// A FILE bind pins the file's inode, and the host's git replaces config by rename, so a long-lived container
// sees the config as it was when the container was created. Config changes are rare and the pool already
// recreates containers on other inputs, so that is accepted rather than churning containers per write.
func repoConfigGuardMount(projectDir string) (MountSpec, bool) {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(envGitConfigRW))); v == "1" || v == "true" {
		return MountSpec{}, false
	}
	if strings.TrimSpace(projectDir) == "" {
		return MountSpec{}, false
	}
	gitDir := filepath.Join(projectDir, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return MountSpec{}, false // not an ordinary checkout (a linked worktree's .git is a file)
	}
	cfg := filepath.Join(gitDir, "config")
	if fi, err := os.Stat(cfg); err != nil || !fi.Mode().IsRegular() {
		return MountSpec{}, false
	}
	return MountSpec{Source: cfg, Dest: cfg, RO: true}, true
}
