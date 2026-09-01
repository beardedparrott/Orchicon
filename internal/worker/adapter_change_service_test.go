package worker

// Service-level regression tests for the ADR-0005 adapter-change contract
// (QA BUG-1): a model_ref-ONLY adapter change — the picker's save path,
// which never sends the explicit adapter input (ADR-0005 D2) — must be
// accepted when the new provider/model pair is valid for the new adapter
// and rejected (InvalidArgument) when it is not. The explicit adapter
// input, when sent, must still agree with the resulting ref. Skipped
// unless ORCHICON_TEST_DSN points at a disposable database (repo
// convention, mirrors bulk_update_worker_model_test.go).

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

const (
	refOrchiconOK  = "orchicon/command-code/deepseek/deepseek-v4-flash"
	refOrchiconBad = "orchicon/anthropic/m" // anthropic is not an orchicon provider
)

// adapterStrPtr returns a pointer to s (proto3 optional string fields).
func adapterStrPtr(s string) *string { return &s }

// draftVersionID returns the id of the worker's latest draft version.
func draftVersionID(t *testing.T, pool *db.Pool, tenantID, workerID string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM worker_versions
		 WHERE tenant_id = $1 AND worker_id = $2 AND status = $3
		 ORDER BY version DESC LIMIT 1`,
		tenantID, workerID, domain.WorkerVersionDraft).Scan(&id)
	if err != nil {
		t.Fatalf("query draft version for %s: %v", workerID, err)
	}
	return id
}

// TestUpdateWorkerVersionRefOnlyAdapterChange: flipping the adapter by
// sending only model_ref (no explicit adapter input) succeeds when the new
// provider/model pair is valid for the new adapter — BUG-1 regression.
func TestUpdateWorkerVersionRefOnlyAdapterChange(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWithDraft(t, ctx, s, "adp-refonly-ok")
	vid := draftVersionID(t, pool, tenantID, id)

	resp, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		ModelRef:  adapterStrPtr(refOrchiconOK),
	}))
	if err != nil {
		t.Fatalf("ref-only adapter change rejected: %v", err)
	}
	if got := resp.Msg.Version.ModelRef; got != refOrchiconOK {
		t.Errorf("model_ref = %q, want %q", got, refOrchiconOK)
	}
	if got := resp.Msg.Version.Adapter; got != "orchicon" {
		t.Errorf("computed adapter = %q, want orchicon", got)
	}
}

// TestUpdateWorkerVersionRefOnlyAdapterChangeInvalidPair: a ref-only
// adapter change landing on a provider unknown for the new adapter is
// rejected InvalidArgument with an actionable error (ADR-0005 D4).
func TestUpdateWorkerVersionRefOnlyAdapterChangeInvalidPair(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWithDraft(t, ctx, s, "adp-refonly-bad")
	vid := draftVersionID(t, pool, tenantID, id)

	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		ModelRef:  adapterStrPtr(refOrchiconBad),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid-pair adapter change: err = %v, want InvalidArgument", err)
	}
	if err == nil || !strings.Contains(err.Error(), "orchicon") || !strings.Contains(err.Error(), "command-code") {
		t.Fatalf("invalid-pair error not actionable: %v", err)
	}
}

// TestUpdateWorkerVersionAdapterInputContract: the explicit adapter input,
// when sent, must agree with the resulting ref (D2); a matching pair
// succeeds and a mismatching pair is rejected InvalidArgument.
func TestUpdateWorkerVersionAdapterInputContract(t *testing.T) {
	pool, s, ctx, tenantID := bulkEnv(t)
	id := createPublishedWithDraft(t, ctx, s, "adp-input-mismatch")
	vid := draftVersionID(t, pool, tenantID, id)

	_, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id,
		VersionId: vid,
		ModelRef:  adapterStrPtr(refOrchiconOK),
		Adapter:   adapterStrPtr("opencode"), // disagrees with the new ref
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("mismatched adapter input: err = %v, want InvalidArgument", err)
	}

	// Matching explicit input succeeds.
	id2 := createPublishedWithDraft(t, ctx, s, "adp-input-match")
	vid2 := draftVersionID(t, pool, tenantID, id2)
	resp, err := s.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
		WorkerId:  id2,
		VersionId: vid2,
		ModelRef:  adapterStrPtr(refOrchiconOK),
		Adapter:   adapterStrPtr("orchicon"),
	}))
	if err != nil {
		t.Fatalf("matching adapter input rejected: %v", err)
	}
	if got := resp.Msg.Version.Adapter; got != "orchicon" {
		t.Errorf("computed adapter = %q, want orchicon", got)
	}
}

// TestCreateWorkerVersionRefOnlyAdapterChange: a new draft version may
// change the adapter via model_ref alone (BUG-1 regression, Create path);
// an invalid pair for the new adapter is rejected.
func TestCreateWorkerVersionRefOnlyAdapterChange(t *testing.T) {
	s, ctx := func() (*Service, context.Context) {
		_, s, ctx, _ := bulkEnv(t)
		return s, ctx
	}()
	id := createPublishedWorker(t, ctx, s, "adp-create-refonly")

	resp, err := s.CreateWorkerVersion(ctx, connect.NewRequest(&apiv1.CreateWorkerVersionRequest{
		WorkerId: id,
		ModelRef: adapterStrPtr(refOrchiconOK),
	}))
	if err != nil {
		t.Fatalf("ref-only adapter change on new draft rejected: %v", err)
	}
	if got := resp.Msg.Version.ModelRef; got != refOrchiconOK {
		t.Errorf("model_ref = %q, want %q", got, refOrchiconOK)
	}
	if got := resp.Msg.Version.Adapter; got != "orchicon" {
		t.Errorf("computed adapter = %q, want orchicon", got)
	}

	// Invalid pair for the new adapter is rejected.
	id2 := createPublishedWorker(t, ctx, s, "adp-create-bad")
	_, err = s.CreateWorkerVersion(ctx, connect.NewRequest(&apiv1.CreateWorkerVersionRequest{
		WorkerId: id2,
		ModelRef: adapterStrPtr(refOrchiconBad),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid-pair CreateWorkerVersion: err = %v, want InvalidArgument", err)
	}
}
