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
	if rr.gotRef.Kind != mcpclient.ScopeProject || rr.gotRef.ProjectID != "proj-1" {
		t.Fatalf("resolver scope = %+v, want the PROJECT scope for proj-1", rr.gotRef)
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
