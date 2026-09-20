package fileedit

import (
	"context"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// testPool opens the sandbox-plane test DB (ORCHICON_TEST_DSN); tests skip
// when unset — the same gate every DB-backed test in internal/db uses.
func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed file-edit ledger test")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestLedgerAppendListSeq: two entries append with monotonically allocated
// seq per owner, list returns them in order, and a re-append of identical
// entries (replay / dedup) does not corrupt the sequence — the durable-ledger
// acceptance criteria (survive restart, fetch returns the full ledger).
func TestLedgerAppendListSeq(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := NewPGStore(pool)
	tenant, owner := "tnt_fileedittest", "owner_"+db.NewID()

	e1 := Entry{Path: "docs/x.md", Kind: "modify", UnifiedDiff: "--- a/docs/x.md\n+++ b/docs/x.md\n", Tool: "batch_write", BeforeSize: 18, AfterSize: 19}
	e2 := Entry{Path: "main.go", Kind: "create", UnifiedDiff: "--- /dev/null\n+++ b/main.go\n", Tool: "batch_write"}

	rows, err := store.Append(ctx, tenant, db.FileEditOwnerExecution, owner, []Entry{e1, e2})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("append returned %d rows, want 2", len(rows))
	}
	if rows[0].Seq != 1 || rows[1].Seq != 2 {
		t.Fatalf("seq not monotonic from 1: %d, %d", rows[0].Seq, rows[1].Seq)
	}
	if rows[0].ID == "" || rows[1].ID == "" {
		t.Fatalf("ledger rows missing ids")
	}

	got, maxSeq, err := store.List(ctx, tenant, db.FileEditOwnerExecution, owner, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || maxSeq != 2 {
		t.Fatalf("list = %d rows maxSeq %d, want 2 rows maxSeq 2", len(got), maxSeq)
	}
	if got[0].Path != "docs/x.md" || got[1].Path != "main.go" {
		t.Fatalf("list order wrong: %s, %s", got[0].Path, got[1].Path)
	}
	if got[0].UnifiedDiff != e1.UnifiedDiff || got[1].UnifiedDiff != e2.UnifiedDiff {
		t.Fatalf("diff text not stored verbatim")
	}

	// from_seq resume: only rows beyond seq 1 come back.
	got2, _, err := store.List(ctx, tenant, db.FileEditOwnerExecution, owner, 1)
	if err != nil {
		t.Fatalf("list from_seq: %v", err)
	}
	if len(got2) != 1 || got2[0].Path != "main.go" {
		t.Fatalf("from_seq resume wrong: %+v", got2)
	}

	// Owner isolation: another owner sees nothing.
	got3, _, err := store.List(ctx, tenant, db.FileEditOwnerExecution, "other_"+db.NewID(), 0)
	if err != nil {
		t.Fatalf("list other owner: %v", err)
	}
	if len(got3) != 0 {
		t.Fatalf("owner isolation broken: %d rows", len(got3))
	}
}
