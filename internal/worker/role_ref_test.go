package worker

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// Service-level tests for the worker header role binding (role_ref): the
// binding round-trips through create/get/list, unknown roles are rejected
// at the API boundary, and — because the binding lives on the header (not
// the version) — it is editable on published workers, unlike
// name/description/purpose which stay draft-only. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database (same convention as
// the other DB-backed worker tests).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/worker/ -run 'Test.*RoleRef' -v

// createTestRole creates a tenant-scoped role for the test tenant.
func createTestRole(t *testing.T, pool *db.Pool, tenantID string) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	role, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{
		ID:           db.NewID(),
		TenantID:     tenantID,
		Name:         "test-role",
		Scope:        "tenant",
		Entitlements: []string{"workitem:read"},
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return role.ID
}

func workerRoleRef(t *testing.T, ctx context.Context, s *Service, id string) string {
	t.Helper()
	resp, err := s.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	return resp.Msg.Worker.RoleRef
}

// TestCreateWorkerRoleRefRoundTrip: a worker created with a role binding
// carries it on the header and Get/List reflect it.
func TestCreateWorkerRoleRefRoundTrip(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	roleID := createTestRole(t, pool, tenantID)

	resp, err := s.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
		Name:       "role-bound",
		ModelRef:   "opencode/deepseek-v4-flash",
		RoleRef:    roleID,
	}))
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	if got := resp.Msg.Worker.RoleRef; got != roleID {
		t.Fatalf("created worker role_ref = %q, want %q", got, roleID)
	}
	if got := workerRoleRef(t, ctx, s, resp.Msg.Worker.Id); got != roleID {
		t.Errorf("GetWorker role_ref = %q, want %q", got, roleID)
	}
	list, err := s.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	for _, it := range list.Msg.Items {
		if it.Worker.Id == resp.Msg.Worker.Id && it.Worker.RoleRef != roleID {
			t.Errorf("ListWorkers role_ref = %q, want %q", it.Worker.RoleRef, roleID)
		}
	}
}

// TestCreateWorkerUnknownRoleRejected: a role_ref that does not exist in
// the tenant is rejected at the boundary (invalid argument).
func TestCreateWorkerUnknownRoleRejected(t *testing.T) {
	_, s, ctx, _ := bulkEnv(t)
	_, err := s.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
		Name:       "bad-role",
		ModelRef:   "opencode/deepseek-v4-flash",
		RoleRef:    "r_does_not_exist",
	}))
	if err == nil {
		t.Fatal("CreateWorker with unknown role_ref: want error")
	}
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want invalid_argument", code)
	}
}

// TestUpdateWorkerRoleRefOnPublished: the role binding is editable on
// published workers (it lives on the header, not the version) — bind and
// clear both work.
func TestUpdateWorkerRoleRefOnPublished(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	roleID := createTestRole(t, pool, tenantID)
	id := createPublishedWorker(t, ctx, s, "pub-role")

	if _, err := s.UpdateWorker(ctx, connect.NewRequest(&apiv1.UpdateWorkerRequest{Id: id, RoleRef: &roleID})); err != nil {
		t.Fatalf("bind role on published: %v", err)
	}
	if got := workerRoleRef(t, ctx, s, id); got != roleID {
		t.Errorf("published role_ref = %q, want %q", got, roleID)
	}
	empty := ""
	if _, err := s.UpdateWorker(ctx, connect.NewRequest(&apiv1.UpdateWorkerRequest{Id: id, RoleRef: &empty})); err != nil {
		t.Fatalf("clear role on published: %v", err)
	}
	if got := workerRoleRef(t, ctx, s, id); got != "" {
		t.Errorf("cleared role_ref = %q, want empty", got)
	}
}

// TestUpdateWorkerPublishedMixedFieldsRejected: a published worker can
// change its role binding only — mixing in draft-only header fields is
// rejected.
func TestUpdateWorkerPublishedMixedFieldsRejected(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	roleID := createTestRole(t, pool, tenantID)
	id := createPublishedWorker(t, ctx, s, "pub-mixed")
	_, err := s.UpdateWorker(ctx, connect.NewRequest(&apiv1.UpdateWorkerRequest{Id: id, Name: "renamed", RoleRef: &roleID}))
	if err == nil {
		t.Fatal("mixed update on published worker: want error")
	}
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want invalid_argument", code)
	}
}

// TestUpdateWorkerRoleRefOnDraft: draft workers accept the role binding
// alongside the other header fields.
func TestUpdateWorkerRoleRefOnDraft(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	roleID := createTestRole(t, pool, tenantID)
	id := createDraftWorker(t, ctx, s, "draft-role")
	if _, err := s.UpdateWorker(ctx, connect.NewRequest(&apiv1.UpdateWorkerRequest{Id: id, Name: "renamed", RoleRef: &roleID})); err != nil {
		t.Fatalf("bind role on draft: %v", err)
	}
	if got := workerRoleRef(t, ctx, s, id); got != roleID {
		t.Errorf("draft role_ref = %q, want %q", got, roleID)
	}
}

// TestUpdateWorkerUnknownRoleRejected: binding to a role that does not
// exist in the tenant is rejected.
func TestUpdateWorkerUnknownRoleRejected(t *testing.T) {
	_, s, ctx, _ := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "bad-role-upd")
	bad := "r_does_not_exist"
	_, err := s.UpdateWorker(ctx, connect.NewRequest(&apiv1.UpdateWorkerRequest{Id: id, RoleRef: &bad}))
	if err == nil {
		t.Fatal("UpdateWorker with unknown role_ref: want error")
	}
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want invalid_argument", code)
	}
}
