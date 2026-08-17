package auth

import (
	"context"
	"os"
	"testing"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"log/slog"
)

// newIdentityCRUDService opens a migration-applied test pool and
// constructs the AuthService over it. The returned context carries the
// tenant so handlers resolve requireTenant(ctx).
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/auth/ -run TestIdentityCRUD -v
func newIdentityCRUDService(t *testing.T) (*db.Pool, *Service, context.Context, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed identity CRUD test")
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
	const tenantID = "tnt_dev"
	svc := NewService(pool, slog.New(slog.DiscardHandler))
	return pool, svc, tenant.WithID(ctx, tenantID), tenantID
}

// callIdentity is a small wrapper that keeps the CRUD assertions terse.
type identityCalls struct {
	create func(*apiv1.CreateIdentityRequest) (*connect.Response[apiv1.CreateIdentityResponse], error)
	update func(*apiv1.UpdateIdentityRequest) (*connect.Response[apiv1.UpdateIdentityResponse], error)
	status func(*apiv1.SetIdentityStatusRequest) (*connect.Response[apiv1.SetIdentityStatusResponse], error)
	del    func(*apiv1.DeleteIdentityRequest) (*connect.Response[apiv1.DeleteIdentityResponse], error)
}

func bindIdentityCalls(svc *Service, ctx context.Context) *identityCalls {
	return &identityCalls{
		create: func(req *apiv1.CreateIdentityRequest) (*connect.Response[apiv1.CreateIdentityResponse], error) {
			return svc.CreateIdentity(ctx, connect.NewRequest(req))
		},
		update: func(req *apiv1.UpdateIdentityRequest) (*connect.Response[apiv1.UpdateIdentityResponse], error) {
			return svc.UpdateIdentity(ctx, connect.NewRequest(req))
		},
		status: func(req *apiv1.SetIdentityStatusRequest) (*connect.Response[apiv1.SetIdentityStatusResponse], error) {
			return svc.SetIdentityStatus(ctx, connect.NewRequest(req))
		},
		del: func(req *apiv1.DeleteIdentityRequest) (*connect.Response[apiv1.DeleteIdentityResponse], error) {
			return svc.DeleteIdentity(ctx, connect.NewRequest(req))
		},
	}
}

