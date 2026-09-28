package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// bootProfileTestHome builds a host home that has BOTH an opencode install
// and the git identity/credential surface, and returns a daemon wired to it.
// ghTokenFn is stubbed so the assertion never shells out to `gh`.
func bootProfileTestHome(t *testing.T) (string, *Daemon) {
	t.Helper()
	home := t.TempDir()
	mkdir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{home}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkfile := func(parts ...string) {
		t.Helper()
		p := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(".config", "opencode")
	mkdir(".local", "share", "opencode")
	mkfile(".opencode", "bin", "opencode")
	mkfile(".gitconfig")
	mkfile(".git-credentials")
	mkdir(".config", "gh")
	mkdir(".local", "share", "gh")
	return home, &Daemon{HostHome: home, ghTokenFn: func() string { return "" }}
}

// argValue reports whether args contains the exact pair (flag, value).
func argValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// envValue reports whether args carries `-e KEY=VALUE`.
func envValue(args []string, key, value string) bool {
	return argValue(args, "-e", key+"="+value)
}

// opencodeAdapterMounts are the three conditional adapter mounts under home.
func opencodeAdapterMounts(home string) []string {
	out := make([]string, 0, 3)
	for _, p := range []string{
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".local", "share", "opencode"),
		filepath.Join(home, ".opencode"),
	} {
		out = append(out, p+":"+p+":ro")
	}
	return out
}

// TestStandardHostMountArgsConditionalOpencode is AC 1/AC 3/AC 5: a
// native-only run's container carries NO opencode config/auth/CLI mount and
// no adapter PATH prefix, while an opencode-demanding run (explicit, mixed,
// or the legacy absent profile) carries all three plus the prefix. The git
// identity/credential surface is NOT adapter-gated and stays in every case.
func TestStandardHostMountArgsConditionalOpencode(t *testing.T) {
	home, d := bootProfileTestHome(t)

	gitMounts := []string{
		filepath.Join(home, ".gitconfig") + ":" + filepath.Join(home, ".gitconfig") + ":ro",
		filepath.Join(home, ".git-credentials") + ":" + filepath.Join(home, ".git-credentials") + ":ro",
		filepath.Join(home, ".config", "gh") + ":" + filepath.Join(home, ".config", "gh") + ":ro",
		filepath.Join(home, ".local", "share", "gh") + ":" + filepath.Join(home, ".local", "share", "gh") + ":ro",
	}
	adapterPATH := filepath.Join(home, ".opencode", "bin") + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	cases := []struct {
		name        string
		kinds       []string
		wantAdapter bool
	}{
		{"native-only", []string{"orchicon"}, false},      // AC 1
		{"opencode-only", []string{"opencode"}, true},     // AC 5
		{"mixed", []string{"opencode", "orchicon"}, true}, // AC 2
		{"absent/legacy profile", nil, true},              // rollout: legacy plane
		{"explicitly empty profile", []string{}, false},   // no demand
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := d.standardHostMountArgs(CreateRequest{AdapterKinds: c.kinds, GitStrategy: "local"})

			// Git surface is never adapter-gated.
			for _, g := range gitMounts {
				if !argValue(args, "-v", g) {
					t.Errorf("missing git mount %s (git is not adapter-dependent): %v", g, args)
				}
			}
			if !envValue(args, "HOME", home) {
				t.Errorf("missing HOME env: %v", args)
			}

			for _, m := range opencodeAdapterMounts(home) {
				got := argValue(args, "-v", m)
				if got != c.wantAdapter {
					t.Errorf("adapter mount %s present=%v, want %v (args %v)", m, got, c.wantAdapter, args)
				}
			}
			if got := envValue(args, "PATH", adapterPATH); got != c.wantAdapter {
				t.Errorf("adapter PATH prefix present=%v, want %v (args %v)", got, c.wantAdapter, args)
			}
		})
	}
}

// TestStandardHostMountArgsNoHostHome pins the unchanged early return.
func TestStandardHostMountArgsNoHostHome(t *testing.T) {
	d := &Daemon{}
	if args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}}); args != nil {
		t.Errorf("no HostHome must produce no args, got %v", args)
	}
}

// TestStandardHostMountArgsGitStrategyGate pins that the boot-profile change
// did not disturb the git-strategy gate: a "none" run keeps the adapter
// mounts (model auth is needed) but loses the whole push surface.
func TestStandardHostMountArgsGitStrategyGate(t *testing.T) {
	home, d := bootProfileTestHome(t)
	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "none"})
	for _, g := range []string{".gitconfig", ".git-credentials"} {
		p := filepath.Join(home, g) + ":" + filepath.Join(home, g) + ":ro"
		if argValue(args, "-v", p) {
			t.Errorf("a none-strategy run must not carry %s: %v", g, args)
		}
	}
	for _, m := range opencodeAdapterMounts(home) {
		if !argValue(args, "-v", m) {
			t.Errorf("a none-strategy opencode run still needs the adapter mount %s: %v", m, args)
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "GH_TOKEN=") {
			t.Errorf("a none-strategy run must not carry a push-capable token: %v", args)
		}
	}
}

