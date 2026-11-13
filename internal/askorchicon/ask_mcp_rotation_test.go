package askorchicon

// ask_mcp_rotation_test.go — BUG 2: saving a secret must refresh a LIVE MCP client.
//
// THE BUG. The per-conversation client's fingerprint was the project id plus the resolved server IDs —
// nothing about headers, env, or the CREDENTIAL. refreshAskMCP compared exactly that, so editing a
// header or rotating a secret left the fingerprint identical and the live client was kept, holding the
// previous credential. The operator's symptom: the correct header written, has_secret_stored true, and
// every tool call still 401 until the plane was restarted.
//
// WHAT THESE TESTS PIN. The fingerprint is taken from the RESOLVED specs — the bytes the client is
// handed — so a rotation (or a header edit) changes it and the client is rebuilt on the next turn. The
// rotation is proved WITHOUT restarting anything: the store is written between two refreshAskMCP calls.

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"errors"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// errTestNoSecret stands in for ResolveSecretRefs refusing a ${NAME} nobody stored.
var errTestNoSecret = errors.New(`secret "MISSING_SECRET" referenced by the server is not stored`)

// installFakeClient makes every client this test starts a fake that records its Close, and returns the
// clients in start order so a test can tell a REBUILD from a kept one.
func installFakeClient(t *testing.T) *[]*askMCPFakeClient {
	t.Helper()
	var started []*askMCPFakeClient
	restore := stubAskMCPConnect(func(context.Context, []mcpclient.ServerSpec) (askMCPClient, error) {
		c := &askMCPFakeClient{
			defs:    []mcpclient.ToolDef{{Name: mcpclient.ToolName("pixellab", "get_balance")}},
			execOut: `{"balance":1}`,
		}
		started = append(started, c)
		return c, nil
	})
	t.Cleanup(restore)
	return &started
}

// rotationResolver is a scope resolver whose ONE server's header is whatever the test currently holds
// — the seam that stands in for "the operator saved a new secret value".
type rotationResolver struct {
	header string
}

func (r *rotationResolver) ResolveScope(_ context.Context, _ mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	return mcpclient.Resolution{
		Servers: []mcpclient.ScopedServer{{
			Spec: mcpclient.ServerSpec{
				ID:      "pixellab",
				Type:    mcpclient.TypeHTTP,
				URL:     "https://api.pixellab.ai/mcp",
				Headers: map[string]string{"Authorization": r.header},
			},
			From:    mcpclient.ScopeProject,
			FromID:  "project:p1",
			EntryID: "pixellab",
		}},
		SelectedIDs: []string{"pixellab"},
	}, nil
}

// seedSecret is the secret-expansion seam's stand-in: it expands ${NAME} from a map the test can
// rotate, so the resolved bytes change without a database.
func testSecretExpander(store map[string]string) func(context.Context, *Service, string, map[string]string, map[string]string) (map[string]string, map[string]string, error) {
	return func(_ context.Context, _ *Service, _ string, env, headers map[string]string) (map[string]string, map[string]string, error) {
		expand := func(m map[string]string) map[string]string {
			out := make(map[string]string, len(m))
			for k, v := range m {
				for name, val := range store {
					v = strings.ReplaceAll(v, "${"+name+"}", val)
				}
				out[k] = v
			}
			return out
		}
		return expand(env), expand(headers), nil
	}
}

// TestAskMCPFingerprintCoversTheResolvedCredential is the fingerprint's own contract: two resolutions
// that differ ONLY in the resolved credential must not fingerprint the same, and a rename of a header
// key must move it too.
func TestAskMCPFingerprintCoversTheResolvedCredential(t *testing.T) {
	base := func(header, key string) []mcpclient.ServerSpec {
		return []mcpclient.ServerSpec{{
			ID:      "pixellab",
			Type:    mcpclient.TypeHTTP,
			Headers: map[string]string{key: header},
		}}
	}
	oldFP := askMCPFingerprint("p1", base("Bearer old-token", "Authorization"), nil)
	newFP := askMCPFingerprint("p1", base("Bearer NEW-token", "Authorization"), nil)
	if oldFP == newFP {
		t.Fatal("a rotated credential produced the SAME fingerprint — the live client would be kept and keep sending the old value (the bug)")
	}
	renamed := askMCPFingerprint("p1", base("Bearer old-token", "X-Api-Key"), nil)
	if oldFP == renamed {
		t.Fatal("renaming the header produced the same fingerprint — what is SENT changed, so the client must rebuild")
	}
	if got := askMCPFingerprint("p1", base("Bearer old-token", "Authorization"), nil); got != oldFP {
		t.Fatalf("the fingerprint is not stable for identical input: %q != %q", got, oldFP)
	}
	// The fingerprint is a HASH of the values, not the values: it must not carry the credential.
	if strings.Contains(oldFP, "old-token") {
		t.Fatalf("the fingerprint carries the plaintext credential: %q", oldFP)
	}
	// Disabling a server is part of what is sent (nothing), so it moves the fingerprint too.
	if withOff := askMCPFingerprint("p1", base("Bearer old-token", "Authorization"), []string{"pixellab"}); withOff == oldFP {
		t.Fatal("disabling a server did not change the fingerprint")
	}
}