func TestIdentityCRUDCreate(t *testing.T) {
	pool, svc, ctx, tenantID := newIdentityCRUDService(t)
	calls := bindIdentityCalls(svc, ctx)

	// A user identity with a local-account-style subject.
	resp, err := calls.create(&apiv1.CreateIdentityRequest{
		IdentityType: "user",
		Subject:      "alice.admin",
		DisplayName:  "Alice Admin",
	})
	if err != nil {
		t.Fatalf("CreateIdentity(user): %v", err)
	}
	u := resp.Msg.Identity
	if u.Id == "" || u.Subject != "alice.admin" || u.IdentityType != "user" || u.Status != "active" || u.Version != 1 {
		t.Fatalf("unexpected user identity: %+v", u)
	}
	t.Cleanup(deleteIdentityRow(t, pool, tenantID, u.Id))

	// A service account with an explicit slug subject.
	resp2, err := calls.create(&apiv1.CreateIdentityRequest{
		IdentityType: "service",
		Subject:      "ci-bot",
		DisplayName:  "CI Bot",
	})
	if err != nil {
		t.Fatalf("CreateIdentity(service explicit): %v", err)
	}
	sa := resp2.Msg.Identity
	if sa.Subject != "ci-bot" || sa.IdentityType != "service" {
		t.Fatalf("unexpected service identity: %+v", sa)
	}
	t.Cleanup(deleteIdentityRow(t, pool, tenantID, sa.Id))

	// A service account with no subject gets a synthetic sa-<ULID> one.
	resp3, err := calls.create(&apiv1.CreateIdentityRequest{
		IdentityType: "service",
		DisplayName:  "Anonymous Bot",
	})
	if err != nil {
		t.Fatalf("CreateIdentity(service synthetic): %v", err)
	}
	syn := resp3.Msg.Identity
	if syn.Subject[:3] != "sa-" || len(syn.Subject) <= 3 {
		t.Fatalf("expected synthetic sa-<ULID> subject, got %q", syn.Subject)
	}
	t.Cleanup(deleteIdentityRow(t, pool, tenantID, syn.Id))

	// Duplicate (tenant, subject) → AlreadyExists.
	if _, err := calls.create(&apiv1.CreateIdentityRequest{
		IdentityType: "user",
		Subject:      "alice.admin",
		DisplayName:  "Duplicate Alice",
	}); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate subject: err = %v, want AlreadyExists", err)
	}

	// Validation rejects bad type / bad subjects / empty display name.
	if _, err := calls.create(&apiv1.CreateIdentityRequest{IdentityType: "robot", Subject: "x", DisplayName: "X"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad identity_type: err = %v, want InvalidArgument", err)
	}
	if _, err := calls.create(&apiv1.CreateIdentityRequest{IdentityType: "user", Subject: "", DisplayName: "X"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty user subject: err = %v, want InvalidArgument", err)
	}
	if _, err := calls.create(&apiv1.CreateIdentityRequest{IdentityType: "user", Subject: "BAD USER", DisplayName: "X"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad user subject charset: err = %v, want InvalidArgument", err)
	}
	if _, err := calls.create(&apiv1.CreateIdentityRequest{IdentityType: "service", Subject: "Bad Subject!", DisplayName: "X"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad service subject charset: err = %v, want InvalidArgument", err)
	}
	if _, err := calls.create(&apiv1.CreateIdentityRequest{IdentityType: "user", Subject: "ok.user", DisplayName: "  "}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("blank display_name: err = %v, want InvalidArgument", err)
	}
}

func TestIdentityCRUDUpdate(t *testing.T) {
	pool, svc, ctx, tenantID := newIdentityCRUDService(t)
	calls := bindIdentityCalls(svc, ctx)

	u := mustCreateIdentity(t, calls, "user", "bob.dev", "Bob Dev")
	t.Cleanup(deleteIdentityRow(t, pool, tenantID, u.Id))

	// Rename bumps the version.
	resp, err := calls.update(&apiv1.UpdateIdentityRequest{Id: u.Id, DisplayName: "Bob Dev (senior)"})
	if err != nil {
		t.Fatalf("UpdateIdentity: %v", err)
	}
	if resp.Msg.Identity.DisplayName != "Bob Dev (senior)" {
		t.Fatalf("display_name = %q, want renamed value", resp.Msg.Identity.DisplayName)
	}
	if resp.Msg.Identity.Version != 2 {
		t.Fatalf("version = %d, want 2 after update", resp.Msg.Identity.Version)
	}

	// Optimistic concurrency: a stale version fails with NotFound.
	if _, err := calls.update(&apiv1.UpdateIdentityRequest{Id: u.Id, DisplayName: "Stale Writer", Version: pointerTo[int32](1)}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("stale version: err = %v, want NotFound", err)
	}

	// Unknown identity → NotFound.
	if _, err := calls.update(&apiv1.UpdateIdentityRequest{Id: "missing-id", DisplayName: "X"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown identity: err = %v, want NotFound", err)
	}

	// Blank display_name is rejected before the DB.
	if _, err := calls.update(&apiv1.UpdateIdentityRequest{Id: u.Id, DisplayName: " "}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("blank display_name: err = %v, want InvalidArgument", err)
	}
}

func TestIdentityCRUDStatus(t *testing.T) {
	pool, svc, ctx, tenantID := newIdentityCRUDService(t)
	calls := bindIdentityCalls(svc, ctx)

	u := mustCreateIdentity(t, calls, "service", "status-bot", "Status Bot")
	t.Cleanup(deleteIdentityRow(t, pool, tenantID, u.Id))

	// Disable.
	resp, err := calls.status(&apiv1.SetIdentityStatusRequest{Id: u.Id, Status: "disabled"})
	if err != nil {
		t.Fatalf("SetIdentityStatus(disabled): %v", err)
	}
	if resp.Msg.Identity.Status != "disabled" {
		t.Fatalf("status = %q, want disabled", resp.Msg.Identity.Status)
	}
	if resp.Msg.Identity.Version != 2 {
		t.Fatalf("version = %d, want 2 after status change", resp.Msg.Identity.Version)
	}

	// Enable (reversible).
	resp2, err := calls.status(&apiv1.SetIdentityStatusRequest{Id: u.Id, Status: "active"})
	if err != nil {
		t.Fatalf("SetIdentityStatus(active): %v", err)
	}
	if resp2.Msg.Identity.Status != "active" {
		t.Fatalf("status = %q, want active after re-enable", resp2.Msg.Identity.Status)
	}

	// Invalid status value is rejected server-side (column is unconstrained text).
	if _, err := calls.status(&apiv1.SetIdentityStatusRequest{Id: u.Id, Status: "deleted"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid status: err = %v, want InvalidArgument", err)
	}

	// Unknown identity → NotFound.
	if _, err := calls.status(&apiv1.SetIdentityStatusRequest{Id: "missing-id", Status: "disabled"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown identity: err = %v, want NotFound", err)
	}
}

func TestIdentityCRUDDelete(t *testing.T) {
	pool, svc, ctx, tenantID := newIdentityCRUDService(t)
	calls := bindIdentityCalls(svc, ctx)

	// Guard A: the caller cannot delete their own identity.
	me := mustCreateIdentity(t, calls, "user", "self.admin", "Self Admin")
	t.Cleanup(deleteIdentityRow(t, pool, tenantID, me.Id))
	selfCtx := tenant.WithID(context.Background(), tenantID)
	selfCtx = WithIdentity(selfCtx, ResolvedIdentity{IdentityID: me.Id, TenantID: tenantID, IsAdmin: true})
	selfCalls := bindIdentityCalls(svc, selfCtx)
	if _, err := selfCalls.del(&apiv1.DeleteIdentityRequest{Id: me.Id}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("self-delete: err = %v, want InvalidArgument", err)
	}

	// Guard B: an active identity must be disabled before it can be deleted.
	victim := mustCreateIdentity(t, calls, "user", "victim.dev", "Victim Dev")
	if _, err := calls.del(&apiv1.DeleteIdentityRequest{Id: victim.Id}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete active: err = %v, want FailedPrecondition", err)
	}

	// Attach dependents (role binding, API key, local credential) so the
	// cascade cleanup is exercised.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin setup tx: %v", err)
	}
	role, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{TenantID: tenantID, Name: "viewer", Entitlements: []string{"project:read"}})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := db.CreateRoleBinding(ctx, ttx.Tx, db.RoleBindingRow{TenantID: tenantID, IdentityID: victim.Id, RoleID: role.ID}); err != nil {
		t.Fatalf("create role binding: %v", err)
	}
	_, prefix, hash := GenerateApiKey()
	if _, err := db.CreateApiKey(ctx, ttx.Tx, db.ApiKeyRow{TenantID: tenantID, IdentityID: victim.Id, Name: "victim-key", KeyPrefix: prefix, KeyHash: hash}); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit setup tx: %v", err)
	}
	// Seed a local credential (its FK cascades; the explicit delete is redundant but asserted).
	cttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin credential tx: %v", err)
	}
	if _, err := db.UpsertLocalCredential(ctx, cttx.Tx, tenantID, victim.Id, "victim-dev", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", false); err != nil {
		t.Fatalf("upsert local credential: %v", err)
	}
	if err := cttx.Commit(ctx); err != nil {
		t.Fatalf("commit credential tx: %v", err)
	}

	// Disable then delete.
	if _, err := calls.status(&apiv1.SetIdentityStatusRequest{Id: victim.Id, Status: "disabled"}); err != nil {
		t.Fatalf("disable victim: %v", err)
	}
	if _, err := calls.del(&apiv1.DeleteIdentityRequest{Id: victim.Id}); err != nil {
		t.Fatalf("DeleteIdentity(disabled): %v", err)
	}

	// The identity row is gone.
	gtx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin verify tx: %v", err)
	}
	defer gtx.Rollback(ctx)
	if _, err := db.GetIdentity(ctx, gtx.Tx, tenantID, victim.Id); err != db.ErrNotFound {
		t.Fatalf("identity survived delete: %v", err)
	}
	// No orphaned role bindings, API keys, or credentials.
	if bs, err := db.ListRoleBindings(ctx, gtx.Tx, tenantID, victim.Id, 100, ""); err != nil || len(bs) != 0 {
		t.Fatalf("role bindings survived delete: %d rows, err=%v", len(bs), err)
	}
	if ks, err := db.ListApiKeys(ctx, gtx.Tx, tenantID, victim.Id, 100, ""); err != nil || len(ks) != 0 {
		t.Fatalf("api keys survived delete: %d rows, err=%v", len(ks), err)
	}
	if _, err := db.GetLocalCredentialByIdentity(ctx, gtx.Tx, tenantID, victim.Id); err != db.ErrNotFound {
		t.Fatalf("local credential survived delete: %v", err)
	}

	// A second delete is NotFound.
	if _, err := calls.del(&apiv1.DeleteIdentityRequest{Id: victim.Id}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("second delete: err = %v, want NotFound", err)
	}

	// Unknown identity → NotFound.
	if _, err := calls.del(&apiv1.DeleteIdentityRequest{Id: "missing-id"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("delete unknown: err = %v, want NotFound", err)
	}
}

// mustCreateIdentity creates an identity through the service and fails
// the test on error. Returns the persisted proto identity.
func mustCreateIdentity(t *testing.T, calls *identityCalls, identityType, subject, displayName string) *apiv1.Identity {
	t.Helper()
	resp, err := calls.create(&apiv1.CreateIdentityRequest{
		IdentityType: identityType,
		Subject:      subject,
		DisplayName:  displayName,
	})
	if err != nil {
		t.Fatalf("CreateIdentity(%s %s): %v", identityType, subject, err)
	}
	return resp.Msg.Identity
}

// deleteIdentityRow returns a cleanup func that removes an identity row
// directly (some fixtures delete via the service already; this is the
// belt-and-braces cleanup for rows the service tests create).
func deleteIdentityRow(t *testing.T, pool *db.Pool, tenantID, id string) func() {
	t.Helper()
	return func() {
		c := context.Background()
		dtx, err := pool.BeginTenantTx(c, tenantID)
		if err != nil {
			return
		}
		defer dtx.Rollback(c)
		_, _ = dtx.Exec(c, `DELETE FROM identities WHERE id = $1`, id)
		_ = dtx.Commit(c)
	}
}

func pointerTo[T any](v T) *T { return &v }
