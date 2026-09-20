package db

// worker_version_number_test.go — resolving a worker version BY NUMBER.
//
// WHY THIS EXISTS. Three dispatch paths carried a pinned version NUMBER and
// asked for it BY ID:
//
//	db.GetWorkerVersionByID(ctx, tx, tenantID, workerID, fmt.Sprintf("v%d", n))
//
// worker_versions.id is a ULID (NewID) and worker_versions_pkey is on id, so a
// "v3" pseudo-id matches no row — ever. Every call site reads the miss as "this
// step is not pinned" and falls back to GetLatestWorkerVersion(…, publishedOnly
// =true), so a step that pinned v3 silently ran whatever was latest published.
//
// Measured on the live plane before the fix: 360 workflow_step_runs carried a
// non-zero `_worker_version`, and 218 of them had a matching PUBLISHED row at
// that number. Those 218 were resolving to the wrong version. (The remaining
// 142 pin a number that no longer exists — stale pins against the re-seeded
// w_se_* workers, e.g. w_se_ai_approver now has no versions at all — where the
// latest-published fallback is the correct answer.)
//
// THE TEST PINS BOTH HALVES. The new number lookup must find the right row, AND
// the old "v%d" idiom must still miss — so a caller that reintroduces it gets a
// failing test that names the reason rather than a silent wrong version.
//
// Guarded by ORCHICON_TEST_DSN and SELF-CONTAINED, following
// worker_cursor_test.go's convention: the tenant, its workers and their versions
// are inserted inside a transaction that is NEVER COMMITTED, so nothing here can
// leave residue. Point it at a DISPOSABLE database — internal/db/prompt.go
// forbids the live plane.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@localhost:5432/orchicon_scratch?sslmode=disable'
//	go test ./internal/db/ -run TestWorkerVersionByNumber -v

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// workerVersionLookupTx opens a throwaway tenant with one worker and an
// interleaved version trail, all inside an uncommitted transaction.
//
// The trail is deliberately NOT a tidy 1..n: v2 is published, v3 is a draft,
// and there is no v4. A test where every version is published and contiguous
// would pass against a lookup that ignored status, and against one that
// returned the newest row regardless of the number asked for.
func workerVersionLookupTx(t *testing.T) (ctx context.Context, tx pgx.Tx, tenant, worker string) {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed worker version lookup test")
	}
	ctx = context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)

	tenant = "tnt_wver_" + NewID()[:8]
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	// ROLLED BACK, never committed: the tenant and every row below vanish with it.
	t.Cleanup(func() { _ = ttx.Rollback(ctx) })
	tx = ttx.Tx
	if _, err := ttx.Exec(ctx, `INSERT INTO tenants (id, slug, name, status) VALUES ($1,$2,$3,'active')`,
		tenant, "wver-"+NewID()[:8], "Worker Version Number Test"); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	worker = "01WVER_" + NewID()[:8]
	if _, err := ttx.Exec(ctx,
		`INSERT INTO workers (id, tenant_id, name, slug, description, purpose, role_ref, status,
			current_version, version, created_by)
		 VALUES ($1,$2,'Versioned', 'versioned','','','','published',2,1,'test')`,
		worker, tenant); err != nil {
		t.Fatalf("insert worker: %v", err)
	}

	// v1 superseded, v2 the live one, v3 an abandoned edit (draft).
	versions := []struct {
		id      string
		number  int
		status  string
		modelRe string
	}{
		{"01WVERV1_" + NewID()[:8], 1, "published", "orchicon/commandcode/one"},
		{"01WVERV2_" + NewID()[:8], 2, "published", "orchicon/commandcode/two"},
		{"01WVERV3_" + NewID()[:8], 3, "draft", "orchicon/commandcode/three"},
	}
	for _, v := range versions {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_versions (id, tenant_id, worker_id, version, status, model_ref)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			v.id, tenant, worker, v.number, v.status, v.modelRe); err != nil {
			t.Fatalf("insert version v%d: %v", v.number, err)
		}
	}
	return ctx, ttx.Tx, tenant, worker
}

