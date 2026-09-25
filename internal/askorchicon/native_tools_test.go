package askorchicon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// askFileRootTestCtx returns a context carrying the test tenant.
func askFileRootTestCtx() context.Context {
	return tenant.WithID(context.Background(), "tnt_dev")
}

// stubAskFileRoot pins the suite boundary to dir for the duration of a
// test (no DB needed) and returns the restore func.
func stubAskFileRoot(dir string) func() {
	return askFileRootStub(func(_ context.Context, _ *db.Pool) (AskFileScope, error) {
		return AskFileScope{Dir: dir, FromConversation: true}, nil
	})
}

// TestNativeAskToolDefsCarryJSONSchema (kept from the pre-parity adapter)
// still holds with the combined surface: product tools ride the registry
// schema rendering.
func TestNativeAskToolDefsCarryJSONSchema(t *testing.T) {
	r := testToolRegistry()
	r.Add(ToolDefinition{
		Name:        "ping",
		Description: "Ping",
		Properties:  map[string]PropertySchema{"target": {Type: "string", Description: "Host"}},
		Required:    []string{"target"},
	})
	p := (&Service{toolRegistry: r}).NativeAskTools()
	defs := p.AskToolDefs(context.Background())
	var found bool
	for _, d := range defs {
		if d.Name != "ping" {
			continue
		}
		found = true
		var schema map[string]any
		if err := json.Unmarshal([]byte(d.ParamsJSON), &schema); err != nil {
			t.Fatalf("ping ParamsJSON is not JSON: %v", err)
		}
		if schema["type"] != "object" {
			t.Fatalf("schema = %v, want object", schema)
		}
		props, _ := schema["properties"].(map[string]any)
		if props["target"] == nil {
			t.Fatalf("schema properties = %v, want target", schema["properties"])
		}
	}
	if !found {
		t.Fatal("ping def missing from native Ask tools")
	}
}

// TestNativeAskToolExecuteUnknownErrors: unknown names error LOUD.
func TestNativeAskToolExecuteUnknownErrors(t *testing.T) {
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	if _, err := p.ExecuteAskTool(context.Background(), "nope", `{}`); err == nil ||
		!strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown tool err = %v, want not-registered", err)
	}
}

// TestNativeAskDefsExposeHostSuite is the parity gate: the combined Ask
// tool surface MUST carry every file/shell tool the worker path serves
// (HostTools' suite) with the SAME arg shapes, plus the product tools.
func TestNativeAskDefsExposeHostSuite(t *testing.T) {
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	got := map[string]string{}
	for _, d := range p.AskToolDefs(context.Background()) {
		got[d.Name] = d.ParamsJSON
	}
	for _, name := range hostSuiteToolNames {
		if got[name] == "" {
			t.Errorf("host suite tool %q missing from AskToolDefs — parity broken", name)
		}
	}
	if got["ask_file_root"] == "" {
		t.Error("ask_file_root probe missing from AskToolDefs")
	}
	// One-grammar check: bash's ParamsJSON must match the worker path's
	// definition verbatim (same definition source).
	var workerDefs string
	for _, d := range askHostToolsForRoot(nil, AskFileScope{}, "").Defs() {
		if d.Name == "bash" {
			workerDefs = d.ParamsJSON
		}
	}
	if got["bash"] != workerDefs {
		t.Errorf("bash ParamsJSON drifted from the worker path:\n ask:    %s\n worker: %s", got["bash"], workerDefs)
	}
}

// TestNativeAskExecuteFileSuiteRoundTrip drives the REAL HostTools engine
// through the Ask provider: write → read → grep → list → edit round trip
// inside a temp project dir.
func TestNativeAskExecuteFileSuiteRoundTrip(t *testing.T) {
	r := testToolRegistry()
	svc := &Service{toolRegistry: r}
	p := svc.NativeAskTools()
	ctx := context.Background()

	// write (thin wrapper) — note the file tools resolve project-relative
	// against the suite root; the suite root here is wired by swapping the
	// service root through askFileRootOverride (no DB in this test).
	tmp := t.TempDir()
	restore := stubAskFileRoot(tmp)
	defer restore()

	out, err := p.ExecuteAskTool(ctx, "write", `{"filePath":"notes.md","content":"# hello\n"}`)
	if err != nil || !strings.Contains(out, "notes.md") {
		t.Fatalf("write: err=%v out=%q", err, out)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "notes.md"))
	if err != nil || string(data) != "# hello\n" {
		t.Fatalf("file content = %q err=%v", data, err)
	}
	// read
	out, err = p.ExecuteAskTool(ctx, "read", `{"path":"notes.md"}`)
	if err != nil || !strings.Contains(out, "# hello") {
		t.Fatalf("read: err=%v out=%q", err, out)
	}
	// grep
	out, err = p.ExecuteAskTool(ctx, "grep", `{"pattern":"hello"}`)
	if err != nil || !strings.Contains(out, "notes.md") {
		t.Fatalf("grep: err=%v out=%q", err, out)
	}
	// list
	out, err = p.ExecuteAskTool(ctx, "list", `{}`)
	if err != nil || !strings.Contains(out, "notes.md") {
		t.Fatalf("list: err=%v out=%q", err, out)
	}
	// edit
	out, err = p.ExecuteAskTool(ctx, "edit", `{"filePath":"notes.md","oldString":"hello","newString":"goodbye"}`)
	if err != nil || !strings.Contains(out, "notes.md") {
		t.Fatalf("edit: err=%v out=%q", err, out)
	}
	data, err = os.ReadFile(filepath.Join(tmp, "notes.md"))
	if err != nil || !strings.Contains(string(data), "goodbye") {
		t.Fatalf("post-edit content = %q err=%v", data, err)
	}
	// batch_write
	out, err = p.ExecuteAskTool(ctx, "batch_write", `{"writes":[{"path":"b/inner.txt","mode":"create","content":"deep"}]}`)
	if err != nil || !strings.Contains(out, "inner.txt") {
		t.Fatalf("batch_write: err=%v out=%q", err, out)
	}
	// batch_read (directory expansion)
	out, err = p.ExecuteAskTool(ctx, "batch_read", `{"paths":["."]}`)
	if err != nil || !strings.Contains(out, "inner.txt") {
		t.Fatalf("batch_read: err=%v out=%q", err, out)
	}
	// bash: in-process, cwd = the project dir
	out, err = p.ExecuteAskTool(ctx, "bash", `{"command":"pwd"}`)
	if err != nil || !strings.Contains(out, filepath.Base(tmp)) {
		t.Fatalf("bash pwd: err=%v out=%q", err, out)
	}
}

