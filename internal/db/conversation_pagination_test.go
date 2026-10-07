package db_test

// conversation_pagination_test.go — the conversation list's keyset pagination must be
// LOSSLESS. It is what the TUI rail now pages with, and a lossy cursor silently drops rows
// the operator never learns about.
//
// The specific hazard: `now()` is the TRANSACTION timestamp, so rows written in ONE
// transaction share `updated_at` EXACTLY. The cursor predicate used to be a strict
// `updated_at < cursor.updated_at`, which every tied row fails — so a page boundary landing
// inside a tie group skipped the rest of that group. The fix compares the whole tuple,
// `(updated_at, id) < (cursor.updated_at, cursor.id)`, which is a total order.
//
// The rows this creates are DELETED in cleanup: a DB-backed test must not leave
// conversations in the tenant it ran against (that is how the rail's newest-100 window got
// filled with test rows in the first place).
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestListConversationsPagination -v

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// deleteProbeConversations removes exactly the rows a run created, and COMMITS. The delete
// is a tenant-scoped statement (RLS covers these tables, so a bare pool.Exec sees nothing),
// and it must be committed — rolling back the transaction would discard the DELETE and leave
// the probe rows in the tenant, which is the pollution that filled the rail's window.
func deleteProbeConversations(t *testing.T, pool *db.Pool, tenant string, ids []string) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	ctx := context.Background()
	ptx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Errorf("cleanup: begin tenant tx: %v", err)
		return
	}
	for _, id := range ids {
		if _, err := ptx.Exec(ctx, `DELETE FROM ask_orchicon_conversations WHERE tenant_id = $1 AND id = $2`, tenant, id); err != nil {
			t.Errorf("cleanup: delete %s: %v", id, err)
		}
	}
	if err := ptx.Commit(ctx); err != nil {
		t.Errorf("cleanup: commit: %v", err)
	}
}

func TestListConversationsPaginationLosesNothingAcrossTies(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_dev"

	// FIVE conversations created in ONE transaction, so every one of them carries the SAME
	// updated_at. This is not contrived: the E2E fixtures write conversations in batches, and
	// any bulk create shares a transaction timestamp.
	const n = 5
	ids := make([]string, 0, n)
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	for i := 0; i < n; i++ {
		row, err := db.CreateConversation(ctx, ttx.Tx, db.ConversationRow{
			ID:       db.NewID(),
			TenantID: tenant,
			Title:    "pagination tie probe",
		})
		if err != nil {
			_ = ttx.Rollback(ctx)
			t.Fatalf("create conversation %d: %v", i, err)
		}
		ids = append(ids, row.ID)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	t.Cleanup(func() {
		deleteProbeConversations(t, pool, tenant, ids)
	})

	// Confirm the premise: THESE rows really do share a timestamp. Scoped by id, not by
	// title — a leftover row from an earlier run (or a concurrent one) would otherwise make
	// this read as "not tied" and quietly retire the guard.
	var distinct int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT updated_at) FROM ask_orchicon_conversations
		  WHERE tenant_id = $1 AND id = ANY($2::text[])`,
		tenant, ids).Scan(&distinct); err != nil {
		t.Fatalf("count distinct updated_at: %v", err)
	}
	if distinct != 1 {
		t.Fatalf("the probe rows have %d distinct updated_at values, want 1 — this test is only meaningful when they TIE", distinct)
	}

	// Page with a limit that lands INSIDE the tie group, and walk to exhaustion the way the
	// rail does.
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 10; page++ {
		// A fresh tenant tx per page, exactly as the service does per request.
		ptx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatalf("page %d: begin tenant tx: %v", page, err)
		}
		rows, err := db.ListConversations(ctx, ptx.Tx, tenant, 2, cursor)
		_ = ptx.Rollback(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Errorf("conversation %s was returned TWICE — the cursor is not a total order", r.ID)
			}
			seen[r.ID] = true
		}
		cursor = rows[len(rows)-1].ID
	}

	for _, id := range ids {
		if !seen[id] {
			t.Errorf("conversation %s was SKIPPED by pagination — a row tied with the page boundary was lost", id)
		}
	}
}
