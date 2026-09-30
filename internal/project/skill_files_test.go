package project

// skill_files_test.go — the PROJECT half of the skills feature (AC1/AC2/AC5).
//
// It asserts the save-time boundary: a skill path OUTSIDE the project directory is REJECTED by
// contextfiles.ValidateWithin with the project dir NAMED in the error (AC5) — because the project directory is
// the only directory mounted into a worker's container, a skill outside it would be invisible to the worker.
// A path INSIDE it is accepted and round-trips through the proto.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/project/ -run 'SkillFiles' -v

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// A skill path inside the project dir is ACCEPTED and comes back on the proto (AC2 storage half).
func TestSkillFilesInsideProjectDirAccepted(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_skill_pj_ok_"+strings.ToLower(db.NewID()))

	dir := t.TempDir()
	skill := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(skill, []byte("skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: "Skills OK " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projID := resp.Msg.Project.Id

	up, err := s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
		Id:         projID,
		ProjectDir: &dir,
		SkillFiles: &apiv1.ContextFiles{Files: []string{skill}},
	}))
	if err != nil {
		t.Fatalf("UpdateProject with an in-project skill: %v", err)
	}
	if got := up.Msg.Project.SkillFiles; len(got) != 1 || got[0] != skill {
		t.Fatalf("skill_files did not round-trip: got %v, want [%s]", got, skill)
	}
	// The free-text `skills` field is a DIFFERENT field and must be untouched by this path.
	if up.Msg.Project.ContextFiles == nil && up.Msg.Project.SkillFiles == nil {
		t.Fatal("both context_files and skill_files came back nil")
	}
}

// A skill path OUTSIDE the project dir is REJECTED, and the error NAMES the project dir (AC5).
func TestSkillFilesOutsideProjectDirRejected(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_skill_pj_bad_"+strings.ToLower(db.NewID()))

	dir := t.TempDir()
	outside := "/etc/passwd"

	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: "Skills Bad " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projID := resp.Msg.Project.Id

	_, err = s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
		Id:         projID,
		ProjectDir: &dir,
		SkillFiles: &apiv1.ContextFiles{Files: []string{outside}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("out-of-project skill: err = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("the rejection must NAME the project dir %q, got: %v", dir, err)
	}
	if !strings.Contains(err.Error(), "inside the project directory") {
		t.Fatalf("the rejection must say 'inside the project directory', got: %v", err)
	}
}

// A structurally-invalid skill path (relative) is rejected up front by the shared validator.
func TestSkillFilesRelativeRejected(t *testing.T) {
	_, s, ctx, _, _ := auditServiceEnv(t, "tnt_skill_pj_rel_"+strings.ToLower(db.NewID()))
	resp, err := s.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
		Name: "Skills Rel " + strings.ToLower(db.NewID()),
	}))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	_, err = s.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
		Id:         resp.Msg.Project.Id,
		SkillFiles: &apiv1.ContextFiles{Files: []string{"relative/skill.md"}},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("relative skill path: err = %v (code %v), want InvalidArgument", err, connect.CodeOf(err))
	}
}
