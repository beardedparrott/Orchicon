package askorchicon

// ask_file_scope_test.go — THE FILE/SHELL SUITE BINDS TO THE CONVERSATION'S PROJECT, and the probe says
// which boundary it is on. No DB: the resolver seam (askFileRootStub) is stubbed, so these run everywhere.
//
// The defect this pins: AskFileRoot used to resolve the TENANT's first active project with a project_dir,
// conversation-blind, while the prompt named the CONVERSATION's project. With two active projects the two
// halves disagreed silently and every relative write landed in the wrong tree. The fix routes both halves
// through one value (withAskConversationProject → AskFileScopeFor), and these tests hold that line.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// THE PROJECT STAMP RIDES THE SAME CONTEXT AS THE MODE AND THE CONVERSATION ID, and none clobbers another —
// that is what lets the prompt half and the tool half read one value instead of two DB rows that drift.
func TestConversationProjectRidesTheTurnContext(t *testing.T) {
	ctx := withAskConversationProject(context.Background(), "p1")
	if got := askConversationProjectFromContext(ctx); got != "p1" {
		t.Fatalf("askConversationProjectFromContext = %q, want \"p1\"", got)
	}
	// Unstamped and nil both mean "no project", deliberately not a sentinel: one representation, one code path.
	if got := askConversationProjectFromContext(context.Background()); got != "" {
		t.Fatalf("unstamped context = %q, want empty", got)
	}
	if got := askConversationProjectFromContext(nil); got != "" {
		t.Fatalf("nil context = %q, want empty", got)
	}
	if got := askConversationProjectFromContext(withAskConversationProject(context.Background(), "")); got != "" {
		t.Fatalf("explicit empty project = %q, want empty", got)
	}

	both := withAskMode(context.Background(), modeIteration)
	both = withAskConversation(both, "conv_1")
	both = withAskConversationProject(both, "p9")
	if askModeFromContext(both) != modeIteration || askConversationFromContext(both) != "conv_1" ||
		askConversationProjectFromContext(both) != "p9" {
		t.Fatalf("stamps clobbered one another: mode=%q conv=%q project=%q",
			askModeFromContext(both), askConversationFromContext(both), askConversationProjectFromContext(both))
	}
}

// THE PROBE ENVELOPE MUST NAME WHICH BOUNDARY IT IS ON. A fallback anchor presented as the conversation's own
// project is exactly the silent lie the fix removes; the machine-readable `scope` is what the consent layer
// (the next work item) branches on.
func TestAskFileRootEnvelopeNamesTheScope(t *testing.T) {
	p := (&Service{}).NativeAskTools()
	ctx := context.Background()

	restoreConv := askFileRootStub(func(_ context.Context, _ *db.Pool) (AskFileScope, error) {
		return AskFileScope{Dir: "/tmp/scoped", ProjectID: "p1", FromConversation: true}, nil
	})
	defer restoreConv()
	out, err := p.ExecuteAskTool(ctx, askFileRootToolName, `{}`)
	if err != nil {
		t.Fatalf("ask_file_root (assigned): %v", err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("assigned envelope is not JSON: %v (%s)", err, out)
	}
	if env["project_dir"] != "/tmp/scoped" || env["scope"] != askScopeConversation || env["project_id"] != "p1" {
		t.Fatalf("assigned envelope = %v, want scope=%q project_id=p1 dir=/tmp/scoped", env, askScopeConversation)
	}

	restoreAnchor := askFileRootStub(func(_ context.Context, _ *db.Pool) (AskFileScope, error) {
		return AskFileScope{Dir: "/tmp/anchor"}, nil
	})
	defer restoreAnchor()
	out, err = p.ExecuteAskTool(ctx, askFileRootToolName, `{}`)
	if err != nil {
		t.Fatalf("ask_file_root (unassigned): %v", err)
	}
	env = nil
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unassigned envelope is not JSON: %v (%s)", err, out)
	}
	if env["project_dir"] != "/tmp/anchor" || env["scope"] != askScopeTenantAnchor {
		t.Fatalf("unassigned envelope = %v, want scope=%q dir=/tmp/anchor", env, askScopeTenantAnchor)
	}
	if _, ok := env["project_id"]; ok {
		t.Fatalf("an unassigned conversation reported a project_id: %v", env)
	}
	note := strings.ToLower(env["note"])
	if !strings.Contains(note, "no project") || !strings.Contains(note, "outside") || !strings.Contains(note, "consent") {
		t.Fatalf("unassigned note must say the conversation has no project, mark work outside scope, and name consent; got %q", env["note"])
	}
	if strings.Contains(env["note"], "IS this conversation's project") {
		t.Fatalf("the fallback anchor is presented as the conversation's own project: %q", env["note"])
	}
}

// THE SUITE USES THE RESOLVED SCOPE, and resolves it EXACTLY ONCE PER CALL: the probe and every host-suite
// call go through the same seam, and nothing is cached across calls (the per-execution resolution the work
// item requires — and the "no extra DB round trip" AC: one resolution, not one per tool).
func TestAskFileSuiteUsesTheResolvedScope(t *testing.T) {
	p := (&Service{}).NativeAskTools()
	dir := t.TempDir()

	calls := 0
	restore := askFileRootStub(func(_ context.Context, _ *db.Pool) (AskFileScope, error) {
		calls++
		return AskFileScope{Dir: dir, FromConversation: true}, nil
	})
	defer restore()

	out, err := p.ExecuteAskTool(context.Background(), "write", `{"filePath":"scoped.md","content":"hi\n"}`)
	if err != nil || !strings.Contains(out, "scoped.md") {
		t.Fatalf("write: err=%v out=%q", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "scoped.md")); err != nil || string(data) != "hi\n" {
		t.Fatalf("file content = %q err=%v", data, err)
	}
	if calls != 1 {
		t.Fatalf("a host-suite call resolved the root %d times, want 1", calls)
	}
	// The probe is the SAME resolution, fresh per call (never a package-level cache).
	before := calls
	if _, err := p.ExecuteAskTool(context.Background(), askFileRootToolName, `{}`); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if calls != before+1 {
		t.Fatalf("the probe resolved the root %d times, want exactly 1 per call", calls-before)
	}
}
