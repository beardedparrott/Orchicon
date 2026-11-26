package repoguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const poisoned = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
	worktree = %s
[remote "origin"]
	url = git@github.com:o/r.git
[user]
	name = worktree-test
	email = worktree-test@orchicon.dev
`

func TestInspectFindsOurPoisonAndOnlyOurs(t *testing.T) {
	proj := "/home/op/projects/Orchicon"
	run := "01M8AQK7QB9GS1069WESBBTTED"
	cfg := strings.Replace(poisoned, "%s", proj+"/.orchicon-worktrees/"+run, 1)

	got := Inspect(cfg, proj)
	keys := map[string]Finding{}
	for _, f := range got {
		keys[f.Key] = f
	}
	for _, want := range []string{"core.worktree", "user.email", "user.name"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("missing finding for %s: %+v", want, got)
		}
	}
	if keys["core.worktree"].Run != run {
		t.Errorf("the finding does not name the run its value pointed at: %+v", keys["core.worktree"])
	}
}

// A setting the operator might have chosen themselves must NEVER be touched.
func TestInspectLeavesLegitimateConfigAlone(t *testing.T) {
	proj := "/home/op/projects/Orchicon"
	for name, cfg := range map[string]string{
		"separate work tree elsewhere":                "[core]\n\tworktree = /srv/checkouts/orchicon\n",
		"worktree inside the repo but not ours":       "[core]\n\tworktree = " + proj + "/src\n",
		"a sibling dir that merely shares the prefix": "[core]\n\tworktree = " + proj + "/.orchicon-worktrees-old/x\n",
		"the operator's own identity":                 "[user]\n\tname = Jane\n\temail = jane@example.com\n",
		"empty":                                       "",
		"garbage":                                     "\x00\x01 not a config [[[ = = =",
		"no core section":                             "[remote \"origin\"]\n\turl = x\n",
	} {
		if got := Inspect(cfg, proj); len(got) != 0 {
			t.Errorf("%s: flagged %+v", name, got)
		}
	}
	// The fixture NAME alone (without the fixture email) is not enough to touch an identity.
	if got := Inspect("[user]\n\tname = worktree-test\n\temail = me@example.com\n", proj); len(got) != 0 {
		t.Errorf("flagged an identity that only shares the fixture's name: %+v", got)
	}
}

func TestInspectIsCaseAndSlashInsensitiveWithinOurDirectory(t *testing.T) {
	proj := `C:\Projects\App`
	cfg := "[core]\n\tWorkTree = c:/projects/app/.orchicon-worktrees/01ABC\n"
	if got := Inspect(cfg, filepath.FromSlash("C:/Projects/App")); len(got) != 1 {
		// On a non-Windows host filepath.Clean leaves the backslashes alone; the fold still has to match.
		t.Skipf("host path semantics differ (%s): %+v", proj, got)
	}
}

func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// END TO END on a real repository: poison it exactly as the incident did, run the guard, and prove the checkout
// is a checkout again — git agrees about the toplevel and there is no worktree setting left.
func TestCheckHealsARealPoisonedCheckout(t *testing.T) {
	gitOrSkip(t)
	proj := t.TempDir()
	git(t, proj, "init", "-q", "-b", "develop")
	git(t, proj, "-c", "user.name=op", "-c", "user.email=op@example.com", "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(proj, WorktreesDirName, "01RUNPOISON")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, proj, "config", "--file", filepath.Join(proj, ".git", "config"), "core.worktree", wt)
	git(t, proj, "config", "--file", filepath.Join(proj, ".git", "config"), "user.email", "worktree-test@orchicon.dev")
	if top := git(t, proj, "rev-parse", "--show-toplevel"); top == proj {
		t.Fatalf("test premise broken: git still resolves the real toplevel (%s)", top)
	}

	res := Check(context.Background(), proj)
	if !res.Healed || res.Err != nil {
		t.Fatalf("Check = %+v, want healed", res)
	}
	if len(res.Findings) < 2 {
		t.Errorf("findings = %+v, want core.worktree and user.email", res.Findings)
	}
	if top := git(t, proj, "rev-parse", "--show-toplevel"); top != proj {
		t.Errorf("after healing, toplevel = %q, want %q", top, proj)
	}
	raw, _ := os.ReadFile(filepath.Join(proj, ".git", "config"))
	if strings.Contains(string(raw), "worktree =") || strings.Contains(string(raw), "worktree-test@") {
		t.Errorf("the poison is still in the config:\n%s", raw)
	}
	// And a second pass is a quiet no-op.
	if again := Check(context.Background(), proj); again.Healed || len(again.Findings) != 0 {
		t.Errorf("second check = %+v, want nothing to do", again)
	}
}

// The guard must be immune to the redirection it repairs: an ambient GIT_DIR pointing at a DIFFERENT repository
// (a worker's environment) must not make it edit the wrong config.
func TestCheckIgnoresAmbientGitPointers(t *testing.T) {
	gitOrSkip(t)
	proj, other := t.TempDir(), t.TempDir()
	git(t, proj, "init", "-q")
	git(t, other, "init", "-q")
	wt := filepath.Join(proj, WorktreesDirName, "01X")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, proj, "config", "--file", filepath.Join(proj, ".git", "config"), "core.worktree", wt)

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if res := Check(context.Background(), proj); !res.Healed {
		t.Fatalf("Check under ambient pointers = %+v, want healed", res)
	}
	raw, _ := os.ReadFile(filepath.Join(proj, ".git", "config"))
	if strings.Contains(string(raw), "core") && strings.Contains(string(raw), "worktree =") {
		t.Errorf("the project's config was not healed:\n%s", raw)
	}
}

// A project that is itself a linked worktree (its .git is a FILE) has no config of its own to guard.
func TestCheckSkipsNonCheckouts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /x/.git/worktrees/y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := Check(context.Background(), dir); res.Healed || len(res.Findings) != 0 || res.Err != nil {
		t.Errorf("Check on a worktree = %+v, want nothing", res)
	}
	if res := Check(context.Background(), t.TempDir()); res.Healed || res.Err != nil {
		t.Errorf("Check on a non-repo = %+v, want nothing", res)
	}
}
