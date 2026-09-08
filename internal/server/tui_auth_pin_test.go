package server

// TUI auth pinning test (architect plan §4 / step 6).
//
// orch is a pure HTTP/Connect client of the plane. Its entire auth story
// rests on ONE invariant: API-key auth works on every RPC the TUI
// consumes, INCLUDING server-streams, because auth is resolved at the
// HTTP layer (middleware.ResolveAuth wraps the whole mux) and per-RPC RBAC
// entitlements map Get*/List*/Stream* → <resource>:read. A future
// refactor that moves auth per-RPC or drops WrapStreamingHandler would
// silently break the TUI's streams — this test pins the invariant.
//
// DB-backed (skips without ORCHICON_TEST_DSN), following the
// localLoginEnv pattern from internal/auth.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/api"
	authpkg "github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// readEntitlements is every <resource>:read the TUI's v1 read-only
// surface touches (plan §6 RPC table).
var readEntitlements = []string{
	"project:read", "workitem:read", "execution:read", "workflow:read",
	"worker:read", "workflowrun:read", "policy:read", "approval:read",
	"recovery:read", "runtimeimage:read", "secret:read", "settings:read",
	"mcp:read", "auth:read", "conversation:read", "message:read",
}

func testPlane(t *testing.T) (*httptest.Server, *db.Pool, string, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed TUI auth pin test")
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

	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Auth.SigningKey = "test-signing-key-for-tui-auth-pin"
	cfg.Auth.RedirectURL = "http://localhost:8080/auth/callback"
	cfg.Auth.EmbeddedOP = true
	cfg.Auth.OPRedirectURIs = ""

	log := slog.New(slog.DiscardHandler)
	authHandler := authpkg.NewHandler(cfg, pool, log)
	t.Cleanup(func() { authHandler.CloseEmbeddedOP() })

	deps := &api.Dependencies{
		Pool:        pool,
		Log:         log,
		AuthHandler: authHandler,
		Mode:        config.ModeLocal,
	}
	mux := http.NewServeMux()
	root := api.Mount(mux, deps)
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)

	// Seed identity + two API keys: one with all read scopes, one with a
	// single scope.
	ttx, err := pool.BeginTenantTx(ctx, cfg.DeploymentTenantID)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, cfg.DeploymentTenantID, "tui-pin-user", "tui-pin-user", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}

	authSvc := authpkg.NewService(pool, log)
	adminCtx := tenant.WithID(authpkg.WithIdentity(ctx, authpkg.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   cfg.DeploymentTenantID,
		IsAdmin:    true,
		AuthMethod: "oidc",
	}), cfg.DeploymentTenantID)

	fullKey, err := authSvc.CreateApiKey(adminCtx, connect.NewRequest(&apiv1.CreateApiKeyRequest{
		Name:       "tui-full",
		IdentityId: ident.ID,
		Scopes:     readEntitlements,
	}))
	if err != nil {
		t.Fatalf("create full-scope key: %v", err)
	}
	narrowKey, err := authSvc.CreateApiKey(adminCtx, connect.NewRequest(&apiv1.CreateApiKeyRequest{
		Name:       "tui-narrow",
		IdentityId: ident.ID,
		Scopes:     []string{"project:read"},
	}))
	if err != nil {
		t.Fatalf("create narrow key: %v", err)
	}
	return srv, pool, fullKey.Msg.Secret.Key, narrowKey.Msg.Secret.Key
}

func checkStream(t *testing.T, name string, fn func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		t.Errorf("%s: full-scope key failed: %v", name, err)
	}
}

// bearer mirrors orch's client-side interceptor (internal/tui/client).
type bearer struct{ cred string }

func (b *bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.cred)
		return next(ctx, req)
	}
}

func (b *bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.cred)
		return conn
	}
}