// TestAskMCPRefreshRebuildsOnSecretRotation is the operator's stated requirement — "saving of the token
// in secrets to auto update" — proved on a LIVE entry with no restart.
func TestAskMCPRefreshRebuildsOnSecretRotation(t *testing.T) {
	store := map[string]string{"PIXELLAB_API_KEY": "Bearer first-token"}
	res := &rotationResolver{header: "Bearer ${PIXELLAB_API_KEY}"}

	svc := New(nil, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(res)
	// The seam that would otherwise hit the tenant secrets store; here it reads the rotatable map.
	oldResolver := askMCPExpandRefs
	askMCPExpandRefs = testSecretExpander(store)
	t.Cleanup(func() { askMCPExpandRefs = oldResolver })
	started := installFakeClient(t)

	ctx := tenant.WithID(context.Background(), "tnt_test")
	ctx = withAskConversation(ctx, "c1")
	ctx = withAskConversationProject(ctx, "p1")

	// FIRST TURN: a live client is started for the server, with the FIRST credential.
	entry1, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (first): %v", err)
	}
	if entry1 == nil || entry1.mgr == nil {
		t.Fatalf("the first turn did not start an MCP client: %+v", entry1)
	}
	fp1 := entry1.fingerprint
	if !strings.Contains(fp1, "pixellab") {
		t.Fatalf("the fingerprint does not name the server: %q", fp1)
	}

	// NOTHING CHANGED: the refresh must keep the SAME live client (no respawn per turn — the whole
	// reason the fingerprint exists).
	svc.refreshAskMCP(ctx)
	same, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (unchanged): %v", err)
	}
	if same != entry1 {
		t.Fatal("an unchanged configuration rebuilt the client — the fingerprint is over-sensitive and every turn would respawn stdio children")
	}

	// THE ROTATION: the secret's value changes while the plane is running. The header string in the
	// definition is untouched — this is a secrets-store write, not a config edit.
	store["PIXELLAB_API_KEY"] = "Bearer SECOND-token"
	svc.refreshAskMCP(ctx)
	entry2, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (after rotation): %v", err)
	}
	if entry2 == entry1 {
		t.Fatal("a rotated secret did NOT rebuild the client — every tool call keeps sending the old credential " +
			"until the plane restarts (the operator's exact bug)")
	}
	if entry2.fingerprint == fp1 {
		t.Fatalf("the rotated credential produced the same fingerprint: %q", entry2.fingerprint)
	}
	if len(*started) != 2 {
		t.Fatalf("expected exactly 2 client starts (initial + after rotation), got %d", len(*started))
	}
	if !(*started)[0].closed {
		t.Error("the retired client was not closed — its stdio children would leak")
	}
	if (*started)[1].closed {
		t.Error("the NEW client was already closed")
	}
}

// TestAskMCPRefreshRebuildsOnHeaderEdit is the OTHER half of the same defect, kept separate because it
// is a config write rather than a secrets-store write: both change the resolved bytes, and one hash
// covers both.
func TestAskMCPRefreshRebuildsOnHeaderEdit(t *testing.T) {
	store := map[string]string{}
	res := &rotationResolver{header: "Bearer literal-first"}
	svc := New(nil, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)), nil, nil, nil)
	svc.SetScopeResolver(res)
	oldResolver := askMCPExpandRefs
	askMCPExpandRefs = testSecretExpander(store)
	t.Cleanup(func() { askMCPExpandRefs = oldResolver })
	installFakeClient(t)

	ctx := tenant.WithID(context.Background(), "tnt_test")
	ctx = withAskConversation(ctx, "c2")
	ctx = withAskConversationProject(ctx, "p1")

	entry1, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor: %v", err)
	}
	// The operator edits the header in the definition.
	res.header = "Bearer literal-SECOND"
	svc.refreshAskMCP(ctx)
	entry2, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (after edit): %v", err)
	}
	if entry2 == entry1 {
		t.Fatal("editing a header did not rebuild the client")
	}
}

// TestAskMCPRefreshKeepsClientWhenASecretCannotBeExpanded pins the FAILURE path, which is the one that
// must not make things worse: once the client is live, a config that is broken RIGHT NOW (a deleted
// secret) keeps the working client rather than tearing the surface down on the next turn — and says so
// in the log, because a silently-kept stale client is the same class of bug as the one this file is
// about.
func TestAskMCPRefreshKeepsClientWhenASecretCannotBeExpanded(t *testing.T) {
	var logs strings.Builder
	store := map[string]string{"PIXELLAB_API_KEY": "Bearer first-token"}
	res := &rotationResolver{header: "Bearer ${PIXELLAB_API_KEY}"}
	svc := New(nil, slog.New(slog.NewTextHandler(&logs, nil)), nil, nil, nil)
	svc.SetScopeResolver(res)
	oldResolver := askMCPExpandRefs
	askMCPExpandRefs = testSecretExpander(store)
	t.Cleanup(func() { askMCPExpandRefs = oldResolver })
	installFakeClient(t)

	ctx := tenant.WithID(context.Background(), "tnt_test")
	ctx = withAskConversation(ctx, "c3")
	ctx = withAskConversationProject(ctx, "p1")

	// A live client, working.
	entry1, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (first): %v", err)
	}

	// The secret is DELETED: expansion now refuses, the way ResolveSecretRefs does for an unstored name.
	askMCPExpandRefs = func(context.Context, *Service, string, map[string]string, map[string]string) (map[string]string, map[string]string, error) {
		return nil, nil, errTestNoSecret
	}
	svc.refreshAskMCP(ctx)

	kept, err := svc.askMCPFor(ctx)
	if err != nil {
		t.Fatalf("askMCPFor (after the secret was deleted) must not fail the turn: %v", err)
	}
	if kept != entry1 {
		t.Fatal("a broken config tore down the working client — the conversation loses its whole MCP surface " +
			"for a secret that may be re-added a minute later")
	}
	if !strings.Contains(logs.String(), "could not expand") {
		t.Errorf("the refresh failure was silent; log was:\n%s", logs.String())
	}
}
