package askorchicon

// ask_file_scope_db_test.go — the DB-backed half of the scope fix: the conversation's project REALLY decides
// where the suite lands, and the prompt block and the tool boundary agree on it. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database (same pattern as tool_project_dir_test.go):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run 'AskFileScope|AskFileRelative|UnassignedConversation|PromptBlockAndToolScope|AssignedButDirless|NoProjectAtAll' -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// conversationScopeCtx is the context a turn runs under: the tenant plus the project stamp chat.go lays down in
// startConversationTurnOpts. "" is an unassigned conversation.
func conversationScopeCtx(projectID string) context.Context {
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	return withAskConversationProject(ctx, projectID)
}

// assignedConversationForTest creates a real conversation row bound to projectID (what the API stores) so the
// prompt half reads the same row the tool half does.
func assignedConversationForTest(t *testing.T, pool *db.Pool, projectID string) db.ConversationRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant, ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return row
}

// tenantAnchorForTest computes the OLD rule's answer independently — the tenant's first project (active, with a
// project_dir) — so a test can prove the conversation's scope is NOT that.
func tenantAnchorForTest(t *testing.T, ctx context.Context, pool *db.Pool) (dir, projectID string) {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{
		TenantID: workItemKindTestTenant,
		Status:   domain.ProjectActive,
	})
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	for _, p := range rows {
		if d := strings.TrimSpace(p.ProjectDir); d != "" {
			return d, p.ID
		}
	}
	return "", ""
}

// AC #1 — WITH TWO OR MORE ACTIVE PROJECTS, the conversation's own project decides. A conversation on B reports
// B's directory; one on A reports A's. The tenant-first anchor, computed independently, is shown to be a
// DIFFERENT answer (so this fails on the old, conversation-blind code).
func TestAskFileScopeFollowsTheConversationNotTheTenantFirst(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := conversationScopeCtx("")
	dirA, dirB := t.TempDir(), t.TempDir()
	projA := createProjectDirForTest(t, ctx, pool, "Scope A", dirA)
	projB := createProjectDirForTest(t, ctx, pool, "Scope B", dirB)

	convA := assignedConversationForTest(t, pool, projA)
	convB := assignedConversationForTest(t, pool, projB)

	scopeA, err := AskFileScopeFor(withAskConversationProject(ctx, convA.ProjectID), pool)
	if err != nil {
		t.Fatalf("scope for conversation A: %v", err)
	}
	if scopeA.Dir != dirA || !scopeA.FromConversation || scopeA.ProjectID != projA {
		t.Fatalf("scope A = %+v, want dir %q from the conversation's project %s", scopeA, dirA, projA)
	}
	scopeB, err := AskFileScopeFor(withAskConversationProject(ctx, convB.ProjectID), pool)
	if err != nil {
		t.Fatalf("scope for conversation B: %v", err)
	}
	if scopeB.Dir != dirB || !scopeB.FromConversation || scopeB.ProjectID != projB {
		t.Fatalf("scope B = %+v, want dir %q from the conversation's project %s", scopeB, dirB, projB)
	}

	anchorDir, anchorID := tenantAnchorForTest(t, ctx, pool)
	if anchorDir == "" {
		t.Fatalf("the test tenant has no active project with a project_dir — fixture problem")
	}
	t.Logf("old tenant-first rule would return %q (project %s); the conversations are on %q / %q", anchorDir, anchorID, dirA, dirB)
	if anchorID != projA && anchorID != projB {
		if scopeA.Dir == anchorDir || scopeB.Dir == anchorDir {
			t.Fatalf("a conversation-scoped dir equals the tenant-wide anchor %q — the binding is still conversation-blind", anchorDir)
		}
	}
}

