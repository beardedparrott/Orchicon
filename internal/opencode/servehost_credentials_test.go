package opencode

// servehost_credentials_test.go — the host serve must be GIVEN the operator's provider credentials.
//
// THE REPORTED FAILURE: every Ask turn through the opencode adapter failed with
//
//	This API key has expired. Create a new API key in OpenCode Console and replace it in your client or
//	secret store. Signing in again will not renew this key.
//
// while the operator's own `opencode` worked. The serve was started with a COPY of the operator's auth.json
// (seedAuth) — a file opencode 2.x last wrote in a 1.x-era install and never updates, because 2.x keeps its
// real credentials in its SQLite store. Re-seeding cannot fix it: the store imports auth.json only ONCE, at
// migration. So the copied key had been expired for weeks.
//
// The container path already resolved credentials from BOTH stores (database winning) and delivered them by
// environment; the HOST serve was never given the same treatment, so the two shapes disagreed about which
// credential a serve runs with.

import (
	"os"
	"strings"
	"testing"
)

// TestTheHostServeIsGivenProviderCredentials is the guard for the incident.
func TestTheHostServeIsGivenProviderCredentials(t *testing.T) {
	orig := credentialResolver
	t.Cleanup(func() { credentialResolver = orig })

	var askedHome string
	credentialResolver = func(home string, kinds ...string) ([]string, []string) {
		askedHome = home
		return []string{"OPENCODE_API_KEY=fresh-key", "OPENROUTER_API_KEY=other"}, []string{"ollama-cloud"}
	}

	h := NewHostServe(nil, "/tmp/serve-data", "")
	pairs, unusable := h.credentialEnv()

	if len(pairs) != 2 || pairs[0] != "OPENCODE_API_KEY=fresh-key" {
		t.Fatalf("credentialEnv = %v, want the resolved KEY=VALUE pairs — a serve started without them "+
			"authenticates from the stale auth.json copy, which is the reported expired-key failure", pairs)
	}
	if len(unusable) != 1 || unusable[0] != "ollama-cloud" {
		t.Errorf("unusable = %v, want the undeliverable ids so the gap is reportable", unusable)
	}
	// IT RESOLVES FROM THE OPERATOR'S REAL DATA HOME, not the serve's private one — the private dir holds the
	// auth.json COPY that is the whole problem.
	if !strings.Contains(askedHome, ".local/share/opencode") || strings.Contains(askedHome, "serve-data") {
		t.Errorf("credentials resolved from %q; they must come from the operator's real opencode data home "+
			"(the source of the SQLite store), not from this serve's private copy", askedHome)
	}
}

// TestNoDataDirMeansNoCredentials: an unconfigured serve resolves nothing rather than inventing a path.
func TestNoDataDirMeansNoCredentials(t *testing.T) {
	orig := credentialResolver
	t.Cleanup(func() { credentialResolver = orig })
	called := false
	credentialResolver = func(string, ...string) ([]string, []string) { called = true; return nil, nil }

	h := NewHostServe(nil, "", "")
	if pairs, _ := h.credentialEnv(); pairs != nil {
		t.Errorf("a serve with no data dir produced credentials: %v", pairs)
	}
	if called {
		t.Error("the resolver ran for a serve with no data dir")
	}
}

// TestTheEnvironmentReachesTheSpawn pins the wiring at the source, because the alternative is spawning a real
// opencode binary — which CI does not have, so a behavioural test would be skipped and guard nothing.
func TestTheEnvironmentReachesTheSpawn(t *testing.T) {
	src := readServehostSource(t)
	i := strings.Index(src, "func (h *HostServe) startOnce(")
	if i < 0 {
		t.Fatal("startOnce is gone")
	}
	body := src[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "h.credentialEnv()") {
		t.Error("startOnce does not call credentialEnv, so the serve is spawned without the resolved " +
			"credentials and authenticates from the stale auth.json copy — the reported expired-key bug")
	}
	if !strings.Contains(body, "env = append(env, pairs...)") {
		t.Error("the resolved pairs are not appended to the serve's environment")
	}
	// THE ORDER MATTERS: this must run on the same env the child is given, before the spawn.
	app := strings.Index(body, "env = append(env, pairs...)")
	spawn := strings.Index(body, "procwin.CommandContext(ctx, binary")
	if app < 0 || spawn < 0 || app > spawn {
		t.Error("the credentials are not applied before the serve is spawned")
	}
	// AND THE SAME SOURCE as the served data — the operator's real home.
	if !strings.Contains(src, "return credentialResolver(h.authSource(), adapter.DefaultAdapterKind)") {
		t.Error("credentialEnv no longer resolves from authSource() with the default adapter kind")
	}
}

// readServehostSource reads this package's servehost.go for the wiring assertions.
func readServehostSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("servehost.go")
	if err != nil {
		t.Fatalf("read servehost.go: %v", err)
	}
	return string(b)
}
