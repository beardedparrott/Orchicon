package worker

// skill_files_test.go — the WORKER VERSION half of the skills storage (AC1/AC3/AC7).
//
// It pins the two things the version-level field must do:
//
//   - skill_files persists on the VERSION (never the worker header) and round-trips through the proto —
//     DISTINCT from the free-text `skills` prompt section, which must be untouched by a skill_files write;
//   - a skill path is validated STRUCTURALLY at save (absolute, no ".."), because a worker version is
//     project-agnostic; the project-containment rule runs at the render boundary (see the scheduler test).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/worker/ -run 'SkillFiles' -v

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// skillFilesOnVersion reads the skill_files jsonb straight from the version row so the assertion is on the
// PERSISTED value, not on a round-tripped proto.
func skillFilesOnVersion(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int) string {
	t.Helper()
	var raw string
	err := pool.QueryRow(context.Background(),
		`SELECT skill_files::text FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver).Scan(&raw)
	if err != nil {
		t.Fatalf("query skill_files for %s v%d: %v", workerID, ver, err)
	}
	return raw
}

// A skill_files write lands on the VERSION row and NOT on the free-text `skills` field (AC1/AC7).
func TestSkillFilesPersistsOnVersionDistinctFromSkillsProse(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "skill-files-distinct")

	vid, _ := latestVersionID(t, pool, tenantID, id)
	skillsProse := "bullet-style skill prose"
	sf := `["/tmp/orchicon-skill-a.md","/tmp/orchicon-skill-b.md"]`
	resp, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:   id,
		VersionId:  vid,
		Skills:     &skillsProse,
		SkillFiles: &sf,
	}))
	if err != nil {
		t.Fatalf("UpdateWorkerVersion: %v", err)
	}
	v := resp.Msg.Version
	if v.Skills != skillsProse {
		t.Fatalf("the free-text `skills` field was disturbed: got %q, want %q", v.Skills, skillsProse)
	}
	if len(v.SkillFiles) != 2 || v.SkillFiles[0] != "/tmp/orchicon-skill-a.md" {
		t.Fatalf("skill_files did not round-trip on the proto: %v", v.SkillFiles)
	}
	// The persisted column, and the fact that it did NOT overwrite the prose column.
	if raw := skillFilesOnVersion(t, pool, tenantID, id, int(v.Version)); raw == "" || raw == "[]" {
		t.Fatalf("skill_files did not persist on the version row: %q", raw)
	}
}

// A structurally-invalid skill path is REJECTED at save (absolute + no ".."), because a worker version has no
// project dir to check containment against.
func TestSkillFilesInvalidPathRejectedOnVersion(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "skill-files-invalid")

	vid, _ := latestVersionID(t, pool, tenantID, id)
	_ = vid
	for _, tc := range []struct {
		name string
		json string
	}{
		{"relative path", `["relative/skill.md"]`},
		{"path traversal", `["/tmp/../etc/passwd"]`},
		{"not an array of strings", `{"a": 1}`},
		{"not JSON", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sf := tc.json
			_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
				WorkerId:   id,
				VersionId:  vid,
				SkillFiles: &sf,
			}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("skill_files %q: err = %v (code %v), want InvalidArgument", tc.json, err, connect.CodeOf(err))
			}
		})
	}
}

// A new version CLONES the source version's skill_files (the field is part of version content).
func TestCreateWorkerVersionClonesSkillFiles(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "skill-files-clone")

	vid, _ := latestVersionID(t, pool, tenantID, id)
	sf := `["/tmp/orchicon-clone-skill.md"]`
	if _, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:   id,
		VersionId:  vid,
		SkillFiles: &sf,
		Republish:  true,
	})); err != nil {
		t.Fatalf("seed skill files on v1: %v", err)
	}
	resp, err := s.CreateWorkerVersion(ctx, connect.NewRequest(&apiv1.CreateWorkerVersionRequest{
		WorkerId: id,
	}))
	if err != nil {
		t.Fatalf("CreateWorkerVersion: %v", err)
	}
	if len(resp.Msg.Version.SkillFiles) != 1 || resp.Msg.Version.SkillFiles[0] != "/tmp/orchicon-clone-skill.md" {
		t.Fatalf("the new version did not clone skill_files: %v", resp.Msg.Version.SkillFiles)
	}
	if raw := skillFilesOnVersion(t, pool, tenantID, id, int(resp.Msg.Version.Version)); raw == "" || raw == "[]" {
		t.Fatalf("cloned skill_files did not persist: %q", raw)
	}
}
