package askorchicon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// These tests exercise the Ask Orchicon create_work_item tool against a
// real Postgres. They are skipped unless ORCHICON_TEST_DSN points at a
// disposable database (the migrations are applied on every run, so the
// database must be safe to re-seed):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestCreateWorkItemKind -v
//
// They guard the acceptance criteria "no more `kind: unknown` for Ask
// Orchicon-created items": the tool must normalize and validate `kind`
// (trim, lowercase, accept only epic/feature/task/subtask) so every
// future create stores a canonical lowercase value that the Connect API
// maps correctly.

func workItemKindTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed ask-orchicon tests")
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
	if err := db.SeedDevWorkers(ctx, pool); err != nil {
		t.Fatalf("seed dev workers: %v", err)
	}
	return pool
}

const workItemKindTestTenant = "tnt_dev"

// createProjectForTest creates a throwaway project in the test tenant so
// the tool's project_id FK resolves.
func createProjectForTest(t *testing.T, ctx context.Context, pool *db.Pool) string {
	t.Helper()
	ttx, err := pool.BeginTenantTx(ctx, workItemKindTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: workItemKindTestTenant,
		Name: "Kind Test", Slug: "kind-test-" + strings.ToLower(db.NewID()),
		Status: "active", Goals: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit project: %v", err)
	}
	return proj.ID
}

// createWorkItemViaTool invokes the real toolCreateWorkItem with the given
// raw kind value and returns the created row's kind.
func createWorkItemViaTool(t *testing.T, ctx context.Context, pool *db.Pool, projectID, kind string) (db.WorkItemRow, error) {
	t.Helper()
	args := map[string]any{
		"project_id": projectID,
		"title":      "Kind normalization test " + strings.ToLower(db.NewID()),
	}
	if kind != "" {
		args["kind"] = kind
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := toolCreateWorkItem(ctx, pool, raw)
	if err != nil {
		return db.WorkItemRow{}, err
	}
	var item db.WorkItemRow
	if err := json.Unmarshal(res, &item); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	return item, nil
}

// TestCreateWorkItemKindCanonicalizes verifies the root-cause fix: any
// casing/whitespace variant of a canonical kind is stored lowercase.
func TestCreateWorkItemKindCanonicalizes(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	for _, tc := range []struct {
		input string
		want  string
	}{
		{"Epic", domain.WorkItemKindEpic},
		{"Task", domain.WorkItemKindTask},
		{"EPIC", domain.WorkItemKindEpic},
		{"  task  ", domain.WorkItemKindTask},
		{"feature", domain.WorkItemKindFeature},
		{"Subtask", domain.WorkItemKindSubtask},
	} {
		item, err := createWorkItemViaTool(t, ctx, pool, projectID, tc.input)
		if err != nil {
			t.Fatalf("create with kind %q: %v", tc.input, err)
		}
		if item.Kind != tc.want {
			t.Errorf("create with kind %q stored %q, want %q", tc.input, item.Kind, tc.want)
		}
	}
}

// TestCreateWorkItemKindDefaultsToTask verifies an omitted kind defaults
// to the canonical task constant.
func TestCreateWorkItemKindDefaultsToTask(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	item, err := createWorkItemViaTool(t, ctx, pool, projectID, "")
	if err != nil {
		t.Fatalf("create without kind: %v", err)
	}
	if item.Kind != domain.WorkItemKindTask {
		t.Errorf("create without kind stored %q, want %q", item.Kind, domain.WorkItemKindTask)
	}
}

// TestCreateWorkItemKindRejectsUnknown verifies unknown kinds are
// rejected with an error that names the four canonical kinds — the tool
// never stores a value the strict read path would map to UNSPECIFIED.
func TestCreateWorkItemKindRejectsUnknown(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	projectID := createProjectForTest(t, ctx, pool)

	for _, input := range []string{"Story", "bug", "epic2"} {
		if _, err := createWorkItemViaTool(t, ctx, pool, projectID, input); err == nil {
			t.Errorf("create with kind %q should have been rejected", input)
		} else if msg := err.Error(); !strings.Contains(msg, "epic") || !strings.Contains(msg, "subtask") {
			t.Errorf("create with kind %q error %q does not name the four canonical kinds", input, msg)
		}
	}
}

// TestNormalizeWorkItemKindUnit exercises the domain helper directly for
// the exact call shapes the tool produces.
func TestNormalizeWorkItemKindUnit(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"Epic", domain.WorkItemKindEpic, true},
		{"epic", domain.WorkItemKindEpic, true},
		{"EPIC", domain.WorkItemKindEpic, true},
		{"  Task  ", domain.WorkItemKindTask, true},
		{"", "", false},
		{"Story", "", false},
		{"recovery_stop", "", false},
	} {
		got, err := domain.NormalizeWorkItemKind(tc.input)
		if tc.ok && err != nil {
			t.Errorf("NormalizeWorkItemKind(%q) unexpected error: %v", tc.input, err)
		}
		if tc.ok && got != tc.want {
			t.Errorf("NormalizeWorkItemKind(%q) = %q, want %q", tc.input, got, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("NormalizeWorkItemKind(%q) expected error, got %q", tc.input, got)
		}
	}
}
