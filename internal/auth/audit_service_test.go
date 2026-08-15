package auth

// Service-level audit tests (design §5 item 3): auth RPC mutations
// (api-key lifecycle, identity CRUD, role assignment, tenant creation)
// write exactly one audit_events row in the same tx; List* read RPCs write
// zero; the api_key.created snapshot never carries KeyHash/KeyPrefix (D7).
// Skipped unless ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/auth/ -run TestAuditService -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func auditTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed audit test")
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
	return pool
}

// auditServiceEnv returns a service + context carrying tenant + a resolved
// identity (the middleware-equivalent actor). The actor identity is seeded
// so the audit FK resolves. tenantID is caller-chosen so sibling tests
// never share audit rows.
func auditServiceEnv(t *testing.T, tenantID string) (*db.Pool, *Service, context.Context, string, string) {
	t.Helper()
	pool := auditTestPool(t)
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	actor, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, "audit-auth-"+strings.ToLower(db.NewID()), "Audit Actor", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	actx := tenant.WithID(context.Background(), tenantID)
	actx = WithIdentity(actx, ResolvedIdentity{
		IdentityID: actor.ID,
		TenantID:   tenantID,
		Subject:    actor.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	svc := NewService(pool, slog.New(slog.DiscardHandler))
	return pool, svc, actx, tenantID, actor.ID
}

func auditEventCount(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) int {
	t.Helper()
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return len(rows)
}

// TestAuditServiceAuthMutations covers the AC's "role/API-key changes,
// tenant ops" via the real RPC surface and asserts reads write nothing.
func TestAuditServiceAuthMutations(t *testing.T) {
	pool, svc, ctx, tenantID, actorID := auditServiceEnv(t, "tnt_audit_auth_mut")

	// CreateApiKey → 1 api_key.created row; the snapshot must NOT carry
	// the key hash/prefix (D7: secrets never enter the trail).
	apiresp, err := svc.CreateApiKey(ctx, connect.NewRequest(&apiv1.CreateApiKeyRequest{
		IdentityId: actorID,
		Name:       "audit key",
		Scopes:     []string{"workitem:read"},
	}))
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	keyID := apiresp.Msg.ApiKey.Id
	if n := auditEventCount(t, pool, tenantID, "api_key.created", "api_key", keyID); n != 1 {
		t.Fatalf("api_key.created rows = %d, want 1", n)
	}
	keyRows, err := pool.ListAuditEvents(context.Background(), tenantID, "api_key.created", "", "api_key", keyID, "", 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(keyRows) != 1 {
		t.Fatalf("expected 1 api_key.created row, got %d", len(keyRows))
	}
	var after map[string]any
	if err := json.Unmarshal(keyRows[0].After, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	raw := string(keyRows[0].After)
	if strings.Contains(raw, "KeyHash") || strings.Contains(raw, "key_hash") ||
		strings.Contains(raw, "KeyPrefix") || strings.Contains(raw, "key_prefix") ||
		strings.Contains(raw, "$argon2id") {
		t.Fatalf("api_key.created snapshot leaks a secret: %s", raw)
	}
	if after["scopes"] == nil || after["name"] != "audit key" {
		t.Fatalf("api_key.created after = %v, want non-secret projection", after)
	}

	// RevokeApiKey → 1 api_key.revoked row.
	if _, err := svc.RevokeApiKey(ctx, connect.NewRequest(&apiv1.RevokeApiKeyRequest{Id: keyID})); err != nil {
		t.Fatalf("RevokeApiKey: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "api_key.revoked", "api_key", keyID); n != 1 {
		t.Fatalf("api_key.revoked rows = %d, want 1", n)
	}

	// CreateIdentity → 1 identity.created row.
	identResp, err := svc.CreateIdentity(ctx, connect.NewRequest(&apiv1.CreateIdentityRequest{
		IdentityType: "user",
		Subject:      "audit.target." + strings.ToLower(db.NewID()),
		DisplayName:  "Audit Target",
	}))
	if err != nil {
		t.Fatalf("CreateIdentity: %v", err)
	}
	targetID := identResp.Msg.Identity.Id
	if n := auditEventCount(t, pool, tenantID, "identity.created", "identity", targetID); n != 1 {
		t.Fatalf("identity.created rows = %d, want 1", n)
	}

	// CreateRole → 1 role.created row; AssignRole → 1 role_binding.assigned.
	roleResp, err := svc.CreateRole(ctx, connect.NewRequest(&apiv1.CreateRoleRequest{
		Name: "audit-role", Scope: "tenant", Entitlements: []string{"workitem:read"},
	}))
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	roleID := roleResp.Msg.Role.Id
	if n := auditEventCount(t, pool, tenantID, "role.created", "role", roleID); n != 1 {
		t.Fatalf("role.created rows = %d, want 1", n)
	}
	bindResp, err := svc.AssignRole(ctx, connect.NewRequest(&apiv1.AssignRoleRequest{
		IdentityId: targetID,
		RoleId:     roleID,
		Scope:      "tenant",
	}))
	if err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	bindID := bindResp.Msg.Binding.Id
	if n := auditEventCount(t, pool, tenantID, "role_binding.assigned", "role_binding", bindID); n != 1 {
		t.Fatalf("role_binding.assigned rows = %d, want 1", n)
	}
	bindRows, err := pool.ListAuditEvents(context.Background(), tenantID, "role_binding.assigned", "", "role_binding", bindID, "", 10)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(bindRows) != 1 {
		t.Fatalf("role_binding.assigned rows = %d, want 1", len(bindRows))
	}
	var bindAfter map[string]any
	if err := json.Unmarshal(bindRows[0].After, &bindAfter); err != nil {
		t.Fatalf("unmarshal binding after: %v", err)
	}
	if bindAfter["identity_id"] != targetID || bindAfter["role_id"] != roleID {
		t.Fatalf("role_binding.assigned after = %v, want identity+role refs", bindAfter)
	}

	// Read RPCs write nothing (ListTenants / ListRoles).
	if _, err := svc.ListRoles(ctx, connect.NewRequest(&apiv1.ListRolesRequest{})); err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "role.created", "role", roleID); n != 1 {
		t.Fatalf("ListRoles wrote audit rows: role.created rows = %d, want still 1", n)
	}
	if _, err := svc.ListTenants(ctx, connect.NewRequest(&apiv1.ListTenantsRequest{})); err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if n := auditEventCount(t, pool, tenantID, "role.created", "role", roleID); n != 1 {
		t.Fatalf("ListTenants wrote audit rows: role.created rows = %d, want still 1", n)
	}
}

// TestAuditServiceCreateTenant covers the tenant op (AC: "tenant ops")
// and pins the actor-scoped row: the audit row lands in the ACTOR's tenant.
func TestAuditServiceCreateTenant(t *testing.T) {
	pool, svc, ctx, tenantID, _ := auditServiceEnv(t, "tnt_audit_auth_tnt")
	slug := "audit-tenant-" + strings.ToLower(db.NewID())

	resp, err := svc.CreateTenant(ctx, connect.NewRequest(&apiv1.CreateTenantRequest{
		Slug: slug,
		Name: "Audit Tenant",
	}))
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	tntID := resp.Msg.Tenant.Id
	if n := auditEventCount(t, pool, tenantID, "tenant.created", "tenant", tntID); n != 1 {
		t.Fatalf("tenant.created rows = %d, want 1 in the actor's tenant", n)
	}
}
