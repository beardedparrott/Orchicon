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

	// THE DEFAULT BRANCH AND THE INTEGRATION BRANCH ARE TWO DIFFERENT THINGS, and reporting one as both is
	// what offered `main` as the branch to cut from and merge into.
	//
	//   - defaultBranch is the REPOSITORY'S OWN DEFAULT: what origin/HEAD points at, i.e. what you get on
	//     clone. On this repo that is `main`, and reporting it is simply factual.
	//   - integrationBranch is WHERE WORK LANDS. On this repo that is `develop`; `main` receives merges FROM
	//     develop at release time and nothing else. The operator's rule, verbatim: "Everything should be
	//     worked off of branches from develop. Main branch is only for actual releases from develop."
	//
	// THE OLD CODE REPORTED ONE VALUE AS BOTH, so a run was offered `main` as its base AND its merge target —
	// contradicting the git rules this platform INJECTS into every git-backed worker's prompt, which hardcode
	// develop: "Work on a branch created off `develop` (the integration branch where all work lands). NEVER
	// commit to, push to, or open a PR into `main` or `develop` directly" (internal/db/prompt.go). The agent
	// brief already notes those injected rules win, so the tool was handing the agent an instruction its own
	// worker would refuse.
	//
	// Note the old fallback ALREADY preferred develop (`develop`, `main`, `master`): the intent was right, and
	// only the origin/HEAD path — which is the path that actually fires on a real clone — bypassed it.
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

	integrationBranch := integrationBranchFor(defaultBranch, locals, remotes)

	out["local_branches"] = locals
	out["remote_branches"] = remotes
	out["default_branch"] = defaultBranch
	// Reported explicitly so the distinction is visible to the caller rather than inferable from a name.
	out["integration_branch"] = integrationBranch
	if integrationBranch != "" {
		out["suggested_base_branch"] = integrationBranch
		out["suggested_merge_branch"] = integrationBranch
	}
	out["note"] = "Offer these as the choices instead of asking for blank branch names: confirm WHICH BRANCH TO " +
		"CLONE OFF (the run's base) and WHICH BRANCH TO MERGE INTO (the PR's target) — both are the " +
		"INTEGRATION branch (`suggested_base_branch` / `suggested_merge_branch`), which is normally `develop`. " +
		"`default_branch` is NOT a merge target: it is only the repository's own default (usually `main`), and " +
		"main receives merges from develop at release time and nothing else, so never offer it as where a run's " +
		"work should land. They are usually the same branch — never assume either — and carry the confirmed " +
		"pair into the worker's brief, because the git rules injected into a worker's prompt name the " +
		"integration branch generically and will otherwise win."
	return json.Marshal(out)
}

// integrationBranchFor picks the branch a run's work should LAND on: the conventional integration branch when
// it exists (locally or on origin), else the repository's own default, else "" (nothing is invented — a caller
// receiving "" must ask rather than assume).
//
// IT IS PURE, AND SEPARATED FOR THAT REASON: this is the decision the whole suggestion rests on and the one
// that was wrong, so it is testable without a git binary, a work tree or a database. The execs around it are
// deliberately narrow and fixed-argv; the JUDGEMENT is what needed a test.
//
// ORIGIN IS CHECKED AS WELL AS DISK. A run may cut from a branch that exists only on the remote, and the
// local-only check is exactly how a repository whose develop has not been fetched yet loses its integration
// branch. `remotes` carries `origin/<name>` as git reports it.
func integrationBranchFor(defaultBranch string, locals, remotes []string) string {
	if branchListContains(locals, "develop") || branchListContains(remotes, "origin/develop") {
		return "develop"
	}
	return defaultBranch
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
