package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// installFakeClaude writes a plausible CLI at $HOME/.local/bin/claude and
// returns the path.
func installFakeClaude(t *testing.T, home string) string {
	t.Helper()
	p := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// The resolution order, which exists because this host has THREE claude
// installs and a PATH that reaches the wrong one runs a CLI a year older that
// rejects our argv outright ("unknown option '--permission-prompts"), exits 1,
// and writes nothing to stdout — a permanent "thinking…" with no error.
func TestClaudeBinaryPath(t *testing.T) {
	t.Run("the explicit override wins", func(t *testing.T) {
		t.Setenv(ClaudeBinEnv, "/opt/custom/claude")
		t.Setenv("HOME", t.TempDir())
		if got := ClaudeBinaryPath(); got != "/opt/custom/claude" {
			t.Fatalf("ClaudeBinaryPath() = %q, want the override", got)
		}
	})

	t.Run("the operator's own install is preferred over a bare PATH name", func(t *testing.T) {
		t.Setenv(ClaudeBinEnv, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := installFakeClaude(t, home)
		if got := ClaudeBinaryPath(); got != want {
			t.Fatalf("ClaudeBinaryPath() = %q, want %q — the operator's install is the one they authenticated and the one whose version matches the catalog", got, want)
		}
	})

	t.Run("no install falls back to the bare name", func(t *testing.T) {
		t.Setenv(ClaudeBinEnv, "")
		t.Setenv("HOME", t.TempDir()) // empty home
		if got := ClaudeBinaryPath(); got != "claude" {
			t.Fatalf("ClaudeBinaryPath() = %q, want the bare fallback so a PATH-only machine still works", got)
		}
	})

	t.Run("a DIRECTORY at the install path is not a usable binary", func(t *testing.T) {
		t.Setenv(ClaudeBinEnv, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		// The file shape the daemon's probe also rejects.
		if err := os.MkdirAll(filepath.Join(home, ".local", "bin", "claude"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if got := ClaudeBinaryPath(); got == filepath.Join(home, ".local", "bin", "claude") {
			t.Fatalf("ClaudeBinaryPath() = %q, want the fallback — a directory cannot be spawned", got)
		}
	})
}

// THE REGRESSION PIN: the Ask argv must spawn the RESOLVED binary, never the
// bare name. The bare name is what let PATH pick the wrong install, and the
// failure it produced was silent.
func TestAskArgvUsesTheResolvedBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	want := installFakeClaude(t, home)

	s := newAskSession(New(quietLogger()), "conv-bin", filepath.Join(t.TempDir(), "ask"))
	argv := s.argv()
	if len(argv) == 0 {
		t.Fatal("empty argv")
	}
	if argv[0] == "claude" {
		t.Fatal("argv[0] is the bare name — PATH decides which install runs, and a system install rejects this argv with no stdout")
	}
	if argv[0] != want {
		t.Fatalf("argv[0] = %q, want the resolved install %q", argv[0], want)
	}
}

// The worker session must resolve it the same way: it shares the launch shape,
// so a fix applied to one transport and not the other would leave dispatched
// executions picking the wrong CLI.
func TestWorkerArgvUsesTheResolvedBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(ClaudeBinEnv, "")
	want := installFakeClaude(t, home)

	h := newHarness(t, func() *fakeProc { return newFakeProc() })
	_ = h
	s := &session{b: New(quietLogger())}
	argv := s.argv()
	if len(argv) == 0 {
		t.Fatal("empty argv")
	}
	if argv[0] != want {
		t.Fatalf("worker argv[0] = %q, want the resolved install %q", argv[0], want)
	}
}
