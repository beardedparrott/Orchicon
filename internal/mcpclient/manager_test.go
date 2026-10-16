package mcpclient

// manager_test.go — the platform-NEUTRAL half of the MCP client suite; the POSIX-only half is
// manager_test_unix.go.
//
// WHY THE SPLIT IS LOAD-BEARING RATHER THAN TIDY: the orphan/sweep tests that used to live in THIS
// untagged file signal pids with `syscall.Kill`, read `/proc/<pid>/{exe,stat,environ}` and re-exec
// this test binary as a fixture child. `syscall.Kill` is ABSENT from the Windows syscall package
// and Windows has no `/proc`, so the untagged file broke the package's TESTS for windows/amd64 and
// windows/arm64:
//
//	GOOS=windows go test -c ./internal/mcpclient
//	internal/mcpclient/manager_test.go:363:20: undefined: syscall.Kill
//
// THAT IS THE EXACT DEFECT CLASS cmd/orchicon/serve_reap_unix.go AND hostproc_unix.go NAME for
// their own files — "a file that shells out to /bin/sh or calls syscall.Kill IS a Unix file, and if
// it lives in an untagged file it is a Windows build failure waiting for the next release". It
// matters because Windows is a FIRST-CLASS host for this repository now (Postgres runs there as a
// host process; see cmd/orchicon/privilege_windows.go and hostproc_windows.go), so a Windows
// test-build break is a real regression surface, not a hypothetical.
//
// WHAT STAYS HERE: discovery, calls, HTTP/header auth, the scope-resolver seam and every error path
// — all portable, all still run on Windows. What moved out degrades DELIBERATELY there, because the
// property it asserts (a child reparented to PID 1 and reaped like a POSIX orphan) has no Windows
// counterpart to assert.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

// stdioSpec returns a ServerSpec pointing at this test binary re-executed
// as the stdio fixture server.
func stdioSpec(id string) ServerSpec {
	return ServerSpec{
		ID:      id,
		Type:    TypeStdio,
		Command: fixtureArgs(),
	}
}

// httpSpec returns a ServerSpec pointing at an in-process streamable HTTP
// fixture server.
func httpSpec(id, url string) ServerSpec {
	return ServerSpec{ID: id, Type: TypeHTTP, URL: url}
}

