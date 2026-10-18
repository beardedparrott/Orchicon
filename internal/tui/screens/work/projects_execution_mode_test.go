package work

// The TUI create form's execution-mode default must equal the server's: local.
// A TUI-created project that sent RUNTIME explicitly would contradict the new
// server default — the "shown one value, given another" defect (AC4).

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func TestExecutionModeFieldDefaultsLocal(t *testing.T) {
	if got := executionModeField(nil); got != "local" {
		t.Fatalf("executionModeField(nil) = %q, want local (the create default)", got)
	}
	// A project whose mode is unreadable (UNSPECIFIED) also reads as local.
	if got := executionModeField(&apiv1.Project{}); got != "local" {
		t.Fatalf("executionModeField(empty project) = %q, want local", got)
	}
	// A stored mode is still shown verbatim — runtime is opt-in, not hidden.
	if got := executionModeField(&apiv1.Project{ExecutionMode: apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME}); got != "runtime" {
		t.Fatalf("executionModeField(runtime) = %q, want runtime", got)
	}
	if got := executionModeField(&apiv1.Project{ExecutionMode: apiv1.ExecutionMode_EXECUTION_MODE_LOCAL}); got != "local" {
		t.Fatalf("executionModeField(local) = %q, want local", got)
	}
}

// The option list labels local as the default, matching the GUI's select.
func TestExecutionModeOptionsLabelLocalAsDefault(t *testing.T) {
	opts := executionModeOptions()
	if len(opts) != 2 {
		t.Fatalf("len(options) = %d, want 2", len(opts))
	}
	byValue := map[string]string{}
	for _, o := range opts {
		byValue[o.Value] = o.Label
	}
	if got := byValue["local"]; got != "local — in-process (default)" {
		t.Fatalf("local label = %q, want it labelled as the default", got)
	}
	if got := byValue["runtime"]; got != "runtime — per-run container (opt-in)" {
		t.Fatalf("runtime label = %q, want it labelled as opt-in", got)
	}
}
