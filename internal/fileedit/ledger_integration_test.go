package fileedit

// Integration test over the REAL ledger path: engine tool output →
// RecordEngineOutput → PGStore.Append → List + RPCService.GetSessionFileEdits,
// with seq resume and event-id == row-id (the stream resume/dedup contract).
// DB-backed; skips without ORCHICON_TEST_DSN (ledger_db_test.go pattern).

import (
	"context"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

const integrationEngineOutput = "batch_write: applied 2 write(s): a.txt, b.md\n" +
	`{"summary":"batch_write: applied 2 write(s): a.txt, b.md","file_edits":[` +
	`{"path":"a.txt","kind":"create","unified_diff":"--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,1 @@\n+a\n","existed_before":false,"existed_after":true,"size_before":0,"size_after":2},` +
	`{"path":"b.md","kind":"modify","unified_diff":"--- a/b.md\n+++ b/b.md\n@@ -1 +1 @@\n-old\n+new\n","existed_before":true,"existed_after":true,"size_before":4,"size_after":4}]}`

func TestLedgerIntegrationEngineOutputToFetch(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant, owner := "tnt_fileeditinteg", "owner_"+db.NewID()

	svc := NewService(NewPGStore(pool), slog.Default())
	parsed, recorded := svc.RecordEngineOutput(ctx, tenant, db.FileEditOwnerExecution, owner, "batch_write", integrationEngineOutput)
	if parsed != 2 || recorded != 2 {
		t.Fatalf("RecordEngineOutput = (%d, %d), want (2, 2)", parsed, recorded)
	}

	// A failed tool output records nothing (AC 4: no phantom rows).
	parsed, recorded = svc.RecordEngineOutput(ctx, tenant, db.FileEditOwnerExecution, owner, "edit", "edit: oldString not found in content")
	if parsed != 0 || recorded != 0 {
		t.Fatalf("failed output = (%d, %d), want (0, 0)", parsed, recorded)
	}

	// The fetch RPC serves exactly the appended rows, seq-ordered.
	rpc := NewRPCService(NewPGStore(pool), slog.Default(), nil)
	res, err := rpc.GetSessionFileEdits(ctx, connect.NewRequest(&apiv1.GetSessionFileEditsRequest{
		TenantId:  tenant,
		OwnerKind: db.FileEditOwnerExecution,
		OwnerId:   owner,
	}))
	if err != nil {
		t.Fatalf("GetSessionFileEdits: %v", err)
	}
	if len(res.Msg.Edits) != 2 || res.Msg.MaxSeq != 2 {
		t.Fatalf("fetch = %d edits maxSeq %d, want 2 edits maxSeq 2", len(res.Msg.Edits), res.Msg.MaxSeq)
	}
	if res.Msg.Edits[0].Path != "a.txt" || res.Msg.Edits[1].Path != "b.md" {
		t.Fatalf("fetch order wrong: %s, %s", res.Msg.Edits[0].Path, res.Msg.Edits[1].Path)
	}
	if res.Msg.Edits[0].Seq != 1 || res.Msg.Edits[1].Seq != 2 {
		t.Fatalf("fetch seq wrong: %d, %d", res.Msg.Edits[0].Seq, res.Msg.Edits[1].Seq)
	}
	// Stream contract: event_id == row id (stable → dedup survives reconnect).
	rows, _, err := NewPGStore(pool).List(ctx, tenant, db.FileEditOwnerExecution, owner, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for i, r := range rows {
		if res.Msg.Edits[i].Id != r.ID {
			t.Fatalf("fetch id %q != row id %q (event-id dedup contract broken)", res.Msg.Edits[i].Id, r.ID)
		}
	}

	// fromSeq resume: only rows beyond seq 1 come back (reconnect catch-up).
	fromSeq := int64(1)
	res2, err := rpc.GetSessionFileEdits(ctx, connect.NewRequest(&apiv1.GetSessionFileEditsRequest{
		TenantId:  tenant,
		OwnerKind: db.FileEditOwnerExecution,
		OwnerId:   owner,
		FromSeq:   &fromSeq,
	}))
	if err != nil {
		t.Fatalf("GetSessionFileEdits fromSeq: %v", err)
	}
	if len(res2.Msg.Edits) != 1 || res2.Msg.Edits[0].Path != "b.md" {
		t.Fatalf("fromSeq resume wrong: %+v", res2.Msg.Edits)
	}
}
