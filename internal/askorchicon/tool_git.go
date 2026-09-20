package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// toolListProjectBranches reports a project's git identity — whether it is git-backed at all, its current branch,
// its default branch, and the local + origin branch names — so a caller can OFFER real branches instead of asking
// for one blind.
//
// WHY IT EXISTS. A dispatch must confirm, every time, which branch to cut the run from and which branch the PR
// merges into. As an OPEN question that burdens the user with recalling exact branch names, and the cost of a
// wrong answer is a run that pushes to the wrong place. Asking WITH the real list makes the confirmation concrete
// and cheap.
//
// THE SAFETY SHAPE IS THE POINT, and it is deliberately narrow: fixed argv, exec'd WITHOUT a shell (so no branch
// name and no argument can ever become a command), the directory pinned with `git -C`, a bounded timeout, and
// `--format` output that needs no parsing of human-readable decoration. Quick Work has `bash` DENIED precisely so
// the mode cannot run arbitrary commands; a tool that handed it one back through this surface would be that
// denial undone. NOTHING here is user-controlled except the project id, and that only selects a directory.
func toolListProjectBranches(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	project, err := db.GetProject(ctx, ttx.Tx, tenantID, params.ProjectID)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(project.ProjectDir)
	if dir == "" {
		return nil, fmt.Errorf("project %q has no project_dir set, so its git state cannot be read", project.Name)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("project_dir %q is not a readable directory on this host, so its git state cannot be "+
			"read — say so rather than guessing at branch names", dir)
	}

	// Fixed argv, no shell. `git -C` pins the directory; the timeout bounds a hung index or lock; and
	// GIT_TERMINAL_PROMPT=0 guarantees a credential prompt can never block the turn waiting on input that will
	// never come (a real failure mode for an unattended process).
	run := func(argv ...string) string {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", append([]string{"-C", dir}, argv...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	out := map[string]any{
		"project_id":   project.ID,
		"project_dir":  dir,
		"git_strategy": project.GitStrategy,
	}
	if project.RepoSlug != nil && strings.TrimSpace(*project.RepoSlug) != "" {
		out["repo_slug"] = *project.RepoSlug
	}

	if run("rev-parse", "--is-inside-work-tree") != "true" {
		out["git_backed"] = false
		out["note"] = "This project's project_dir is not a git work tree, so there is no branch to cut from and no " +
			"PR to open: a run here works IN PLACE. Report the git_strategy above and say plainly that no branch " +
			"choice applies — do not ask for branches that do not exist."
		return json.Marshal(out)
	}
	out["git_backed"] = true

	out["current_branch"] = run("rev-parse", "--abbrev-ref", "HEAD")

	splitLines := func(raw string) []string {
		var lines []string
		for _, ln := range strings.Split(raw, "\n") {
			if v := strings.TrimSpace(ln); v != "" {
				lines = append(lines, v)
			}
		}
		return lines
	}
	locals := splitLines(run("branch", "--format=%(refname:short)"))

	// Remote refs come back as `origin/<name>`; drop the symbolic `origin/HEAD` pseudo-branch, which is not a
	// branch anyone can target.
	var remotes []string
	for _, r := range splitLines(run("branch", "-r", "--format=%(refname:short)")) {
		if strings.HasSuffix(r, "/HEAD") {
			continue
		}
		remotes = append(remotes, r)
	}

	// The default branch, most authoritative first: what origin/HEAD points at, else a conventional integration
	// branch when it exists locally. Never invented — an empty value is reported as empty.
	defaultBranch := ""
	if s := run("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); s != "" {
		defaultBranch = strings.TrimPrefix(s, "origin/")
	}
	if defaultBranch == "" {
		for _, candidate := range []string{"develop", "main", "master"} {
			if branchListContains(locals, candidate) {
				defaultBranch = candidate
				break
			}
		}
	}

	out["local_branches"] = locals
	out["remote_branches"] = remotes
	out["default_branch"] = defaultBranch
	if defaultBranch != "" {
		out["suggested_base_branch"] = defaultBranch
		out["suggested_merge_branch"] = defaultBranch
	}
	out["note"] = "Offer these as the choices instead of asking for blank branch names: confirm WHICH BRANCH TO " +
		"CLONE OFF (the run's base) and WHICH BRANCH TO MERGE INTO (the PR's target). They are usually the same " +
		"branch and are not always the default — never assume either, and carry the confirmed pair into the " +
		"worker's brief, because the git rules injected into a worker's prompt name the integration branch " +
		"generically and will otherwise win."
	return json.Marshal(out)
}

// branchListContains is a tiny membership test for the branch names above (no import for a two-item scan).
func branchListContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
