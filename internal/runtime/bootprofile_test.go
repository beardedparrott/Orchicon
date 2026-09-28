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

// TestRequestedKinds pins the boot-profile semantics, including the
// mixed-version rollout rule: a NIL profile (a pre-change plane) is
// "unspecified" and resolves to the default kind, so an opencode run never
// silently loses its mounts/serve — while a NON-NIL profile (even an empty
// one) is honored verbatim (AC 1/AC 3/AC 5).
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

// TestServeKindsFor is the serve multiplexing filter: exactly the
// serve-dependent kinds warm a serve, and today that is opencode alone
// (AC 2/AC 5).
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

// TestServeDependentKindMatchesLifecycle pins that the boot profile's
// serve-dependency predicate is the SAME evaluation the scheduler's
// run-start gate uses (Lifecycle.ServeDependent), not a forked copy.
func TestServeDependentKindMatchesLifecycle(t *testing.T) {
	l := &Lifecycle{}
	for _, k := range []string{"", "opencode", "orchicon", "claude", "codex", "  "} {
		if got, want := serveDependentKind(k), l.ServeDependent(k); got != want {
			t.Errorf("serveDependentKind(%q) = %v, but Lifecycle.ServeDependent = %v", k, got, want)
		}
	}
}

// TestAdapterInstallPathsTotalOverCatalog is AC 6(a)/(b): the classification
// table must be TOTAL over the LIVE kind list (the builtin provider catalog,
// which already declares "claude" while only opencode/orchicon are
// dispatcher-registered). An unclassified declared kind fails loudly here,
// so the NEXT adapter (codex-shaped growth) cannot slip past the
// mount-never-bake invariant by construction.
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
	// orchicon is the product binary: its mount-never-bake rule is the
	// daemon's /usr/local/bin/orchicon bind mount, NOT an adapter install —
	// so it is classified but contributes no host install.
	if paths, declared := adapterInstallPaths("/home/u", "orchicon"); !declared || len(paths) != 0 {
		t.Errorf("orchicon must be classified with no adapter install paths, got %v declared=%v", paths, declared)
	}
}

// TestAdapterHostMountsConditional is the AC 1/AC 3 mount conditionality in
// pure form: an opencode-demanding home yields the three install mounts in
// the historical order; a native-only home yields none; each mount is
// gated on the probe's existence and file/dir shape.
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
		"-v", filepath.Join(home, ".local", "share", "opencode") + ":" + filepath.Join(home, ".local", "share", "opencode") + ":ro",
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
	// Missing install: nothing (probe-gated, mirroring the original os.Stat).
	if got := adapterHostMounts(t.TempDir(), "opencode"); len(got) != 0 {
		t.Errorf("absent install must contribute no adapter mounts, got %v", got)
	}
}

// TestAdapterHostMountsFileShapeGate pins the probe's file/dir requirement:
// a DIRECTORY where the CLI binary is expected is not a valid install (the
// original daemon gate required ~/.opencode/bin/opencode to be a file).
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

// TestAdapterKindsWireDistinction pins the rollout signal ACROSS THE WIRE:
// the boot profile rides CreateRequest through JSON to the daemon, and the
// nil-vs-empty distinction is load-bearing — a NIL profile (a pre-change
// plane, or `"adapter_kinds":null`) is the legacy "unspecified" case and
// resolves to the default (opencode) demand, while an explicitly EMPTY
// profile means "this run demands no adapter kind" and mounts nothing
// (AC 1/AC 3). Keeping the field free of `omitempty` is what preserves an
// empty profile as `[]` instead of dropping it to an absent/null field;
// this test fails if someone adds omitempty, which would silently re-mount
// model config/auth into a native-only container.
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

// claudeMountHome builds a fake host home laid out exactly as the claude
// NATIVE installer leaves it: a writable config home (~/.claude/), the
// rewritten ~/.claude.json, a launcher SYMLINK at ~/.local/bin/claude, and the
// install root (~/.local/share/claude/versions/<ver>) the symlink resolves
// into. Used by the mount and symlink-resolution tests.
func claudeMountHome(t *testing.T) (home, launcher, versionedBinary string) {
	t.Helper()
	home = t.TempDir()
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

// TestAdapterHostMountsClaudeRW asserts the EXACT -v arg strings for claude:
// the config home and ~/.claude.json are READ-WRITE (a session writes its
// transcript tree), while the CLI launcher dir and the install root stay
// READ-ONLY. It also pins the regression direction: the rw concept must never
// leak to another adapter kind.
func TestAdapterHostMountsClaudeRW(t *testing.T) {
	home, _, _ := claudeMountHome(t)
	want := []string{
		"-v", filepath.Join(home, ".claude") + ":" + filepath.Join(home, ".claude") + ":rw",
		"-v", filepath.Join(home, ".claude.json") + ":" + filepath.Join(home, ".claude.json") + ":rw",
		"-v", filepath.Join(home, ".local", "bin") + ":" + filepath.Join(home, ".local", "bin") + ":ro",
		"-v", filepath.Join(home, ".local", "share", "claude") + ":" + filepath.Join(home, ".local", "share", "claude") + ":ro",
	}
	if got := adapterHostMounts(home, "claude"); !reflect.DeepEqual(got, want) {
		t.Errorf("claude mounts =\n%v\nwant\n%v", got, want)
	}
	// Exactly two RW mounts — nothing else is writable.
	rw := 0
	for _, a := range adapterHostMounts(home, "claude") {
		if strings.HasSuffix(a, ":rw") {
			rw++
		}
	}
	if rw != 2 {
		t.Errorf("expected exactly 2 read-write claude mounts, got %d", rw)
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

// TestClaudeLauncherSymlinkTargetMounted is the "the launcher cannot dangle
// in-container" assertion: the launcher is a SYMLINK into the install root, so
// the symlink's resolved target must live under one of the dirs the daemon
// actually mounts (both are mounted at identical absolute host paths).
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
	// The install root must be mounted as its own declaration (the design
	// mounts at identical absolute paths, so the link resolves only if the
	// root rides along).
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
