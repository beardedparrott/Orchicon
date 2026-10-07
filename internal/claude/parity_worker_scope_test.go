package claude

// parity_worker_scope_test.go — AC 5: for ONE scope, native and claude resolve
// the IDENTICAL set (order and provenance included).
//
// Both adapters consume mcpclient.ScopeResolver and nothing else, so they cannot
// disagree about WHICH servers apply — only about how each renders them into its
// own config format. This test gives that claim teeth: ONE stub resolver, ONE
// scope ref, and a triple-by-triple comparison of what each adapter observed.
//
// It lives in package claude because claude → orchicon is the only acyclic
// import direction (orchicon does not import claude), so this is the one place
// both adapters can be driven together.

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/orchicon"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// parityScopeResolver records the ref both adapters ask for and answers with one
// ordered resolution whose provenance mixes the two scopes — the shape a worker
// execution actually gets (project rows first, then the version's own set).
type parityScopeResolver struct {
	res mcpclient.Resolution
	got []mcpclient.ScopeRef
}

func (p *parityScopeResolver) ResolveScope(_ context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	p.got = append(p.got, ref)
	return p.res, nil
}

// TestNativeAndClaudeResolveTheIdenticalWorkerSet is AC 5: the same scope must
// yield the same (id, From, FromID) triples on both adapters, in the same order.
func TestNativeAndClaudeResolveTheIdenticalWorkerSet(t *testing.T) {
	stub := &parityScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "project-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/proj"}},
				From: mcpclient.ScopeProject, FromID: "project:p1", EntryID: "project-srv"},
			{Spec: mcpclient.ServerSpec{ID: "ver-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/ver"}},
				From: mcpclient.ScopeWorker, FromID: "inline:w1@3"},
		},
		SelectedIDs: []string{"project-srv", "ver-srv"},
	}}

	ctx := context.Background()
	exec := db.ExecutionRow{ID: "exec-1", TenantID: "tnt_parity", ProjectID: "p1", WorkerID: "w1"}
	perms := []byte(`{"mcp_servers":[{"id":"ver-srv","type":"stdio","command":["/bin/ver"]}]}`)
	manifest := scheduler.ExecutionManifest{ProjectID: "p1", WorkerID: "w1", WorkerVersion: 3, Permissions: perms}

	// NATIVE: the execution path's own resolution.
	nb := orchicon.NewBridge(nil, t.TempDir(), quietLogger())
	nb.SetScopeResolver(stub)
	nres, err := nb.ResolveExecutionMCP(ctx, exec, manifest)
	if err != nil {
		t.Fatalf("native ResolveExecutionMCP: %v", err)
	}

	// CLAUDE: the same resolution, through the shared resolver.
	cb := New(quietLogger())
	cb.SetScopeResolver(stub)
	cres, err := cb.resolveMCP(ctx, "tnt_parity", mcpclient.ScopeRef{
		Kind: mcpclient.ScopeWorker, ProjectID: "p1", WorkerID: "w1",
		Version: 3, OwnPermissions: perms,
	})
	if err != nil {
		t.Fatalf("claude resolveMCP: %v", err)
	}

	if len(nres.Servers) != len(cres.Servers) {
		t.Fatalf("native resolved %d servers, claude %d", len(nres.Servers), len(cres.Servers))
	}
	for i := range nres.Servers {
		n, c := nres.Servers[i], cres.Servers[i]
		if n.Spec.ID != c.Spec.ID {
			t.Errorf("server %d: native id %q, claude id %q", i, n.Spec.ID, c.Spec.ID)
		}
		if n.From != c.From {
			t.Errorf("server %d (%s): native From %q, claude From %q", i, n.Spec.ID, n.From, c.From)
		}
		if n.FromID != c.FromID {
			t.Errorf("server %d (%s): native FromID %q, claude FromID %q", i, n.Spec.ID, n.FromID, c.FromID)
		}
	}

	// Both asked at the SAME scope, and the provenance string both would log is
	// identical — the observation the operator reads.
	if len(stub.got) != 2 {
		t.Fatalf("expected both adapters to consult the resolver, got %d calls", len(stub.got))
	}
	for _, ref := range stub.got {
		if ref.Kind != mcpclient.ScopeWorker {
			t.Errorf("an adapter resolved scope %q, want worker", ref.Kind)
		}
	}
	if a, b := mcpclient.ProvenanceString(nres.Servers), mcpclient.ProvenanceString(cres.Servers); a != b {
		t.Errorf("provenance differs: native %q, claude %q", a, b)
	}
}
