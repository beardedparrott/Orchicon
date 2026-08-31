package askorchicon

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// DB-backed tests for the create_workflow tool path. They guard the
// ghost-record fix: toolCreateWorkflow must produce a draft version-1 row
// (steps persisted), a workflow.created audit row, and a publishable
// workflow — not the header-only husk the old implementation created
// (which also silently dropped the steps argument). Skipped unless
// ORCHICON_TEST_DSN is set; shared helpers live in tool_workers_test.go.

func createWorkflowViaTool(t *testing.T, ctx context.Context, pool *db.Pool, args string) (db.WorkflowRow, map[string]any) {
	t.Helper()
	raw, err := toolCreateWorkflow(ctx, pool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("toolCreateWorkflow: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var w db.WorkflowRow
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("re-encode response: %v", err)
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("decode workflow row: %v", err)
	}
	return w, resp
}

// normalizeJSON decodes JSON for storage-insensitive comparison (jsonb
// normalizes key order and whitespace on round-trip).
func normalizeJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode JSON %q: %v", s, err)
	}
	return v
}

func TestToolCreateWorkflowSeedsSteps(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	steps := `[{"id":"st1","name":"Research","kind":"task","ref":"worker:research","depends_on":[]},{"id":"st2","name":"Write","kind":"task","ref":"worker:write","depends_on":["st1"]}]`
	w, resp := createWorkflowViaTool(t, ctx, pool,
		`{"name":"Ghost WF","description":"seeded description","steps":`+steps+`,"git_strategy":"local"}`)
	if w.ID == "" {
		t.Fatal("response missing workflow ID")
	}
	if v, _ := resp["version"].(float64); int(v) != 1 {
		t.Fatalf("response version = %v, want 1", resp["version"])
	}
	if vid, _ := resp["version_id"].(string); vid == "" {
		t.Fatal("response missing version_id")
	}
	if w.Status != "draft" || w.CurrentVersion != 0 {
		t.Fatalf("workflow = %s/%d, want draft/0", w.Status, w.CurrentVersion)
	}
	if w.Type != "template" {
		t.Fatalf("type = %q, want template (no project)", w.Type)
	}
	if w.GitStrategy == nil || *w.GitStrategy != "local" {
		t.Fatalf("git_strategy = %v, want local", w.GitStrategy)
	}

	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, w.ID, 1)
	if err != nil {
		t.Fatalf("get workflow version 1: %v", err)
	}
	if ver.Status != "draft" {
		t.Fatalf("version status = %s, want draft", ver.Status)
	}
	if ver.VersionNote != "seeded description" {
		t.Fatalf("version_note = %q, want the description fallback", ver.VersionNote)
	}
	if !reflect.DeepEqual(normalizeJSON(t, steps), normalizeJSON(t, string(ver.Steps))) {
		t.Fatalf("steps not persisted:\n want %s\n got  %s", steps, ver.Steps)
	}
	if string(ver.Inputs) != "{}" || string(ver.Outputs) != "{}" {
		t.Fatalf("json defaults not canonical: inputs=%s outputs=%s", ver.Inputs, ver.Outputs)
	}
	ttx.Rollback(ctx)

	if n := auditRowCount(t, pool, tenantID, "workflow.created", w.ID); n != 1 {
		t.Fatalf("workflow.created audit rows = %d, want 1", n)
	}
}

func TestToolCreateWorkflowPublishable(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	w, _ := createWorkflowViaTool(t, ctx, pool, `{"name":"Publish WF"}`)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.PublishWorkflowVersion(ctx, ttx.Tx, tenantID, w.ID, 1)
	if err != nil {
		t.Fatalf("publish draft v1: %v", err)
	}
	if ver.Status != "published" {
		t.Fatalf("version status = %s, want published", ver.Status)
	}
}

func TestToolCreateWorkflowTypeAndProject(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	// No project_id → template.
	w, _ := createWorkflowViaTool(t, ctx, pool, `{"name":"Template WF"}`)
	if w.Type != "template" {
		t.Fatalf("type = %q, want template", w.Type)
	}
	// With an active project → one_shot; explicit version_note wins over description.
	pid := createProjectWithStatus(t, ctx, pool, tenantID, "active")
	w2, _ := createWorkflowViaTool(t, ctx, pool,
		`{"name":"One Shot WF","project_id":"`+pid+`","description":"ignored note","version_note":"explicit note","type":"one_shot"}`)
	if w2.Type != "one_shot" {
		t.Fatalf("type = %q, want one_shot", w2.Type)
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, w2.ID, 1)
	if err != nil {
		t.Fatalf("get workflow version: %v", err)
	}
	if ver.VersionNote != "explicit note" {
		t.Fatalf("version_note = %q, want explicit note to win", ver.VersionNote)
	}
}

func TestToolCreateWorkflowValidation(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	cases := []string{
		`{"name":"   "}`,                                  // empty name
		`{"name":"Bad Steps","steps":{"a":1}}`,            // steps must be an array
		`{"name":"Bad Strategy","git_strategy":"rebase"}`, // invalid git_strategy
		`{"name":"Bad Type","type":"dag"}`,                // invalid type
	}
	for _, args := range cases {
		if _, err := toolCreateWorkflow(ctx, pool, json.RawMessage(args)); err == nil {
			t.Errorf("toolCreateWorkflow(%s) succeeded, want error", args)
		}
	}
	// Explicit null behaves like absent: steps default to [].
	w, _ := createWorkflowViaTool(t, ctx, pool, `{"name":"Null Steps","steps":null,"inputs":null,"outputs":null}`)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ver, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, w.ID, 1)
	if err != nil {
		t.Fatalf("get workflow version: %v", err)
	}
	if string(ver.Steps) != "[]" {
		t.Fatalf("steps = %s, want [] for null input", ver.Steps)
	}
}

func TestToolCreateWorkflowInactiveProject(t *testing.T) {
	pool, ctx, tenantID := toolGhostEnv(t)
	pid := createProjectWithStatus(t, ctx, pool, tenantID, "paused")
	_, err := toolCreateWorkflow(ctx, pool, json.RawMessage(`{"name":"Never","project_id":"`+pid+`"}`))
	if err == nil || !strings.Contains(err.Error(), "project not active") {
		t.Fatalf("err = %v, want project-not-active failure", err)
	}
}

// createProjectWithStatus creates a throwaway project in the tenant so
// the tool's project_id path resolves (status active/paused).
func createProjectWithStatus(t *testing.T, ctx context.Context, pool *db.Pool, tenantID, status string) string {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: tenantID,
		Name: "WF Tool Test " + status, Slug: "wf-tool-" + status + "-" + strings.ToLower(db.NewID()),
		Status: status, Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	return proj.ID
}
