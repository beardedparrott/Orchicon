package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
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
// Resolution: worker → project → none
// ---------------------------------------------------------------------------

type fakeSource struct {
	servers  []ServerSpec
	worker   []string
	project  []string
	listErr  error
	workerID string
	projID   string
}

func (f *fakeSource) ServerList(context.Context) ([]ServerSpec, error) { return f.servers, f.listErr }
func (f *fakeSource) WorkerSelection(_ context.Context, workerID string) ([]string, error) {
	f.workerID = workerID
	return f.worker, nil
}
func (f *fakeSource) ProjectSelection(_ context.Context, projectID string) ([]string, error) {
	f.projID = projectID
	return f.project, nil
}

func TestResolveWorkerOverProject(t *testing.T) {
	src := &fakeSource{
		servers: []ServerSpec{{ID: "w"}, {ID: "p"}, {ID: "none"}},
		worker:  []string{"w"},
		project: []string{"p"},
	}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Servers) != 1 || r.Servers[0].ID != "w" {
		t.Fatalf("worker selection should win, got %+v", r.Servers)
	}
}

func TestResolveProjectFallback(t *testing.T) {
	src := &fakeSource{
		servers: []ServerSpec{{ID: "p"}},
		project: []string{"p"},
	}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Servers) != 1 || r.Servers[0].ID != "p" {
		t.Fatalf("project selection should apply when no worker selection, got %+v", r.Servers)
	}
}

func TestResolveNone(t *testing.T) {
	src := &fakeSource{servers: []ServerSpec{{ID: "p"}}}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Servers) != 0 || len(r.SelectedIDs) != 0 {
		t.Fatalf("no selection → no servers, got %+v", r)
	}
}

func TestResolveMissingServer(t *testing.T) {
	src := &fakeSource{
		servers: []ServerSpec{{ID: "p"}},
		worker:  []string{"missing"},
	}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Missing) != 1 || r.Missing[0] != "missing" {
		t.Fatalf("selected-but-unconfigured should be reported missing, got %+v", r)
	}
}

func TestResolveNoopSource(t *testing.T) {
	r, err := Resolve(context.Background(), NoopConfigSource{}, "wid", "pid")
	if err != nil || len(r.Servers) != 0 {
		t.Fatalf("noop source: %+v err=%v", r, err)
	}
}

func TestManifestWorkerSelectionParses(t *testing.T) {
	src := ManifestConfigSource{
		TenantServers:   []ServerSpec{{ID: "s1"}, {ID: "s2"}},
		PermissionsJSON: []byte(`{"mcp_servers":[{"id":"s1","command":"fixture"}]}`),
	}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Servers) != 1 || r.Servers[0].ID != "s1" {
		t.Fatalf("manifest worker selection not applied: %+v", r.Servers)
	}
}

func TestManifestMalformedPermissionsDegrades(t *testing.T) {
	src := ManifestConfigSource{
		TenantServers:   []ServerSpec{{ID: "s1"}},
		PermissionsJSON: []byte(`{not json`),
	}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Servers) != 0 {
		t.Fatalf("malformed permissions should degrade to no selection: %+v", r.Servers)
	}
}

func TestResolveSelectedButUnconfiguredFailsActionably(t *testing.T) {
	// A selected id with no configured spec → Missing; the manager surfaces
	// an actionable error at Start for "fail" onError.
	src := &fakeSource{worker: []string{"ghost"}}
	r, err := Resolve(context.Background(), src, "wid", "pid")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Missing) != 1 {
		t.Fatalf("expected missing, got %+v", r)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle: Close kills stdio children; orphan prevention via parent death
// ---------------------------------------------------------------------------

// TestCloseKillsChild spawns a stdio fixture child and verifies Close
// terminates it.
func TestCloseKillsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	m := startManager(t, stdioSpec("srv"))
	pid := findChildPid(t, "srv")
	if pid == 0 {
		t.Fatal("no fixture child found")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, pid, 3*time.Second)
}

// TestOrphanPrevention verifies the PDEATHSIG wiring directly: spawn the
// fixture via newStdioTransport (the same path the manager uses), then
// kill the parent of the child (this test process) and assert the child
// dies with it. We achieve "kill the parent" by running the assertion in
// a child helper process whose only job is to spawn the fixture, print
// its pid, and sleep until killed.
func TestOrphanPrevention(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	helper := exec.Command(os.Args[0], "-test.run=TestOrphanHelper")
	helper.Env = append(os.Environ(), "ORCHICON_MCP_ORPHAN_HELPER=1")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	pid := readPidLine(t, stdout)
	// The child is alive: verify, then kill the helper. PDEATHSIG must take
	// the fixture child down with it.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("child %d not alive before kill: %v", pid, err)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = helper.Process.Wait()
	waitGone(t, pid, 5*time.Second)
}

