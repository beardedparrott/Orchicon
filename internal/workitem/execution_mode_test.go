package workitem

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// TestExecutionModeFromProto pins the wire -> row mapping: EXECUTION_MODE_
// UNSPECIFIED means INHERIT and must stay nil (NULL in the row, never ""), while
// the concrete values become the strings the DB CHECK constraint accepts.
func TestExecutionModeFromProto(t *testing.T) {
	cases := []struct {
		name string
		in   apiv1.ExecutionMode
		want *string
	}{
		{"unspecified means inherit", apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED, nil},
		{"runtime", apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME, strPtrForTest(db.ExecutionModeRuntime)},
		{"local", apiv1.ExecutionMode_EXECUTION_MODE_LOCAL, strPtrForTest(db.ExecutionModeLocal)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := executionModeFromProto(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("executionModeFromProto(%v) = %q, want nil (inherit)", tc.in, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("executionModeFromProto(%v) = nil, want %q", tc.in, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("executionModeFromProto(%v) = %q, want %q", tc.in, *got, *tc.want)
			}
		})
	}
}

// TestExecutionModeToProto pins the row -> wire mapping, including the rule that
// nil (inherit) reports ok=false so the optional field is left UNSET rather than
// sent as a sentinel — "unset" and "runtime" are different statements.
func TestExecutionModeToProto(t *testing.T) {
	cases := []struct {
		name   string
		in     *string
		want   apiv1.ExecutionMode
		wantOK bool
	}{
		{"nil means inherit, not a value", nil, apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED, false},
		{"runtime", strPtrForTest(db.ExecutionModeRuntime), apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME, true},
		{"local", strPtrForTest(db.ExecutionModeLocal), apiv1.ExecutionMode_EXECUTION_MODE_LOCAL, true},
		{"padded local still maps", strPtrForTest("  local  "), apiv1.ExecutionMode_EXECUTION_MODE_LOCAL, true},
		{"unrecognised value is not an override", strPtrForTest("bogus"), apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := executionModeToProto(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("executionModeToProto(%v) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestExecutionModeEnumFor pins the resolved-mode mapping behind the read-only
// effective_execution_mode field: anything that is not explicitly local reads as
// runtime, matching db.EffectiveExecutionMode's fail-open contract.
func TestExecutionModeEnumFor(t *testing.T) {
	if got := executionModeEnumFor(db.ExecutionModeLocal); got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Errorf("local = %v, want EXECUTION_MODE_LOCAL", got)
	}
	if got := executionModeEnumFor(db.ExecutionModeRuntime); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Errorf("runtime = %v, want EXECUTION_MODE_RUNTIME", got)
	}
	if got := executionModeEnumFor(""); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Errorf("empty = %v, want EXECUTION_MODE_RUNTIME (a resolved mode is never unset)", got)
	}
	if got := executionModeEnumFor("bogus"); got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Errorf("bogus = %v, want EXECUTION_MODE_RUNTIME (fail-open toward the container)", got)
	}
}

// TestValidateExecutionMode pins the ONE validation rule shared by the Connect
// API and the askorchicon MCP tools: "" = inherit, runtime|local = the concrete
// modes, anything else is rejected (so a typo can never become a silent third
// state).
func TestValidateExecutionMode(t *testing.T) {
	valid := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{db.ExecutionModeRuntime, db.ExecutionModeRuntime},
		{db.ExecutionModeLocal, db.ExecutionModeLocal},
		{" local ", db.ExecutionModeLocal},
	}
	for _, tc := range valid {
		got, err := ValidateExecutionMode(tc.in)
		if err != nil {
			t.Fatalf("ValidateExecutionMode(%q) error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ValidateExecutionMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"container", "Runtime", "LOCAL", "none"} {
		if _, err := ValidateExecutionMode(bad); err == nil {
			t.Fatalf("ValidateExecutionMode(%q) accepted an invalid mode", bad)
		}
	}
}

func strPtrForTest(s string) *string { return &s }