// TestNativeAskFileSuiteReachesTheRealFilesystem is the INTERACTIVE boundary:
// the Ask suite's allowed set is the whole filesystem. It supersedes the old
// TestNativeAskFileSuiteEscapeRefused, which asserted the confined behaviour
// this change deliberately removes from the Ask path (the confinement now
// lives ONLY on the worker path — see hosttools_unrestricted_test.go).
func TestNativeAskFileSuiteReachesTheRealFilesystem(t *testing.T) {
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	ctx := context.Background()
	root := t.TempDir()
	restore := stubAskFileRoot(root)
	defer restore()

	// A file the operator can read, OUTSIDE the suite root/anchor.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("real filesystem reach"), 0o644); err != nil {
		t.Fatal(err)
	}

	// read: an absolute path outside the anchor now succeeds with no escape error.
	out, err := p.ExecuteAskTool(ctx, "read", `{"path":`+jsonString(outside)+`}`)
	if err != nil {
		t.Fatalf("read outside the suite root failed (interactive boundary too narrow): %v", err)
	}
	if !strings.Contains(out, "real filesystem reach") {
		t.Fatalf("read returned %q, want the outside file's content", out)
	}

	// batch_read: the composite tool reaches it too.
	out, err = p.ExecuteAskTool(ctx, "batch_read", `{"paths":[`+jsonString(outside)+`]}`)
	if err != nil || !strings.Contains(out, "real filesystem reach") {
		t.Fatalf("batch_read outside the suite root: err=%v out=%q", err, out)
	}

	// A plain relative path STILL resolves against the conversation's project.
	if _, err := p.ExecuteAskTool(ctx, "write", `{"filePath":"inside.txt","content":"anchored"}`); err != nil {
		t.Fatalf("relative write: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "inside.txt")); err != nil || string(data) != "anchored" {
		t.Fatalf("relative path did not anchor at the suite root: data=%q err=%v", data, err)
	}
}

// TestNativeAskBashCwdIsTheSuiteRoot: bash starts in the conversation's project
// (the suite anchor) and can operate elsewhere after a `cd`.
func TestNativeAskBashCwdIsTheSuiteRoot(t *testing.T) {
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	ctx := context.Background()
	root := t.TempDir()
	restore := stubAskFileRoot(root)
	defer restore()

	out, err := p.ExecuteAskTool(ctx, "bash", `{"command":"pwd"}`)
	if err != nil {
		t.Fatalf("bash pwd: %v", err)
	}
	if !strings.Contains(out, root) {
		t.Fatalf("bash default cwd = %q, want the suite root %q", out, root)
	}

	elsewhere := t.TempDir()
	out, err = p.ExecuteAskTool(ctx, "bash", `{"command":"cd `+elsewhere+` && pwd"}`)
	if err != nil {
		t.Fatalf("bash cd elsewhere: %v", err)
	}
	if !strings.Contains(out, elsewhere) {
		t.Fatalf("bash cd elsewhere printed %q, want %q", out, elsewhere)
	}
}

