package worker

// republish_test.go — the ATOMIC save: `republish` on UpdateWorkerVersion and
// `publish` on CreateWorkerVersion.
//
// THE CONTRACT THESE PIN. Editing a worker used to be three client gestures —
// revert the published version to draft, save, publish again — with two ways to
// end up wrong:
//
//   - a CANCEL after the revert left the worker stranded in draft, invisible in
//     the UI and no longer dispatchable by the latest-published resolution; and
//   - a failure between save and publish did the same.
//
// The flag moves the whole sequence inside ONE server-side transaction, so the
// worker is published before and after and the intermediate draft is never
// observable to another connection. The user's four requirements, as tests:
//
//  1. edit then cancel  → nothing happens (no RPC is sent; and, server-side, a
//     FAILED save still leaves the worker published — TestRepublishFailure…)
//  2. edit then save    → republished in place, version number UNCHANGED
//  3. new version+cancel→ nothing happens (the TUI does not create on open;
//     server-side the analogue is that publish=false still only drafts, so the
//     live version is untouched — TestCreateWorkerVersionWithoutPublish…)
//  4. new version+save  → the new version IS the live one, worker published
//
// A recurring assertion here is "no draft version exists afterwards": the whole
// point of the change is that no save path can leave a stray draft behind.
//
// Skipped unless ORCHICON_TEST_DSN points at a DISPOSABLE database (repo
// convention, mirrors bulk_update_worker_model_test.go — internal/db/prompt.go
// forbids the live plane):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@localhost:5432/orchicon_scratch?sslmode=disable'
//	go test ./internal/worker/ -run 'TestRepublish|TestCreateWorkerVersion' -v

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// latestVersionID returns the id and number of the worker's newest version,
// whatever its status — the version a "save the edit" targets.
func latestVersionID(t *testing.T, pool *db.Pool, tenantID, workerID string) (string, int) {
	t.Helper()
	var id string
	var ver int
	err := pool.QueryRow(context.Background(),
		`SELECT id, version FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2
		 ORDER BY version DESC LIMIT 1`, tenantID, workerID).Scan(&id, &ver)
	if err != nil {
		t.Fatalf("query latest version for %s: %v", workerID, err)
	}
	return id, ver
}

// draftVersionCount counts the worker's draft versions. Zero is the invariant
// every save path in this file must leave behind.
func draftVersionCount(t *testing.T, pool *db.Pool, tenantID, workerID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND status = $3`,
		tenantID, workerID, domain.WorkerVersionDraft).Scan(&n)
	if err != nil {
		t.Fatalf("count drafts for %s: %v", workerID, err)
	}
	return n
}

// workerVersionRole returns a version's stored `role` prompt field — the field
// the republish tests actually change, so "the edit was saved" is observable
// independently of the status assertions.
func workerVersionRole(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int) string {
	t.Helper()
	var role string
	err := pool.QueryRow(context.Background(),
		`SELECT role FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver).Scan(&role)
	if err != nil {
		t.Fatalf("query v%d role for %s: %v", ver, workerID, err)
	}
	return role
}