// claudeMountTestHome adds the claude surface (config home, ~/.claude.json,
// the launcher symlink and its install root) to the standard boot-profile test
// home, so a MIXED opencode+claude request can be exercised against one home.
func claudeMountTestHome(t *testing.T) (string, *Daemon) {
	t.Helper()
	home, d := bootProfileTestHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(versions, "9.9.9")
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(binDir, "claude")); err != nil {
		t.Fatal(err)
	}
	return home, d
}

// claudeAdapterMounts are the four conditional claude mounts under home, in
// declaration order, with the RW/RO split on the exact -v strings.
func claudeAdapterMounts(home string) []string {
	p := func(parts ...string) string {
		return filepath.Join(append([]string{home}, parts...)...)
	}
	return []string{
		p(".claude") + ":" + p(".claude") + ":rw",
		p(".claude.json") + ":" + p(".claude.json") + ":rw",
		p(".local", "bin", "claude") + ":" + p(".local", "bin", "claude") + ":ro",
		p(".local", "share", "claude") + ":" + p(".local", "share", "claude") + ":ro",
	}
}

// TestStandardHostMountArgsClaudeRWAndPath is the daemon-level half: a
// claude-demanding run carries all four claude installs with the RW/RO split
// and the ~/.local/bin PATH prefix; an opencode-only run carries neither the
// claude mounts nor a claude PATH entry; and a MIXED run carries both mounts
// and both PATH entries (opencode first, per requestedKinds' sort).
func TestStandardHostMountArgsClaudeRWAndPath(t *testing.T) {
	home, d := claudeMountTestHome(t)

	suffix := ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	ocPATH := filepath.Join(home, ".opencode", "bin") + suffix
	clPATH := filepath.Join(home, ".local", "bin") + suffix
	// requestedKinds SORTS the boot profile, so "claude" precedes
	// "opencode" for a mixed run (same order as the mount block).
	bothPATH := filepath.Join(home, ".local", "bin") + ":" + filepath.Join(home, ".opencode", "bin") + suffix

	assertMounts := func(t *testing.T, args []string, want []string, wantPresent bool) {
		t.Helper()
		for _, m := range want {
			if got := argValue(args, "-v", m); got != wantPresent {
				t.Errorf("mount %s present=%v, want %v (args %v)", m, got, wantPresent, args)
			}
		}
	}

	t.Run("claude-only", func(t *testing.T) {
		args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"claude"}, GitStrategy: "local"})
		assertMounts(t, args, claudeAdapterMounts(home), true)
		assertMounts(t, args, opencodeAdapterMounts(home), false)
		if !envValue(args, "PATH", clPATH) {
			t.Errorf("claude-only PATH must be %q, args %v", clPATH, args)
		}
	})

	t.Run("opencode-only", func(t *testing.T) {
		args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})
		assertMounts(t, args, claudeAdapterMounts(home), false)
		assertMounts(t, args, opencodeAdapterMounts(home), true)
		if !envValue(args, "PATH", ocPATH) {
			t.Errorf("opencode-only PATH must stay %q, args %v", ocPATH, args)
		}
	})

	t.Run("mixed", func(t *testing.T) {
		args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode", "claude"}, GitStrategy: "local"})
		assertMounts(t, args, claudeAdapterMounts(home), true)
		assertMounts(t, args, opencodeAdapterMounts(home), true)
		if !envValue(args, "PATH", bothPATH) {
			t.Errorf("mixed PATH must be %q, args %v", bothPATH, args)
		}
	})
}

// TestClaudeCreatePreflightFailsActionably pins the create-time surfacing:
// a claude-demanding run with no reachable host credential is rejected with
// the canonical sign-in instruction, and each of the three credential sources
// (host login, env var, create secret) satisfies the preflight.
func TestClaudeCreatePreflightFailsActionably(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	home := t.TempDir()
	d := &Daemon{HostHome: home}
	if d.claudeAuthAvailable(CreateRequest{}) {
		t.Fatal("no host login, no env key, no secret must report unauthenticated")
	}

	// 1. The operator's persisted host login (written by `claude` sign-in).
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.claudeAuthAvailable(CreateRequest{}) {
		t.Fatal("a persisted ~/.claude/.credentials.json must satisfy the preflight")
	}

	// 2. An ANTHROPIC_API_KEY create secret.
	noCred := &Daemon{HostHome: t.TempDir()}
	if noCred.claudeAuthAvailable(CreateRequest{}) {
		t.Fatal("an empty home with no env key must report unauthenticated")
	}
	if !noCred.claudeAuthAvailable(CreateRequest{Secrets: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-x"}}) {
		t.Fatal("an ANTHROPIC_API_KEY create secret must satisfy the preflight")
	}

	// 3. An ANTHROPIC_API_KEY in the daemon's own environment.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-host")
	if !noCred.claudeAuthAvailable(CreateRequest{}) {
		t.Fatal("an ANTHROPIC_API_KEY in the daemon env must satisfy the preflight")
	}

	// The message names the exact host-side remedy (actionable, never a hang).
	for _, want := range []string{"not authenticated", "host", "claude", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(adapter.ClaudeAuthRequiredMessage, want) {
			t.Errorf("auth message %q is missing %q", adapter.ClaudeAuthRequiredMessage, want)
		}
	}
}
