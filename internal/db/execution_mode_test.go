package db

import "testing"

// TestEffectiveExecutionModeValue pins the precedence decision at every level of
// the chain: ITEM -> PROJECT -> tenant default. It is a pure table test (no
// database) precisely because this function is what every dispatch site
// ultimately agrees on.
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

// TestDefaultExecutionModeIsRuntime documents the current tenant default. When
// the local-default task flips DefaultExecutionMode this assertion is the ONE
// place that must change with it.
func TestDefaultExecutionModeIsRuntime(t *testing.T) {
	if DefaultExecutionMode != ExecutionModeRuntime {
		t.Fatalf("DefaultExecutionMode = %q, want %q", DefaultExecutionMode, ExecutionModeRuntime)
	}
	if got := effectiveExecutionModeValue("", ""); got != ExecutionModeRuntime {
		t.Fatalf("no item, no project: got %q, want runtime (a read failure must never degrade to local)", got)
	}
}

// TestEphemeralExecutionModeIsLocal pins the Quick Work decision (local, and
// therefore engine-less-safe) so an accidental change to it is a test failure
// rather than a LOUD runtime failure in an invisible item.
func TestEphemeralExecutionModeIsLocal(t *testing.T) {
	if got := EphemeralExecutionMode(); got != ExecutionModeLocal {
		t.Fatalf("EphemeralExecutionMode() = %q, want local", got)
	}
}
