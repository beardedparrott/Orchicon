package askorchicon

// tool_mcpservers_scope_test.go — the Ask `list_mcp_servers` tool is
// SCOPE-ADDRESSED, exactly as the MCPService RPC it mirrors (AC 3: "create/list
// are scope-addressed"). Before this, the tool called Service.ListForTenant
// unconditionally, so the Ask surface returned every owner's definitions while
// the platform's ListMCPServers(project_id/conversation_id) narrowed to one
// owner — a live drift between the Ask registry and the RPC surface. This is the
// regression guard for that.
//
// Gate: ORCHICON_TEST_DSN (a disposable database), the repo-wide convention.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// seedScopedMCPServers writes one project-owned and one conversation-owned
// definition and returns their names, plus the two owner ids.
func seedScopedMCPServers(t *testing.T, pool *db.Pool) (projectID, convID, projName, convName string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer func() { _ = ttx.Rollback(ctx) }()

	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, Name: "List Scope",
		Slug: "list-scope-" + strings.ToLower(db.NewID()), Status: "active",
		Goals: []byte("[]"), ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	conv, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: proj.ID,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	projName = "listsvc-proj-" + strings.ToLower(db.NewID())[:8]
	convName = "listsvc-conv-" + strings.ToLower(db.NewID())[:8]
	for _, r := range []db.MCPServerRow{
		{ID: projName, TenantID: workItemKindTestTenant, Name: projName,
			Transport: "streamable-http", URL: "https://mcp.example/proj", Enabled: true, ProjectID: proj.ID,
			Env: map[string]string{}, Headers: map[string]string{}, InstallResult: []byte("{}")},
		{ID: convName, TenantID: workItemKindTestTenant, Name: convName,
			Transport: "streamable-http", URL: "https://mcp.example/conv", Enabled: true, ConversationID: conv.ID,
			Env: map[string]string{}, Headers: map[string]string{}, InstallResult: []byte("{}")},
	} {
		if _, err := db.UpsertMCPServer(ctx, ttx.Tx, r); err != nil {
			t.Fatalf("upsert %s: %v", r.Name, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return proj.ID, conv.ID, projName, convName
}

// listedServerNames runs toolListMCPServers with the given JSON args and returns
// the names in the compact-list envelope.
func listedServerNames(t *testing.T, ctx context.Context, pool *db.Pool, args string) map[string]bool {
	t.Helper()
	raw, err := toolListMCPServers(ctx, pool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("toolListMCPServers(%s): %v", args, err)
	}
	var envelope struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode list envelope: %v", err)
	}
	got := make(map[string]bool, len(envelope.Items))
	for _, it := range envelope.Items {
		got[it.Name] = true
	}
	return got
}

// AC 3: the Ask list tool narrows to the owner it is addressed with.
// (The unscoped case is deliberately NOT asserted: the shared test tenant
// carries other suites' rows and the compact list caps at 25, so an unscoped
// page cannot be relied on to carry a given name. Narrowing is the AC.)
func TestAskListMCPServersIsScopeAddressed(t *testing.T) {
	pool := workItemKindTestPool(t)
	projectID, convID, projName, convName := seedScopedMCPServers(t, pool)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)

	// project scope: the project's own definition, NOT the conversation's.
	projList := listedServerNames(t, ctx, pool, `{"project_id":"`+projectID+`"}`)
	if !projList[projName] {
		t.Errorf("project-scoped list = %v, missing the project-owned %q", projList, projName)
	}
	if projList[convName] {
		t.Errorf("project-scoped list = %v, leaked the conversation-owned %q — the tool is not scope-addressed", projList, convName)
	}

	// conversation scope: the conversation's own definition, NOT another's project one.
	convList := listedServerNames(t, ctx, pool, `{"conversation_id":"`+convID+`"}`)
	if !convList[convName] {
		t.Errorf("conversation-scoped list = %v, missing the conversation-owned %q", convList, convName)
	}
	if convList[projName] {
		t.Errorf("conversation-scoped list = %v, leaked the project-owned %q — the tool is not scope-addressed", convList, projName)
	}
}
