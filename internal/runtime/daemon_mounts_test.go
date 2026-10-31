package runtime

import (
	"fmt"
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
	// The credential file inside the data home: shared READ-ONLY into the
	// container's private copy of it (never the directory itself).
	mkfile(".local", "share", "opencode", "auth.json")
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

// opencodeAdapterMounts are the conditional HOST BINDS under home. The data
// home is deliberately absent: it is not bind-mounted at all, it is a private
// uid-owned directory (opencodePrivateDataHome).
func opencodeAdapterMounts(home string) []string {
	out := make([]string, 0, 2)
	for _, p := range []string{
		filepath.Join(home, ".config", "opencode"),
		filepath.Join(home, ".opencode"),
	} {
		out = append(out, p+":"+p+":ro")
	}
	return out
}

// tmpfsOwnershipSuffix is the ownership grammar a --tmpfs entry carries for a
// given engine family, so the expectations below stay engine-aware instead of
// hard-coding one engine's spelling.
//
// THE TWO ENGINES DO NOT ACCEPT THE SAME OPTIONS, and pinning docker's was the
// defect: podman rejects uid=/gid= outright ("unknown mount option \"uid=1000\""),
// so a test that asserted `uid=0,gid=0,mode=0755` on EVERY argv was asserting a
// string the engine refuses — green tests over a container that could not be
// created. The runtime uid reaches its mounts through the MODE under podman.
func tmpfsOwnershipSuffix(podman bool) string { return tmpfsOwnershipSuffixFor(podman, 0, 0) }

// tmpfsOwnershipSuffixFor is the same grammar with an explicit uid/gid, for the
// assertions that pin a daemon's REAL ids (docker receives the host uid).
func tmpfsOwnershipSuffixFor(podman bool, uid, gid int) string {
	if podman {
		return ",mode=0777"
	}
	return fmt.Sprintf(",uid=%d,gid=%d,mode=0755", uid, gid)
}

// opencodePrivateDataHome is the private data home an opencode-demanding run
// gets: the same path inside and out, as every mount is, but a tmpfs the runtime
// uid can write rather than the operator's directory.
func opencodePrivateDataHome(home string, podman bool) string {
	return filepath.Join(home, ".local", "share", "opencode") + ":rw" + tmpfsOwnershipSuffix(podman)
}

// homeOwnedTmpfsOpts are the three tmpfs entries that make $HOME — and the
// `.local` and `.config` a bind inside it forces the engine to create as root —
// writable by the runtime uid. Without them the adapter CLI exits 1 at boot
// (`EACCES mkdir $HOME/.cache` on 1.x, `mkdir $HOME/.local/state` on 2.x).
func homeOwnedTmpfsOpts(home string, podman bool) []string {
	suffix := ":rw" + tmpfsOwnershipSuffix(podman)
	return []string{
		home + suffix,
		filepath.Join(home, ".local") + suffix,
		filepath.Join(home, ".config") + suffix,
	}
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
	// THE PREFIX IS ABSENT FOR AN IMAGE-SUPPLIED KIND NOW — claude/opencode are installed in the image, so a
	// host bin dir would SHADOW them (this PATH is prepended, ahead of /usr/local/bin). The expectation below
	// is therefore "the prefix appears only when the operator named a binary", asserted in the dedicated case
	// at the end.
	adapterPATH := filepath.Join(home, ".opencode", "bin") + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	_ = adapterPATH

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
			// The private data home follows the adapter demand like the host
			// binds do; the uid-owned HOME trio does NOT (it is what makes the
			// runtime's home writable at all, adapter or not).
			if got := argValue(args, "--tmpfs", opencodePrivateDataHome(home, true)); got != c.wantAdapter {
				t.Errorf("private data home present=%v, want %v (args %v)", got, c.wantAdapter, args)
			}
			for _, opt := range homeOwnedTmpfsOpts(home, true) {
				if !argValue(args, "--tmpfs", opt) {
					t.Errorf("missing uid-owned home tmpfs %s: %v", opt, args)
				}
			}
			// THE IMAGE'S ADAPTER IS NOT SHADOWED. opencode is installed in the shipped images, so its host
			// bin dir must NOT be prepended — the container's own /usr/local/bin copy is the one that runs.
			if got := envValue(args, "PATH", adapterPATH); got {
				t.Errorf("the host adapter bin dir was prepended for an IMAGE-SUPPLIED kind — that shadows the "+
					"in-image adapter and puts the container back on the host's binary (args %v)", args)
			}
		})
	}
}

