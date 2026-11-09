package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// claudeHome builds a temp HOME that mimics the operator's claude install: ~/.claude (transcript
// home), ~/.claude.json, the launcher symlink under ~/.local/bin.
func claudeHome(t *testing.T) string {
	t.Helper()
	home := canonicalTempHome(t)
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

// TestAdapterInstallsClaudeFindable asserts the CLI is FINDABLE: the launcher AND its install root
// are declared alongside the config/transcript home.
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

// TestAdapterHostMountsReadWriteSplit asserts that NO host bind is writable, and that the writable
// state the CLI needs is delivered as a PRIVATE per-container directory instead.
func TestAdapterHostMountsReadWriteSplit(t *testing.T) {
	home := claudeHome(t)
	args := adapterHostMounts(home, adapter.KindClaude)
	joined := strings.Join(args, " ")
	for _, rw := range []string{
		filepath.Join(home, ".claude") + ":" + filepath.Join(home, ".claude") + ":rw",
		filepath.Join(home, ".claude.json") + ":" + filepath.Join(home, ".claude.json") + ":rw",
	} {
		if strings.Contains(joined, rw) {
			t.Errorf("STILL a read-write HOST bind %q — concurrent containers would share one writable tree "+
				"(args: %v)", rw, args)
		}
	}
	// The writable home must still EXIST for the CLI — as a private, uid-owned mount.
	priv := adapterPrivateHomes(home, adapter.KindClaude)
	if len(priv) == 0 {
		t.Fatalf("claude has no private install: its config/transcript home must be writable per container")
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

// TestAdapterPathPrefixPerKind asserts PATH comes from the demanded kind's mounted bin dir,
// filtered to EXISTING dirs (never an absent host dir).
func TestAdapterPathPrefixPerKind(t *testing.T) {
	home := claudeHome(t)
	dirs := adapterPathPrefixFor(home, "linux", adapter.KindClaude)
	if len(dirs) != 1 || dirs[0] != filepath.Join(home, ".local", "bin") {
		t.Fatalf("adapterPathPrefix(claude) on a Linux host = %v, want the host bin dir — the host's install "+
			"is what the container execs, so its directory must precede /usr/local/bin", dirs)
	}
	// THE MACOS ARM: a Mach-O install cannot run in a Linux container, so the image's CLI is the route and
	// the host dir must NOT shadow it.
	if got := adapterPathPrefixFor(home, "darwin", adapter.KindClaude); len(got) != 0 {
		t.Fatalf("adapterPathPrefix(claude) on darwin = %v, want NONE — a darwin binary cannot exec in a "+
			"Linux container, so the image's CLI must not be shadowed", got)
	}
	// An explicit choice is still honoured, and is the same answer the gate reaches on its own.
	t.Setenv(adapter.BinEnv(adapter.KindClaude), filepath.Join(home, ".local", "bin", "claude"))
	if got := adapterPathPrefixFor(home, "linux", adapter.KindClaude); len(got) != 1 || got[0] != filepath.Join(home, ".local", "bin") {
		t.Fatalf("with %s set, adapterPathPrefix(claude) = %v, want the host dir", adapter.BinEnv(adapter.KindClaude), got)
	}
	// OPENCODE TOO, AND FOR THE OTHER HALF OF THE SAME PROBLEM: its container child is `opencode
	// serve`, exec'd BY NAME by the supervisor.
	t.Setenv(adapter.BinEnv(adapter.KindClaude), "")
	ocHome := home
	if err := os.MkdirAll(filepath.Join(ocHome, ".opencode", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocHome, ".opencode", "bin", "opencode"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := adapterPathPrefixFor(ocHome, "linux", adapter.DefaultAdapterKind); len(got) != 1 || got[0] != filepath.Join(ocHome, ".opencode", "bin") {
		t.Fatalf("adapterPathPrefix(opencode) on a Linux host = %v, want the host bin dir — the container "+
			"runs `opencode serve` by name", got)
	}
	// THE MACOS ARM: a Mach-O opencode cannot exec in a Linux container, so the image's must not be shadowed.
	if got := adapterPathPrefixFor(ocHome, "darwin", adapter.DefaultAdapterKind); len(got) != 0 {
		t.Fatalf("adapterPathPrefix(opencode) on darwin = %v, want NONE — a darwin binary cannot exec in a "+
			"Linux container", got)
	}
	t.Setenv(adapter.BinEnv(adapter.KindClaude), "")
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