// jsonString renders s as a JSON string literal for embedding in an args blob.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestHostSuiteSplitAgreesWithAskmode pins acceptance criterion 6: the
// read-set and the gate-set come from ONE definition (askmode) and the host
// suite's partition agrees with it exactly — a name added to the suite that
// askmode denies, or a gate name missing from the suite, fails here.
func TestHostSuiteSplitAgreesWithAskmode(t *testing.T) {
	// The gate-set, derived from askmode (never re-listed as an independent
	// source of truth beyond this pin).
	gateNames := askmode.DeniedNames(askmode.Brainstorm)
	gate := map[string]bool{}
	for _, n := range gateNames {
		gate[n] = true
	}
	wantGate := map[string]bool{"bash": true, "batch_write": true, "edit": true, "write": true}
	if len(gate) != len(wantGate) {
		t.Fatalf("askmode gate-set = %v, want %v", gateNames, wantGate)
	}
	for n := range wantGate {
		if !gate[n] {
			t.Fatalf("askmode no longer denies %q — the host suite gate-set drifted", n)
		}
	}

	suite := map[string]bool{}
	for _, n := range hostSuiteToolNames {
		suite[n] = true
	}
	// Every gate name must be present in the suite (the enforced surface).
	for _, n := range gateNames {
		if !suite[n] {
			t.Fatalf("askmode denies %q but it is not in hostSuiteToolNames", n)
		}
	}
	// Brainstorm and Quick Work deny the same set: the suite partition must
	// match askmode's decision for every host-suite name, both directions.
	for _, mode := range []string{askmode.Brainstorm, askmode.QuickWork} {
		for _, n := range hostSuiteToolNames {
			if got, want := askmode.Allows(mode, n), !gate[n]; got != want {
				t.Errorf("askmode.Allows(%s, %q) = %v, want %v — the read-set/gate-set disagree", mode, n, got, want)
			}
		}
	}
	// Iteration denies the plan tools, which are disjoint from the host suite.
	for _, n := range hostSuiteToolNames {
		if !askmode.Allows(askmode.Iteration, n) {
			t.Errorf("Iteration mode denies host-suite tool %q — the gate-set and read-set must stay disjoint", n)
		}
	}

	// Drift guard on the suite list itself: the defs the suite advertises and
	// the dispatch list cannot diverge.
	defs := map[string]bool{}
	for _, d := range askHostToolsForRoot(nil, AskFileScope{}, "").Defs() {
		defs[d.Name] = true
	}
	for _, n := range hostSuiteToolNames {
		if !defs[n] {
			t.Errorf("hostSuiteToolNames lists %q but Defs() does not advertise it", n)
		}
	}
	for n := range defs {
		if !suite[n] {
			t.Errorf("Defs() advertises %q but hostSuiteToolNames does not list it", n)
		}
	}
}

// TestNativeAskExecuteProductToolRoutesThroughRegistry: product tools still
// resolve through the registry (list path untouched).
func TestNativeAskExecuteProductToolRoutesThroughRegistry(t *testing.T) {
	r := testToolRegistry()
	r.Add(ToolDefinition{
		Name: "echo_tool",
		Fn: func(_ context.Context, _ *db.Pool, _ json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	})
	p := (&Service{toolRegistry: r}).NativeAskTools()
	out, err := p.ExecuteAskTool(context.Background(), "echo_tool", `{}`)
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("echo_tool: err=%v out=%q", err, out)
	}
}

// TestNativeAskBashGuardBlocksDestructive proves the OS-level guard rides
// Ask-path bash: `sudo`/`dd`-class binaries are refused even though bash
// itself runs in-process (the worker path's backstop, now on Ask too).
func TestNativeAskBashGuardBlocksDestructive(t *testing.T) {
	r := testToolRegistry()
	p := (&Service{toolRegistry: r}).NativeAskTools()
	tmp := t.TempDir()
	restore := stubAskFileRoot(tmp)
	defer restore()

	// sudo is an always-block binary: the guard shim exits 1.
	out, err := p.ExecuteAskTool(context.Background(), "bash", `{"command":"sudo true"}`)
	if err != nil {
		t.Fatalf("sudo probe returned transport error (want refusal as result): %v", err)
	}
	if !strings.Contains(out, "ORCHICON GUARD") {
		t.Fatalf("bash `sudo` was not guard-refused: out=%q", out)
	}
	// rm scoped: a target OUTSIDE the project dir is refused.
	out, err = p.ExecuteAskTool(context.Background(), "bash", `{"command":"rm /etc/passwd"}`)
	if err != nil {
		t.Fatalf("rm probe returned transport error (want refusal as result): %v", err)
	}
	if !strings.Contains(out, "ORCHICON GUARD") {
		t.Fatalf("bash `rm /etc/passwd` was not guard-refused: out=%q", out)
	}
	// And the guard dir is genuinely FIRST on PATH for Ask bash.
	out, err = p.ExecuteAskTool(context.Background(), "bash", `{"command":"echo $PATH"}`)
	if err != nil {
		t.Fatalf("PATH probe: %v", err)
	}
	if !strings.Contains(out, "orchicon-guard-") {
		t.Fatalf("guard dir not first on Ask bash PATH: %q", out)
	}
}

// TestCloseAskGuardResetsState: shutdown is idempotent and re-arms the
// lazy guard (test re-mounts rebuild fresh).
func TestCloseAskGuardResetsState(t *testing.T) {
	if _, err := askGuardForExec(); err != nil {
		t.Fatalf("guard create: %v", err)
	}
	CloseAskGuard()
	askGuardState.Lock()
	inited := askGuardState.init
	askGuardState.Unlock()
	if inited {
		t.Fatal("CloseAskGuard left the guard initialized")
	}
}