// newHTTPFixture starts an in-process streamable HTTP MCP fixture server
// and returns its URL plus a cleanup func.
func newHTTPFixture(t *testing.T) string {
	t.Helper()
	srv := fixtureServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

// startManager connects a Manager to the given specs and returns it.
func startManager(t *testing.T, specs ...ServerSpec) *Manager {
	t.Helper()
	m := NewManager(nil)
	defs, err := m.Start(context.Background(), specs)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("Start: no tools discovered")
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// ---------------------------------------------------------------------------
// Discovery: stdio + HTTP, mcp__<server>__<tool> naming, schema intact
// ---------------------------------------------------------------------------

func TestDiscoveryStdio(t *testing.T) {
	m := startManager(t, stdioSpec("srv"))
	defs := m.Defs()
	if len(defs) < 3 {
		t.Fatalf("expected >=3 tools, got %d", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, want := range []string{"mcp__srv__echo", "mcp__srv__fail", "mcp__srv__slow"} {
		if !names[want] {
			t.Errorf("missing tool %q; got %v", want, names)
		}
	}
	// Schema must pass through intact (the model sees the real signature).
	var schema map[string]any
	for _, d := range defs {
		if d.Name == "mcp__srv__echo" {
			if err := json.Unmarshal([]byte(d.ParamsJSON), &schema); err != nil {
				t.Fatalf("ParamsJSON not valid JSON: %v", err)
			}
			props, _ := schema["properties"].(map[string]any)
			if props == nil {
				t.Fatalf("schema missing properties: %v", schema)
			}
			if _, ok := props["message"]; !ok {
				t.Errorf("echo schema missing message property: %v", props)
			}
			break
		}
	}
}

func TestDiscoveryHTTP(t *testing.T) {
	m := startManager(t, httpSpec("remote", newHTTPFixture(t)))
	defs := m.Defs()
	if len(defs) < 3 {
		t.Fatalf("expected >=3 tools, got %d", len(defs))
	}
	found := false
	for _, d := range defs {
		if d.Name == "mcp__remote__echo" {
			found = true
			if !strings.Contains(d.ParamsJSON, "message") {
				t.Errorf("echo schema not passed through: %s", d.ParamsJSON)
			}
		}
	}
	if !found {
		t.Fatal("mcp__remote__echo not discovered over HTTP")
	}
}

func TestDiscoveryMultiServer(t *testing.T) {
	m := startManager(t, stdioSpec("a"), httpSpec("b", newHTTPFixture(t)))
	defs := m.Defs()
	seen := map[string]bool{}
	for _, d := range defs {
		seen[d.Name] = true
	}
	for _, want := range []string{"mcp__a__echo", "mcp__b__echo"} {
		if !seen[want] {
			t.Errorf("missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Calls: success, error, timeout, cancel
// ---------------------------------------------------------------------------

func TestCallStdioEcho(t *testing.T) {
	m := startManager(t, stdioSpec("srv"))
	got, err := m.Execute(context.Background(), "mcp__srv__echo", `{"message":"hello"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(got, "hello") {
		t.Fatalf("unexpected result %q", got)
	}
}

func TestCallHTTPEcho(t *testing.T) {
	m := startManager(t, httpSpec("remote", newHTTPFixture(t)))
	got, err := m.Execute(context.Background(), "mcp__remote__echo", `{"message":"hi"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(got, "hi") {
		t.Fatalf("unexpected result %q", got)
	}
}

func TestCallToolError(t *testing.T) {
	m := startManager(t, stdioSpec("srv"))
	_, err := m.Execute(context.Background(), "mcp__srv__fail", `{}`)
	if err == nil {
		t.Fatal("expected error for failing tool")
	}
	if !strings.Contains(err.Error(), "fixture failure") {
		t.Fatalf("error not actionable: %v", err)
	}
}

func TestCallTimeout(t *testing.T) {
	spec := stdioSpec("srv")
	spec.Timeout = 300 * time.Millisecond
	m := startManager(t, spec)
	start := time.Now()
	_, err := m.Execute(context.Background(), "mcp__srv__slow", `{"seconds":5}`)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error not actionable: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestCallCancel(t *testing.T) {
	m := startManager(t, stdioSpec("srv"))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := m.Execute(ctx, "mcp__srv__slow", `{"seconds":5}`)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error not actionable: %v", err)
	}
}

func TestCallInvalidArgs(t *testing.T) {
	m := startManager(t, stdioSpec("srv"))
	_, err := m.Execute(context.Background(), "mcp__srv__echo", `not-json`)
	if err == nil {
		t.Fatal("expected error for invalid args")
	}
}

func TestCallUnknownTool(t *testing.T) {
	m := startManager(t, stdioSpec("srv"))
	_, err := m.Execute(context.Background(), "mcp__srv__nope", `{}`)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown-tool error, got %v", err)
	}
	_, err = m.Execute(context.Background(), "other__tool", `{}`)
	if err == nil {
		t.Fatal("expected error for tool on unselected server")
	}
}

// ---------------------------------------------------------------------------
// HTTP header auth
// ---------------------------------------------------------------------------

func TestHTTPHeaderAuth(t *testing.T) {
	var mu sync.Mutex
	gotAuth := ""
	srv := fixtureServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()

	spec := httpSpec("auth", ts.URL)
	spec.Headers = map[string]string{"Authorization": "Bearer secret"}
	m := NewManager(nil)
	if _, err := m.Start(context.Background(), []ServerSpec{spec}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization header not sent: %q", gotAuth)
	}
}

// ---------------------------------------------------------------------------
// Resolution: the ScopeResolver seam
// ---------------------------------------------------------------------------

// stubScopeResolver is a ScopeResolver backed by literals — the ONE seam the
// adapters consume, so a test can drive resolution without storage.
type stubScopeResolver struct {
	res Resolution
}

func (s *stubScopeResolver) ResolveScope(_ context.Context, _ ScopeRef) (Resolution, error) {
	return s.res, nil
}

// TestScopeResolverStubShape pins the seam: a resolver returns the union's
// servers with provenance, and the no-op resolver resolves every scope to
// nothing (never an error).
func TestScopeResolverStubShape(t *testing.T) {
	var src ScopeResolver = &stubScopeResolver{res: Resolution{
		Servers: []ScopedServer{{
			Spec:    ServerSpec{ID: "s1"},
			From:    ScopeProject,
			FromID:  "project:p1",
			EntryID: "s1",
		}},
		SelectedIDs: []string{"s1"},
	}}
	got, err := src.ResolveScope(context.Background(), ScopeRef{Kind: ScopeProject, ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 1 || got.Servers[0].Spec.ID != "s1" {
		t.Fatalf("resolved servers = %+v, want the one server", got.Servers)
	}
	if got.Servers[0].From != ScopeProject || got.Servers[0].EntryID != "s1" {
		t.Fatalf("provenance lost: %+v", got.Servers[0])
	}
}

func TestNoopScopeResolverResolvesToNothing(t *testing.T) {
	got, err := NoopScopeResolver{}.ResolveScope(context.Background(), ScopeRef{Kind: ScopeProject})
	if err != nil || len(got.Servers) != 0 || len(got.SelectedIDs) != 0 {
		t.Fatalf("noop resolver: %+v err=%v", got, err)
	}
}

// TestScopeRefZeroResolvesToNothing pins that the zero ScopeRef is never a
// wildcard: an unknown kind resolves to nothing rather than the tenant set.
func TestScopeRefZeroResolvesToNothing(t *testing.T) {
	got, err := NoopScopeResolver{}.ResolveScope(context.Background(), ScopeRef{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 0 || len(got.Missing) != 0 || len(got.Disabled) != 0 || len(got.Skills) != 0 {
		t.Fatalf("zero scope must resolve to nothing, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestStartUnreachableHTTPFailsActionably(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer ts.Close()
	m := NewManager(nil)
	_, err := m.Start(context.Background(), []ServerSpec{httpSpec("dead", ts.URL)})
	if err == nil {
		t.Fatal("expected connect failure")
	}
	if !strings.Contains(err.Error(), "dead") {
		t.Fatalf("error should name the server: %v", err)
	}
}

func TestStartStdioMissingBinaryFailsActionably(t *testing.T) {
	m := NewManager(nil)
	spec := ServerSpec{ID: "nobin", Type: TypeStdio, Command: []string{"/nonexistent/orchicon-mcp"}}
	_, err := m.Start(context.Background(), []ServerSpec{spec})
	if err == nil {
		t.Fatal("expected connect failure")
	}
	if !strings.Contains(err.Error(), "nobin") {
		t.Fatalf("error should name the server: %v", err)
	}
}

func TestStartSelectedButUnconfigured(t *testing.T) {
	m := NewManager(nil)
	spec := ServerSpec{ID: "empty", Type: TypeStdio} // no command
	_, err := m.Start(context.Background(), []ServerSpec{spec})
	if err == nil {
		t.Fatal("expected unconfigured error")
	}
	if !strings.Contains(err.Error(), "no command") {
		t.Fatalf("error should be actionable: %v", err)
	}
}

func TestManagerClosed(t *testing.T) {
	m := NewManager(nil)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), []ServerSpec{stdioSpec("srv")}); err == nil {
		t.Fatal("Start after Close should fail")
	}
}

// ---------------------------------------------------------------------------
// splitCommandLine
// ---------------------------------------------------------------------------

func TestSplitCommandLine(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`/usr/bin/npx -y @modelcontextprotocol/server-filesystem`, []string{"/usr/bin/npx", "-y", "@modelcontextprotocol/server-filesystem"}},
		{`echo "hello world"`, []string{"echo", "hello world"}},
		{`a "b c" d`, []string{"a", "b c", "d"}},
		{``, nil},
	}
	for _, c := range cases {
		got := splitCommandLine(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitCommandLine(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
