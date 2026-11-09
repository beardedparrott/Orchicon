package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRunWorktreeBaseResolvesRunWorktree verifies the composite worktree MCP base decision: a
// git-backed project resolves to the run's deterministic worktree path.
func TestRunWorktreeBaseResolvesRunWorktree(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	// The "non-repo" half below needs a directory that is genuinely outside any git repository.
	t.Setenv("GIT_CEILING_DIRECTORIES", base)

	// Non-repo project: run proceeds in place at the project dir.
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runWorktreeBase(ctx, plain, "run-1"); got != plain {
		t.Fatalf("non-repo project base = %q, want %q (in-place)", got, plain)
	}

	// Git-backed project: the run's worktree lives at the deterministic path.
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "config", "user.email", "test@example.com"},
		{"-C", repo, "config", "user.name", "test"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}
	want := filepath.Join(repo, worktreeDirName, "run-7")
	if got := runWorktreeBase(ctx, repo, "run-7"); got != want {
		t.Fatalf("git-backed base = %q, want %q (run worktree)", got, want)
	}

	// Empty project dir resolves to empty (no serve config base).
	if got := runWorktreeBase(ctx, "", "run-1"); got != "" {
		t.Fatalf("empty project dir base = %q, want empty", got)
	}
}

// TestPoolEnvKeyServeConfigInvalidatesKey is covered by TestPoolEnvKeyStabilityAndInvalidation;
// this is a focused regression for the stale-serve-config reuse class (a warm container baked
// before.
func TestPoolEnvKeyServeConfigInvalidatesKey(t *testing.T) {
	req := CreateRequest{Image: "img:v1", Mounts: []MountSpec{{Source: "/p", Dest: "/p"}}}
	kNoCfg := poolEnvKey(req, "fp")
	kCfg := poolEnvKey(CreateRequest{Image: "img:v1", Mounts: req.Mounts, ServeConfig: "cfg-a"}, "fp")
	kCfgOther := poolEnvKey(CreateRequest{Image: "img:v1", Mounts: req.Mounts, ServeConfig: "cfg-b"}, "fp")
	if kCfg == kNoCfg {
		t.Fatalf("serve config presence must invalidate the pool key")
	}
	if kCfgOther == kCfg {
		t.Fatalf("different serve config must invalidate the pool key")
	}
}
