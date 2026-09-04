package worker

// Service-level tests for BulkUpdateWorkerModel: per-worker branching
// (draft-in-place, revert+republish), skip reasons (deprecated, retired,
// not-found, no published version), and validation. Skipped unless
// ORCHICON_TEST_DSN points at a disposable database:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/worker/ -run TestBulkUpdateWorkerModel -v

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// bulkTestPool opens a migration-applied test pool (repo convention:
func bulkTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed bulk test")
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
	return pool
}

// seedBulkIdentity creates the actor the audit FK points at.
func seedBulkIdentity(t *testing.T, pool *db.Pool, tenantID, subject string) db.IdentityRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ident, _, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, subject, "Bulk Actor", "user")
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit identity: %v", err)
	}
	return ident
}

// bulkEnv opens a migration-applied pool, seeds an identity, and returns
// the service + ctx. Each test gets its own tenant so audit rows from
// sibling tests never bleed across.
func bulkEnv(t *testing.T) (*db.Pool, *Service, context.Context, string) {
	t.Helper()
	pool := bulkTestPool(t)
	tenantID := "tnt_bulk_" + strings.ToLower(db.NewID())
	ident := seedBulkIdentity(t, pool, tenantID, "bulk-wk-"+strings.ToLower(db.NewID()))
	ctx := tenant.WithID(context.Background(), tenantID)
	ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
		IdentityID: ident.ID,
		TenantID:   tenantID,
		Subject:    ident.Subject,
		AuthMethod: "oidc",
		IsAdmin:    true,
	})
	s := New(pool, slog.New(slog.DiscardHandler))
	return pool, s, ctx, tenantID
}

// createDraftWorker creates a worker whose only version is a draft.
func createDraftWorker(t *testing.T, ctx context.Context, s *Service, name string) string {
	t.Helper()
	resp, err := s.CreateWorker(ctx, connect.NewRequest(&apiv1.CreateWorkerRequest{
		Name:       name,
		ModelRef:   "opencode/deepseek-v4-flash",
	}))
	if err != nil {
		t.Fatalf("CreateWorker %s: %v", name, err)
	}
	return resp.Msg.Worker.Id
}

// createPublishedWorker creates a worker that is published with v1 and
// no draft on top.
func createPublishedWorker(t *testing.T, ctx context.Context, s *Service, name string) string {
	t.Helper()
	id := createDraftWorker(t, ctx, s, name)
	if _, err := s.PublishWorkerVersion(ctx, connect.NewRequest(&apiv1.PublishWorkerVersionRequest{
		WorkerId: id,
	})); err != nil {
		t.Fatalf("PublishWorkerVersion %s: %v", name, err)
	}
	return id
}

// createPublishedWithDraft creates a worker that is published (v1) and
// then has a fresh draft v2 stacked on top.
func createPublishedWithDraft(t *testing.T, ctx context.Context, s *Service, name string) string {
	t.Helper()
	id := createPublishedWorker(t, ctx, s, name)
	if _, err := s.CreateWorkerVersion(ctx, connect.NewRequest(&apiv1.CreateWorkerVersionRequest{
		WorkerId: id,
	})); err != nil {
		t.Fatalf("CreateWorkerVersion %s: %v", name, err)
	}
	return id
}

// workerLatestVersionStatus returns the latest version's status for a worker.
func workerLatestVersionStatus(t *testing.T, pool *db.Pool, tenantID, workerID string) (int, string) {
	t.Helper()
	var ver int
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT version, status FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2
		 ORDER BY version DESC LIMIT 1`, tenantID, workerID).Scan(&ver, &status)
	if err != nil {
		t.Fatalf("query latest version for %s: %v", workerID, err)
	}
	return ver, status
}

// workerVersionStatus returns the status of the worker's version with the
// given number.
func workerVersionStatus(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int) (int, string) {
	t.Helper()
	var version int
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT version, status FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver).Scan(&version, &status)
	if err != nil {
		t.Fatalf("query v%d for %s: %v", ver, workerID, err)
	}
	return version, status
}

func workerVersionModel(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int) string {
	t.Helper()
	var model string
	err := pool.QueryRow(context.Background(),
		`SELECT model_ref FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver).Scan(&model)
	if err != nil {
		t.Fatalf("query v%d model for %s: %v", ver, workerID, err)
	}
	return model
}

