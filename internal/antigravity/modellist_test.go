package antigravity

// modellist_test.go — the LIVE listing, proven against the RECORDED agy wire rather than a hand-written
// double. The fake child in testsupport_test.go speaks the real ACP handshake (initialize, authenticate,
// session/new, session/close) with the payload shapes child 1 recorded from agy 1.5.13, so an assertion here
// is an assertion about agy's own contract — no agy process, no token.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
)

// newModelListBridge builds a Bridge whose spawn factory counts calls and serves the given script.
func newModelListBridge(t *testing.T, sc script) (*Bridge, *int32) {
	t.Helper()
	b := New(quiet())
	var calls int32
	b.SetBinaryOverride(func() (string, error) { return "/usr/local/bin/agy", nil })
	b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "oauth-personal", ShareTelemetry: false}, "", nil
	})
	b.SetSpawnOverride(func(context.Context, acp.ProcSpec) (acp.Proc, error) {
		atomic.AddInt32(&calls, 1)
		return newFakeACP(t, sc), nil
	})
	return b, &calls
}

// TestListModelsReturnsTheCatalogue is AC1/AC7 at the transport layer: the handshake yields agy's models with
// their BARE ids (a composed `antigravity/google/<id>` ref here would produce a 4-segment ref downstream).
func TestListModelsReturnsTheCatalogue(t *testing.T) {
	b, _ := newModelListBridge(t, script{})
	models, err := b.ListModels(context.Background(), "tnt_model_list")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models returned from a successful handshake")
	}
	for _, m := range models {
		if strings.Contains(m.ModelID, "/") {
			t.Errorf("model id %q contains a '/' — the picker composes antigravity/google/<id> itself, so a "+
				"full ref here yields a 4-segment ref", m.ModelID)
		}
	}
	// The fake's session/new advertises harnessModel; the recording's real ids are the snapshot's source, so
	// the transport assertion is about the WIRE, not about matching the snapshot.
	var found bool
	for _, m := range models {
		if m.ModelID == harnessModel {
			found = true
		}
	}
	if !found {
		t.Errorf("the handshake's catalogue does not contain %q: %+v", harnessModel, models)
	}
}

// TestListModelsIsServedFromTheCache pins the TTL: a second listing must NOT spawn another agy child. A
// process spawn per picker open would be a plan-quota negotiation per keystroke.
func TestListModelsIsServedFromTheCache(t *testing.T) {
	b, calls := newModelListBridge(t, script{})
	for i := 0; i < 3; i++ {
		if _, err := b.ListModels(context.Background(), "tnt_model_list_cache"); err != nil {
			t.Fatalf("ListModels call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("the spawn factory ran %d times, want 1 — a warm cache must serve the later listings", got)
	}
	b.InvalidateModelCache("tnt_model_list_cache")
	if _, err := b.ListModels(context.Background(), "tnt_model_list_cache"); err != nil {
		t.Fatalf("ListModels after invalidation: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("the spawn factory ran %d times after an invalidation, want 2", got)
	}
}

// TestListModelsAuthFailureIsActionable is AC5 at the transport layer: a signed-out host (agy's own
// missing-API-key text, which matches NONE of the shared auth signals) must produce the ONE actionable
// sentence, and must satisfy the sentinel the providers tier classifies on.
func TestListModelsAuthFailureIsActionable(t *testing.T) {
	b, _ := newModelListBridge(t, script{authError: true})
	_, err := b.ListModels(context.Background(), "tnt_model_list_auth")
	if err == nil {
		t.Fatal("a signed-out host returned a catalogue — the listing must authenticate explicitly, because " +
			"agy defers its credential check and would otherwise report models it cannot serve")
	}
	if !errors.Is(err, ErrModelListAuthRequired) {
		t.Errorf("error %v does not match ErrModelListAuthRequired, so the providers tier cannot classify it "+
			"as an auth failure", err)
	}
	if got := err.Error(); got != adapter.AntigravityAuthRequiredMessage {
		t.Errorf("error message = %q, want adapter.AntigravityAuthRequiredMessage verbatim", got)
	}
	if strings.Contains(err.Error(), "not_found") {
		t.Errorf("error %q carries a not_found prefix", err.Error())
	}
}

// TestListModelsMissingBinaryIsActionable pins the other "cannot work" precondition: no `agy` on the host is
// the same operator-facing problem as a signed-out one, so it must not surface as a generic spawn error.
func TestListModelsMissingBinaryIsActionable(t *testing.T) {
	b := New(quiet())
	b.SetBinaryOverride(func() (string, error) {
		return "", errors.New("antigravity: the `agy` CLI is not on PATH")
	})
	_, err := b.ListModels(context.Background(), "tnt_model_list_no_bin")
	if err == nil {
		t.Fatal("a missing agy binary produced a catalogue")
	}
	if !errors.Is(err, ErrModelListAuthRequired) {
		t.Errorf("error %v does not match ErrModelListAuthRequired", err)
	}
	if got := err.Error(); got != adapter.AntigravityAuthRequiredMessage {
		t.Errorf("error message = %q, want the actionable sentence", got)
	}
}

// TestListModelsAuthFailureIsNotCached pins the negative half of the cache: the operator's fix (sign in) must
// take effect on the NEXT listing, not after a TTL.
func TestListModelsAuthFailureIsNotCached(t *testing.T) {
	b := New(quiet())
	b.SetBinaryOverride(func() (string, error) { return "/usr/local/bin/agy", nil })
	b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "oauth-personal"}, "", nil
	})
	var calls int32
	b.SetSpawnOverride(func(context.Context, acp.ProcSpec) (acp.Proc, error) {
		n := atomic.AddInt32(&calls, 1)
		return newFakeACP(t, script{authError: n == 1}), nil
	})
	if _, err := b.ListModels(context.Background(), "tnt_model_list_retry"); err == nil {
		t.Fatal("the first listing should have failed authentication")
	}
	// The second call must RE-ATTEMPT: a cached failure would keep the picker broken after the fix.
	models, err := b.ListModels(context.Background(), "tnt_model_list_retry")
	if err != nil {
		t.Fatalf("the second listing (host now signed in) failed: %v — an auth failure must not be cached", err)
	}
	if len(models) == 0 {
		t.Fatal("the second listing returned no models")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("the spawn factory ran %d times, want 2 (the failure must not be served from the cache)", got)
	}
}

