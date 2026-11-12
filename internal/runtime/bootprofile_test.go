package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// TestRequestedKinds pins the boot-profile semantics, including the mixed-version rollout rule: a
// NIL profile (a pre-change plane) is "unspecified" and resolves to the default kind.
func TestRequestedKinds(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"absent (legacy plane)", nil, []string{adapter.DefaultAdapterKind}},
		{"explicitly empty", []string{}, []string{}},
		{"native only", []string{"orchicon"}, []string{"orchicon"}},
		{"opencode only", []string{"opencode"}, []string{"opencode"}},
		{"mixed", []string{"opencode", "orchicon"}, []string{"opencode", "orchicon"}},
		{"deduped and sorted", []string{"orchicon", "opencode", "opencode"}, []string{"opencode", "orchicon"}},
		{"blank folds to default", []string{"  "}, []string{adapter.DefaultAdapterKind}},
	}
	for _, c := range cases {
		got := requestedKinds(CreateRequest{AdapterKinds: c.in})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: requestedKinds(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestDemandsKind is the mount/serve gate: only a profile that actually
// demands opencode may mount its config/auth/CLI or warm its serve.
func TestDemandsKind(t *testing.T) {
	absent := CreateRequest{}
	if !demandsKind(absent, adapter.DefaultAdapterKind) {
		t.Error("an absent profile must demand opencode (legacy behavior preserved)")
	}
	native := CreateRequest{AdapterKinds: []string{"orchicon"}}
	if demandsKind(native, adapter.DefaultAdapterKind) {
		t.Error("a native-only profile must NOT demand opencode (AC 1)")
	}
	if !demandsKind(native, "orchicon") {
		t.Error("a native-only profile must demand orchicon")
	}
	mixed := CreateRequest{AdapterKinds: []string{"opencode", "orchicon"}}
	if !demandsKind(mixed, adapter.DefaultAdapterKind) || !demandsKind(mixed, "orchicon") {
		t.Error("a mixed profile must demand both kinds (AC 2)")
	}
	empty := CreateRequest{AdapterKinds: []string{}}
	if demandsKind(empty, adapter.DefaultAdapterKind) {
		t.Error("an explicitly empty profile must demand nothing")
	}
}

// TestServeKindsFor is the serve multiplexing filter: exactly the serve-dependent kinds warm a
// serve, and today that is opencode alone (AC 2/AC 5).
func TestServeKindsFor(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"opencode", "orchicon"}, []string{"opencode"}},
		{[]string{"orchicon"}, []string{}},
		{[]string{}, []string{}},
		{[]string{"opencode"}, []string{"opencode"}},
	}
	for _, c := range cases {
		if got := serveKindsFor(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("serveKindsFor(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestServeDependentKindMatchesLifecycle pins that the boot profile's serve-dependency predicate is
// the SAME evaluation the scheduler's run-start gate uses (Lifecycle.ServeDependent).
func TestServeDependentKindMatchesLifecycle(t *testing.T) {
	l := &Lifecycle{}
	for _, k := range []string{"", "opencode", "orchicon", "claude", "codex", "  "} {
		if got, want := serveDependentKind(k), l.ServeDependent(k); got != want {
			t.Errorf("serveDependentKind(%q) = %v, but Lifecycle.ServeDependent = %v", k, got, want)
		}
	}
}

// TestAdapterInstallPathsTotalOverCatalog is AC 6(a)/(b): the classification table must be TOTAL
// over the LIVE kind list (the builtin provider catalog.
func TestAdapterInstallPathsTotalOverCatalog(t *testing.T) {
	kinds := adapter.BuiltinAdapterKinds()
	if len(kinds) < 3 {
		t.Fatalf("builtin catalog declared only %d adapter kinds — the live kind source shrank", len(kinds))
	}
	for kind := range kinds {
		paths, declared := adapterInstallPaths("/home/u", kind)
		if !declared {
			t.Fatalf("adapter kind %q is declared by the builtin catalog but NOT classified in adapterInstalls — classify it so the mount-never-bake guard covers it", kind)
		}
		_ = paths
	}
	// An invented/undeclared kind must be reported unclassified (never a
	// silent pass).
	if _, declared := adapterInstallPaths("/home/u", "codex-not-declared-yet"); declared {
		t.Error("an unclassified kind must report declared=false")
	}
	// orchicon is the product binary: its mount-never-bake rule is the daemon's /usr/local/bin/orchicon
	// bind mount, NOT an adapter install — so it is classified but contributes no host install.
	if paths, declared := adapterInstallPaths("/home/u", "orchicon"); !declared || len(paths) != 0 {
		t.Errorf("orchicon must be classified with no adapter install paths, got %v declared=%v", paths, declared)
	}
}

// TestAdapterHostMountsConditional is the AC 1/AC 3 mount conditionality in pure form: an
// opencode-demanding home yields the three install mounts in the historical order.
func TestAdapterHostMountsConditional(t *testing.T) {
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

	want := []string{
		"-v", filepath.Join(home, ".config", "opencode") + ":" + filepath.Join(home, ".config", "opencode") + ":ro",
		"-v", filepath.Join(home, ".opencode") + ":" + filepath.Join(home, ".opencode") + ":ro",
	}
	if got := adapterHostMounts(home, "opencode"); !reflect.DeepEqual(got, want) {
		t.Errorf("opencode mounts = %v, want %v", got, want)
	}
	// Native-only: nothing opencode-related, even though the host HAS an
	// opencode install (the leak AC 1 closes).
	if got := adapterHostMounts(home, "orchicon"); len(got) != 0 {
		t.Errorf("native-only kind must contribute no adapter mounts, got %v", got)
	}
	// Empty home: nothing.
	if got := adapterHostMounts("", "opencode"); len(got) != 0 {
		t.Errorf("empty home must contribute no adapter mounts, got %v", got)
	}
	// THE PATH MUST BE EMPTIED TOO, and that is a behaviour change rather than a test workaround: the
	// resolver now ASKS THE OS's PATH (internal/adapter discovery) after the known install lists.
	t.Setenv("PATH", t.TempDir())
	if got := adapterHostMounts(t.TempDir(), "opencode"); len(got) != 0 {
		t.Errorf("absent install must contribute no adapter mounts, got %v", got)
	}
}

// TestAdapterHostMountsFileShapeGate pins the probe's file/dir requirement: a DIRECTORY where the
// CLI binary is expected is not a valid install (the original daemon gate required.
func TestAdapterHostMountsFileShapeGate(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".opencode", "bin", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := adapterHostMounts(home, "opencode")
	for _, a := range got {
		if a == filepath.Join(home, ".opencode")+":"+filepath.Join(home, ".opencode")+":ro" {
			t.Fatalf("a directory at the CLI path must not mount as an install: %v", got)
		}
	}
}

// TestAdapterKindsWireDistinction pins the rollout signal ACROSS THE WIRE: the boot profile rides
// CreateRequest through JSON to the daemon, and the nil-vs-empty distinction is load-bearing.
func TestAdapterKindsWireDistinction(t *testing.T) {
	roundTrip := func(in []string) []string {
		t.Helper()
		blob, err := json.Marshal(CreateRequest{AdapterKinds: in})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if in == nil && !strings.Contains(string(blob), `"adapter_kinds":null`) {
			t.Fatalf("a nil profile must marshal as an explicit null adapter_kinds, got %s", blob)
		}
		var out CreateRequest
		if err := json.Unmarshal(blob, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out.AdapterKinds
	}

	if got := roundTrip(nil); got != nil {
		t.Errorf("a nil (legacy/unspecified) profile must survive as nil, got %#v", got)
	}
	if got := roundTrip([]string{}); got == nil {
		t.Error("an explicitly empty profile must survive as EMPTY, not nil — nil means the default opencode demand (mounts + serve)")
	}
	if got := requestedKinds(CreateRequest{AdapterKinds: roundTrip([]string{})}); len(got) != 0 {
		t.Errorf("a round-tripped empty profile must demand nothing, got %v", got)
	}
	if got := requestedKinds(CreateRequest{AdapterKinds: roundTrip(nil)}); !reflect.DeepEqual(got, []string{adapter.DefaultAdapterKind}) {
		t.Errorf("a round-tripped nil profile must resolve to the default demand, got %v", got)
	}
	if got := requestedKinds(CreateRequest{AdapterKinds: roundTrip([]string{"orchicon"})}); !reflect.DeepEqual(got, []string{"orchicon"}) {
		t.Errorf("a round-tripped native-only profile must survive verbatim, got %v", got)
	}
}

// canonicalTempHome returns a throwaway home directory whose path contains NO symlinked component.
func canonicalTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("canonicalize temp home %s: %v", dir, err)
	}
	return resolved
}

// claudeMountHome builds a fake host home laid out exactly as the claude NATIVE installer leaves
// it: a writable config home (~/.claude/), the rewritten ~/.claude.json.
func claudeMountHome(t *testing.T) (home, launcher, versionedBinary string) {
	t.Helper()
	home = canonicalTempHome(t)
	mkdir := func(parts ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(append([]string{home}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkfile := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkdir(".claude")
	mkfile(".claude", ".credentials.json") // the claude.ai OAuth credential (0600 on a real host)
	mkfile(".claude.json")
	mkdir(".local", "bin")
	versionedBinary = mkfile(".local", "share", "claude", "versions", "9.9.9")
	launcher = filepath.Join(home, ".local", "bin", "claude")
	if err := os.Symlink(versionedBinary, launcher); err != nil {
		t.Fatal(err)
	}
	return home, launcher, versionedBinary
}

// The CLI launcher and install root stay READ-ONLY host binds (executing the image's adapter does
// not need them, but an operator who named a host binary still gets it).
func TestAdapterHostMountsClaudeRW(t *testing.T) {
	home, _, _ := claudeMountHome(t)
	want := []string{
		"-v", filepath.Join(home, ".local", "bin", "claude") + ":" + filepath.Join(home, ".local", "bin", "claude") + ":ro",
		"-v", filepath.Join(home, ".local", "share", "claude") + ":" + filepath.Join(home, ".local", "share", "claude") + ":ro",
	}
	if got := adapterHostMounts(home, "claude"); !reflect.DeepEqual(got, want) {
		t.Errorf("claude mounts =\n%v\nwant\n%v", got, want)
	}
	// NOTHING the container can write may be a host bind: that is the whole fix.
	for _, a := range adapterHostMounts(home, "claude") {
		if strings.HasSuffix(a, ":rw") {
			t.Errorf("mount %q is READ-WRITE on the host — concurrent containers would share one writable "+
				"tree; the config/transcript home must be PRIVATE per container", a)
		}
	}
	// The credential is still delivered: private installs bind their authFile read-only inside the copy.
	priv := adapterPrivateHomes(home, "claude")
	if len(priv) == 0 {
		t.Fatal("claude has no private install — its config/transcript home must be private per container")
	}
	foundAuth := false
	for _, ph := range priv {
		if ph.authFile != "" {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Error("no private install declares an authFile — a private home with no credential would give the " +
			"container an unauthenticated claude")
	}

	// Regression: an opencode install is still mounted read-only everywhere.
	homeOC := t.TempDir()
	for _, d := range [][]string{{".config", "opencode"}, {".local", "share", "opencode"}} {
		if err := os.MkdirAll(filepath.Join(append([]string{homeOC}, d...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ocBin := filepath.Join(homeOC, ".opencode", "bin")
	if err := os.MkdirAll(ocBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocBin, "opencode"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	ocMounts := adapterHostMounts(homeOC, "opencode")
	if len(ocMounts) == 0 {
		t.Fatal("expected opencode mounts on a populated opencode home")
	}
	for _, a := range ocMounts {
		if strings.HasSuffix(a, ":rw") {
			t.Errorf("the read-write concept must never leak to opencode: %q", a)
		}
	}
}

// TestClaudeLauncherSymlinkTargetMounted is the "the launcher cannot dangle in-container"
// assertion: the launcher is a SYMLINK into the install root.
func TestClaudeLauncherSymlinkTargetMounted(t *testing.T) {
	home, launcher, versioned := claudeMountHome(t)
	if got, err := os.Readlink(launcher); err != nil || got != versioned {
		t.Fatalf("launcher must be a symlink to the install root, readlink=%q err=%v", got, err)
	}
	resolved, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		t.Fatalf("the launcher does not resolve: %v", err)
	}
	// Extract the mounted src dir of every `-v src:src:mode` pair.
	args := adapterHostMounts(home, "claude")
	var srcs []string
	for i := 0; i+1 < len(args); i += 2 {
		spec := args[i+1]
		if j := strings.Index(spec, ":"); j > 0 {
			srcs = append(srcs, spec[:j])
		}
	}
	mounted := false
	for _, s := range srcs {
		if resolved == s || strings.HasPrefix(resolved, s+string(os.PathSeparator)) {
			mounted = true
			break
		}
	}
	if !mounted {
		t.Fatalf("the launcher symlink target %s is under NONE of the mounted dirs %v — it would DANGLE inside the container", resolved, srcs)
	}
	// DIRECTION CHECK.
	rel, err := filepath.Rel(filepath.Join(home, ".local", "share", "claude"), resolved)
	if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
		t.Fatalf("the launcher target %s escapes the mounted install root: rel=%q err=%v", resolved, rel, err)
	}
	// The install root must be mounted as its own declaration (the design mounts at identical absolute
	// paths, so the link resolves only if the root rides along).
	root := filepath.Join(home, ".local", "share", "claude")
	found := false
	for _, s := range srcs {
		if s == root {
			found = true
		}
	}
	if !found {
		t.Fatalf("the claude install root %s is not among the mounted dirs %v", root, srcs)
	}
}

// --- the container half: an unrunnable adapter must be refused, not started ------------------------

// TestAnUnrunnableHostAdapterIsFineWhenTheImageSuppliesIt is the WINDOWS CONTAINER-MODE CASE, and
// it reverses the test that stood here.
func TestAnUnrunnableHostAdapterIsFineWhenTheImageSuppliesIt(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A Windows-shaped install: the exact shape the operator's host has.
	if err := os.WriteFile(filepath.Join(dir, "claude.exe"), []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCHICON_CLAUDE_BIN", filepath.Join(dir, "claude.exe"))

	if problems := CheckAdaptersForContainer(home, "claude"); len(problems) != 0 {
		t.Fatalf("problems = %v, want NONE — the image supplies claude, so the host's .exe is irrelevant "+
			"and refusing here is what made container mode impossible on Windows", problems)
	}
}

// TestTheRefusalStillFiresWhenTheImageCannotCoverTheKind keeps the guard doing its real job.
func TestTheRefusalStillFiresWhenTheImageCannotCoverTheKind(t *testing.T) {
	problems := containerAdapterProblems(func(kind string) (string, error) { return "/host/thing.exe", nil },
		func(string) (imageAdapterKind, bool) { return imageAdapterKind{}, false }, "claude")
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one refusal — nothing supplies the kind and the host's binary "+
			"is a PE", problems)
	}
	for _, want := range []string{"claude", "Windows executable", "Linux"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("the refusal must name %q — it is the operator's only pointer to the remedy; got: %s",
				want, problems[0])
		}
	}
	// And the same inputs WITH an image that supplies the kind must refuse nothing. This is the pair that
	// makes the rule legible: identical host, different image, opposite answers.
	if got := containerAdapterProblems(func(kind string) (string, error) { return "/host/thing.exe", nil },
		func(kind string) (imageAdapterKind, bool) { return imageAdapterKind{kind: kind, bin: kind}, true },
		"claude"); len(got) != 0 {
		t.Fatalf("problems = %v, want none — the image supplies the kind, so the host's PE is irrelevant", got)
	}
}

// TestANativeAdapterBinaryIsNotRefused is the working case: an adapter built for the container's kernel.
func TestANativeAdapterBinaryIsNotRefused(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".opencode", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if problems := CheckAdaptersForContainer(home, "opencode"); len(problems) != 0 {
		t.Fatalf("a native adapter was refused: %v", problems)
	}
}

// TestNoHomeConfiguredRefusesNothing keeps a supported shape working: a daemon with no HostHome mounts no
// adapter at all, so a container it creates simply runs without one.
func TestNoHomeConfiguredRefusesNothing(t *testing.T) {
	if problems := CheckAdaptersForContainer("", "opencode", "claude"); len(problems) != 0 {
		t.Fatalf("a daemon with no host home refused container creation: %v — it mounts no adapter, so it "+
			"has nothing to judge", problems)
	}
}

// TestTheContainerPathFollowsTheResolver is the agreement this change is FOR: the directory handed to the
// container's PATH must be the directory the plane actually resolved the adapter to, not a hardcoded one.
func TestTheContainerPathFollowsTheResolver(t *testing.T) {
	home := t.TempDir()
	// An install the OLD table did not know about: npm's node_modules, which is where the operator's
	// opencode really lives.
	bin := filepath.Join(home, "custom", "opencode")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCHICON_OPENCODE_BIN", bin)

	dirs := adapterBinDirs(home, "opencode")
	if len(dirs) == 0 || dirs[0] != filepath.Dir(bin) {
		t.Fatalf("adapterBinDirs = %v, want %q first — the container's PATH must name where the adapter "+
			"actually is, or it cannot find what the plane found", dirs, filepath.Dir(bin))
	}
	// AND IT IS MOUNTED, or the PATH entry points at a directory the container cannot see.
	mounts := adapterHostMounts(home, "opencode")
	want := "-v " + filepath.Dir(bin) + ":" + filepath.Dir(bin) + ":ro"
	if !strings.Contains(strings.Join(mounts, " "), want) {
		t.Errorf("the resolved directory is not mounted; mounts = %v, want to contain %q", mounts, want)
	}
}