// AC #2 — A RELATIVE WRITE LANDS IN THE CONVERSATION'S PROJECT, not the tenant's first: the write from a
// conversation on B appears under B's dir and NOT under A's.
func TestAskFileRelativeWriteLandsInTheConversationProject(t *testing.T) {
	pool := workItemKindTestPool(t)
	base := conversationScopeCtx("")
	dirA, dirB := t.TempDir(), t.TempDir()
	projA := createProjectDirForTest(t, base, pool, "Write A", dirA)
	projB := createProjectDirForTest(t, base, pool, "Write B", dirB)

	p := (&Service{pool: pool}).NativeAskTools()

	convB := assignedConversationForTest(t, pool, projB)
	out, err := p.ExecuteAskTool(withAskConversationProject(base, convB.ProjectID), "write", `{"filePath":"scoped.md","content":"in B\n"}`)
	if err != nil || !strings.Contains(out, "scoped.md") {
		t.Fatalf("write from B: err=%v out=%q", err, out)
	}
	if _, err := os.Stat(filepath.Join(dirB, "scoped.md")); err != nil {
		t.Fatalf("the relative write did NOT land in the conversation's project %q: %v", dirB, err)
	}
	if _, err := os.Stat(filepath.Join(dirA, "scoped.md")); err == nil {
		t.Fatalf("the relative write landed in project A (%q) — the binding is still tenant-wide", dirA)
	}

	// And the other way round, from a conversation on A.
	convA := assignedConversationForTest(t, pool, projA)
	out, err = p.ExecuteAskTool(withAskConversationProject(base, convA.ProjectID), "write", `{"filePath":"in-a.md","content":"in A\n"}`)
	if err != nil || !strings.Contains(out, "in-a.md") {
		t.Fatalf("write from A: err=%v out=%q", err, out)
	}
	if _, err := os.Stat(filepath.Join(dirA, "in-a.md")); err != nil {
		t.Fatalf("the write from conversation A did not land in A's project %q: %v", dirA, err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "in-a.md")); err == nil {
		t.Fatalf("the write from conversation A landed in B (%q)", dirB)
	}
}

// AC #3 — AN UNASSIGNED CONVERSATION gets the tenant anchor but is TOLD it has no project, and the anchor is
// marked OUTSIDE its scope (the machine-readable hook the consent layer reads — AC #6).
func TestUnassignedConversationGetsTheAnchorAndIsToldItHasNoProject(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := conversationScopeCtx("")
	// Guarantee an anchor exists in the shared tenant.
	createProjectDirForTest(t, ctx, pool, "Anchor", t.TempDir())

	scope, err := AskFileScopeFor(ctx, pool)
	if err != nil {
		t.Fatalf("unassigned scope: %v", err)
	}
	if scope.FromConversation {
		t.Fatal("an unassigned conversation reported FromConversation=true — the fallback is masquerading as its own scope")
	}
	if scope.ProjectID != "" {
		t.Fatalf("unassigned scope carries a project id %q", scope.ProjectID)
	}
	anchorDir, _ := tenantAnchorForTest(t, ctx, pool)
	if anchorDir == "" || scope.Dir != anchorDir {
		t.Fatalf("scope.Dir = %q, want the tenant anchor %q", scope.Dir, anchorDir)
	}

	p := (&Service{pool: pool}).NativeAskTools()
	out, err := p.ExecuteAskTool(ctx, askFileRootToolName, `{}`)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v (%s)", err, out)
	}
	if env["scope"] != askScopeTenantAnchor {
		t.Fatalf("unassigned envelope scope = %q, want %q", env["scope"], askScopeTenantAnchor)
	}
	note := strings.ToLower(env["note"])
	if !strings.Contains(note, "no project") {
		t.Fatalf("the envelope does not say the conversation has no project: %q", env["note"])
	}
	if !strings.Contains(note, "outside") || !strings.Contains(note, "consent") {
		t.Fatalf("the envelope does not mark the anchor as outside scope / needing consent: %q", env["note"])
	}
	if strings.Contains(env["note"], "IS this conversation's project") {
		t.Fatalf("the envelope presents the fallback as the conversation's own project: %q", env["note"])
	}

	// The PROMPT tells the same story.
	prompt := buildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry(), nil, true, nil, "", "")
	if !strings.Contains(prompt, "not assigned to a project") {
		t.Fatal("the unassigned prompt no longer states the chat has no project")
	}
	if !strings.Contains(prompt, "outside this chat's scope") {
		t.Fatal("the unassigned prompt does not mark the anchor as outside the chat's scope")
	}
}

