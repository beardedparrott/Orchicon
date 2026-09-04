package orchicon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hosttools_test.go: the host tool suite (ADR-0007's deferred tool-suite
// task). Pins: the full core suite is advertised; read/write/edit/grep/
// glob/list/bash round-trip against a real temp dir; every path escapes
// the containment boundary; the bridge composes host + MCP (dedup, host
// wins).

func TestHostToolsDefsShape(t *testing.T) {
	h := NewHostTools(t.TempDir(), "")
	defs := h.Defs()
	want := []string{"batch_read", "batch_grep", "batch_write", "read", "grep", "write", "edit", "list", "glob", "bash", "todoread"}
	if len(defs) != len(want) {
		t.Fatalf("Defs = %d tools, want %d", len(defs), len(want))
	}
	seen := map[string]bool{}
	for i, d := range defs {
		if d.Name != want[i] {
			t.Errorf("Defs[%d] = %q, want %q (advertised order is the model-facing priority)", i, d.Name, want[i])
		}
		if d.Description == "" || d.ParamsJSON == "" {
			t.Errorf("%s: empty description or params", d.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(d.ParamsJSON), &schema); err != nil {
			t.Errorf("%s: ParamsJSON invalid: %v", d.Name, err)
		}
		seen[d.Name] = true
	}
	// Loop parity gates (isFileWritingTool / stashMutableToolCall).
	for _, n := range []string{"write", "edit", "batch_write"} {
		if !seen[n] {
			t.Errorf("loop treats %q as a file-writing tool; not advertised", n)
		}
	}
	// The engine's static capabilities promise this suite — the registry
	// must actually carry it (the "tool registry not configured" bug).
	for _, n := range []string{"read", "write", "edit", "glob", "grep", "bash", "todowrite-adjacent-todoread"} {
		_ = n
	}
	for _, n := range []string{"read", "write", "edit", "glob", "grep", "bash"} {
		if !seen[n] {
			t.Errorf("capabilities advertise %q; registry does not", n)
		}
	}
}

func TestHostToolsReadWriteEditRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := NewHostTools(dir, "")
	ctx := context.Background()

	if _, err := h.Execute(ctx, "write", `{"filePath":"notes/hello.md","content":"# Hi\n\nworld"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Parent dirs created implicitly by the engine (batch_write create).
	out, err := h.Execute(ctx, "read", `{"path":"notes/hello.md"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("read output missing content: %q", out)
	}
	if _, err := h.Execute(ctx, "edit", `{"filePath":"notes/hello.md","oldString":"world","newString":"there"}`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	out, err = h.Execute(ctx, "read", `{"path":"notes/hello.md"}`)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !strings.Contains(out, "there") || strings.Contains(out, "world") {
		t.Errorf("edit did not apply: %q", out)
	}
}

func TestHostToolsGrepGlobList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHostTools(dir, "")
	ctx := context.Background()

	grepOut, err := h.Execute(ctx, "grep", `{"pattern":"package sub"}`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(grepOut, filepath.Join("sub", "b.go")) {
		t.Errorf("grep miss: %q", grepOut)
	}
	globOut, err := h.Execute(ctx, "glob", `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var g struct {
		Matches []string `json:"matches"`
	}
	if err := json.Unmarshal([]byte(globOut), &g); err != nil {
		t.Fatalf("glob json: %v", err)
	}
	if len(g.Matches) != 2 {
		t.Errorf("glob matches = %v, want 2 (a.go + sub/b.go)", g.Matches)
	}
	listOut, err := h.Execute(ctx, "list", `{"paths":["."]}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listOut, "sub") {
		t.Errorf("list miss: %q", listOut)
	}
}

func TestHostToolsBash(t *testing.T) {
	dir := t.TempDir()
	h := NewHostTools(dir, "")
	ctx := context.Background()

	out, err := h.Execute(ctx, "bash", `{"command":"pwd && echo hi"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(out, "hi") {
		t.Errorf("bash output: %q", out)
	}
	if !strings.HasPrefix(out, dir) {
		t.Errorf("bash cwd = want %s in output %q", dir, out)
	}
	// Non-zero exit is a RESULT (stdout+stderr returned), not an error.
	out, err = h.Execute(ctx, "bash", `{"command":"echo oops >&2; exit 3"}`)
	if err != nil {
		t.Fatalf("bash nonzero exit: %v", err)
	}
	if !strings.Contains(out, "oops") {
		t.Errorf("bash nonzero-exit output: %q", out)
	}
	// Timeout is enforced (1s wall).
	_, err = h.Execute(ctx, "bash", `{"command":"sleep 30","timeout_seconds":1}`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("bash timeout err = %v, want timed out", err)
	}
}

func TestHostToolsContainment(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHostTools(dir, "")
	ctx := context.Background()

	// Escape attempts are refused by the engine (or resolve to nothing).
	if _, err := h.Execute(ctx, "read", `{"path":"../secret.txt"}`); err == nil {
		t.Error("read escaped the worktree root — containment broken")
	}
	if _, err := h.Execute(ctx, "read", `{"path":"`+filepath.Join(outside, "secret.txt")+`"}`); err == nil {
		t.Error("read accepted an absolute path outside the boundary")
	}
	// Scratch is writable, project root (empty here) is not reachable.
	if _, err := h.Execute(ctx, "write", `{"filePath":"ok.txt","content":"x"}`); err != nil {
		t.Errorf("in-boundary write failed: %v", err)
	}
}

func TestCombinedRegistryComposeDedupHostWins(t *testing.T) {
	host := NewHostTools(t.TempDir(), "")
	mcp := &fakeRegistry{defs: []ToolDef{
		{Name: "mcp__srv__weather", ParamsJSON: `{}`},
		{Name: "read", ParamsJSON: `{}`}, // shadow attempt: host wins
	}}
	c := &combinedRegistry{primary: host, secondary: mcp}
	defs := c.Defs()
	count := 0
	seen := map[string]bool{}
	for _, d := range defs {
		if seen[d.Name] {
			t.Errorf("duplicate def %q after dedup", d.Name)
		}
		seen[d.Name] = true
		count++
	}
	if !seen["mcp__srv__weather"] || !seen["read"] {
		t.Errorf("defs missing MCP or host tools: %v", seen)
	}
	if count != len(host.Defs())+1 {
		t.Errorf("defs = %d, want host(%d)+1 unique MCP", count, len(host.Defs()))
	}
	// Execute routes by name.
	out, err := c.Execute(context.Background(), "read", `{"path":"x"}`)
	if err != nil || out == "" {
		t.Errorf("host read via combined: %v %q", err, out)
	}
	if _, err := c.Execute(context.Background(), "mcp__srv__weather", `{}`); err != nil {
		t.Errorf("mcp tool via combined: %v", err)
	}
}

// fakeRegistry is a minimal secondary registry for composition tests.
type fakeRegistry struct{ defs []ToolDef }

func (f *fakeRegistry) Defs() []ToolDef { return f.defs }
func (f *fakeRegistry) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	return `{"ok":"` + name + `"}`, nil
}