// TestWorkerVersionByNumberResolvesThePinnedVersion is the core property: the
// number asked for is the number returned, whatever the newest row happens to be.
func TestWorkerVersionByNumberResolvesThePinnedVersion(t *testing.T) {
	ctx, tx, tenant, worker := workerVersionLookupTx(t)

	for _, want := range []struct {
		number  int
		modelRe string
	}{
		{1, "orchicon/commandcode/one"},
		{2, "orchicon/commandcode/two"},
	} {
		got, err := GetWorkerVersionByNumber(ctx, tx, tenant, worker, want.number)
		if err != nil {
			t.Fatalf("v%d: %v", want.number, err)
		}
		if got.Version != want.number {
			t.Errorf("asked for v%d, got v%d — the lookup must key on the version NUMBER", want.number, got.Version)
		}
		if got.ModelRef != want.modelRe {
			t.Errorf("v%d: model_ref = %q, want %q — a pin must return THAT version's content, not the latest row's",
				want.number, got.ModelRef, want.modelRe)
		}
	}
}

// TestWorkerVersionByNumberIgnoresDrafts: a draft is not dispatchable, so a
// pinned number that exists only as a draft must NOT resolve. This is the guard
// that keeps an abandoned edit (the state the TUI must never leave behind) from
// becoming the version a run executes.
func TestWorkerVersionByNumberIgnoresDrafts(t *testing.T) {
	ctx, tx, tenant, worker := workerVersionLookupTx(t)

	if _, err := GetWorkerVersionByNumber(ctx, tx, tenant, worker, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("v3 is a DRAFT and must not resolve for dispatch, got err = %v", err)
	}
}

// TestWorkerVersionByNumberMissesCleanly: an absent number, another worker's
// number, and another tenant's row are all ErrNotFound — the documented signal
// for the callers' latest-published fallback. A miss must never surface as a
// random row.
func TestWorkerVersionByNumberMissesCleanly(t *testing.T) {
	ctx, tx, tenant, worker := workerVersionLookupTx(t)

	if _, err := GetWorkerVersionByNumber(ctx, tx, tenant, worker, 4); !errors.Is(err, ErrNotFound) {
		t.Errorf("v4 does not exist, want ErrNotFound, got %v", err)
	}
	if _, err := GetWorkerVersionByNumber(ctx, tx, tenant, "01WVER_nobody", 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("another worker's v2 must not resolve, got %v", err)
	}
	if _, err := GetWorkerVersionByNumber(ctx, tx, "tnt_wver_other", worker, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant's v2 must not resolve, got %v", err)
	}
}

// TestWorkerVersionByIDCannotResolveAVersionNumber is the REGRESSION PIN for the
// original defect: it reproduces the exact call the three dispatch paths made,
// against the exact data shape they faced, and asserts it misses.
//
// It is intentionally testing BROKEN-INPUT behaviour rather than the fix. If
// someone later "simplifies" a call site back to the id form — or makes
// GetWorkerVersionByID fall back to a number match, which would silently change
// the meaning of every genuine id lookup in the service layer — this test fails
// and says why.
func TestWorkerVersionByIDCannotResolveAVersionNumber(t *testing.T) {
	ctx, tx, tenant, worker := workerVersionLookupTx(t)

	// v2 exists and is published, so this is not a "no such version" case: the
	// only reason it can miss is that "v2" is not an id.
	if _, err := GetWorkerVersionByNumber(ctx, tx, tenant, worker, 2); err != nil {
		t.Fatalf("precondition: v2 must resolve by number, got %v", err)
	}
	pseudoID := fmt.Sprintf("v%d", 2)
	if _, err := GetWorkerVersionByID(ctx, tx, tenant, worker, pseudoID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWorkerVersionByID(%q) must MISS — worker_versions.id is a ULID, so a \"v%%d\" pseudo-id "+
			"never matched a row and every pinned version silently degraded to latest-published. "+
			"Use GetWorkerVersionByNumber for a pinned version number. got err = %v", pseudoID, err)
	}
}
