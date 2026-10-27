package scheduler

// arm_runtime_columns_test.go — A LOCAL RUN MUST NOT CLAIM A CONTAINER.
//
// The operator's report, from a Windows run that failed to start:
//
//	ExecutionMode (project) : "local"
//	RuntimeImage  (run)     : "ghcr.io/beardedparrott/orchicon-runtime:latest"
//	RuntimeReady  (run)     : true
//
// with all three tenant images at Status: absent. RuntimeReady: true asserted readiness of an image that did
// not exist, and the contradiction sent the diagnosis toward a container theory for several runs — while the
// real question was whether the project's directory was reachable at all.
//
// The rule these tests pin: what a run CARRIES is what it USES. A local run uses no container, so it names no
// image and asserts no readiness.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestLocalRunRecordsNoImageAndNoReadiness is the reported case, exactly.
func TestLocalRunRecordsNoImageAndNoReadiness(t *testing.T) {
	// A local run in a project whose default image RESOLVES perfectly well. Resolution succeeding is precisely
	// what used to leak onto the row: the value was correct as an INHERITED default and wrong as this run's
	// image.
	image, ready := armRuntimeColumns(true, "ghcr.io/beardedparrott/orchicon-runtime:latest", true, true)

	if image != "" {
		t.Errorf("local run recorded RuntimeImage = %q, want empty — it never starts a container, so naming an "+
			"image it will not use is a claim that misdirects diagnosis", image)
	}
	if ready != nil {
		t.Errorf("local run asserted RuntimeReady = %v, want the column UNTOUCHED — the readiness gate has one "+
			"owner (the pass's gate-clearing block), and a container that will never exist has no readiness "+
			"to assert", *ready)
	}
}

// TestLocalRunIgnoresEveryRuntimeInput guards against the fix depending on one combination: whatever the
// project resolves to, and whatever the daemon reports, a local run records nothing.
func TestLocalRunIgnoresEveryRuntimeInput(t *testing.T) {
	for _, resolved := range []string{"", "custom/image:1", "ghcr.io/beardedparrott/orchicon-runtime:latest"} {
		for _, needsServe := range []bool{true, false} {
			for _, enabled := range []bool{true, false} {
				image, ready := armRuntimeColumns(true, resolved, needsServe, enabled)
				if image != "" || ready != nil {
					t.Errorf("local run with resolved=%q needsServe=%v runtimeEnabled=%v recorded (%q, %v) — "+
						"want (\"\", nil) in every combination", resolved, needsServe, enabled, image, ready)
				}
			}
		}
	}
}

// TestRuntimeRunStillRecordsItsImage is the regression guard: the runtime path is unchanged by this fix.
func TestRuntimeRunStillRecordsItsImage(t *testing.T) {
	image, ready := armRuntimeColumns(false, "custom/image:1", false, true)
	if image != "custom/image:1" {
		t.Errorf("runtime run recorded image %q, want its resolved image", image)
	}
	if ready == nil || !*ready {
		t.Fatalf("a serve-less runtime run must arm ready (it still gets a container, but waits on no serve); "+
			"got %v", ready)
	}

	// A serve-NEEDING run arms NOT ready: it must wait for the serve probe.
	if _, ready := armRuntimeColumns(false, "custom/image:1", true, true); ready == nil || *ready {
		t.Errorf("a serve-needing runtime run must arm NOT ready; got %v", ready)
	}

	// Unresolvable image falls back to the base image rather than an empty string — the always-container
	// contract for runtime runs.
	if image, _ := armRuntimeColumns(false, "", false, true); image != db.RuntimeBaseImage() {
		t.Errorf("runtime run with no resolved image recorded %q, want the base image", image)
	}
}