// forceVersionStatus puts a single version into a lifecycle state the API
// cannot reach directly (there is no "deprecate this version" RPC on this
// service, only worker-level deprecation), so the deprecated-version guard can
// be exercised.
func forceVersionStatus(t *testing.T, pool *db.Pool, tenantID, workerID string, ver int, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE worker_versions SET status = $4
		 WHERE tenant_id = $1 AND worker_id = $2 AND version = $3`,
		tenantID, workerID, ver, status); err != nil {
		t.Fatalf("force v%d of %s to %s: %v", ver, workerID, status, err)
	}
}

// assertNoDrafts is the shared invariant: after ANY save, the worker has no
// draft version. A stray draft is what the per-gesture flow produced, and it is
// the state the operator asked us never to leave behind.
func assertNoDrafts(t *testing.T, pool *db.Pool, tenantID, workerID, after string) {
	t.Helper()
	if n := draftVersionCount(t, pool, tenantID, workerID); n != 0 {
		t.Errorf("%s: worker has %d DRAFT version(s); a save must never leave an unpublished draft behind", after, n)
	}
}

// TestRepublishPublishedVersionIsOneAtomicSave is requirement 2: editing a
// published worker and saving republishes the SAME version in place. The
// version number must not advance and current_version must not move, because
// the operator edited the current version rather than creating a new one.
func TestRepublishPublishedVersionIsOneAtomicSave(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "republish-published")
	vid, ver := latestVersionID(t, pool, tenantID, id)
	if ver != 1 {
		t.Fatalf("pre-condition: latest version = %d, want 1", ver)
	}

	publishedBefore := len(listOutboxRows(t, pool, tenantID, "worker.published"))

	resp, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		Role:      adapterStrPtr("you are the release-notes writer"),
		Republish: true,
	}))
	if err != nil {
		t.Fatalf("republish save on a published worker: %v", err)
	}

	// The response describes the version as PUBLISHED, not as the draft it was
	// briefly during the transaction: the caller must never see the
	// intermediate state.
	if got := resp.Msg.Version.GetStatus(); got != apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_PUBLISHED {
		t.Errorf("response version status = %v, want PUBLISHED (the intermediate draft must not be observable)", got)
	}
	if got := int(resp.Msg.Version.GetVersion()); got != ver {
		t.Errorf("response version number = %d, want %d (republish edits in place; it does not fork)", got, ver)
	}

	// The row is published, the edit landed, the number did not move.
	workerHasWorkerVersion(t, pool, tenantID, id, ver, domain.WorkerVersionPublished)
	if got := workerVersionRole(t, pool, tenantID, id, ver); got != "you are the release-notes writer" {
		t.Errorf("v%d role = %q, want the edited value", ver, got)
	}
	assertNoDrafts(t, pool, tenantID, id, "after a republish save")
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != ver {
		t.Errorf("current_version = %d, want %d (unchanged)", cv, ver)
	}

	// The trail says what happened: a republish, not a plain draft edit, and
	// exactly one publish event.
	if rows := listAuditRows(t, pool, tenantID, "worker.version_republished", "worker", id); len(rows) != 1 {
		t.Errorf("worker.version_republished audit rows = %d, want 1", len(rows))
	}
	if rows := listAuditRows(t, pool, tenantID, "worker.version_updated", "worker", id); len(rows) != 0 {
		t.Errorf("worker.version_updated audit rows = %d, want 0 — a republish must not be recorded as a plain draft edit", len(rows))
	}
	if got := len(listOutboxRows(t, pool, tenantID, "worker.published")) - publishedBefore; got != 1 {
		t.Errorf("worker.published outbox delta = %d, want 1", got)
	}
}

// TestRepublishDraftVersionPublishesInPlace: when the latest version is already
// a draft, a republish save updates it and publishes it — the same "one save
// and the worker is live" outcome, reached without a revert.
func TestRepublishDraftVersionPublishesInPlace(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createDraftWorker(t, ctx, s, "republish-draft")
	vid, ver := latestVersionID(t, pool, tenantID, id)

	resp, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		Role:      adapterStrPtr("draft edited and published in one save"),
		Republish: true,
	}))
	if err != nil {
		t.Fatalf("republish save on a draft-only worker: %v", err)
	}
	if got := int(resp.Msg.Version.GetVersion()); got != ver {
		t.Errorf("version number = %d, want %d (no fork)", got, ver)
	}
	workerHasWorkerVersion(t, pool, tenantID, id, ver, domain.WorkerVersionPublished)
	if got := workerVersionRole(t, pool, tenantID, id, ver); got != "draft edited and published in one save" {
		t.Errorf("v%d role = %q, want the edited value", ver, got)
	}
	assertNoDrafts(t, pool, tenantID, id, "after publishing a draft through republish")
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != ver {
		t.Errorf("current_version = %d, want %d", cv, ver)
	}
}

// TestRepublishFailureLeavesTheWorkerPublished is requirement 1's server-side
// guarantee, and the reason the whole sequence lives in one transaction.
//
// The failure is forced AFTER the revert: a model_ref longer than maxNameLen
// fails validation, and model validation runs strictly after
// RevertWorkerVersionToDraft. If the revert were not transactional (the
// per-gesture flow), the worker would now be stranded in draft — unpublished,
// invisible, and skipped by every latest-published resolution. Asserting it is
// still published is therefore a real assertion about the transaction boundary,
// not a restatement of the happy path.
func TestRepublishFailureLeavesTheWorkerPublished(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "republish-rollback")
	vid, ver := latestVersionID(t, pool, tenantID, id)
	roleBefore := workerVersionRole(t, pool, tenantID, id, ver)

	tooLong := strings.Repeat("a", 601) // maxNameLen is 500 (validate.go:35)
	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		ModelRef:  adapterStrPtr(tooLong),
		Republish: true,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversized model_ref: err = %v, want InvalidArgument (the test needs the failure to land AFTER the revert)", err)
	}

	// The revert rolled back with the rest of the transaction.
	workerHasWorkerVersion(t, pool, tenantID, id, ver, domain.WorkerVersionPublished)
	assertNoDrafts(t, pool, tenantID, id, "after a FAILED republish save")
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != ver {
		t.Errorf("current_version = %d, want %d (a failed save must not move the pointer)", cv, ver)
	}
	if got := workerVersionRole(t, pool, tenantID, id, ver); got != roleBefore {
		t.Errorf("v%d role = %q after a failed save, want %q (unchanged)", ver, got, roleBefore)
	}
}

// TestRepublishFlagAbsentKeepsTheDraftOnlyContract: without the flag,
// UpdateWorkerVersion behaves exactly as before — a published version is
// refused. Callers that did not opt in must not gain the ability to write to a
// published version.
func TestRepublishFlagAbsentKeepsTheDraftOnlyContract(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "republish-optin")
	vid, ver := latestVersionID(t, pool, tenantID, id)
	roleBefore := workerVersionRole(t, pool, tenantID, id, ver)

	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		Role:      adapterStrPtr("this must not be written"),
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("published version without republish: err = %v, want FailedPrecondition", err)
	}
	workerHasWorkerVersion(t, pool, tenantID, id, ver, domain.WorkerVersionPublished)
	if got := workerVersionRole(t, pool, tenantID, id, ver); got != roleBefore {
		t.Errorf("v%d role = %q, want unchanged %q", ver, got, roleBefore)
	}
	assertNoDrafts(t, pool, tenantID, id, "after a refused non-republish edit")
}

// TestRepublishRejectsDeprecatedVersion: a deprecated version cannot be
// reverted to draft (RevertWorkerVersionToDraft only matches 'published'), so
// republish must refuse rather than silently do something else. The refusal is
// the original FailedPrecondition — no half-applied edit.
func TestRepublishRejectsDeprecatedVersion(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "republish-deprecated")
	vid, ver := latestVersionID(t, pool, tenantID, id)
	forceVersionStatus(t, pool, tenantID, id, ver, domain.WorkerVersionDeprecated)

	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		Role:      adapterStrPtr("must not land"),
		Republish: true,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("republish on a DEPRECATED version: err = %v, want FailedPrecondition", err)
	}
	workerHasWorkerVersion(t, pool, tenantID, id, ver, domain.WorkerVersionDeprecated)
	if got := workerVersionRole(t, pool, tenantID, id, ver); got != "" {
		t.Errorf("v%d role = %q, want untouched", ver, got)
	}
}

// TestCreateWorkerVersionPublishMakesItLive is requirement 4: a new version
// saved with publish becomes the live version in one call — the number
// advances, the worker is published, and no draft is left over.
func TestCreateWorkerVersionPublishMakesItLive(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "create-publish-live")

	publishedBefore := len(listOutboxRows(t, pool, tenantID, "worker.published"))

	resp, err := s.CreateWorkerVersion(ctx, connect.NewRequest(&apiv1.CreateWorkerVersionRequest{
		WorkerId: id,
		Role:     adapterStrPtr("the second version"),
		Publish:  true,
	}))
	if err != nil {
		t.Fatalf("CreateWorkerVersion with publish: %v", err)
	}
	if got := int(resp.Msg.Version.GetVersion()); got != 2 {
		t.Errorf("new version number = %d, want 2", got)
	}
	if got := resp.Msg.Version.GetStatus(); got != apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_PUBLISHED {
		t.Errorf("new version status = %v, want PUBLISHED", got)
	}

	// v1 stays published (it is history), v2 is published and current.
	workerHasWorkerVersion(t, pool, tenantID, id, 1, domain.WorkerVersionPublished)
	workerHasWorkerVersion(t, pool, tenantID, id, 2, domain.WorkerVersionPublished)
	assertNoDrafts(t, pool, tenantID, id, "after creating a version with publish")
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != 2 {
		t.Errorf("current_version = %d, want 2 (follows the newly published version)", cv)
	}
	if got := workerVersionRole(t, pool, tenantID, id, 2); got != "the second version" {
		t.Errorf("v2 role = %q, want the edited value", got)
	}
	// Both facts are in the trail: the version was created AND published.
	if rows := listAuditRows(t, pool, tenantID, "worker.version_created", "worker", id); len(rows) != 1 {
		t.Errorf("worker.version_created audit rows = %d, want 1", len(rows))
	}
	if rows := listAuditRows(t, pool, tenantID, "worker.published", "worker", id); len(rows) != 2 {
		t.Errorf("worker.published audit rows = %d, want 2 (v1 setup + v2)", len(rows))
	}
	if got := len(listOutboxRows(t, pool, tenantID, "worker.published")) - publishedBefore; got != 1 {
		t.Errorf("worker.published outbox delta = %d, want 1", got)
	}
}

// TestCreateWorkerVersionWithoutPublishStillDrafts is requirement 3's
// server-side half. The TUI satisfies "new version, then cancel" by simply not
// calling this RPC until save — so a cancel creates nothing at all. This test
// pins the behaviour that makes that safe: with publish absent the call is
// UNCHANGED, drafting a version and leaving current_version pointing at the
// still-live published one.
func TestCreateWorkerVersionWithoutPublishStillDrafts(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWorker(t, ctx, s, "create-draft-only")

	resp, err := s.CreateWorkerVersion(ctx, connect.NewRequest(&apiv1.CreateWorkerVersionRequest{
		WorkerId: id,
		Role:     adapterStrPtr("a draft nobody published"),
	}))
	if err != nil {
		t.Fatalf("CreateWorkerVersion without publish: %v", err)
	}
	if got := resp.Msg.Version.GetStatus(); got != apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_DRAFT {
		t.Errorf("status = %v, want DRAFT — publish was not requested", got)
	}
	if n := draftVersionCount(t, pool, tenantID, id); n != 1 {
		t.Errorf("draft versions = %d, want 1", n)
	}
	// The live version is untouched, which is what "the latest is still in
	// published form" means when a draft does exist.
	workerHasWorkerVersion(t, pool, tenantID, id, 1, domain.WorkerVersionPublished)
	if cv := workerCurrentVersion(t, pool, tenantID, id); cv != 1 {
		t.Errorf("current_version = %d, want 1 (the draft is not live)", cv)
	}
}
