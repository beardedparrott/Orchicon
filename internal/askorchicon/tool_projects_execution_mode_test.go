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
