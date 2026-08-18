package runtime

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var hexDigestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHostInputsFingerprintStabilityAndInvalidation drives the pure
// fingerprint function through every read-once host input: each component
// change (config content, auth content, adapter binary mtime, provider
// package add) must flip the fingerprint, while a change to a NOT-stale
// input (~/.gitconfig — git reads it per invocation) must NOT.
func TestHostInputsFingerprintStabilityAndInvalidation(t *testing.T) {
	home := t.TempDir()
	cfgJSON := filepath.Join(home, ".config", "opencode", "opencode.json")
	writeTestFile(t, cfgJSON, `{"provider":{"baseURL":"https://one.example"}}`)

	fp := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))
	if fp == "" || !hexDigestRe.MatchString(fp) {
		t.Fatalf("expected a hex digest fingerprint, got %q", fp)
	}

	// Stability: identical state → identical fingerprint.
	if again := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); again != fp {
		t.Fatalf("identical host inputs must produce the same fingerprint")
	}

	// 1. opencode config content change → different.
	writeTestFile(t, cfgJSON, `{"provider":{"baseURL":"https://two.example"}}`)
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("opencode.json content change must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))

	// A second config file (.jsonc) appearing → different.
	writeTestFile(t, filepath.Join(home, ".config", "opencode", "opencode.jsonc"), `{"models":{}}`)
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("a new opencode.jsonc must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))

	// 2. auth.json appearing / content change → different.
	auth := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	writeTestFile(t, auth, `{"providers":{"openai":{"key":"k1"}}}`)
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("auth.json appearing must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))
	writeTestFile(t, auth, `{"providers":{"openai":{"key":"rotated-key"}}}`)
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("auth.json content change (key rotation) must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))

	// 3. adapter install appearing → different.
	writeTestFile(t, filepath.Join(home, ".opencode", "bin", "opencode"), "\x7fELF fake binary")
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("adapter binary appearing must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))

	// Adapter binary replacement (mtime change) → different (stat fingerprint).
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(home, ".opencode", "bin", "opencode"), old, old); err != nil {
		t.Fatal(err)
	}
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("adapter binary mtime change must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))

	// Provider npm package added under node_modules → different.
	pkgFile := filepath.Join(home, ".opencode", "node_modules", "@ai-sdk", "openai-compatible", "dist", "index.js")
	writeTestFile(t, pkgFile, "export {}")
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got == fp {
		t.Fatalf("a provider package add under node_modules must change the fingerprint")
	}
	fp = hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789"))

	// 4. GH token rotation (different prefix) → different.
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_ZZZZZZZZ123456789")); got == fp {
		t.Fatalf("GH token rotation must change the fingerprint")
	}

	// NOT-stale input: ~/.gitconfig (read per invocation, not fingerprinted).
	writeTestFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\temail = new@example.com\n")
	if got := hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")); got != fp {
		t.Fatalf(".gitconfig change must NOT change the fingerprint (it is read per run)")
	}
}

// TestHostInputsFingerprintEmptyAndExclusions covers the degenerate cases:
// an empty home, or a home holding only NOT-stale inputs, yields "" (the pool
// key reduces to image+mounts), and the return value is always a hex digest.
func TestHostInputsFingerprintEmptyAndExclusions(t *testing.T) {
	home := t.TempDir()

	// No token and nothing fingerprinted → "". ghFp == "" mirrors the daemon
	// resolving no GH token.
	if got := hostInputsFingerprint("", ""); got != "" {
		t.Fatalf("empty home must yield an empty fingerprint, got %q", got)
	}
	if got := hostInputsFingerprint(home, ""); got != "" {
		t.Fatalf("empty home must yield an empty fingerprint, got %q", got)
	}

	// Only NOT-stale inputs present → nothing fingerprinted → "".
	writeTestFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\temail = a@b.c\n")
	writeTestFile(t, filepath.Join(home, ".config", "gh", "hosts.yml"), "github.com:\n")
	writeTestFile(t, filepath.Join(home, ".git-credentials"), "https://x@github.com\n")
	if got := hostInputsFingerprint(home, ""); got != "" {
		t.Fatalf("only NOT-stale inputs present must yield an empty fingerprint, got %q", got)
	}
}

// TestGhTokenFingerprint verifies the non-sensitive token digest: only the
// length + first-12-char prefix feed it, so rotation (prefix/length change)
// is detected while the full token never appears in the output.
func TestGhTokenFingerprint(t *testing.T) {
	tok := "ghp_abcdefghij123456789"
	fp := ghTokenFingerprint(tok)
	if !hexDigestRe.MatchString(fp) {
		t.Fatalf("expected a 64-char hex digest, got %q", fp)
	}
	if strings.Contains(fp, "ghp_") {
		t.Fatalf("token material leaked into the fingerprint: %q", fp)
	}
	// The output is a hash — it must not round-trip any token prefix either.
	decoded, _ := hex.DecodeString(fp)
	if len(decoded) != 32 {
		t.Fatalf("fingerprint must be 32 raw bytes (sha256), got %d", len(decoded))
	}

	if ghTokenFingerprint("") != "" {
		t.Fatalf("empty token must yield an empty fingerprint")
	}
	if ghTokenFingerprint(tok) != ghTokenFingerprint(tok) {
		t.Fatalf("same token must yield the same fingerprint")
	}
	// Different prefix (rotation) → different.
	if ghTokenFingerprint(tok) == ghTokenFingerprint("ghp_ZZZZZZZZZZ123456789") {
		t.Fatalf("token prefix change must change the fingerprint")
	}
	// Different length → different.
	if ghTokenFingerprint(tok) == ghTokenFingerprint(tok+"extra") {
		t.Fatalf("token length change must change the fingerprint")
	}
}

// TestGhTokenTTLCache verifies the TTL-cached resolver: repeated resolution
// within the TTL calls the resolver once (no `gh auth token` subprocess per
// checkout), and cache expiry re-resolves — so a rotation is picked up within
// the bounded staleness window.
func TestGhTokenTTLCache(t *testing.T) {
	val := "ghp_abcdefghij123456789"
	calls := 0
	d := &Daemon{ghTokenFn: func() string { calls++; return val }}

	if got := d.ghToken(); got != val {
		t.Fatalf("ghToken = %q, want %q", got, val)
	}
	if got := d.ghToken(); got != val {
		t.Fatalf("ghToken = %q, want %q", got, val)
	}
	if calls != 1 {
		t.Fatalf("TTL cache miss: resolver called %d times within the TTL, want 1", calls)
	}

	// Simulate expiry → the resolver must be consulted again (a rotation is
	// picked up).
	d.ghTokenTime = time.Time{}
	if got := d.ghToken(); got != val {
		t.Fatalf("ghToken after expiry = %q, want %q", got, val)
	}
	if calls != 2 {
		t.Fatalf("resolver must be re-called after cache expiry, calls=%d", calls)
	}

	// A rotated token is visible after expiry.
	val = "ghp_NEWPREFIX123456789"
	d.ghTokenTime = time.Time{}
	if got := d.ghToken(); got != val {
		t.Fatalf("rotated token not visible after expiry: %q, want %q", got, val)
	}
}

// --- Behavioral checkout test ---

// poolCleanNamesForKey snapshots the clean pool's container names for one env key.
func poolCleanNamesForKey(p *daemonPool, key string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.clean[key]...)
}

func poolCleanNames(p *daemonPool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var names []string
	for _, ns := range p.clean {
		names = append(names, ns...)
	}
	return names
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// waitForPoolKey blocks until the clean list for env key has >= want entries.
// Key-scoped: the pool can simultaneously hold containers under the OLD key
// (a stale warm container awaiting idle-reap), so count-based waits are
// ambiguous.
func waitForPoolKey(t *testing.T, p *daemonPool, key string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(poolCleanNamesForKey(p, key)) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (clean[%s]=%v)", what, key, poolCleanNamesForKey(p, key))
}

// TestPoolCheckoutReuseAndInvalidation is the behavioral acceptance test: with
// unchanged read-once host inputs the pool reuses a warm container across
// runs (no fresh create — no perf regression); changing a read-once host
// input forces the next checkout to create a FRESH container, and the stale
// old-key clean entry is never handed out. Docker and createContainer are
// stubbed; a live httptest server stands in for the serve (serveUsableAt
// performs a real /global/health + /session round-trip).
func TestPoolCheckoutReuseAndInvalidation(t *testing.T) {
	home := t.TempDir()
	cfgJSON := filepath.Join(home, ".config", "opencode", "opencode.json")
	writeTestFile(t, cfgJSON, `{"provider":{"baseURL":"https://one.example"}}`)

	// Live stand-in for the container's opencode serve: checkout's
	// reuse-path liveness check (serveUsableAt) needs /global/health and
	// /session to answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health", "/session":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var checkoutCreates, resetCreates int
	d := &Daemon{
		HostHome:  home,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ghTokenFn: func() string { return "ghp_abcdefgh123456789" },
		dockerFn: func(args ...string) (string, error) {
			if len(args) >= 3 && args[0] == "inspect" && args[1] == "--format" && args[2] == "{{.State.Running}}" {
				return "true", nil
			}
			return "", nil
		},
		createFn: func(name string, req CreateRequest) (*CreateResponse, error) {
			if req.WorkflowID != "" {
				checkoutCreates++
			} else {
				resetCreates++
			}
			return &CreateResponse{Name: name, Running: true, ServePort: 1, ServePassword: "pw", ServeURL: srv.URL}, nil
		},
	}
	d.pool = newDaemonPool(d)
	req := func(runID string) CreateRequest { return CreateRequest{WorkflowID: runID, Image: "img:v1"} }
	hostFp := func() string { return hostInputsFingerprint(home, ghTokenFingerprint("ghp_abcdefgh123456789")) }
	key1 := poolEnvKey(req("run-1"), hostFp())

	// First checkout: pool empty → fresh create.
	c1, err := d.pool.checkout(context.Background(), "run-1", req("run-1"))
	if err != nil {
		t.Fatalf("checkout run-1: %v", err)
	}
	if checkoutCreates != 1 {
		t.Fatalf("first checkout must create fresh; checkoutCreates=%d", checkoutCreates)
	}
	if c1.Name == "" {
		t.Fatalf("fresh checkout must return a container name")
	}
	d.pool.release("run-1")
	waitForPoolKey(t, d.pool, key1, 1, "run-1 reset to pool")
	firstReset := poolCleanNamesForKey(d.pool, key1)[0]

	// Unchanged host inputs → the next checkout REUSES the warm container.
	c2, err := d.pool.checkout(context.Background(), "run-2", req("run-2"))
	if err != nil {
		t.Fatalf("checkout run-2: %v", err)
	}
	if c2.Name != firstReset {
		t.Fatalf("expected warm reuse of %s, got %s", firstReset, c2.Name)
	}
	if checkoutCreates != 1 {
		t.Fatalf("warm reuse must NOT create a fresh container; checkoutCreates=%d", checkoutCreates)
	}
	d.pool.release("run-2")
	waitForPoolKey(t, d.pool, key1, 1, "run-2 reset to pool")
	staleClean := poolCleanNamesForKey(d.pool, key1)[0] // still under the OLD key

	// Change a read-once host input → next checkout must create FRESH and
	// must NOT hand out the old-key clean container.
	writeTestFile(t, cfgJSON, `{"provider":{"baseURL":"https://two.example"}}`)
	key2 := poolEnvKey(req("run-1"), hostFp())
	if key2 == key1 {
		t.Fatalf("a host-input change must change the pool env key")
	}
	c3, err := d.pool.checkout(context.Background(), "run-3", req("run-3"))
	if err != nil {
		t.Fatalf("checkout run-3: %v", err)
	}
	if checkoutCreates != 2 {
		t.Fatalf("a host-input change must force a fresh create; checkoutCreates=%d", checkoutCreates)
	}
	if c3.Name == staleClean {
		t.Fatalf("the stale old-key container %s must never be handed out", staleClean)
	}
	if containsName(poolCleanNames(d.pool), c3.Name) {
		t.Fatalf("fresh container %s must be leased, not left clean", c3.Name)
	}
	// The old-key clean entry survives (idle-reap reaps it later) but is
	// unreachable under the new key.
	if !containsName(poolCleanNamesForKey(d.pool, key1), staleClean) {
		t.Fatalf("expected the old-key clean container %s to remain pooled (unreachable), got %v", staleClean, poolCleanNamesForKey(d.pool, key1))
	}

	// The new state is stable: release + re-checkout under the changed config
	// REUSES (no thrash).
	d.pool.release("run-3")
	waitForPoolKey(t, d.pool, key2, 1, "run-3 reset to pool under new key")
	secondReset := poolCleanNamesForKey(d.pool, key2)[0]
	c4, err := d.pool.checkout(context.Background(), "run-4", req("run-4"))
	if err != nil {
		t.Fatalf("checkout run-4: %v", err)
	}
	if c4.Name != secondReset {
		t.Fatalf("expected warm reuse of %s under the changed config, got %s", secondReset, c4.Name)
	}
	if checkoutCreates != 2 {
		t.Fatalf("changed-config reuse must not create fresh; checkoutCreates=%d", checkoutCreates)
	}

	if resetCreates != 3 {
		t.Fatalf("expected 3 background resets (run-1, run-2, run-3), got %d", resetCreates)
	}
}
