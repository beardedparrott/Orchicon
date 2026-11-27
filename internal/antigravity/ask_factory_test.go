package antigravity

// ask_factory_test.go — the Ask conversation's child runs on the LOCAL transport, and a plane with a
// runtime client wired must not break it.
//
// WHY THIS TEST EXISTS AT ALL. `internal/aigateway` proves the bridge is Ask-capable, and `ask_harness_test.go`
// proves the transport against the recorded wire — both with a factory OVERRIDE, which is exactly the
// configuration in which this bug is invisible. On a real plane the bridge holds a *runtime.Client, and the
// Ask path called spawnFactory(), which prefers the CONTAINER transport whenever one is wired. A
// conversation is not a workflow run: it has no run id, and the container factory refuses that spec with
//
//	acp: no workflow run id for container spawn — the daemon leases containers by RUN id
//
// so EVERY antigravity Ask turn died on its first spawn. MEASURED on a local-mode plane (:18130, `agy`
// 1.5.13 installed and signed in, a project with a project_dir, an ITERATION conversation on
// antigravity/google/gemini-4-neon-low): the turn's only event was
//
//	{"error":{"message":"create conversation session: antigravity ask: spawn: acp: no workflow run id for container spawn","code":"turn_failed"}}
//
// No fixture caught it because the fixtures override the factory. The model listing had the SAME defect and
// was pinned the same way (modellist_test.go TestListModelsRunsOnTheLocalTransportEvenWithARuntimeClient);
// this is the Ask half of that pin.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// TestAskFactoryIsLocalEvenWithARuntimeClient is the pin: askFactory() must never return the container
// transport, because the spec an Ask conversation spawns with can never carry a workflow run id.
//
// IT IS BEHAVIOURAL, NOT A FUNCTION-VALUE COMPARISON. The assertion hands askFactory() the ProcSpec an Ask
// child really has (no WorkflowRunID) and requires a real host child to run. The container transport refuses
// that exact spec, so a regression here fails on the spawn rather than on an inscrutable identity check.
func TestAskFactoryIsLocalEvenWithARuntimeClient(t *testing.T) {
	b := New(quiet())
	fn := b.askFactory()
	if fn == nil {
		t.Fatal("askFactory returned nil")
	}

	// THE SPEC AN ASK CONVERSATION ACTUALLY SPAWNS WITH: procSpec.ExecID = "ask:<conv>", Cwd set, and NO
	// workflow run id (a conversation has no run to lease a container by).
	spec := acp.ProcSpec{Argv: []string{"sh", "-c", "echo ask-local-ok"}, ExecID: "ask:conv_test"}
	p, err := fn(context.Background(), spec)
	if err != nil {
		t.Fatalf("askFactory refused the spec an Ask conversation spawns with: %v — that is the container "+
			"transport, and every antigravity Ask turn on a plane with a runtime client would fail on its "+
			"first spawn", err)
	}
	defer func() { _ = p.Close() }()

	select {
	case line := <-p.Lines():
		if !strings.Contains(string(line), "ask-local-ok") {
			t.Errorf("child output = %q, want it to contain ask-local-ok", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Ask child produced no output — the local transport did not run it")
	}
}

// TestAskFactoryHonoursTheTestOverride keeps the fixtures honest: every Ask test in this package drives the
// recorded wire through SetSpawnOverride, so an askFactory() that ignored the override would silently send
// the whole suite at the host's real `agy`.
func TestAskFactoryHonoursTheTestOverride(t *testing.T) {
	b := New(quiet())
	called := false
	b.SetSpawnOverride(func(context.Context, acp.ProcSpec) (acp.Proc, error) {
		called = true
		return newFakeACP(t, script{}), nil
	})
	p, err := b.askFactory()(context.Background(), acp.ProcSpec{Argv: []string{"agy", "acp"}})
	if err != nil {
		t.Fatalf("the overridden factory returned an error: %v", err)
	}
	_ = p.Close()
	if !called {
		t.Fatal("askFactory did not honour SetSpawnOverride — the Ask fixtures would drive the host's real agy")
	}
}

// TestAskTransportDoesNotNeedARunID is the direct statement of the cause, so a future reader does not have
// to infer it from a teardown of the other two: the Ask ProcSpec carries no WorkflowRunID, and that is what
// the container transport cannot serve.
func TestAskTransportDoesNotNeedARunID(t *testing.T) {
	spec := acp.ProcSpec{Argv: []string{"agy", "acp"}, ExecID: "ask:conv_x"}
	if spec.WorkflowRunID != "" {
		t.Fatal("the Ask ProcSpec must not carry a workflow run id — a conversation is not a run")
	}
	// And the container factory's own refusal names that field, so a regression that routes Ask through
	// spawnFactory() cannot be mistaken for an unrelated spawn failure.
	var opener stubStdioOpener
	if _, err := acp.ContainerFactory(opener)(context.Background(), spec); err == nil ||
		!strings.Contains(err.Error(), "no workflow run id") {
		t.Fatalf("the container transport's refusal changed shape (%v) — the diagnosis in askFactory's "+
			"comment and this test both depend on it naming the missing run id", err)
	}
}

// stubStdioOpener satisfies acp.StdioOpener without opening anything: the container factory refuses the
// spec before it ever reaches the opener, and the opener panics if that ordering ever changes.
type stubStdioOpener struct{}

func (stubStdioOpener) Stdio(context.Context, string, runtime.StdioRequest) (*runtime.StdioSession, error) {
	panic("the container factory must refuse a run-less spec before calling the opener")
}