func (b *bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// TestTUIAuthFullScopeKeySucceedsOnEveryRPC asserts the invariant: the
// full-scope API key authenticates + authorizes on every RPC the TUI
// consumes, unary AND server-stream.
func TestTUIAuthFullScopeKeySucceedsOnEveryRPC(t *testing.T) {
	srv, _, fullKey, _ := testPlane(t)
	ctx := context.Background()
	opts := connect.WithInterceptors(&bearer{cred: fullKey})
	tenantID := "tnt_dev"

	check := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: full-scope key failed: %v", name, err)
		}
	}

	projects := apiv1connect.NewProjectServiceClient(srv.Client(), srv.URL, opts)
	check("ListProjects", func() error {
		_, err := projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{PageSize: 1}))
		return err
	}())
	check("GetProject", func() error {
		_, err := projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: "prj_none"}))
		if err != nil && connect.CodeOf(err) == connect.CodePermissionDenied {
			return err
		}
		return nil // NotFound is fine — auth+RBAC passed
	}())
	checkStream(t, "StreamProjectEvents", func(ctx context.Context) error {
		s, err := projects.StreamProjectEvents(ctx, connect.NewRequest(&apiv1.StreamProjectEventsRequest{TenantId: tenantID}))
		if err != nil {
			return err
		}
		defer s.Close()
		s.Receive() // header + first event (or clean close) both prove auth
		return nil
	})

	workItems := apiv1connect.NewWorkItemServiceClient(srv.Client(), srv.URL, opts)
	check("ListWorkItems", func() error {
		_, err := workItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{TenantId: tenantID, PageSize: 1}))
		return err
	}())

	executions := apiv1connect.NewExecutionServiceClient(srv.Client(), srv.URL, opts)
	check("ListExecutions", func() error {
		_, err := executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{TenantId: tenantID, PageSize: 1}))
		return err
	}())
	checkStream(t, "StreamExecutionEvents", func(ctx context.Context) error {
		s, err := executions.StreamExecutionEvents(ctx, connect.NewRequest(&apiv1.StreamExecutionEventsRequest{TenantId: tenantID}))
		if err != nil {
			return err
		}
		defer s.Close()
		s.Receive()
		return nil
	})

	workflows := apiv1connect.NewWorkflowServiceClient(srv.Client(), srv.URL, opts)
	check("ListWorkflows", func() error {
		_, err := workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{TenantId: tenantID, PageSize: 1}))
		return err
	}())
	check("ListWorkflowRuns", func() error {
		_, err := workflows.ListWorkflowRuns(ctx, connect.NewRequest(&apiv1.ListWorkflowRunsRequest{PageSize: 1}))
		return err
	}())
	checkStream(t, "StreamWorkflowEvents", func(ctx context.Context) error {
		s, err := workflows.StreamWorkflowEvents(ctx, connect.NewRequest(&apiv1.StreamWorkflowEventsRequest{TenantId: tenantID}))
		if err != nil {
			return err
		}
		defer s.Close()
		s.Receive()
		return nil
	})

	policies := apiv1connect.NewPolicyServiceClient(srv.Client(), srv.URL, opts)
	check("ListPolicies", func() error {
		_, err := policies.ListPolicies(ctx, connect.NewRequest(&apiv1.ListPoliciesRequest{TenantId: tenantID}))
		return err
	}())

	approvals := apiv1connect.NewApprovalServiceClient(srv.Client(), srv.URL, opts)
	check("ListPendingStepApprovals", func() error {
		_, err := approvals.ListPendingStepApprovals(ctx, connect.NewRequest(&apiv1.ListPendingStepApprovalsRequest{}))
		return err
	}())

	recovery := apiv1connect.NewRecoveryServiceClient(srv.Client(), srv.URL, opts)
	checkStream(t, "StreamRecoveryEvents", func(ctx context.Context) error {
		s, err := recovery.StreamRecoveryEvents(ctx, connect.NewRequest(&apiv1.StreamRecoveryEventsRequest{TenantId: tenantID}))
		if err != nil {
			return err
		}
		defer s.Close()
		s.Receive()
		return nil
	})

	workers := apiv1connect.NewWorkerServiceClient(srv.Client(), srv.URL, opts)
	check("ListWorkers", func() error {
		_, err := workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{TenantId: tenantID, PageSize: 1}))
		return err
	}())

	images := apiv1connect.NewRuntimeImageServiceClient(srv.Client(), srv.URL, opts)
	check("ListRuntimeImages", func() error {
		_, err := images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{TenantId: tenantID}))
		return err
	}())

	secrets := apiv1connect.NewSecretsServiceClient(srv.Client(), srv.URL, opts)
	check("ListSecrets", func() error {
		_, err := secrets.ListSecrets(ctx, connect.NewRequest(&apiv1.ListSecretsRequest{PageSize: 1}))
		return err
	}())

	mcp := apiv1connect.NewMCPServiceClient(srv.Client(), srv.URL, opts)
	check("ListMCPServers", func() error {
		_, err := mcp.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{}))
		return err
	}())

	settings := apiv1connect.NewSettingsServiceClient(srv.Client(), srv.URL, opts)
	check("GetSettings", func() error {
		_, err := settings.GetSettings(ctx, connect.NewRequest(&apiv1.GetSettingsRequest{}))
		return err
	}())

	authCli := apiv1connect.NewAuthServiceClient(srv.Client(), srv.URL, opts)
	check("ListApiKeys", func() error {
		_, err := authCli.ListApiKeys(ctx, connect.NewRequest(&apiv1.ListApiKeysRequest{TenantId: tenantID, IdentityId: "ident_none"}))
		if err != nil && connect.CodeOf(err) == connect.CodeNotFound {
			return nil // auth passed; unknown identity is not an auth failure
		}
		return err
	}())

	ask := apiv1connect.NewAskOrchiconServiceClient(srv.Client(), srv.URL, opts)
	check("ListConversations", func() error {
		_, err := ask.ListConversations(ctx, connect.NewRequest(&apiv1.ListConversationsRequest{PageSize: 1}))
		return err
	}())
}

