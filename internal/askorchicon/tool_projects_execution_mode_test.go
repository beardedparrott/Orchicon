package askorchicon

// tool_projects_execution_mode_test.go — the Ask Orchicon tool entry point's
// execution_mode default and validation (AC1 per entry point + AC3). Skipped
// unless ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestToolCreateProjectExecutionMode -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func askCreateProject(t *testing.T, pool *db.Pool, args string) (db.ProjectRow, error) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)
	out, err := toolCreateProject(ctx, pool, json.RawMessage(args))
	if err != nil {
		return db.ProjectRow{}, err
	}
	var row db.ProjectRow
	if err := json.Unmarshal(out, &row); err != nil {
		t.Fatalf("unmarshal created project: %v", err)
	}
	return row, nil
}

// Omitted mode → local.
func TestToolCreateProjectExecutionModeDefaultsLocal(t *testing.T) {
	pool := workItemKindTestPool(t)
	row, err := askCreateProject(t, pool, `{"title":"Mode Ask Default `+strings.ToLower(db.NewID())+`"}`)
	if err != nil {
		t.Fatalf("toolCreateProject: %v", err)
	}
	if row.ExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("omitted mode created %q, want %q", row.ExecutionMode, db.ExecutionModeLocal)
	}
}

// Explicit runtime is honoured.
func TestToolCreateProjectExecutionModeRuntimeHonoured(t *testing.T) {
	pool := workItemKindTestPool(t)
	row, err := askCreateProject(t, pool, `{"title":"Mode Ask Runtime `+strings.ToLower(db.NewID())+`","execution_mode":"runtime"}`)
	if err != nil {
		t.Fatalf("toolCreateProject: %v", err)
	}
	if row.ExecutionMode != db.ExecutionModeRuntime {
		t.Fatalf("runtime created %q, want runtime", row.ExecutionMode)
	}
}

// A bogus mode is REJECTED (it used to be silently coerced to runtime).
func TestToolCreateProjectExecutionModeInvalidRejected(t *testing.T) {
	pool := workItemKindTestPool(t)
	name := "Mode Ask Bad " + strings.ToLower(db.NewID())
	_, err := askCreateProject(t, pool, `{"title":"`+name+`","execution_mode":"banana"}`)
	if err == nil {
		t.Fatal("toolCreateProject with execution_mode=banana succeeded, want rejection")
	}
	if !strings.Contains(err.Error(), db.ExecutionModeInvalidMsg) {
		t.Fatalf("error = %q, want %q", err.Error(), db.ExecutionModeInvalidMsg)
	}
}

// The UPDATE path routes through the same parser as create: a spelling create
// accepts is not rejected on update, a bogus value still is, and a blank value
// is not silently treated as "unchanged" (it is not a mode).
func TestToolUpdateProjectExecutionModeParsing(t *testing.T) {
	pool := workItemKindTestPool(t)
	ctx := tenant.WithID(context.Background(), workItemKindTestTenant)

	created, err := askCreateProject(t, pool, `{"title":"Mode Ask Update `+strings.ToLower(db.NewID())+`"}`)
	if err != nil {
		t.Fatalf("toolCreateProject: %v", err)
	}
	if created.ExecutionMode != db.ExecutionModeLocal {
		t.Fatalf("created mode = %q, want local", created.ExecutionMode)
	}

	out, err := toolUpdateProject(ctx, pool, json.RawMessage(`{"id":"`+created.ID+`","execution_mode":" Runtime "}`))
	if err != nil {
		t.Fatalf("toolUpdateProject(\" Runtime \"): %v", err)
	}
	var updated db.ProjectRow
	if err := json.Unmarshal(out, &updated); err != nil {
		t.Fatalf("unmarshal updated project: %v", err)
	}
	if updated.ExecutionMode != db.ExecutionModeRuntime {
		t.Fatalf("updated mode = %q, want runtime", updated.ExecutionMode)
	}

	for _, raw := range []string{`"banana"`, `"   "`, `""`} {
		if _, err := toolUpdateProject(ctx, pool, json.RawMessage(`{"id":"`+created.ID+`","execution_mode":`+raw+`}`)); err == nil {
			t.Fatalf("toolUpdateProject(execution_mode=%s) succeeded, want rejection", raw)
		} else if !strings.Contains(err.Error(), db.ExecutionModeInvalidMsg) {
			t.Fatalf("toolUpdateProject(execution_mode=%s) error = %q, want %q", raw, err.Error(), db.ExecutionModeInvalidMsg)
		}
	}
}
