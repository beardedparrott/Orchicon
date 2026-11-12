package tui

// plane_ops_single_source_test.go — the plane's ACTIONS have exactly one implementation.

import (
	"os"
	"strings"
	"testing"
)

// mutatingVerbs are the planecontrol calls that CHANGE host state. Each must be reached through
// planebind, never called directly from this package.
var mutatingVerbs = []string{
	"planecontrol.Start(",
	"planecontrol.Stop(",
	"planecontrol.Restart(",
}

func TestPlaneActionsHaveOneImplementation(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, verb := range mutatingVerbs {
			if strings.Contains(string(src), verb) {
				t.Errorf("%s calls %s directly.\n"+
					"  The plane's actions must go through internal/planebind, which is the SAME\n"+
					"  surface the desktop binds — otherwise the two clients can disagree about what\n"+
					"  'stopped' means, which is the defect that produced '✓ dev is stopped' over a\n"+
					"  live postgres on port 5432.", name, strings.TrimSuffix(verb, "("))
			}
		}
	}
	// VACUITY GUARD: a scan that found no files proves nothing.
	if scanned < 5 {
		t.Fatalf("scanned only %d non-test files — the walk is not looking where it thinks", scanned)
	}
}

// AND THE TUI MUST ACTUALLY USE IT — "not calling planecontrol" is also satisfied by not having the
// feature at all.
func TestPlaneActionsGoThroughPlanebind(t *testing.T) {
	src, err := os.ReadFile("plane_lifecycle.go")
	if err != nil {
		t.Fatalf("read plane_lifecycle.go: %v", err)
	}
	if !strings.Contains(string(src), "planebind.Service{") {
		t.Error("the TUI's plane actions do not construct a planebind.Service, so they are either " +
			"bypassing the shared contract or the verb panel has been removed")
	}
}
