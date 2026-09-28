package opencode

// config_profile_test.go — THE TWO PROFILES.
//
// config_permission_test.go (untouched) remains the WORKER regression net: it
// asserts the worker denies by name. This file asserts the other half — that
// the worker output is byte-for-byte what it was before the split, that the
// zero value still selects it, and that the interactive profile is what the Ask
// serve needs (allows the filesystem, asks on writes, keeps the never-allow
// class, and does NOT inherit the worker-only composite read/grep deny).

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/neverallow"
)

var updateGolden = flag.Bool("update", false, "regenerate the golden permission file")

const workerPermissionGoldenPath = "testdata/worker_permission_golden.json"

// permissionSubtree returns the marshalled `permission` block BuildConfigContent
// injects (encoding/json sorts the keys, so the bytes are deterministic).
func permissionSubtree(t *testing.T, o ConfigOptions) []byte {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(BuildConfigContent(o)), &cfg); err != nil {
		t.Fatalf("config content is not valid JSON: %v", err)
	}
	perm, ok := cfg["permission"]
	if !ok {
		t.Fatal("config content carries no permission block")
	}
	b, err := json.Marshal(perm)
	if err != nil {
		t.Fatalf("marshal permission subtree: %v", err)
	}
	return b
}

// THE WORKER PATH IS UNCHANGED, PINNED BYTE-FOR-BYTE. The interactive profile
// exists so Ask stops living in the worker sandbox; the sandbox an execution
// gets must not move by a single byte, or every dispatched worker inherits a
// permission change nobody asked for.
func TestWorkerPermissionRulesGolden(t *testing.T) {
	got := permissionSubtree(t, ConfigOptions{})
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(workerPermissionGoldenPath), 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(workerPermissionGoldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d bytes)", workerPermissionGoldenPath, len(got))
		return
	}
	want, err := os.ReadFile(workerPermissionGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with: go test ./internal/opencode -run TestWorkerPermissionRulesGolden -update)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Errorf("the worker permission subtree CHANGED. got %d bytes, want %d:\ngot:  %s\nwant: %s", len(got), len(want), got, want)
	}
}

// THE DEFAULT IS EXPLICITLY THE WORKER. The zero value and an unrecognised
// value must both select the worker rules: the profile is opted INTO, so a
// missing or misspelled one fails closed rather than widening a session.
func TestDefaultProfileIsWorker(t *testing.T) {
	marshal := func(p PermissionProfile) []byte {
		t.Helper()
		b, err := json.Marshal(permissionRules(p, false))
		if err != nil {
			t.Fatalf("marshal %q profile: %v", p, err)
		}
		return b
	}
	zero := marshal("")
	if !bytes.Equal(zero, marshal(ProfileWorker)) {
		t.Errorf("the zero-value profile is not the worker profile:\nzero:   %s\nworker: %s", zero, marshal(ProfileWorker))
	}
	if !bytes.Equal(zero, marshal(PermissionProfile("bogus"))) {
		t.Errorf("an unrecognised profile did not fall back to the worker profile:\nzero:  %s\nbogus: %s", zero, marshal(PermissionProfile("bogus")))
	}

	// ...and the same at the document level: an unset profile produces the
	// identical injected config as an explicit worker one, and a DIFFERENT one
	// from the interactive profile.
	unset := BuildConfigContent(ConfigOptions{SkipUserMCP: true})
	explicit := BuildConfigContent(ConfigOptions{SkipUserMCP: true, PermissionProfile: ProfileWorker})
	if unset != explicit {
		t.Errorf("BuildConfigContent differs between an unset profile and an explicit worker one")
	}
	if interactive := BuildConfigContent(ConfigOptions{SkipUserMCP: true, PermissionProfile: ProfileInteractive}); interactive == unset {
		t.Errorf("the interactive profile produced the worker config — the selector is not wired into the document")
	}
}

// THE INTERACTIVE PROFILE, in full.
func TestInteractiveProfile(t *testing.T) {
	// compositeTools is passed TRUE to prove the interactive profile ignores it.
	perm := permissionRules(ProfileInteractive, true)

	ext, ok := perm["external_directory"].(map[string]any)
	if !ok {
		t.Fatalf("the interactive profile has no external_directory rule map, got %#v", perm["external_directory"])
	}
	if got := ext["*"]; got != "allow" {
		t.Errorf("external_directory[*] = %#v, want \"allow\" — an interactive session reaches the operator's own filesystem", got)
	}

	for _, key := range []string{"edit", "write"} {
		m, ok := perm[key].(map[string]any)
		if !ok {
			t.Fatalf("the interactive profile has no %q rule map, got %#v", key, perm[key])
		}
		if got := m["*"]; got != "ask" {
			t.Errorf("%s[*] = %#v, want \"ask\" — a write must raise opencode's own ask, not be auto-decided", key, got)
		}
	}

	bash, ok := perm["bash"].(map[string]any)
	if !ok {
		t.Fatalf("the interactive profile has no bash rule map, got %#v", perm["bash"])
	}
	if got, present := bash["*"]; present {
		t.Errorf("bash carries a catch-all rule (%#v) — unmatched commands must keep opencode's default ask", got)
	}
	for _, p := range neverallow.DenyRules() {
		if got, ok := bash[p].(string); !ok || got != "deny" {
			t.Errorf("the never-allow command %q is not denied in the interactive profile (got %#v)", p, bash[p])
		}
	}

	task, ok := perm["task"].(map[string]any)
	if !ok {
		t.Fatalf("the interactive profile has no task rule map, got %#v", perm["task"])
	}
	if got := task["*"]; got != "deny" {
		t.Errorf("task[*] = %#v, want \"deny\" — the subagent tool is denied in every profile", got)
	}

	// The composite-tools read/grep deny is WORKER-ONLY: an interactive session
	// may use whichever read tool it reaches for.
	for _, key := range []string{readToolDeny, grepToolDeny} {
		if _, present := perm[key]; present {
			t.Errorf("the interactive profile carries a %q deny — the composite-tool carve-out is worker-only", key)
		}
	}
	workerComposite := permissionRules(ProfileWorker, true)
	for _, key := range []string{readToolDeny, grepToolDeny} {
		m, ok := workerComposite[key].(map[string]any)
		if !ok || m["*"] != "deny" {
			t.Errorf("the worker profile must still deny %q with composite tools on, got %#v", key, workerComposite[key])
		}
	}
	if _, present := permissionRules(ProfileWorker, false)[readToolDeny]; present {
		t.Errorf("the worker profile denied %q without composite tools", readToolDeny)
	}
}
