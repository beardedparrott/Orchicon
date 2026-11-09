package server

// QA regression pin (DB-backed, real service): the NATIVE Ask path's live file-edit hook must
// ledger under the exact owner TUPLE both clients query — (ask_conversation, <conversation id>).

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/fileedit"
)

func askAttributionPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed Ask-attribution test")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fetchTuples issues the exact client query (owner_kind, owner_id) and returns
// the rows the Ask diff pane would render, in seq order.
func fetchTuples(t *testing.T, rpc *fileedit.RPCService, tenant, ownerKind, ownerID string) []*apiv1.FileEdit {
	t.Helper()
	res, err := rpc.GetSessionFileEdits(context.Background(), connect.NewRequest(&apiv1.GetSessionFileEditsRequest{
		TenantId:  tenant,
		OwnerKind: ownerKind,
		OwnerId:   ownerID,
	}))
	if err != nil {
		t.Fatalf("GetSessionFileEdits(%s,%s): %v", ownerKind, ownerID, err)
	}
	return res.Msg.Edits
}

// TestAskLiveHookRowIsRetrievableByClientTuple is the real-service delta-proof: the PRODUCTION
// constructor, built for the Ask owner, ledgers the engine payload under (ask_conversation.
func TestAskLiveHookRowIsRetrievableByClientTuple(t *testing.T) {
	pool := askAttributionPool(t)
	ctx := context.Background()
	tenant := "tnt_askattr_qa"
	convID := "conv_" + db.NewID()

	svc := fileedit.NewService(fileedit.NewPGStore(pool), slog.Default())
	rpc := fileedit.NewRPCService(fileedit.NewPGStore(pool), slog.Default(), nil)

	// The exact production wiring for the native Ask path (server.go:733-ish).
	askHook := newFileEditHook(svc, slog.Default(), db.FileEditOwnerAskConversation)

	// executeToolCalls calls the hook with (execDir == "") — the engine payload is exact ground truth
	// and needs no repo.
	nonRepo := t.TempDir()
	askHook(ctx, convID, tenant, "", "write",
		map[string]any{"filePath": "notes/a.txt"}, engineWriteOutput)

	// The client's OWN query must return the live row.
	tuples := fetchTuples(t, rpc, tenant, db.FileEditOwnerAskConversation, convID)
	if len(tuples) != 1 {
		t.Fatalf("ask pane query returned %d rows, want 1 (non-repo dir %s)", len(tuples), nonRepo)
	}
	if tuples[0].Tool != "write" {
		t.Fatalf("row tool = %q, want write (a reconcile:git row means the sweep, not the live hook)", tuples[0].Tool)
	}
	if tuples[0].Path != "notes/a.txt" {
		t.Fatalf("row path = %q, want notes/a.txt", tuples[0].Path)
	}

	// NO ORPHAN: the same edit must leave nothing under the synthetic
	// (execution, "orchicon-ask:<convID>") owner every client ignores.
	orphanOwner := "orchicon-ask:" + convID
	if rows := fetchTuples(t, rpc, tenant, db.FileEditOwnerExecution, orphanOwner); len(rows) != 0 {
		t.Fatalf("orphan rows under (execution, %q) = %d, want 0", orphanOwner, len(rows))
	}
	// And nothing under (execution, <convID>) either.
	if rows := fetchTuples(t, rpc, tenant, db.FileEditOwnerExecution, convID); len(rows) != 0 {
		t.Fatalf("rows under (execution, %q) = %d, want 0 — an Ask edit must never attribute as execution", convID, len(rows))
	}
}

// TestExecutionLiveHookRowIsUnchangedByAskWiring is the no-regression half, real-service: the SAME
// constructor built for the execution owner still ledgers under (execution.
func TestExecutionLiveHookRowIsUnchangedByAskWiring(t *testing.T) {
	pool := askAttributionPool(t)
	ctx := context.Background()
	tenant := "tnt_askattr_qa"
	execID := "exec_" + db.NewID()

	svc := fileedit.NewService(fileedit.NewPGStore(pool), slog.Default())
	rpc := fileedit.NewRPCService(fileedit.NewPGStore(pool), slog.Default(), nil)

	execHook := newFileEditHook(svc, slog.Default(), db.FileEditOwnerExecution)
	execHook(ctx, execID, tenant, t.TempDir(), "write",
		map[string]any{"filePath": "notes/a.txt"}, engineWriteOutput)

	rows := fetchTuples(t, rpc, tenant, db.FileEditOwnerExecution, execID)
	if len(rows) != 1 || rows[0].Tool != "write" {
		t.Fatalf("execution query = %d rows (tool %v), want 1 row with tool=write", len(rows), rows)
	}
	// The execution row must NOT bleed into the Ask pane.
	if rows := fetchTuples(t, rpc, tenant, db.FileEditOwnerAskConversation, execID); len(rows) != 0 {
		t.Fatalf("execution edit surfaced in the Ask pane query = %d rows, want 0", len(rows))
	}
}
