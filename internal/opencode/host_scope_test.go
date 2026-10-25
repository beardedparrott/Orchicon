package opencode

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// tenantRecordingResolver records the tenant it saw on the context (the
// mcpsettings.Resolver reads it via tenant.FromContext and refuses an
// unscoped context).
type tenantRecordingResolver struct {
	calls     int
	gotTenant string
	gotRef    mcpclient.ScopeRef
}

func (r *tenantRecordingResolver) ResolveScope(ctx context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	r.calls++
	r.gotTenant = tenant.FromContext(ctx)
	r.gotRef = ref
	return mcpclient.Resolution{}, nil
}

// TestHostResolvedSetScopesTheTenant is the regression guard for the AC 6
// silent-absence defect: hostResolvedSet resolved the project MCP scope on a
// context that carried NO tenant (the reconciler's dispatch context derives
// straight from the plane's signal context and nothing in the scheduler sets
// tenant.WithID). mcpsettings.Resolver errors on an unscoped context, so every
// in-process session resolved the EMPTY set and silently landed on the DEFAULT
// serve with NONE of its project's servers — the exact silent absence AC 6
// forbids. The execution's own tenant must be scoped onto the resolver context.
func TestHostResolvedSetScopesTheTenant(t *testing.T) {
	a := newTestAdapter(t)
	rr := &tenantRecordingResolver{}
	a.SetHostScopeResolver(rr)

	// An UNTENANTED context, exactly what the reconciler hands the adapter.
	set := a.hostResolvedSet(context.Background(), scheduler.ExecutionManifest{
		ExecutionID: "exec-1", ProjectID: "proj-1",
	}, "tnt_exec")

	if rr.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", rr.calls)
	}
	if rr.gotTenant != "tnt_exec" {
		t.Fatalf("resolver saw tenant %q, want %q — an unscoped context makes mcpsettings.Resolver error and every in-process session silently loses its project's servers (AC 6)", rr.gotTenant, "tnt_exec")
	}
	// THE SCOPE IS WORKER, NOT PROJECT — and this assertion used to demand the opposite, which is why
	// the worker half was missing on this adapter alone.
	//
	// ScopeProject resolves ONLY the project's owned rows. A worker whose version declares its own
	// `mcp_servers` therefore had them listed in its settings and absent from every execution here,
	// while claude and the native bridge (both ScopeWorker) resolved them normally. resolveWorker
	// unions the project rows with the ref's own set, so ScopeWorker IS "project ∪ this version" —
	// the union the requirement actually names, and NOT the cross-RUN union that stays forbidden.
	if rr.gotRef.Kind != mcpclient.ScopeWorker {
		t.Errorf("resolver scope kind = %v, want ScopeWorker — ScopeProject drops the executing "+
			"version's own MCP servers, so a worker's declared servers never reach its runs on this "+
			"adapter", rr.gotRef.Kind)
	}
	// The fields the worker scope NEEDS must be carried, or the union silently degrades back to
	// "project only" — the exact failure this test exists for.
	want := scheduler.ExecutionManifest{
		ProjectID: "proj-1", WorkerID: "wk-1", WorkerVersion: 3, Permissions: []byte(`{"mcp_servers":[]}`),
	}
	got := a.hostResolvedSet(context.Background(), scheduler.ExecutionManifest{
		ExecutionID: "exec-2", ProjectID: want.ProjectID, WorkerID: want.WorkerID,
		WorkerVersion: want.WorkerVersion, Permissions: want.Permissions,
	}, "tnt_exec")
	_ = got
	if rr.gotRef.ProjectID != want.ProjectID {
		t.Errorf("resolver ProjectID = %q, want %q", rr.gotRef.ProjectID, want.ProjectID)
	}
	if rr.gotRef.WorkerID != want.WorkerID {
		t.Errorf("resolver WorkerID = %q, want %q — without it the worker's own servers cannot be "+
			"resolved", rr.gotRef.WorkerID, want.WorkerID)
	}
	if rr.gotRef.Version != want.WorkerVersion {
		t.Errorf("resolver Version = %d, want %d — a PINNED dispatch must resolve the version it "+
			"pinned, not the latest published one", rr.gotRef.Version, want.WorkerVersion)
	}
	if string(rr.gotRef.OwnPermissions) != string(want.Permissions) {
		t.Errorf("resolver OwnPermissions = %q, want %q — the version's inline specs travel on the "+
			"manifest because the runner is the only place that knows which version is executing",
			rr.gotRef.OwnPermissions, want.Permissions)
	}
	_ = set
}

// TestHostResolvedSetEmptyWithoutProjectOrResolver pins the default-serve
// mapping: no project (standalone dispatch / Ask) or no resolver wired yields
// the EMPTY set, which HostServePool maps to the default serve — today's
// single-serve behaviour, unchanged.
func TestHostResolvedSetEmptyWithoutProjectOrResolver(t *testing.T) {
	a := newTestAdapter(t)
	rr := &tenantRecordingResolver{}
	a.SetHostScopeResolver(rr)

	if got := a.hostResolvedSet(context.Background(), scheduler.ExecutionManifest{ExecutionID: "e"}, "tnt"); len(got.Servers) != 0 {
		t.Fatalf("no project must yield the empty set, got %+v", got)
	}
	if rr.calls != 0 {
		t.Fatalf("no project must not call the resolver, calls = %d", rr.calls)
	}

	a2 := newTestAdapter(t) // no resolver wired
	if got := a2.hostResolvedSet(context.Background(), scheduler.ExecutionManifest{ExecutionID: "e", ProjectID: "p"}, "tnt"); len(got.Servers) != 0 {
		t.Fatalf("no resolver must yield the empty set, got %+v", got)
	}
}
