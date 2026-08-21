package auth

import (
	"context"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"log/slog"
)

// newRoleCRUDService opens a migration-applied test pool and constructs
// the AuthService over it, mirroring newIdentityCRUDService. Each test
// picks a unique tenantID so sibling tests never share role rows.
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/auth/ -run TestRoleCRUD -v
func newRoleCRUDService(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed role CRUD test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	svc := NewService(pool, slog.New(slog.DiscardHandler))
	return pool, svc, tenant.WithID(ctx, tenantID)
}

func mustCreateRole(t *testing.T, svc *Service, ctx context.Context, name string, ents []string) *apiv1.Role {
	t.Helper()
	resp, err := svc.CreateRole(ctx, connect.NewRequest(&apiv1.CreateRoleRequest{
		Name: name, Scope: "tenant", Entitlements: ents,
	}))
	if err != nil {
		t.Fatalf("CreateRole(%s): %v", name, err)
	}
	return resp.Msg.Role
}

func TestRoleCRUDUpdate(t *testing.T) {
	_, svc, ctx := newRoleCRUDService(t, "tnt_role_update")
	role := mustCreateRole(t, svc, ctx, "viewer", []string{"project:read"})

	// Rename + re-entitle in one call.
	resp, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id:           role.Id,
		Name:         pointerTo("reviewer"),
		Entitlements: &apiv1.EntitlementSet{Values: []string{"workitem:read", "workitem:write"}},
	}))
	if err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	got := resp.Msg.Role
	if got.Name != "reviewer" {
		t.Fatalf("updated name = %q, want reviewer", got.Name)
	}
	if len(got.Entitlements) != 2 || got.Entitlements[0] != "workitem:read" {
		t.Fatalf("updated entitlements = %v, want [workitem:read workitem:write]", got.Entitlements)
	}
	if got.Version <= role.Version {
		t.Fatalf("version not bumped: %d -> %d", role.Version, got.Version)
	}

	// name-only update leaves entitlements intact.
	resp2, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: role.Id, Name: pointerStr("approver"),
	}))
	if err != nil {
		t.Fatalf("UpdateRole (name only): %v", err)
	}
	got2 := resp2.Msg.Role
	if got2.Name != "approver" {
		t.Fatalf("name-only update name = %q, want approver", got2.Name)
	}
	if len(got2.Entitlements) != 2 {
		t.Fatalf("name-only update clobbered entitlements: %v", got2.Entitlements)
	}

	// empty (no fields) → InvalidArgument.
	if _, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{Id: role.Id})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty update code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	// malformed entitlement (missing ':') rejected by the tightened
	// validator. A well-formed resource:action string passes.
	if _, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: role.Id, Entitlements: &apiv1.EntitlementSet{Values: []string{"nocolon"}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed entitlement code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	if resp3, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: role.Id, Entitlements: &apiv1.EntitlementSet{Values: []string{"project:read"}},
	})); err != nil {
		t.Fatalf("well-formed resource:action rejected: %v", err)
	} else if len(resp3.Msg.Role.Entitlements) != 1 || resp3.Msg.Role.Entitlements[0] != "project:read" {
		t.Fatalf("well-formed update entitlements = %v, want [project:read]", resp3.Msg.Role.Entitlements)
	}

	// clearing entitlements to zero via an empty EntitlementSet is honored
	// (distinct from an absent field).
	if resp4, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: role.Id, Entitlements: &apiv1.EntitlementSet{},
	})); err != nil {
		t.Fatalf("clear entitlements: %v", err)
	} else if len(resp4.Msg.Role.Entitlements) != 0 {
		t.Fatalf("cleared entitlements = %v, want []", resp4.Msg.Role.Entitlements)
	}

	// optimistic concurrency: wrong version → NotFound.
	if _, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: role.Id, Name: pointerStr("x"), Version: pointerTo(int32(1)),
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("version-mismatch code = %v, want NotFound", connect.CodeOf(err))
	}

	// nonexistent role → NotFound.
	if _, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: "does-not-exist", Name: pointerStr("y"),
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing role code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestRoleCRUDAdminRoleImmutable(t *testing.T) {
	_, svc, ctx := newRoleCRUDService(t, "tnt_role_admin")
	admin := mustCreateRole(t, svc, ctx, "admin", []string{"*"})

	// Renaming the admin role would break the name=="admin" tenant-admin
	// bypass, so it is rejected.
	if _, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: admin.Id, Name: pointerStr("superuser"),
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("edit admin name code = %v, want InvalidArgument", connect.CodeOf(err))
	}
	// Re-entitling the admin role could weaken the bypass.
	if _, err := svc.UpdateRole(ctx, connect.NewRequest(&apiv1.UpdateRoleRequest{
		Id: admin.Id, Entitlements: &apiv1.EntitlementSet{Values: []string{"workitem:read"}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("edit admin entitlements code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	// Delete is also guarded (already covered by TestRoleCRUDDelete); the
	// role row is unchanged.
	if _, err := svc.DeleteRole(ctx, connect.NewRequest(&apiv1.DeleteRoleRequest{Id: admin.Id})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("delete admin code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestRoleCRUDDelete(t *testing.T) {
	pool, svc, ctx := newRoleCRUDService(t, "tnt_role_del")

	// Create an admin role and a normal role.
	admin := mustCreateRole(t, svc, ctx, "admin", []string{"*"})
	role := mustCreateRole(t, svc, ctx, "viewer", []string{"project:read"})

	// Bind the viewer role to a real identity so we can assert the
	// bindings are cleaned up.
	identResp, err := svc.CreateIdentity(ctx, connect.NewRequest(&apiv1.CreateIdentityRequest{
		IdentityType: "user", Subject: "role-del-" + strings.ToLower(db.NewID()), DisplayName: "Role Del User",
	}))
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	identID := identResp.Msg.Identity.Id
	bindResp, err := svc.AssignRole(ctx, connect.NewRequest(&apiv1.AssignRoleRequest{
		IdentityId: identID, RoleId: role.Id, Scope: "tenant",
	}))
	if err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	bindID := bindResp.Msg.Binding.Id

	// admin role cannot be deleted.
	if _, err := svc.DeleteRole(ctx, connect.NewRequest(&apiv1.DeleteRoleRequest{Id: admin.Id})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("delete admin code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	// Deleting the viewer role removes its bindings too.
	if _, err := svc.DeleteRole(ctx, connect.NewRequest(&apiv1.DeleteRoleRequest{Id: role.Id})); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}

	// Role gone → NotFound on re-delete.
	if _, err := svc.DeleteRole(ctx, connect.NewRequest(&apiv1.DeleteRoleRequest{Id: role.Id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("re-delete code = %v, want NotFound", connect.CodeOf(err))
	}

	// Binding cleaned up: ListRoleBindings for the identity no longer
	// returns the deleted role's binding.
	bindings, err := svc.ListRoleBindings(ctx, connect.NewRequest(&apiv1.ListRoleBindingsRequest{
		IdentityId: identID,
	}))
	if err != nil {
		t.Fatalf("ListRoleBindings: %v", err)
	}
	for _, b := range bindings.Msg.Bindings {
		if b.Id == bindID {
			t.Fatalf("binding %s survived role deletion", bindID)
		}
	}
	_ = pool
}

func TestValidateEntitlementsFormat(t *testing.T) {
	valid := []string{
		"*",
		"*:write",
		"project:*",
		"workitem:read",
		"policy:supersede",
		"aigateway:read",
		"runtimeimage:write",
	}
	for _, e := range valid {
		got, err := validateEntitlements([]string{e})
		if err != nil {
			t.Fatalf("validateEntitlements(%q) rejected valid: %v", e, err)
		}
		if len(got) != 1 || got[0] != e {
			t.Fatalf("validateEntitlements(%q) = %v, want [%q]", e, got, e)
		}
	}

	invalid := []string{
		"nocolon",              // missing ':'
		":write",               // empty resource
		"project:",             // empty action
		"foo bar:baz",          // whitespace in resource
		"project:create,write", // comma not permitted here
	}
	for _, e := range invalid {
		if _, err := validateEntitlements([]string{e}); err == nil {
			t.Fatalf("validateEntitlements(%q) accepted invalid", e)
		}
	}

	// empty list → empty, nil error
	if got, err := validateEntitlements(nil); err != nil || len(got) != 0 {
		t.Fatalf("validateEntitlements(nil) = %v, %v", got, err)
	}
	// blank entries are skipped, not rejected
	if got, err := validateEntitlements([]string{" ", "workitem:read"}); err != nil || len(got) != 1 {
		t.Fatalf("validateEntitlements(blank) = %v, %v", got, err)
	}
}

func pointerStr(v string) *string { return &v }
