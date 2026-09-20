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
