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
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	rows, err := pool.ListAuditEvents(context.Background(), tenantID, action, "", targetType, targetID, "", 100, time.Time{}, time.Time{})
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
	keyRows, err := pool.ListAuditEvents(context.Background(), tenantID, "api_key.created", "", "api_key", keyID, "", 10, time.Time{}, time.Time{})
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
	bindRows, err := pool.ListAuditEvents(context.Background(), tenantID, "role_binding.assigned", "", "role_binding", bindID, "", 10, time.Time{}, time.Time{})
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

// TestAuditServiceTimeRangeFilter covers the ListAuditEvents time window:
// start_time is an inclusive lower bound, end_time an exclusive upper bound
// on occurred_at, and absent bounds select all rows. Rows are written with
// explicit occurred_at timestamps so the window assertions are exact.
func TestAuditServiceTimeRangeFilter(t *testing.T) {
	tenantID := "tnt_audit_tr_" + strings.ToLower(db.NewID())
	pool, svc, ctx, _, actorID := auditServiceEnv(t, tenantID)
	now := time.Now().UTC()

	write := func(targetID string, at time.Time) {
		t.Helper()
		ttx, err := pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer ttx.Rollback(context.Background())
		if err := db.CreateAuditEvent(ctx, ttx.Tx, db.AuditEventRow{
			TenantID:        tenantID,
			ActorIdentityID: actorID,
			ActorType:       "user",
			AuthMethod:      "oidc",
			Action:          "audit.filter.seeded",
			TargetType:      "audit_filter_test",
			TargetID:        targetID,
			Before:          []byte("{}"),
			After:           []byte(`{"t":1}`),
			OccurredAt:      at,
		}); err != nil {
			t.Fatalf("create audit event: %v", err)
		}
		if err := ttx.Commit(context.Background()); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	write("row-early", now.Add(-2*time.Hour))
	write("row-mid", now.Add(-1*time.Hour))
	write("row-late", now.Add(-5*time.Minute))

	list := func(start, end *timestamppb.Timestamp) []*apiv1.AuditEvent {
		t.Helper()
		resp, err := svc.ListAuditEvents(ctx, connect.NewRequest(&apiv1.ListAuditEventsRequest{
			Action:    "audit.filter.seeded",
			StartTime: start,
			EndTime:   end,
		}))
		if err != nil {
			t.Fatalf("ListAuditEvents: %v", err)
		}
		return resp.Msg.Events
	}

	// No bounds → all three rows.
	if got := list(nil, nil); len(got) != 3 {
		t.Fatalf("unbounded list = %d rows, want 3", len(got))
	}

	// [now-90m, now-30m) → only the -1h row.
	got := list(timestamppb.New(now.Add(-90*time.Minute)), timestamppb.New(now.Add(-30*time.Minute)))
	if len(got) != 1 || got[0].TargetId != "row-mid" {
		t.Fatalf("window [now-90m, now-30m) = %d rows (%v), want 1 row-mid", len(got), targetIDs(got))
	}

	// start_time only (>= now-90m) → the -1h and -5m rows.
	got = list(timestamppb.New(now.Add(-90*time.Minute)), nil)
	if len(got) != 2 {
		t.Fatalf("start-only list = %d rows, want 2", len(got))
	}

	// end_time only (< now-30m) → the -2h and -1h rows.
	got = list(nil, timestamppb.New(now.Add(-30*time.Minute)))
	if len(got) != 2 {
		t.Fatalf("end-only list = %d rows, want 2", len(got))
	}

	// Window entirely before the rows → empty.
	if got = list(timestamppb.New(now.Add(-5*time.Hour)), timestamppb.New(now.Add(-4*time.Hour))); len(got) != 0 {
		t.Fatalf("past window = %d rows, want 0", len(got))
	}

	// Inclusive start: row at exactly the start bound is included.
	got = list(timestamppb.New(now.Add(-1*time.Hour)), timestamppb.New(now.Add(-30*time.Minute)))
	if len(got) != 1 || got[0].TargetId != "row-mid" {
		t.Fatalf("inclusive-start window = %d rows (%v), want 1 row-mid", len(got), targetIDs(got))
	}

	// Exclusive end: row at exactly the end bound is excluded.
	if got = list(timestamppb.New(now.Add(-2*time.Hour)), timestamppb.New(now.Add(-1*time.Hour))); len(got) != 1 || got[0].TargetId != "row-early" {
		t.Fatalf("exclusive-end window = %d rows (%v), want 1 row-early", len(got), targetIDs(got))
	}
}

func targetIDs(events []*apiv1.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.TargetId)
	}
	return out
}