// AC #4 — THE PROMPT BLOCK AND THE TOOL BOUNDARY AGREE IN EVERY CASE: assigned, the block names exactly the
// directory the suite binds to; unassigned, neither half presents a foreign project as this chat's own.
func TestPromptBlockAndToolScopeAgree(t *testing.T) {
	pool := workItemKindTestPool(t)
	base := conversationScopeCtx("")
	dirB := t.TempDir()
	projB := createProjectDirForTest(t, base, pool, "Agree B", dirB)
	convB := assignedConversationForTest(t, pool, projB)
	ctxB := withAskConversationProject(base, convB.ProjectID)

	scope, err := AskFileScopeFor(ctxB, pool)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	svc := New(pool, slog.Default(), nil, nil, nil)

	// ASSIGNED: the prompt block names EXACTLY the directory the suite binds to.
	block := svc.conversationProjectContext(ctxB, workItemKindTestTenant, convB)
	if !strings.Contains(block, scope.Dir) {
		t.Fatalf("the prompt block does not name the tool boundary %q:\n%s", scope.Dir, block)
	}
	full := buildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry(), nil, true, nil, "", block)
	if !strings.Contains(full, "## This conversation's project") || !strings.Contains(full, scope.Dir) {
		t.Fatalf("the full prompt does not carry the conversation-project block naming %q", scope.Dir)
	}

	// UNASSIGNED: the block is empty (the caller prints the unassigned text) and the tool scope is the anchor.
	unassigned := db.ConversationRow{TenantID: workItemKindTestTenant}
	if got := svc.conversationProjectContext(base, workItemKindTestTenant, unassigned); got != "" {
		t.Fatalf("an unassigned conversation produced a project block %q, want \"\"", got)
	}
	anchorScope, err := AskFileScopeFor(base, pool)
	if err != nil {
		t.Fatalf("anchor scope: %v", err)
	}
	if anchorScope.FromConversation {
		t.Fatal("the unassigned tool scope claims to come from the conversation")
	}
	unassignedFull := buildSystemPrompt(modeIteration, testAgentConfig(), testToolRegistry(), nil, true, nil, "", "")
	if !strings.Contains(unassignedFull, "not assigned to a project") {
		t.Fatal("the unassigned prompt does not state the chat has no project")
	}
	if strings.Contains(unassignedFull, "belongs to the project") {
		t.Fatal("the unassigned prompt names a project as this chat's own")
	}
}

// A CONVERSATION ASSIGNED TO A DIRLESS PROJECT fails with the ACTIONABLE text that names the fix.
func TestAssignedButDirlessProjectErrorsActionably(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := conversationScopeCtx("")
	projID := createProjectDirForTest(t, ctx, pool, "No Dir Scope", "")

	_, err := AskFileScopeFor(withAskConversationProject(ctx, projID), pool)
	if err == nil || !strings.Contains(err.Error(), "create_project_directory") {
		t.Fatalf("dirless conversation project: err = %v, want the actionable create_project_directory hint", err)
	}
}

// A TENANT WITH NO ACTIVE PROJECT CARRYING A DIR fails with the same actionable text. Unreachable in the shared
// test tenant once any fixture project exists, so it skips loudly rather than pretending to cover it.
func TestNoProjectAtAllErrorsActionably(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := conversationScopeCtx("")
	if dir, _ := tenantAnchorForTest(t, ctx, pool); dir != "" {
		t.Skip("the shared test tenant already has an active project with a project_dir; the no-project error is unreachable here")
	}
	_, err := AskFileScopeFor(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "create_project_directory") {
		t.Fatalf("no-project tenant: err = %v, want the actionable create_project_directory hint", err)
	}
}
