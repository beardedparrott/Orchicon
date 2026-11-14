package claude

// mcp_rotation_test.go — claude ask: a SAVED SECRET must reach the next turn.
//
// The child's --mcp-config is fixed at fork/exec, so a credential can only be applied by RESPawning. The
// staleness check is what decides to respawn — and it was comparing `mayact=true;<server ids>`, which
// cannot see a rotated secret for two reasons: the ids do not move, and the raw resolution it read still
// holds ${SECRET_NAME} REFERENCES because they are expanded during renderMCP.
//
// These tests drive the two halves: the fingerprint's own contract, and the stale decision on a LIVE
// session whose rendered credential changes.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// rotationScope is a resolver whose one server's header is whatever the test holds — the stand-in for
// "the operator saved a new secret value".
type rotationScope struct {
	header string
}

func (r *rotationScope) ResolveScope(_ context.Context, _ mcpclient.ScopeRef) (mcpclient.Resolution, error) {
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

// TestMCPFingerprintCoversTheRenderedCredential is the fingerprint's contract: it must move when the
// credential does, and it must not carry the credential in plaintext.
func TestMCPFingerprintCoversTheRenderedCredential(t *testing.T) {
	server := func(header, key string) []MCPServer {
		return []MCPServer{{Name: "pixellab", URL: "https://api.pixellab.ai/mcp", Headers: map[string]string{key: header}}}
	}
	oldFP := mcpFingerprint(true, server("Bearer old-token", "Authorization"))
	newFP := mcpFingerprint(true, server("Bearer NEW-token", "Authorization"))
	if oldFP == newFP {
		t.Fatal("a rotated credential produced the SAME fingerprint — the child would never be respawned and would keep sending the old value")
	}
	if renamed := mcpFingerprint(true, server("Bearer old-token", "X-Api-Key")); renamed == oldFP {
		t.Fatal("renaming the header did not move the fingerprint; what the child SENDS changed")
	}
	if again := mcpFingerprint(true, server("Bearer old-token", "Authorization")); again != oldFP {
		t.Fatalf("the fingerprint is not stable for identical input: %q != %q", again, oldFP)
	}
	if strings.Contains(oldFP, "old-token") {
		t.Fatalf("the fingerprint carries the plaintext credential: %q", oldFP)
	}
	// The mode's may-act flag decides whether the operator's servers are offered at all, so it is part of
	// what the child was launched with.
	if withheld := mcpFingerprint(false, server("Bearer old-token", "Authorization")); withheld == oldFP {
		t.Fatal("the may-act flag is not in the fingerprint, so a mode change would not respawn")
	}
}

// TestAskSessionGoesStaleOnASecretRotation is the operator's requirement on the claude shape: the session
// is LIVE and its definition is untouched — only the SECRET's value changes — and the staleness check must
// fire so the next turn respawns with the new credential.
func TestAskSessionGoesStaleOnASecretRotation(t *testing.T) {
	store := map[string]string{"PIXELLAB_API_KEY": "Bearer first-token"}
	scope := &rotationScope{header: "Bearer ${PIXELLAB_API_KEY}"}

	b := New(quietLogger())
	b.SetScopeResolver(scope)
	// The real resolver reads the tenant secrets store; here it reads the rotatable map, so a rotation is
	// one assignment.
	b.SetMCPSecretResolver(func(_ context.Context, _ string, env, headers map[string]string) (map[string]string, map[string]string, error) {
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
	})

	as := newAskSession(b, "conv-rotate", filepath.Join(t.TempDir(), "ask"))
	as.tenantID = "tnt_test"
	as.projectID = "p1"
	as.mu.Lock()
	as.alive = true // a live child: the fingerprint was recorded by argv() at spawn
	as.mu.Unlock()

	// Build the argv once so the fingerprint is recorded, exactly as a spawn would.
	_ = as.argv()
	if as.mcpFingerprint == "" {
		t.Fatal("no fingerprint was recorded")
	}
	// NOTHING CHANGED: the child is not stale, so a turn does not respawn it.
	if as.sessionMCPStale() {
		t.Fatal("an unchanged set reported the child stale — every turn would respawn it")
	}

	// THE ROTATION: the secret's value changes; the definition is untouched.
	store["PIXELLAB_API_KEY"] = "Bearer SECOND-token"
	if !as.sessionMCPStale() {
		t.Fatal("a rotated secret did NOT mark the child stale — claude ask would keep sending the old credential " +
			"until the plane restarts (the same defect fixed in the native path)")
	}
}

// TestAskSessionGoesStaleOnAHeaderEdit is the config half: editing the header string, with no secret
// involved at all.
func TestAskSessionGoesStaleOnAHeaderEdit(t *testing.T) {
	scope := &rotationScope{header: "Bearer literal-first"}
	b := New(quietLogger())
	b.SetScopeResolver(scope)
	b.SetMCPSecretResolver(nil)

	as := newAskSession(b, "conv-edit", filepath.Join(t.TempDir(), "ask"))
	as.tenantID = "tnt_test"
	as.projectID = "p1"
	as.mu.Lock()
	as.alive = true
	as.mu.Unlock()
	_ = as.argv()
	if as.sessionMCPStale() {
		t.Fatal("an unchanged set reported the child stale")
	}
	scope.header = "Bearer literal-SECOND"
	if !as.sessionMCPStale() {
		t.Fatal("editing the header did not mark the child stale")
	}
}
