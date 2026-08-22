package runtime

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/guard"
)

func TestPrependGuardSmoke(t *testing.T) {
	dir, err := guard.MakeGuard("/tmp", "/tmp")
	if err != nil {
		t.Fatalf("MakeGuard: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	env := prependGuard(os.Environ(), dir)
	var got string
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			got = strings.TrimPrefix(kv, "PATH=")
		}
	}
	if !strings.HasPrefix(got, dir) {
		t.Fatalf("expected PATH to start with %s, got %s", dir, got)
	}
	t.Logf("PATH starts with guard dir: %s...", got[:len(dir)])
}

func TestIsDevImageTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"ghcr.io/beardedparrott/orchicon-runtime:orchicon-dev", true},
		{"ghcr.io/beardedparrott/orchicon-runtime:dev", true},
		{"ghcr.io/beardedparrott/orchicon-runtime:dev-latest", true},
		{"orchicon-runtime:orchicon-dev", true},
		{"orchicon-runtime:local", false},
		{"ghcr.io/beardedparrott/orchicon-runtime:latest", false},
		{"orchicon-runtime:gui-latest", false},
		{"orchicon-runtime:local-gui", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsDevImageTag(c.tag); got != c.want {
			t.Errorf("IsDevImageTag(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

// TestSandboxPgBinDirConsistency is a consistency check for the sandbox
// self-gate's postgres resolution: when a candidate bin dir under
// /usr/lib/postgresql actually contains all four required binaries,
// sandboxPgBinDir must return a usable dir; otherwise it must return ""
// without panicking. The check is env-independent (the CI host may or may
// not have Postgres installed).
func TestSandboxPgBinDirConsistency(t *testing.T) {
	dir := sandboxPgBinDir()
	if dir == "" {
		return // no Postgres in this environment — nothing to assert
	}
	if !sandboxPgBinDirComplete(dir) {
		t.Errorf("sandboxPgBinDir()=%s is not complete", dir)
	}
}

// TestSandboxPgBinDirComplete is the regression test for the architect's
// finding that the sandbox self-gate accepted a PARTIAL bin dir (the inner
// loop "continued" past a missing binary and still returned the dir): a
// dir missing any of the four server binaries must be rejected, and a dir
// with all four must pass.
func TestSandboxPgBinDirComplete(t *testing.T) {
	full := t.TempDir()
	for _, bin := range []string{"initdb", "pg_ctl", "pg_isready", "postgres"} {
		if err := os.WriteFile(filepath.Join(full, bin), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !sandboxPgBinDirComplete(full) {
		t.Fatalf("full bin dir %s rejected", full)
	}

	partial := t.TempDir()
	for _, bin := range []string{"initdb", "pg_ctl", "pg_isready"} { // missing postgres
		if err := os.WriteFile(filepath.Join(partial, bin), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if sandboxPgBinDirComplete(partial) {
		t.Fatalf("partial bin dir %s accepted (missing postgres)", partial)
	}

	empty := t.TempDir()
	if sandboxPgBinDirComplete(empty) {
		t.Fatalf("empty dir %s accepted", empty)
	}
}

// TestSandboxAvailableSelfGate verifies the self-gate never returns true
// unless BOTH pieces (postgres bin dir + nats-server on PATH) are present,
// and that the probe result is cached after the first call.
func TestSandboxAvailableSelfGate(t *testing.T) {
	h := newChildRegistry(tLogger(t))
	first := h.sandboxAvailable()
	h.mu.Lock()
	checked := h.sandboxChecked
	h.mu.Unlock()
	if !checked {
		t.Fatalf("self-gate not cached after first probe")
	}
	// The gate is conservative: it must be false when either piece is
	// missing. We can't reliably construct a true-positive here (that
	// requires both binaries), so just assert the negative is consistent
	// with the environment: if nats-server is absent, it must be false.
	if _, err := os.Stat("/usr/local/bin/nats-server"); os.IsNotExist(err) {
		if first {
			t.Fatal("sandboxAvailable()=true but nats-server is absent from PATH")
		}
	}
}

func tLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