func workerCurrentVersion(t *testing.T, pool *db.Pool, tenantID, workerID string) int {
	t.Helper()
	var cv int
	err := pool.QueryRow(context.Background(),
		`SELECT current_version FROM workers WHERE tenant_id = $1 AND id = $2`,
		tenantID, workerID).Scan(&cv)
	if err != nil {
		t.Fatalf("query current_version for %s: %v", workerID, err)
	}
	return cv
}

// forceWorkerStatus mutates a worker into the given lifecycle state for
// the tests that need to bypass the lifecycle (deprecated/retired
// transitions can't be reached from CreateWorker → PublishWorkerVersion).
func forceWorkerStatus(t *testing.T, pool *db.Pool, tenantID, workerID, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE workers SET status = $3, updated_at = now(), version = version + 1
		 WHERE tenant_id = $1 AND id = $2`, tenantID, workerID, status); err != nil {
		t.Fatalf("force worker %s to %s: %v", workerID, status, err)
	}
}

// workerHasWorkerVersion asserts the worker has a version with the given
// status.
func workerHasWorkerVersion(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int, want string) {
	t.Helper()
	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver).Scan(&status)
	if err != nil {
		t.Fatalf("query v%d for %s: %v", ver, workerID, err)
	}
	if status != want {
		t.Errorf("worker %s v%d: status = %s, want %s", workerID, ver, status, want)
	}
}

// resultByWorker returns the per-worker outcome for a given worker id.
func resultByWorker(results []*apiv1.BulkUpdateWorkerModelResult, workerID string) *apiv1.BulkUpdateWorkerModelResult {
	for _, r := range results {
		if r.WorkerId == workerID {
			return r
		}
	}
	return nil
}

// TestBulkUpdateValidation: empty ids / too many ids / blank model_ref all
// rejected with InvalidArgument.
func TestBulkUpdateValidation(t *testing.T) {
	pool, s, ctx, _ := bulkEnv(t)
	_ = pool

	t.Run("empty ids", func(t *testing.T) {
		_, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
			WorkerIds: []string{},
			ModelRef:  "anthropic/claude-sonnet-4",
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("empty ids: err = %v, want InvalidArgument", err)
		}
	})

	t.Run("blank model_ref", func(t *testing.T) {
		_, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
			WorkerIds: []string{"w_anything"},
			ModelRef:  "   ",
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("blank model_ref: err = %v, want InvalidArgument", err)
		}
	})

	t.Run("too many ids", func(t *testing.T) {
		ids := make([]string, 101)
		for i := range ids {
			ids[i] = "w_x"
		}
		_, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
			WorkerIds: ids,
			ModelRef:  "anthropic/claude-sonnet-4",
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf(">100 ids: err = %v, want InvalidArgument", err)
		}
	})
}

// TestBulkUpdateDraftInPlace: a worker with only a draft version has its
// model_ref updated in place and the version is published. Version number
// is unchanged.
func TestBulkUpdateDraftInPlace(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "bulk-draft-in-place")

	resp, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: []string{id},
		ModelRef:  "anthropic/claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("BulkUpdateWorkerModel: %v", err)
	}
	if len(resp.Msg.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Msg.Results))
	}
	r := resp.Msg.Results[0]
	if got := r.Outcome.(*apiv1.BulkUpdateWorkerModelResult_Updated).Updated; got.Version != 1 || got.ModelRef != "anthropic/claude-sonnet-4" {
		t.Fatalf("Updated outcome = %+v, want version=1 model_ref=anthropic/claude-sonnet-4", got)
	}

	ver, status := workerVersionStatus(t, pool, tenantID, id, 1)
	if ver != 1 {
		t.Fatalf("version number = %d, want 1 (no fork)", ver)
	}
	if status != domain.WorkerVersionPublished {
		t.Errorf("latest version status = %s, want published", status)
	}
	if got := workerVersionModel(t, pool, tenantID, id, 1); got != "anthropic/claude-sonnet-4" {
		t.Errorf("v1 model_ref = %q, want anthropic/claude-sonnet-4", got)
	}
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != 1 {
		t.Errorf("current_version = %d, want 1", cv)
	}
}

// TestBulkUpdateRevertAndRepublish: a worker that is published with no
// draft has its latest published version reverted to draft, updated, and
// republished — same version number, current_version unchanged.
func TestBulkUpdateRevertAndRepublish(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "bulk-revert-republish")
	// Sanity: published v1 with no draft.
	if _, status := workerVersionStatus(t, pool, tenantID, id, 1); status != domain.WorkerVersionPublished {
		t.Fatalf("pre-condition: v1 status = %s, want published", status)
	}

	resp, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: []string{id},
		ModelRef:  "anthropic/claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("BulkUpdateWorkerModel: %v", err)
	}
	if got := resp.Msg.Results[0].Outcome.(*apiv1.BulkUpdateWorkerModelResult_Updated).Updated; got.Version != 1 {
		t.Fatalf("Updated outcome version = %d, want 1 (no fork)", got.Version)
	}

	ver, status := workerVersionStatus(t, pool, tenantID, id, 1)
	if ver != 1 {
		t.Fatalf("version number = %d, want 1 (no fork)", ver)
	}
	if status != domain.WorkerVersionPublished {
		t.Errorf("v1 status = %s, want published", status)
	}
	if got := workerVersionModel(t, pool, tenantID, id, 1); got != "anthropic/claude-sonnet-4" {
		t.Errorf("v1 model_ref = %q, want anthropic/claude-sonnet-4", got)
	}
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != 1 {
		t.Errorf("current_version = %d, want 1 (unchanged)", cv)
	}
}

// TestBulkUpdatePublishedWithDraft: a worker that has both a published v1
// and a draft v2 stacked on top has the LATEST version (v2, draft)
// edited in place and published — matching the manual edit flow. Version
// 2 is the affected version (not v1).
func TestBulkUpdatePublishedWithDraft(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWithDraft(t, ctx, s, "bulk-published-with-draft")

	resp, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: []string{id},
		ModelRef:  "anthropic/claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("BulkUpdateWorkerModel: %v", err)
	}
	if got := resp.Msg.Results[0].Outcome.(*apiv1.BulkUpdateWorkerModelResult_Updated).Updated; got.Version != 2 {
		t.Fatalf("Updated outcome version = %d, want 2 (latest draft)", got.Version)
	}
	// v2 is now published.
	workerHasWorkerVersion(t, pool, tenantID, id, 2, domain.WorkerVersionPublished)
	// v1 stays published — no revert happened.
	workerHasWorkerVersion(t, pool, tenantID, id, 1, domain.WorkerVersionPublished)
	if got := workerVersionModel(t, pool, tenantID, id, 2); got != "anthropic/claude-sonnet-4" {
		t.Errorf("v2 model_ref = %q, want anthropic/claude-sonnet-4", got)
	}
	// v1 model_ref untouched.
	if got := workerVersionModel(t, pool, tenantID, id, 1); got != "opencode/deepseek-v4-flash" {
		t.Errorf("v1 model_ref = %q, want opencode/deepseek-v4-flash (untouched)", got)
	}
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != 2 {
		t.Errorf("current_version = %d, want 2 (follows latest published)", cv)
	}
}

// TestBulkUpdateSkipReasons: a single batch with deprecated / retired /
// not-found / draft workers produces one updated + multiple skipped
// outcomes; ordering preserved; per-worker tx commits independently.
func TestBulkUpdateSkipReasons(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)

	// worker A: draft → updated.
	draftID := createDraftWorker(t, ctx, s, "bulk-skip-draft")
	// worker B: published → updated.
	publishedID := createPublishedWorker(t, ctx, s, "bulk-skip-published")
	// worker C: deprecated → skipped (DEPRECATED).
	deprecatedID := createPublishedWorker(t, ctx, s, "bulk-skip-deprecated")
	forceWorkerStatus(t, pool, tenantID, deprecatedID, domain.WorkerDeprecated)
	// worker D: retired → skipped (RETIRED). To retire a worker it must
	// pass through deprecated first; the force path short-circuits the
	// lifecycle for the test.
	retiredID := createPublishedWorker(t, ctx, s, "bulk-skip-retired")
	forceWorkerStatus(t, pool, tenantID, retiredID, domain.WorkerRetired)
	// worker E: nonexistent → skipped (NOT_FOUND).
	ghostID := "w_ghost"

	ids := []string{draftID, publishedID, deprecatedID, retiredID, ghostID}
	resp, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: ids,
		ModelRef:  "anthropic/claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("BulkUpdateWorkerModel: %v", err)
	}
	if len(resp.Msg.Results) != 5 {
		t.Fatalf("results = %d, want 5", len(resp.Msg.Results))
	}
	if resp.Msg.UpdatedCount != 2 {
		t.Errorf("updated_count = %d, want 2", resp.Msg.UpdatedCount)
	}
	if resp.Msg.SkippedCount != 3 {
		t.Errorf("skipped_count = %d, want 3 (deprecated + retired + not-found)", resp.Msg.SkippedCount)
	}
	if resp.Msg.ErrorCount != 0 {
		t.Errorf("error_count = %d, want 0", resp.Msg.ErrorCount)
	}

	// Ordering preserved: results[i].worker_id == ids[i].
	for i, want := range ids {
		if got := resp.Msg.Results[i].WorkerId; got != want {
			t.Errorf("results[%d].worker_id = %q, want %q", i, got, want)
		}
	}

	// draft and published: Updated.
	if _, ok := resultByWorker(resp.Msg.Results, draftID).Outcome.(*apiv1.BulkUpdateWorkerModelResult_Updated); !ok {
		t.Errorf("draft worker outcome = %T, want Updated", resultByWorker(resp.Msg.Results, draftID).Outcome)
	}
	if _, ok := resultByWorker(resp.Msg.Results, publishedID).Outcome.(*apiv1.BulkUpdateWorkerModelResult_Updated); !ok {
		t.Errorf("published worker outcome = %T, want Updated", resultByWorker(resp.Msg.Results, publishedID).Outcome)
	}

	// deprecated → DEPRECATED skipped.
	dep := resultByWorker(resp.Msg.Results, deprecatedID)
	if sk, ok := dep.Outcome.(*apiv1.BulkUpdateWorkerModelResult_Skipped); !ok {
		t.Errorf("deprecated outcome = %T, want Skipped", dep.Outcome)
	} else if sk.Skipped.Reason != apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_DEPRECATED {
		t.Errorf("deprecated reason = %v, want DEPRECATED", sk.Skipped.Reason)
	}
	// deprecated worker model_ref unchanged.
	if got := workerVersionModel(t, pool, tenantID, deprecatedID, 1); got != "opencode/deepseek-v4-flash" {
		t.Errorf("deprecated worker v1 model_ref = %q, want untouched", got)
	}

	// retired → RETIRED skipped.
	ret := resultByWorker(resp.Msg.Results, retiredID)
	if sk, ok := ret.Outcome.(*apiv1.BulkUpdateWorkerModelResult_Skipped); !ok {
		t.Errorf("retired outcome = %T, want Skipped", ret.Outcome)
	} else if sk.Skipped.Reason != apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_RETIRED {
		t.Errorf("retired reason = %v, want RETIRED", sk.Skipped.Reason)
	}

	// not-found → NOT_FOUND skipped.
	gh := resultByWorker(resp.Msg.Results, ghostID)
	if sk, ok := gh.Outcome.(*apiv1.BulkUpdateWorkerModelResult_Skipped); !ok {
		t.Errorf("ghost outcome = %T, want Skipped", gh.Outcome)
	} else if sk.Skipped.Reason != apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NOT_FOUND {
		t.Errorf("ghost reason = %v, want NOT_FOUND", sk.Skipped.Reason)
	}
}

// TestBulkUpdatePartialSuccessIsolation: a partial-success batch leaves
// the successful workers' state committed even when one worker id is
// unknown.
func TestBulkUpdatePartialSuccessIsolation(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	live := createDraftWorker(t, ctx, s, "bulk-isolation-live")

	resp, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: []string{live, "w_missing"},
		ModelRef:  "anthropic/claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("BulkUpdateWorkerModel: %v", err)
	}
	if resp.Msg.UpdatedCount != 1 || resp.Msg.SkippedCount != 1 {
		t.Fatalf("counts: updated=%d skipped=%d, want 1/1", resp.Msg.UpdatedCount, resp.Msg.SkippedCount)
	}
	if got := workerVersionModel(t, pool, tenantID, live, 1); got != "anthropic/claude-sonnet-4" {
		t.Errorf("live v1 model_ref = %q, want committed even with a not-found peer", got)
	}
}

// TestBulkUpdateAuditAndOutbox: each successful mutation writes a
// worker.published outbox row and a worker.bulk_model_updated audit row;
// the batch as a whole writes exactly one worker.bulk_model_updated_batch
// audit row; skipped workers write nothing.
func TestBulkUpdateAuditAndOutbox(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)

	draftID := createDraftWorker(t, ctx, s, "bulk-audit-draft")
	publishedID := createPublishedWorker(t, ctx, s, "bulk-audit-published")
	deprecatedID := createPublishedWorker(t, ctx, s, "bulk-audit-deprecated")
	forceWorkerStatus(t, pool, tenantID, deprecatedID, domain.WorkerDeprecated)

	// Capture the outbox state BEFORE the bulk call so the post-assertion
	// can pin down exactly the rows the bulk operation added. The bulk
	// path re-uses the existing worker.published event type (same as
	// PublishWorkerVersion), so a plain post-count would conflate setup
	// events with bulk-emitted ones.
	beforePublished := len(listOutboxRows(t, pool, tenantID, "worker.published"))

	_, err := s.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: []string{draftID, publishedID, deprecatedID},
		ModelRef:  "anthropic/claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("BulkUpdateWorkerModel: %v", err)
	}

	// Per-mutation audit: exactly 2 worker.bulk_model_updated rows (one
	// per successful mutation). This action name is unique to the bulk
	// path so the count is exact.
	rows := listAuditRows(t, pool, tenantID, "worker.bulk_model_updated", "worker", "")
	if len(rows) != 2 {
		t.Errorf("worker.bulk_model_updated rows = %d, want 2", len(rows))
	}
	// Each successful mutation also fires a worker.published outbox event
	// (same envelope PublishWorkerVersion emits). Assert the delta rather
	// than the total so we don't conflate setup events with bulk events.
	afterPublished := listOutboxRows(t, pool, tenantID, "worker.published")
	if got := len(afterPublished) - beforePublished; got != 2 {
		t.Errorf("worker.published outbox delta = %d, want 2", got)
	}
	// Skipped workers must NOT appear in the bulk-emitted rows: the
	// bulk-only audit list above is already exact, so for the outbox
	// assert no deprecatedID row was *added* during the bulk call by
	// checking that the post-list for the deprecated worker has not
	// grown. The simplest way is to verify deprecatedID is absent from
	// the post-call list — which can only be true if setup did not
	// publish it AND bulk did not write to it. Setup does publish it,
	// so instead we confirm the deprecated worker's id never appears in
	// the bulk-only audit rows.
	for _, r := range rows {
		if r == deprecatedID {
			t.Errorf("deprecated worker should not appear in bulk audit rows")
		}
	}
	for _, id := range afterPublished {
		if id == deprecatedID {
			// Only acceptable if this row is one of the 2 setup
			// publishes (createPublishedWorker for deprecatedID).
			// We can't tell from here, so tighten by checking the
			// bulk-emitted audit rows only — already done above.
			_ = id
		}
	}

	// Exactly one batch summary audit row, target=worker (entity kind) +
	// target_id=tenant (no single worker applies; mirrors BatchDeleteExecutions).
	batchRows := listAuditRows(t, pool, tenantID, "worker.bulk_model_updated_batch", "worker", tenantID)
	if len(batchRows) != 1 {
		t.Errorf("worker.bulk_model_updated_batch rows = %d, want 1", len(batchRows))
	}
}

// listAuditRows returns (worker_id list) of audit rows matching the
// filter. targetID is matched exactly when non-empty.
func listAuditRows(t *testing.T, pool *db.Pool, tenantID, action, targetType, targetID string) []string {
	t.Helper()
	q := `SELECT target_id FROM audit_events WHERE tenant_id = $1 AND action = $2 AND target_type = $3`
	args := []any{tenantID, action, targetType}
	if targetID != "" {
		q += ` AND target_id = $4`
		args = append(args, targetID)
	}
	rows, err := pool.Query(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return ids
}

// listOutboxRows returns aggregate_ids of outbox events matching the
// event_type for this tenant.
func listOutboxRows(t *testing.T, pool *db.Pool, tenantID, eventType string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT aggregate_id FROM outbox WHERE tenant_id = $1 AND event_type = $2`,
		tenantID, eventType)
	if err != nil {
		t.Fatalf("query outbox rows: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return ids
}
