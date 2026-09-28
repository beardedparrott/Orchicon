package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Two host-resident planes (dev + prod) must never share a PID file: a
// `serve --stop` for one instance would kill the other. ORCHICON_SERVE_STATE_DIR
// makes the state root per instance; its absence must reproduce the previous
// paths exactly.
func TestServeStateDir(t *testing.T) {
	t.Run("default is .dev", func(t *testing.T) {
		t.Setenv(serveStateDirEnv, "sentinel")
		if err := os.Unsetenv(serveStateDirEnv); err != nil {
			t.Fatal(err)
		}
		if got := serveStateDir(); got != ".dev" {
			t.Errorf("got %q, want .dev", got)
		}
	})
	t.Run("empty means default", func(t *testing.T) {
		t.Setenv(serveStateDirEnv, "")
		if got := serveStateDir(); got != ".dev" {
			t.Errorf("got %q, want .dev", got)
		}
	})
	t.Run("override wins", func(t *testing.T) {
		t.Setenv(serveStateDirEnv, "/home/op/.local/share/orchicon-dev/serve")
		if got := serveStateDir(); got != "/home/op/.local/share/orchicon-dev/serve" {
			t.Errorf("got %q, want the override verbatim", got)
		}
	})
}

// The package vars are initialised before any test can set the env, so assert
// their SHAPE (the env-derived value is covered by TestServeStateDir above).
func TestServeStatePathsShape(t *testing.T) {
	for name, got := range map[string]string{"pid": servePIDFile, "log": serveLogFile} {
		if got == "" || filepath.Dir(got) == "" {
			t.Fatalf("%s path looks unset: %q", name, got)
		}
	}
	if want := filepath.Join(".dev", "pids", "orchicon.pid"); servePIDFile != want {
		t.Errorf("servePIDFile = %q, want %q (the pre-change value)", servePIDFile, want)
	}
	if want := filepath.Join(".dev", "logs", "orchicon.log"); serveLogFile != want {
		t.Errorf("serveLogFile = %q, want %q (the pre-change value)", serveLogFile, want)
	}
}
