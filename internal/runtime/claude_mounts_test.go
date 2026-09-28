package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// claudeHome builds a temp HOME that mimics the operator's claude install:
// ~/.claude (transcript home), ~/.claude.json, the launcher symlink under
// ~/.local/bin, and its install root under ~/.local/share/claude.
func claudeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".claude", "projects"))
	mustWrite(t, filepath.Join(home, ".claude.json"), "{}")
	root := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.261")
	mustMkdir(t, root)
	mustWrite(t, filepath.Join(root, "cli.js"), "// cli")
	mustMkdir(t, filepath.Join(home, ".local", "bin"))
	if err := os.Symlink(filepath.Join(root, "cli.js"), filepath.Join(home, ".local", "bin", "claude")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return home
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// TestAdapterInstallsClaudeFindable asserts the CLI is FINDABLE: the launcher
// AND its install root are declared alongside the config/transcript home, and
// the symlink target's ROOT is among the mounted dirs so the launcher cannot
// dangle in-container.
func TestAdapterInstallsClaudeFindable(t *testing.T) {
	home := claudeHome(t)
	paths, declared := adapterInstallPaths(home, adapter.KindClaude)
	if !declared {
		t.Fatal("claude must be classified in the install table")
	}
	joined := strings.Join(paths, "\n")
	for _, want := range []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".local", "share", "claude"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude.json"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude install set missing %s (have %v)", want, paths)
		}
	}

	// The symlink-root invariant: some mounted DIR is a prefix of the
	// resolved symlink target.
	target, err := filepath.EvalSymlinks(filepath.Join(home, ".local", "bin", "claude"))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	installs, _ := adapterInstalls(home, adapter.KindClaude)
	var covered bool
	for _, in := range installs {
		if !in.dir {
			continue
		}
		dir := in.mount
		if dir == "" {
			dir = in.probe
		}
		if strings.HasPrefix(target, dir+string(filepath.Separator)) {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("symlink target %s is not covered by any mounted dir — the launcher would dangle in-container", target)
	}
}

// TestAdapterHostMountsReadWriteSplit asserts the exact -v strings: the
// config/transcript home is RW (claude writes its JSONL there), every other
// install stays RO.
func TestAdapterHostMountsReadWriteSplit(t *testing.T) {
	home := claudeHome(t)
	args := adapterHostMounts(home, adapter.KindClaude)
	joined := strings.Join(args, " ")
	for _, rw := range []string{
		filepath.Join(home, ".claude") + ":" + filepath.Join(home, ".claude") + ":rw",
		filepath.Join(home, ".claude.json") + ":" + filepath.Join(home, ".claude.json") + ":rw",
	} {
		if !strings.Contains(joined, rw) {
			t.Errorf("missing rw mount %q (args: %v)", rw, args)
		}
	}
	for _, ro := range []string{
		filepath.Join(home, ".local", "share", "claude") + ":" + filepath.Join(home, ".local", "share", "claude") + ":ro",
		filepath.Join(home, ".local", "bin", "claude") + ":" + filepath.Join(home, ".local", "bin", "claude") + ":ro",
	} {
		if !strings.Contains(joined, ro) {
			t.Errorf("missing ro mount %q (args: %v)", ro, args)
		}
	}
}

// TestAdapterBakeNeedlesClaudeBenign pins that probing the claude install
// yields only benign needles — never a version string such as 2.1.261.
func TestAdapterBakeNeedlesClaudeBenign(t *testing.T) {
	home := claudeHome(t)
	paths, _ := adapterInstallPaths(home, adapter.KindClaude)
	needles := adapterBakeNeedles(adapter.KindClaude, paths)
	allowed := map[string]bool{"claude": true, ".claude": true, "claude.json": true, ".claude.json": true}
	for _, n := range needles {
		if !allowed[n] {
			t.Errorf("unexpected bake needle %q (all: %v)", n, needles)
		}
		if strings.ContainsAny(n, "0123456789") {
			t.Errorf("version-shaped bake needle %q", n)
		}
	}
	if !containsStr(needles, "claude") || !containsStr(needles, "claude.json") {
		t.Errorf("needles = %v, want claude + claude.json", needles)
	}
}

// TestAdapterPathPrefixPerKind asserts PATH comes from the demanded kind's
// mounted bin dir, filtered to EXISTING dirs (never an absent host dir).
func TestAdapterPathPrefixPerKind(t *testing.T) {
	home := claudeHome(t)
	dirs := adapterPathPrefix(home, adapter.KindClaude)
	if len(dirs) != 1 || dirs[0] != filepath.Join(home, ".local", "bin") {
		t.Fatalf("adapterPathPrefix(claude) = %v", dirs)
	}
	if got := adapterPathPrefix(home, "orchicon"); len(got) != 0 {
		t.Fatalf("native adapter must contribute no bin dir, got %v", got)
	}
	if got := adapterPathPrefix(home, ""); len(got) != 0 {
		t.Fatalf("empty kind list must contribute nothing, got %v", got)
	}
}

// TestAdapterFingerprintRootsClaude confirms a claude CLI install feeds the
// pool fingerprint while the config/transcript home does NOT.
func TestAdapterFingerprintRootsClaude(t *testing.T) {
	home := claudeHome(t)
	roots := adapterFingerprintRoots(home)
	joined := strings.Join(roots, "\n")
	if !strings.Contains(joined, filepath.Join(home, ".local", "share", "claude")) {
		t.Fatalf("claude install root not fingerprinted: %v", roots)
	}
	if strings.Contains(joined, filepath.Join(home, ".claude")) {
		t.Fatalf("config/transcript home must NOT be fingerprinted (would churn the pool): %v", roots)
	}
}