// TestOrphanHelper spawns the stdio fixture through the same transport the
// manager uses (newStdioTransport), prints the child pid, then sleeps
// until the parent test kills it. When it is killed, PDEATHSIG must take
// the fixture child with it.
func TestOrphanHelper(t *testing.T) {
	if os.Getenv("ORCHICON_MCP_ORPHAN_HELPER") != "1" {
		return
	}
	tr, err := newStdioTransport(stdioSpec("orphan"))
	if err != nil {
		fmt.Println("ERR", err)
		return
	}
	if err := tr.Command.Start(); err != nil {
		fmt.Println("ERR", err)
		return
	}
	fmt.Println(tr.Command.Process.Pid)
	// Keep stdin open so the fixture stays alive; we are SIGKILLed by the
	// test, and PDEATHSIG then kills the fixture child.
	for {
		time.Sleep(time.Hour)
	}
}

// findChildPid locates the fixture stdio child (env marker
// ORCHICON_MCP_STDIO=1 + ORCHICON_MCP_SERVER_ID=<id>) under this
// process's own children.
func findChildPid(t *testing.T, serverID string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return 0
		}
		for _, e := range entries {
			pid := atoi(e.Name())
			if pid <= 0 {
				continue
			}
			env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
			if err != nil {
				continue
			}
			if !envHasMarker(env) || envServerID(env) != serverID {
				continue
			}
			stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			ppid, ok := parsePPID(stat)
			if ok && ppid == os.Getpid() {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// A zombie (state Z) is dead — it only persists until its parent
		// (PID 1 after reparenting) reaps it. Treat it as gone.
		if state, ok := procState(pid); ok && state == 'Z' {
			return
		}
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s", pid, timeout)
}

// procState reads the state character (3rd field) of /proc/<pid>/stat.
func procState(pid int) (byte, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	idx := strings.LastIndex(string(stat), ")")
	if idx < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[idx+1:]))
	if len(fields) == 0 {
		return 0, false
	}
	return fields[0][0], true
}

func atoi(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := atoi(s)
	if n == 0 {
		t.Fatalf("bad pid %q", s)
	}
	return n
}

// ---------------------------------------------------------------------------
// Sweep
// ---------------------------------------------------------------------------

// TestSweepReapsOrphans verifies SweepStaleChildren kills an MCP child
// whose parent is dead (PPID 1), while leaving a live parent's child
// alone. The orphan is simulated by spawning a fixture child WITHOUT
// PDEATHSIG (as an older control plane might have), then killing its
// intermediate parent process so it reparents to PID 1.
func TestSweepReapsOrphans(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	helper := exec.Command(os.Args[0], "-test.run=TestSweepHelper")
	helper.Env = append(os.Environ(), "ORCHICON_MCP_SWEEP_HELPER=1")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	pid := readPidLine(t, stdout)
	// Helper exits on its own (no PDEATHSIG on the child), so the fixture
	// child reparents to PID 1 and becomes an orphan.
	_ = helper.Wait()
	time.Sleep(300 * time.Millisecond) // let the kernel reparent

	log := testLogger(t)
	SweepStaleChildren(context.Background(), log)
	waitGone(t, pid, 3*time.Second)
}

// readPidLine reads the first line of a subprocess's stdout and parses it
// as a pid, tolerating trailing test-framework output (e.g. "PASS").
func readPidLine(t *testing.T, r io.Reader) int {
	t.Helper()
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		t.Fatalf("no pid line: %v", sc.Err())
	}
	return atoiOrFail(t, strings.TrimSpace(sc.Text()))
}

// TestSweepHelper spawns a fixture stdio child WITHOUT PDEATHSIG (so the
// child survives its parent's death and reparents to PID 1), prints the
// child pid, then dies immediately. The parent test then sweeps.
func TestSweepHelper(t *testing.T) {
	if os.Getenv("ORCHICON_MCP_SWEEP_HELPER") != "1" {
		return
	}
	argv := fixtureArgs()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID=orphan",
	)
	// No SysProcAttr: no PDEATHSIG, so the child survives us.
	if err := cmd.Start(); err != nil {
		fmt.Println("ERR", err)
		return
	}
	fmt.Println(cmd.Process.Pid)
	_ = cmd.Process.Release()
}

// testLogger builds a discard slog logger for sweep calls.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
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