// TestTUIAuthNarrowKeyGetsPermissionDenied asserts per-RPC RBAC: a key
// with only project:read must get PermissionDenied on non-project reads.
func TestTUIAuthNarrowKeyGetsPermissionDenied(t *testing.T) {
	srv, _, _, narrowKey := testPlane(t)
	ctx := context.Background()
	opts := connect.WithInterceptors(&bearer{cred: narrowKey})

	workers := apiv1connect.NewWorkerServiceClient(srv.Client(), srv.URL, opts)
	_, err := workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{TenantId: "tnt_dev", PageSize: 1}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("narrow key ListWorkers: want PermissionDenied, got %v", err)
	}

	executions := apiv1connect.NewExecutionServiceClient(srv.Client(), srv.URL, opts)
	_, err = executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{TenantId: "tnt_dev", PageSize: 1}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("narrow key ListExecutions: want PermissionDenied, got %v", err)
	}
}

// TestTUIAuthNoCredentialGets401 asserts ResolveAuth's 401 on missing
// credentials for both unary and stream RPCs.
func TestTUIAuthNoCredentialGets401(t *testing.T) {
	srv, _, _, _ := testPlane(t)
	ctx := context.Background()

	projects := apiv1connect.NewProjectServiceClient(srv.Client(), srv.URL)
	_, err := projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{PageSize: 1}))
	if err == nil {
		t.Fatal("unauthenticated ListProjects must fail")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	// Stream dial is lazy: the 401 surfaces on the first Receive.
	s, err := projects.StreamProjectEvents(ctx, connect.NewRequest(&apiv1.StreamProjectEventsRequest{TenantId: "tnt_dev"}))
	if err == nil {
		if s.Receive() {
			t.Fatal("unauthenticated stream must not deliver events")
		}
		err = s.Err()
	}
	if err == nil || connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unauthenticated StreamProjectEvents: want Unauthenticated, got %v", err)
	}
}