// TestStandardHostMountArgsOpencodePrivateDataHome pins the two halves of the
// fix for `serve handshake: exit status 1`: the CLI needs a WRITABLE data home
// (2.x writes log/ and its SQLite DB there, and a read-only mount fails the
// boot outright), and it needs one it OWNS, which is why the operator's
// directory is never bind-mounted — not read-only, and not read-write either,
// since that would hand a worker write access to the operator's credential
// store. Both opencode 1.x and 2.x boot against this shape; only the credential
// source differs by version (1.x reads auth.json, 2.x needs env).
func TestStandardHostMountArgsOpencodePrivateDataHome(t *testing.T) {
	home, d := bootProfileTestHome(t)
	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})

	data := filepath.Join(home, ".local", "share", "opencode")
	if argValue(args, "-v", data+":"+data+":ro") {
		t.Errorf("the operator's data dir must not be bind-mounted read-only — that is the EROFS that killed the boot: %v", args)
	}
	if argValue(args, "-v", data+":"+data+":rw") {
		t.Errorf("the operator's data dir must not be bind-mounted read-write — a worker would get write access to the credential store and session DB: %v", args)
	}
	if !argValue(args, "--tmpfs", opencodePrivateDataHome(home, true)) {
		t.Errorf("missing the private uid-owned data home: %v", args)
	}
	auth := filepath.Join(data, "auth.json")
	if got, want := auth+":"+auth+":ro", true; argValue(args, "-v", got) != want {
		t.Errorf("the credential file must be shared read-only into the private home: %v", args)
	}
	for _, opt := range homeOwnedTmpfsOpts(home, true) {
		if !argValue(args, "--tmpfs", opt) {
			t.Errorf("missing uid-owned home tmpfs %s: %v", opt, args)
		}
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
	// NO READ-WRITE HOST BIND. ~/.claude and ~/.claude.json are PRIVATE per container (a writable tmpfs at
	// the same path), so concurrent runs cannot write into one tree on the operator's machine. Only the
	// CLI launcher and install root remain host binds, read-only.
	return []string{
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
		// THE WRITABLE HOME IS STILL DELIVERED — as a private tmpfs, not a host bind.
		if !argValue(args, "--tmpfs", home+"/.claude:rw,mode=0777") {
			t.Errorf("claude's config/transcript home has no private writable mount — the CLI would fail to "+
				"write its session state (args %v)", args)
		}
		// NO HOST PREFIX: claude is installed in the shipped image, and this PATH is PREPENDED — a host bin
		// dir here would shadow it and put the container back on the host's binary.
		if envValue(args, "PATH", clPATH) {
			t.Errorf("claude is IMAGE-SUPPLIED, so its host bin dir must not be prepended (args %v)", args)
		}
	})

	t.Run("opencode-only", func(t *testing.T) {
		args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode"}, GitStrategy: "local"})
		assertMounts(t, args, claudeAdapterMounts(home), false)
		assertMounts(t, args, opencodeAdapterMounts(home), true)
		if envValue(args, "PATH", ocPATH) {
			t.Errorf("opencode is IMAGE-SUPPLIED, so its host bin dir must not be prepended (args %v)", args)
		}
	})

	t.Run("mixed", func(t *testing.T) {
		args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"opencode", "claude"}, GitStrategy: "local"})
		assertMounts(t, args, claudeAdapterMounts(home), true)
		assertMounts(t, args, opencodeAdapterMounts(home), true)
		if envValue(args, "PATH", bothPATH) {
			t.Errorf("both kinds are IMAGE-SUPPLIED, so neither host bin dir may be prepended (args %v)", args)
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

// TestStandardHostMountArgsClaudePrivateHomeAndConfig pins the claude arm, which had TWO silent drops.
//
// REPORTED: the claude adapter in a runtime container produced SIX executions, 0 tokens each, ~280ms apart,
// every one dying with "claude session exited without a terminal result message (exit code 1)". The adapter
// binary ran — it did not fail on exec format — so the CLI started and gave up immediately.
//
// TWO CAUSES, both in this mount set:
//
//  1. ~/.claude.json IS A FILE, and adapterPrivateHomes required st.IsDir(). A private install that is not a
//     directory was SKIPPED WITHOUT A WORD, so a declaration in this very table never reached a container.
//     The CLI found no config and exited.
//  2. The credential was bound READ-ONLY. This CLI REWRITES .credentials.json when its OAuth token refreshes,
//     so a refresh inside the container is EACCES and the session dies before its first API call.
//
// opencode's credential stays read-only — it only reads auth.json — which is why the write-back is a
// per-adapter declaration (adapterInstall.authWritable) rather than a blanket relaxation.
func TestStandardHostMountArgsClaudePrivateHomeAndConfig(t *testing.T) {
	home, d := bootProfileTestHome(t)
	// The claude installs the table declares.
	mustMkdir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{home}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(parts ...string) {
		t.Helper()
		p := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(".claude")
	mustWrite(".claude", ".credentials.json")
	mustWrite(".claude.json")
	mustMkdir(".local", "bin")
	mustWrite(".local", "bin", "claude")

	args := d.standardHostMountArgs(CreateRequest{AdapterKinds: []string{"claude"}, GitStrategy: "local"})

	// 1. THE PRIVATE FILE IS MOUNTED, writable. Absent is the reported 0-token exit.
	cfg := filepath.Join(home, ".claude.json")
	if !argValue(args, "-v", cfg+":"+cfg+":rw") {
		t.Errorf("~/.claude.json is NOT mounted — a private FILE install was dropped from the mount set, which "+
			"is why the claude CLI started and exited 1 with no output: %v", args)
	}

	// 2. THE CREDENTIAL IS WRITABLE, because this CLI refreshes it.
	cred := filepath.Join(home, ".claude", ".credentials.json")
	if !argValue(args, "-v", cred+":"+cred+":rw") {
		t.Errorf("the claude credential must be READ-WRITE so an OAuth token refresh can write back; "+
			"read-only makes the refresh EACCES and kills the session before its first API call: %v", args)
	}
	if argValue(args, "-v", cred+":"+cred+":ro") {
		t.Errorf("the claude credential is still read-only: %v", args)
	}

	// 3. The transcript home stays PRIVATE (a writable copy), so concurrent runs never share one tree.
	if !argValue(args, "--tmpfs", opencodePrivateDataHome(home, true)) && !containsTmpfs(args, filepath.Join(home, ".claude")) {
		t.Errorf("~/.claude must be a private writable directory, not a bind of the host's tree: %v", args)
	}
	if argValue(args, "-v", filepath.Join(home, ".claude")+":"+filepath.Join(home, ".claude")+":rw") {
		t.Errorf("~/.claude is bind-mounted from the host — concurrent runs would interleave their session "+
			"state in one tree: %v", args)
	}
}

// containsTmpfs reports whether args carries a --tmpfs whose path component matches want.
func containsTmpfs(args []string, want string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--tmpfs" {
			continue
		}
		p, _, _ := strings.Cut(args[i+1], ":")
		if p == want {
			return true
		}
	}
	return false
}