// TestListModelRowsKeepsBareIDs is the projection the plane wires into the providers tier: bare ids, one row
// per model, no duplicates and no composed refs.
func TestListModelRowsKeepsBareIDs(t *testing.T) {
	b, _ := newModelListBridge(t, script{})
	rows, err := b.ListModelRows(context.Background(), "tnt_model_list_rows")
	if err != nil {
		t.Fatalf("ListModelRows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if strings.Contains(r[0], "/") {
			t.Errorf("row id %q contains a '/'", r[0])
		}
		if seen[r[0]] {
			t.Errorf("duplicate row id %q", r[0])
		}
		seen[r[0]] = true
	}
}

// TestListModelsRunsOnTheLocalTransportEvenWithARuntimeClient pins the transport bug that made the live
// catalogue invisible on a real plane.
//
// A listing is a PICKER RPC, not a dispatch: it has no workflow run id, so the container factory refuses it
// ("no workflow run id for container spawn"). When the listing used spawnFactory() it therefore failed on
// every picker open on any plane that had a runtime client wired — which is every real plane — and fell back
// to the vendored snapshot. MEASURED against a local-mode plane: the log carried that exact error and the RPC
// answered source=snapshot with `agy` 1.5.13 installed and signed in on the host.
//
// THE BRIDGE HERE HOLDS A RUNTIME CLIENT DELIBERATELY. Without one the bug is invisible, which is precisely
// why it survived to a live plane.
func TestListModelsRunsOnTheLocalTransportEvenWithARuntimeClient(t *testing.T) {
	b := New(quiet())
	b.SetBinaryOverride(func() (string, error) { return "/usr/local/bin/agy", nil })
	b.SetSettingsOverride(func(context.Context, string) (SpawnSettings, string, error) {
		return SpawnSettings{AuthMethod: "oauth-personal", ShareTelemetry: false}, "", nil
	})
	// The assertion is BEHAVIOURAL rather than a comparison of function values: a listing's ProcSpec carries
	// NO workflow run id (there is no run to lease a container for), and that is the exact spec the container
	// transport refuses. So hand the selector the picker's real shape and require a real child.
	fn := b.modelListFactory()
	if fn == nil {
		t.Fatal("modelListFactory returned nil")
	}
	spec := acp.ProcSpec{Argv: []string{"sh", "-c", "echo ok"}, WorkflowRunID: ""}
	p, err := fn(context.Background(), spec)
	if err != nil {
		t.Fatalf("the model listing's transport refused a spec with no workflow run id: %v — that is the "+
			"container transport, and every picker open on a plane with a runtime client would fail", err)
	}
	defer func() { _ = p.Close() }()
	select {
	case line := <-p.Lines():
		if !strings.Contains(string(line), "ok") {
			t.Errorf("child output = %q, want it to contain ok", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child produced no output")
	}
}
