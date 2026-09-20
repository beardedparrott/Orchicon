package db

// worker_cursor_test.go — the WORKER LIST's page cursor.
//
// WHY THIS EXISTS. The operator saw the Workers pane render 30 rows with the same names repeated three
// times over, while the tenant holds 19 workers. It was not duplicated data (the table has none) and
// not a render bug: `ListWorkersWithActiveVersion` paged with a bare `id > $n` cursor while ORDERING by
// `created_at`, and those are two different orders.
//
// The dev tenant is a perfect illustration of why that is fatal: its seeded workers have ids like
// `w_se_qa_engineer`, and 'w' sorts AFTER every digit — so every one of them is "greater than" the last
// row of a created_at-ordered page. The walk returned the whole list, then those 7 again, then 4 of them
// again (measured: page1=19, page2=7, page3=4). The same mismatch also SKIPS rows: a page whose last row
// happens to sort late hides everything after it in the id order that was not already shown.
//
// THE DATA SHAPE IS THE TEST. Both failure modes need ids that disagree with the sort order, so these
// fixtures mix `w_`-prefixed ids (which sort after ULIDs) with `01…` ids, and interleave their
// created_at. A test using tidy sequential ids would pass against the broken code.
//
// Guarded by ORCHICON_TEST_DSN like every other DB-backed test in this package, and SELF-CONTAINED: the
// tenant and its workers are inserted inside a transaction that is NEVER COMMITTED, so nothing here can
// leave residue. Point it at a DISPOSABLE database — internal/db/prompt.go forbids the live plane.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@localhost:5432/orchicon_scratch?sslmode=disable'
//	go test ./internal/db/ -run TestWorkerListCursor -v

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// workerCursorTx opens a throwaway tenant inside an uncommitted transaction.
func workerCursorTx(t *testing.T) (context.Context, pgx.Tx, string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed worker cursor test")
	}
	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)

	tenant := "tnt_wcursor_" + NewID()[:8]
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	// ROLLED BACK, never committed: the tenant and its workers vanish with the transaction.
	t.Cleanup(func() { _ = ttx.Rollback(ctx) })
	if _, err := ttx.Exec(ctx, `INSERT INTO tenants (id, slug, name, status) VALUES ($1,$2,$3,'active')`,
		tenant, "wcursor-"+NewID()[:8], "Worker Cursor Test"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return ctx, ttx.Tx, tenant
}

// seedCursorWorkers inserts workers whose ID ORDER DISAGREES with their created_at order, which is the
// shape that breaks a mismatched cursor. The created_at values interleave `01…` and `w_…` ids so neither
// order contains the other.
func seedCursorWorkers(t *testing.T, ctx context.Context, tx pgx.Tx, tenant string) []string {
	t.Helper()
	rows := []struct {
		id        string
		name      string
		createdAt string
	}{
		{"01AAA_first", "Alpha", "2026-01-01 00:00:01+00"},
		{"w_zzz_second", "Zulu", "2026-01-01 00:00:02+00"},
		{"01BBB_third", "Bravo", "2026-01-01 00:00:03+00"},
		{"w_aaa_fourth", "Yankee", "2026-01-01 00:00:04+00"},
		{"01CCC_fifth", "Charlie", "2026-01-01 00:00:05+00"},
	}
	var ids []string
	for _, r := range rows {
		if _, err := tx.Exec(ctx,
			`INSERT INTO workers (id, tenant_id, name, slug, description, purpose, role_ref, status,
				current_version, version, created_by, created_at, updated_at)
			 VALUES ($1,$2,$3,$3,'','','','published',0,1,'test',$4::timestamptz,$4::timestamptz)`,
			r.id, tenant, r.name, r.createdAt); err != nil {
			t.Fatalf("insert worker %s: %v", r.id, err)
		}
		ids = append(ids, r.id)
	}
	return ids
}

// walkWorkerPages walks the list the way the service does — take the last row's id as the next token
// while the page came back full — and returns every id it saw, in order.
func walkWorkerPages(t *testing.T, ctx context.Context, tx pgx.Tx, tenant string, pageSize int) []string {
	t.Helper()
	var seen []string
	token := ""
	for page := 0; page < 50; page++ {
		rows, err := ListWorkersWithActiveVersion(ctx, tx, ListWorkersFilter{
			TenantID: tenant, PageSize: pageSize, AfterID: token,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, r := range rows {
			seen = append(seen, r.ID)
		}
		if len(rows) < pageSize {
			break
		}
		token = rows[len(rows)-1].ID
	}
	return seen
}

// assertEveryWorkerExactlyOnce is the property: a paginated walk returns each row ONCE. Both failure
// modes of a mismatched cursor break it — a repeat inflates the count, and a skip shrinks it.
func assertEveryWorkerExactlyOnce(t *testing.T, want, got []string, pageSize int) {
	t.Helper()
	count := map[string]int{}
	for _, id := range got {
		count[id]++
	}
	for _, id := range want {
		switch count[id] {
		case 1:
		case 0:
			t.Errorf("page size %d: %s was SKIPPED — the cursor walked past a row that was never shown", pageSize, id)
		default:
			t.Errorf("page size %d: %s appeared %d times — the cursor re-served a row already shown", pageSize, id, count[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("page size %d: the walk returned %d rows for %d workers (got %v)", pageSize, len(got), len(want), got)
	}
}

// TestWorkerListCursorIsStableAcrossPageSizes is the regression pin. Every page size is exercised
// because the two failure modes need different ones: a page LARGER than the row count re-serves the
// whole list (the operator's 30-of-19), while a small page SKIPS (a late-sorting row's cursor hides
// everything after it in id order).
func TestWorkerListCursorIsStableAcrossPageSizes(t *testing.T) {
	ctx, tx, tenant := workerCursorTx(t)
	want := seedCursorWorkers(t, ctx, tx, tenant)

	for _, pageSize := range []int{1, 2, 3, 4, 5, 10, 100} {
		assertEveryWorkerExactlyOnce(t, want, walkWorkerPages(t, ctx, tx, tenant, pageSize), pageSize)
	}
}

// TestWorkerListCursorHoldsUnderEachSort: the cursor has to agree with WHATEVER order the query used, so
// the same walk is checked for every supported sort key and direction. A fix that only handled the
// default would leave the same bug one keystroke away (the pane sorts by name and by status too).
func TestWorkerListCursorHoldsUnderEachSort(t *testing.T) {
	ctx, tx, tenant := workerCursorTx(t)
	want := seedCursorWorkers(t, ctx, tx, tenant)

	for _, sortBy := range []string{"", "name", "status"} {
		for _, order := range []string{"", "desc"} {
			var seen []string
			token := ""
			for page := 0; page < 50; page++ {
				rows, err := ListWorkersWithActiveVersion(ctx, tx, ListWorkersFilter{
					TenantID: tenant, PageSize: 2, AfterID: token,
					SortBy: sortBy, SortOrder: order,
				})
				if err != nil {
					t.Fatalf("sort %q/%q page %d: %v", sortBy, order, page, err)
				}
				for _, r := range rows {
					seen = append(seen, r.ID)
				}
				if len(rows) < 2 {
					break
				}
				token = rows[len(rows)-1].ID
			}
			assertEveryWorkerExactlyOnce(t, want, seen, 2)
		}
	}
}
