package diffs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// fakeFileEditService implements FileEditServiceHandler, recording the
// tenant_id it received (the critical wiring — FileEdit RPCs require it).
type fakeFileEditService struct {
	apiv1connect.UnimplementedFileEditServiceHandler
	gotTenant string
}

func (f *fakeFileEditService) GetSessionFileEdits(ctx context.Context, req *connect.Request[apiv1.GetSessionFileEditsRequest]) (*connect.Response[apiv1.GetSessionFileEditsResponse], error) {
	f.gotTenant = req.Msg.GetTenantId()
	edits := []*apiv1.FileEdit{
		{Id: "e1", Path: "docs/x.md", Kind: "modify", Seq: 1, UnifiedDiff: "--- a/docs/x.md\n+++ b/docs/x.md\n@@ -1,3 +1,3 @@\n line1\n-line2\n+line2x\n line3\n"},
		{Id: "e2", Path: "cmd/new.go", Kind: "create", Seq: 2, UnifiedDiff: "--- /dev/null\n+++ b/cmd/new.go\n@@ -0,0 +1,3 @@\n+package main\n+\n+func main() {}\n"},
	}
	return connect.NewResponse(&apiv1.GetSessionFileEditsResponse{Edits: edits, MaxSeq: 2}), nil
}

// fakeAuthService returns one identity with a tenant_id (the tenant the
// FileEdit RPCs must receive).
type fakeAuthService struct {
	apiv1connect.UnimplementedAuthServiceHandler
}

func (f *fakeAuthService) ListIdentities(ctx context.Context, req *connect.Request[apiv1.ListIdentitiesRequest]) (*connect.Response[apiv1.ListIdentitiesResponse], error) {
	return connect.NewResponse(&apiv1.ListIdentitiesResponse{
		Identities: []*apiv1.Identity{{Id: "id1", TenantId: "tnt_resolved"}},
	}), nil
}

// TestStoreResolveTenantAndFetch verifies the architect-flagged wiring: the
// pane resolves its tenant via Auth.ListIdentities, caches it, and passes it
// (non-empty) to GetSessionFileEdits — so the RPC never 400s with
// "tenant_id must not be empty".
func TestStoreResolveTenantAndFetch(t *testing.T) {
	ff := &fakeFileEditService{}
	fp, fpHandler := apiv1connect.NewFileEditServiceHandler(ff)
	fa := &fakeAuthService{}
	ap, apHandler := apiv1connect.NewAuthServiceHandler(fa)
	mux := http.NewServeMux()
	mux.Handle(fp, fpHandler)
	mux.Handle(ap, apHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL, Token: "oc_test"}, srv.Client())
	store := NewStore(cl)

	snap, err := store.Fetch(context.Background(), "execution", "exec-1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if ff.gotTenant != "tnt_resolved" {
		t.Errorf("GetSessionFileEdits received tenant_id %q, want tnt_resolved", ff.gotTenant)
	}
	if snap.MaxDurableSeq != 2 {
		t.Errorf("maxSeq = %d, want 2", snap.MaxDurableSeq)
	}
	if len(snap.Edits) != 2 {
		t.Errorf("edits = %d, want 2", len(snap.Edits))
	}
	// Tenant is cached (no second ListIdentities round-trip needed).
	if store.Tenant() != "tnt_resolved" {
		t.Errorf("cached tenant = %q, want tnt_resolved", store.Tenant())
	}
	// GroupByFile over the fetched ledger tallies adds/dels from each file's
	// latest diff.
	groups := GroupByFile(snap.Edits)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
}
