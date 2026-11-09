package db

import "testing"

// TestEffectiveExecutionModeValue pins the precedence decision at every level of the chain: ITEM ->
// PROJECT -> tenant default.
func TestEffectiveExecutionModeValue(t *testing.T) {
	cases := []struct {
		name    string
		item    string
		project string
		want    string
	}{
		{"item local overrides runtime project", ExecutionModeLocal, ExecutionModeRuntime, ExecutionModeLocal},
		{"item runtime overrides local project", ExecutionModeRuntime, ExecutionModeLocal, ExecutionModeRuntime},
		{"inherit local project", "", ExecutionModeLocal, ExecutionModeLocal},
		{"inherit runtime project", "", ExecutionModeRuntime, ExecutionModeRuntime},
		{"unreadable project degrades to tenant default", "", "", DefaultExecutionMode},
		{"item local wins with no project at all", ExecutionModeLocal, "", ExecutionModeLocal},
		{"unrecognised item value falls through to project", "bogus", ExecutionModeLocal, ExecutionModeLocal},
		{"whitespace-only item value means inherit", "   ", ExecutionModeRuntime, ExecutionModeRuntime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveExecutionModeValue(tc.item, tc.project); got != tc.want {
				t.Fatalf("effectiveExecutionModeValue(%q, %q) = %q, want %q", tc.item, tc.project, got, tc.want)
			}
		})
	}
}

// TestDefaultExecutionModeIsTheLastLeg pins the tenant default and that it is what an execution
// with neither an item nor a readable project resolves to.
func TestDefaultExecutionModeIsTheLastLeg(t *testing.T) {
	if DefaultExecutionMode != ExecutionModeLocal {
		t.Fatalf("DefaultExecutionMode = %q, want %q (containers are opt-in)", DefaultExecutionMode, ExecutionModeLocal)
	}
	if got := effectiveExecutionModeValue("", ""); got != DefaultExecutionMode {
		t.Fatalf("no item, no project: got %q, want the tenant default %q", got, DefaultExecutionMode)
	}
}

// TestEphemeralExecutionModeIsLocal pins the Quick Work decision (local, and therefore
// engine-less-safe) so an accidental change to it is a test failure rather than a LOUD runtime
// failure.
func TestEphemeralExecutionModeIsLocal(t *testing.T) {
	if got := EphemeralExecutionMode(); got != ExecutionModeLocal {
		t.Fatalf("EphemeralExecutionMode() = %q, want local", got)
	}
}
