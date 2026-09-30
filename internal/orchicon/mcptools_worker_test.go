package orchicon

// mcptools_worker_test.go — AC 1 (native half) and AC 3 (native half).
//
// AC 1: the native path must LOG the resolved MCP set WITH provenance. Before
// this child the native path logged nothing, which is why an MCP problem was
// unfalsifiable: the operator saw only the model reporting it could not call a
// tool.
//
// AC 3: a server that cannot run must fail the session ACTIONABLY, naming the
// server AND the scope it came from — never a silently degraded session.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// stubScopeResolver is the ONE resolution seam, backed by literals, so a native
// test can drive the execution path without storage. It also records the ref the
// bridge asked for, so the SCOPE itself is assertable.
type stubScopeResolver struct {
	res mcpclient.Resolution
	got mcpclient.ScopeRef
}

func (s *stubScopeResolver) ResolveScope(_ context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	s.got = ref
	return s.res, nil
}

// nativeWorkerResolution is the fixture: a project-owned server then a
// version-owned inline server.
func nativeWorkerResolution() mcpclient.Resolution {
	return mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "proj-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/proj"}},
				From: mcpclient.ScopeProject, FromID: "project:p1", EntryID: "proj-srv"},
			{Spec: mcpclient.ServerSpec{ID: "ver-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/ver"}},
				From: mcpclient.ScopeWorker, FromID: "inline:w1@3"},
		},
		SelectedIDs: []string{"proj-srv", "ver-srv"},
	}
}

// AC 2/AC 1 (native leg): the execution path resolves the WORKER scope carrying
// the version's inline permissions and logs the set WITH provenance, WITHOUT
// connecting (a resolution is an observation).
func TestResolveExecutionMCPUsesWorkerScopeAndCarriesVersionSpecs(t *testing.T) {
	stub := &stubScopeResolver{res: nativeWorkerResolution()}
	b := NewBridge(nil, t.TempDir(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	b.SetScopeResolver(stub)

	exec := db.ExecutionRow{ID: "exec-1", TenantID: "tnt_native", ProjectID: "p1", WorkerID: "w1"}
	perms := []byte(`{"mcp_servers":[{"id":"ver-srv","type":"stdio","command":["/bin/ver"]}]}`)
	manifest := scheduler.ExecutionManifest{WorkerVersion: 3, Permissions: perms}

	res, err := b.ResolveExecutionMCP(context.Background(), exec, manifest)
	if err != nil {
		t.Fatalf("ResolveExecutionMCP: %v", err)
	}
	if stub.got.Kind != mcpclient.ScopeWorker {
		t.Errorf("scope = %q, want worker", stub.got.Kind)
	}
	if stub.got.Version != 3 || stub.got.WorkerID != "w1" || stub.got.ProjectID != "p1" {
		t.Errorf("ref = %+v, want worker w1 v3 in project p1", stub.got)
	}
	if string(stub.got.OwnPermissions) != string(perms) {
		t.Errorf("version permissions did not reach the resolver: %s", stub.got.OwnPermissions)
	}
	if len(res.Servers) != 2 {
		t.Fatalf("resolved %d servers, want 2", len(res.Servers))
	}
}

// AC 1 (native half): the per-session line names the servers AND their
// provenance. This is THE observation that was missing.
func TestMCPResolutionLogsProvenance(t *testing.T) {
	// Point the specs at a real executable so Start succeeds and the log line is
	// reached (the assertion is about the log, not about connecting).
	stub := &stubScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "proj-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/true"}},
				From: mcpclient.ScopeProject, FromID: "project:p1", EntryID: "proj-srv"},
			{Spec: mcpclient.ServerSpec{ID: "ver-srv", Type: mcpclient.TypeStdio, Command: []string{"/bin/true"}},
				From: mcpclient.ScopeWorker, FromID: "inline:w1@3"},
		},
		SelectedIDs: []string{"proj-srv", "ver-srv"},
	}}
	var buf bytes.Buffer
	b := NewBridge(nil, t.TempDir(), slog.New(slog.NewTextHandler(&buf, nil)))
	b.SetScopeResolver(stub)

	exec := db.ExecutionRow{ID: "exec-1", TenantID: "tnt_native", ProjectID: "p1", WorkerID: "w1"}
	manifest := scheduler.ExecutionManifest{WorkerVersion: 3}

	mt, err := b.mcpResolveAndStart(context.Background(), exec, manifest)
	if mt != nil {
		t.Cleanup(func() { _ = mt.Close() })
	}
	if err != nil {
		// A connect failure is allowed here (the fake /bin/true is not an MCP
		// server) — the LOG LINE is written before the connect, so the assertion
		// still holds. What must never happen is the line being absent.
		if !strings.Contains(err.Error(), "MCP connect") {
			t.Fatalf("mcpResolveAndStart: %v", err)
		}
	}

	line := buf.String()
	if !strings.Contains(line, "orchicon: MCP servers registered") {
		t.Fatalf("the native path logged no MCP resolution line: %s", line)
	}
	if !strings.Contains(line, "provenance=") {
		t.Errorf("the resolution line carries no provenance: %s", line)
	}
	for _, want := range []string{"ver-srv=inline:w1@3", "proj-srv=project:p1"} {
		if !strings.Contains(line, want) {
			t.Errorf("provenance %q missing from the log: %s", want, line)
		}
	}
}

// AC 3 (native half): a server that cannot run FAILS the call and the error names
// the server AND its scope. There is no degrade path.
func TestMCPConnectFailureNamesServerAndScope(t *testing.T) {
	stub := &stubScopeResolver{res: mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{
			{Spec: mcpclient.ServerSpec{ID: "ghost", Type: mcpclient.TypeStdio,
				Command: []string{"/nonexistent/definitely-not-here"}},
				From: mcpclient.ScopeWorker, FromID: "inline:w1@3"},
		},
		SelectedIDs: []string{"ghost"},
	}}
	b := NewBridge(nil, t.TempDir(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	b.SetScopeResolver(stub)

	exec := db.ExecutionRow{ID: "exec-1", TenantID: "tnt_native", ProjectID: "p1", WorkerID: "w1"}
	_, err := b.mcpResolveAndStart(context.Background(), exec, scheduler.ExecutionManifest{WorkerVersion: 3})
	if err == nil {
		t.Fatal("a server that cannot run did not fail the session")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"ghost"`) {
		t.Errorf("the failure does not name the server: %v", msg)
	}
	if !strings.Contains(msg, "inline:w1@3") {
		t.Errorf("the failure does not name the scope: %v", msg)
	}
}
