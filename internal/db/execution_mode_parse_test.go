package db_test

// Pure unit tests for the ONE execution-mode default/parser (no DSN needed).
//
// The default is an operator decision: local (in-process, fenced) is what a
// project lands in when the caller sends no mode; containers are opt-in
// (runtime). Every entry point — Ask tools, Connect API, web UI, TUI, db layer
// — routes through these so they cannot drift.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// TestDefaultExecutionModeIsLocal pins the operator's decision.
func TestDefaultExecutionModeIsLocal(t *testing.T) {
	if db.DefaultExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("DefaultExecutionMode = %q, want %q", db.DefaultExecutionMode, db.ExecutionModeLocal)
	}
}

// TestParseExecutionMode covers the create path (unset = DefaultExecutionMode),
// the update path (unset = ""), the two accepted values, and rejection.
func TestParseExecutionMode(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		unset   string
		want    string
		wantErr bool
	}{
		{name: "empty uses the create default", raw: "", unset: db.DefaultExecutionMode, want: db.ExecutionModeLocal},
		{name: "empty stays unset for update", raw: "", unset: "", want: ""},
		{name: "local accepted", raw: "local", unset: db.DefaultExecutionMode, want: db.ExecutionModeLocal},
		{name: "runtime accepted", raw: "runtime", unset: db.DefaultExecutionMode, want: db.ExecutionModeRuntime},
		{name: "whitespace and case tolerated", raw: "  Runtime ", unset: db.DefaultExecutionMode, want: db.ExecutionModeRuntime},
		{name: "unknown rejected on create", raw: "banana", unset: db.DefaultExecutionMode, wantErr: true},
		{name: "unknown rejected on update", raw: "banana", unset: "", wantErr: true},
		{name: "unspecified string rejected", raw: "unspecified", unset: db.DefaultExecutionMode, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.ParseExecutionMode(tc.raw, tc.unset)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseExecutionMode(%q, %q) = %q, want error", tc.raw, tc.unset, got)
				}
				if err.Error() != db.ExecutionModeInvalidMsg {
					t.Fatalf("error = %q, want %q", err.Error(), db.ExecutionModeInvalidMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExecutionMode(%q, %q): %v", tc.raw, tc.unset, err)
			}
			if got != tc.want {
				t.Fatalf("ParseExecutionMode(%q, %q) = %q, want %q", tc.raw, tc.unset, got, tc.want)
			}
		})
	}
}

// TestExecutionModeInvalidMsgNamesBothValues keeps the enum and the message in
// lockstep: if a third mode is ever considered, this fails until both change.
func TestExecutionModeInvalidMsgNamesBothValues(t *testing.T) {
	const want = "execution_mode must be runtime or local"
	if db.ExecutionModeInvalidMsg != want {
		t.Fatalf("ExecutionModeInvalidMsg = %q, want %q", db.ExecutionModeInvalidMsg, want)
	}
}
