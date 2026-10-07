package claude

// worker_union_test.go — AC 1 (claude half) and AC 2's claude leg.
//
// The claude WORKER path must resolve at the WORKER scope — the project's owned
// definitions ∪ THE EXECUTING VERSION's inline specs, the latter carried on
// ExecutionManifest.Permissions — and must LOG what it resolved WITH provenance,
// so an absent or mis-scoped MCP surface is falsifiable instead of showing up
// only as the model saying it cannot call a tool.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// recordingScopeResolver is a ScopeResolver that CAPTURES the ref it was asked
// for and returns a fixed resolution — so a test can assert both WHAT was
// resolved and WHICH scope the adapter asked about.
type recordingScopeResolver struct {
	ref mcpclient.Resolution
	got mcpclient.ScopeRef
}

func (r *recordingScopeResolver) ResolveScope(_ context.Context, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	r.got = ref
	return r.ref, nil
}

// workerUnionResolution is the fixture: a project-owned server then a
// version-owned inline server, in the documented order.
func workerUnionResolution() mcpclient.Resolution {
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

// captureLog swaps slog.Default for a buffer so a test can read the per-session
// line (logMCPResolution logs to slog.Default, which is the process logger).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// AC 2 (claude leg): a worker execution resolves the WORKER scope, carrying the
// version's inline permissions, and the version's spec reaches the
// `--mcp-config` claude is actually launched with.
func TestWorkerArgvResolvesTheWorkerScopeWithVersionSpecs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	stub := &recordingScopeResolver{ref: workerUnionResolution()}
	b := New(quietLogger())
	b.SetScopeResolver(stub)
	logBuf := captureLog(t)

	perms := []byte(`{"mcp_servers":[{"id":"ver-srv","type":"stdio","command":["/bin/ver"]}]}`)
	s := &session{b: b, tenantID: "tnt_worker"}
	s.manifest = scheduler.ExecutionManifest{
		ProjectID: "p1", WorkerID: "w1", WorkerVersion: 3, Permissions: perms,
	}

	argv := s.argv()

	// The resolver was asked for the WORKER scope with the PINNED version and
	// the caller-held permissions — the version's inline specs path.
	if stub.got.Kind != mcpclient.ScopeWorker {
		t.Errorf("resolved scope = %q, want worker", stub.got.Kind)
	}
	if stub.got.WorkerID != "w1" || stub.got.ProjectID != "p1" {
		t.Errorf("resolved ref = %+v, want worker w1 in project p1", stub.got)
	}
	if stub.got.Version != 3 {
		t.Errorf("resolved version = %d, want the PINNED 3", stub.got.Version)
	}
	if string(stub.got.OwnPermissions) != string(perms) {
		t.Errorf("the version's inline permissions did not reach the resolver: %s", stub.got.OwnPermissions)
	}

	// BOTH halves reach the argv (the union), not just the version's.
	var cfg string
	for i, a := range argv {
		if a == "--mcp-config" && i+1 < len(argv) {
			cfg = argv[i+1]
		}
	}
	if cfg == "" {
		t.Fatal("the worker argv has no --mcp-config")
	}
	for _, want := range []string{`"orchicon"`, `"ver-srv"`, `"proj-srv"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the rendered config is missing %q: %s", want, cfg)
		}
	}

	// AC 1 (claude half): the per-session line names the servers AND their
	// provenance, so a mis-scoped or absent surface is visible.
	line := logBuf.String()
	if !strings.Contains(line, "claude: MCP servers registered") {
		t.Fatalf("no per-session MCP resolution line was logged: %s", line)
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

// The rendered config is the RESOLVED set: a resolution error must not smuggle a
// half-set through. renderMCP keeps the loud failure on a missing selection.
func TestRenderMCPStillFailsOnAMissingSelection(t *testing.T) {
	b := New(quietLogger())
	builtin := MCPServer{Name: "orchicon", Command: "/bin/orchicon", Args: []string{"mcp"}}
	_, err := b.renderMCP(context.Background(), "tnt_dev", mcpclient.Resolution{
		SelectedIDs: []string{"ghost"},
		Missing:     []string{"ghost"},
	}, builtin)
	if err == nil {
		t.Fatal("a selected-but-unconfigured server was accepted")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("the failure does not name the missing id: %v", err)
	}
}

// A failed worker resolution logs the built-in alone with NO provenance: the
// log must never claim a provenance for a server the session did not get.
func TestWorkerResolutionFailureLogsNoPhantomProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	installFakeClaude(t, home)

	b := New(quietLogger())
	b.SetScopeResolver(&recordingScopeResolver{ref: mcpclient.Resolution{
		SelectedIDs: []string{"ghost"},
		Missing:     []string{"ghost"},
	}})
	logBuf := captureLog(t)

	s := &session{b: b, tenantID: "tnt_worker", execID: "exec-1"}
	s.manifest = scheduler.ExecutionManifest{ProjectID: "p1", WorkerID: "w1", WorkerVersion: 3}
	argv := s.argv()
	if len(argv) == 0 {
		t.Fatal("argv is empty")
	}
	line := logBuf.String()
	if strings.Contains(line, "provenance=ghost") {
		t.Errorf("the log claims provenance for a server that did not resolve: %s", line)
	}

	// And ask-side helper is unchanged: the project-scope wrapper still works.
	if _, err := b.resolveMCPServers(context.Background(), "tnt_dev", "", "p1",
		MCPServer{Name: "orchicon", Command: filepath.Join(home, "orchicon")}); err == nil {
		t.Error("resolveMCPServers swallowed the missing selection")
	}
}
