package askorchicon

// tool_git_test.go — WHERE A RUN'S WORK LANDS IS NOT THE REPOSITORY'S DEFAULT BRANCH.
//
// The operator, reading a dispatch suggestion this tool produced for the Orchicon project:
//
//	"If something lists main as the merge branch it's wrong and needs to be updated. Where did you read that
//	 at? Everything should be worked off of branches from develop. Main branch is only for actual releases
//	 from develop."
//
// They were right, and the mechanism was a conflation rather than a stray string: the tool took GitHub's
// default branch (origin/HEAD -> main) and reported it as the SUGGESTED BASE **and** SUGGESTED MERGE branch,
// so a run was offered `main` for both. Meanwhile the git rules this platform injects into every git-backed
// worker's prompt hardcode develop — "Work on a branch created off `develop` (the integration branch where
// all work lands). NEVER commit to, push to, or open a PR into `main` or `develop` directly"
// (internal/db/prompt.go) — and the agent brief notes those injected rules WIN. The tool was therefore
// handing the agent an instruction its own worker would refuse.
//
// These tests pin the DECISION (integrationBranchFor), which is pure, so they need no git binary, no work
// tree and no database — the execs around it are fixed-argv and were never the part that was wrong.

import "testing"

func TestIntegrationBranchPrefersDevelopOverTheRepositoryDefault(t *testing.T) {
	// THE REGRESSION, exactly as the real repository looks: origin/HEAD resolves to main, and develop exists.
	// The suggestion must be develop, because that is where work lands.
	got := integrationBranchFor("main",
		[]string{"develop", "main"},
		[]string{"origin/develop", "origin/main"})
	if got != "develop" {
		t.Fatalf("integrationBranchFor = %q, want \"develop\" — reporting the repository's default "+
			"(main) as the branch work merges into is the bug the operator reported", got)
	}
}

func TestIntegrationBranchSeesDevelopOnTheRemoteOnly(t *testing.T) {
	// A run may cut from a branch that exists only on origin. A local-only check is how a repository whose
	// develop has not been fetched yet silently loses its integration branch — and falls back to main, which
	// is the same wrong suggestion by a different route.
	got := integrationBranchFor("main",
		[]string{"main"},
		[]string{"origin/develop", "origin/main"})
	if got != "develop" {
		t.Fatalf("integrationBranchFor = %q, want \"develop\" from the remote refs alone", got)
	}
}

func TestIntegrationBranchFallsBackToTheDefaultWhenThereIsNoDevelop(t *testing.T) {
	// A repository that genuinely has no develop keeps its own default as the suggestion: the tool must not
	// invent an integration branch that does not exist.
	for _, tc := range []struct {
		name     string
		locals   []string
		remotes  []string
		fallback string
	}{
		{"main only", []string{"main"}, []string{"origin/main"}, "main"},
		{"master only", []string{"master"}, []string{"origin/master"}, "master"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := integrationBranchFor(tc.fallback, tc.locals, tc.remotes); got != tc.fallback {
				t.Fatalf("integrationBranchFor = %q, want the fallback %q", got, tc.fallback)
			}
		})
	}
}

func TestIntegrationBranchIsEmptyWhenNothingIsKnowable(t *testing.T) {
	// Nothing to derive: defaultBranch is "" (no origin/HEAD, no conventional candidate) and no develop. An
	// empty answer is the honest one — a caller receiving it must ASK rather than assume, which is what the
	// note tells it to do.
	if got := integrationBranchFor("", nil, nil); got != "" {
		t.Fatalf("integrationBranchFor = %q, want \"\" — a branch must never be invented", got)
	}
}
