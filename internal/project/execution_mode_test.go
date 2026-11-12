package project

// execution_mode_test.go — the execution_mode default and boundary validation at
// the CONNECT API entry point: the surface the web UI and the TUI both call, so a
// project created through either without a mode is local (AC1, AC3, AC6, AC7).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/project/ -run TestProjectExecutionMode -v

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// An omitted mode defaults to local.
func TestProjectExecutionModeCreateDefaultsLocal(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_mode_def_"+strings.ToLower(db.NewID()))

	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: "Mode Default " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if got := resp.Msg.Project.ExecutionMode; got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("omitted mode → %v, want EXECUTION_MODE_LOCAL", got)
	}
}

// An explicit UNSPECIFIED (what a client that sends the zero value sends) also
// defaults to local — creation treats it as "no mode", not as an error.
func TestProjectExecutionModeCreateUnspecifiedDefaultsLocal(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_mode_uns_"+strings.ToLower(db.NewID()))

	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name:          "Mode Unspecified " + strings.ToLower(db.NewID()),
		ExecutionMode: apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED,
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if got := resp.Msg.Project.ExecutionMode; got != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("UNSPECIFIED → %v, want EXECUTION_MODE_LOCAL", got)
	}
}

// An explicit runtime is still honoured — containers are opt-in, not removed.
func TestProjectExecutionModeCreateRuntimeHonoured(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_mode_run_"+strings.ToLower(db.NewID()))

	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name:          "Mode Runtime " + strings.ToLower(db.NewID()),
		ExecutionMode: apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME,
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if got := resp.Msg.Project.ExecutionMode; got != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("RUNTIME → %v, want EXECUTION_MODE_RUNTIME", got)
	}
}

// An unknown enum value is rejected at the boundary with the shared message.
func TestProjectExecutionModeCreateUnknownRejected(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_mode_bad_"+strings.ToLower(db.NewID()))

	_, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name:          "Mode Bad " + strings.ToLower(db.NewID()),
		ExecutionMode: apiv1.ExecutionMode(99),
	}))
	if err == nil {
		t.Fatal("CreateProject with enum 99 succeeded, want InvalidArgument")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), db.ExecutionModeInvalidMsg) {
		t.Fatalf("error = %q, want %q", err.Error(), db.ExecutionModeInvalidMsg)
	}
}

// Changing the mode takes effect for SUBSEQUENT runs; an absent field on update
// still means "unchanged", so an existing project is never re-defaulted (AC6, AC7).
func TestProjectExecutionModeUpdate(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_mode_upd_"+strings.ToLower(db.NewID()))

	created, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: "Mode Update " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	id := created.Msg.Project.Id
	if created.Msg.Project.ExecutionMode != apiv1.ExecutionMode_EXECUTION_MODE_LOCAL {
		t.Fatalf("new project mode = %v, want LOCAL", created.Msg.Project.ExecutionMode)
	}

	rt := apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME
	up, err := s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{Id: id, ExecutionMode: &rt}))
	if err != nil {
		t.Fatalf("UpdateProject(runtime): %v", err)
	}
	if up.Msg.Project.ExecutionMode != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("after update mode = %v, want RUNTIME", up.Msg.Project.ExecutionMode)
	}

	// An update that does NOT mention the mode leaves it alone (AC7).
	title := "renamed " + strings.ToLower(db.NewID())
	up, err = s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{Id: id, Name: &title}))
	if err != nil {
		t.Fatalf("UpdateProject(name only): %v", err)
	}
	if up.Msg.Project.ExecutionMode != apiv1.ExecutionMode_EXECUTION_MODE_RUNTIME {
		t.Fatalf("mode after a name-only update = %v, want RUNTIME (unchanged)", up.Msg.Project.ExecutionMode)
	}

	// A present-but-unknown value is a client bug and is rejected.
	bad := apiv1.ExecutionMode(99)
	_, err = s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{Id: id, ExecutionMode: &bad}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("UpdateProject(99) err = %v, want InvalidArgument", err)
	}
}
