package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two host-resident planes (dev + prod) must never share a PID file: a `serve --stop` for one
// instance would kill the other.
func TestServeStateDir(t *testing.T) {
	// AN INSTANCE NOW OUTRANKS THE OVERRIDE, so these cases must state that no instance is in play.
	unsetInstance := func(t *testing.T) {
		t.Helper()
		t.Setenv("ORCHICON_INSTANCE", "")
		if err := os.Unsetenv("ORCHICON_INSTANCE"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("default is .dev", func(t *testing.T) {
		unsetInstance(t)
		t.Setenv(serveStateDirEnv, "sentinel")
		if err := os.Unsetenv(serveStateDirEnv); err != nil {
			t.Fatal(err)
		}
		if got := serveStateDir(); got != ".dev" {
			t.Errorf("got %q, want .dev", got)
		}
	})
	t.Run("empty means default", func(t *testing.T) {
		unsetInstance(t)
		t.Setenv(serveStateDirEnv, "")
		if got := serveStateDir(); got != ".dev" {
			t.Errorf("got %q, want .dev", got)
		}
	})
	t.Run("an instance outranks an ambient override", func(t *testing.T) {
		// THE COLLISION THIS PREVENTS: a shell carrying prod's ORCHICON_SERVE_STATE_DIR started a dev
		// plane, which then wrote its log into prod's log file. The instance decides.
		t.Setenv("ORCHICON_INSTANCE", "dev")
		t.Setenv(serveStateDirEnv, "/home/op/.local/share/orchicon-prod/serve")
		got := serveStateDir()
		if strings.Contains(got, "orchicon-prod") {
			t.Errorf("serveStateDir() = %q — an ambient override for ANOTHER instance still decides where "+
				"this instance's plane writes its log", got)
		}
		if !strings.Contains(got, "orchicon-dev") {
			t.Errorf("serveStateDir() = %q, want the dev instance's own state dir", got)
		}
		// AND THE FILES, NOT JUST THE DIRECTORY — this is the Windows-detach regression guard, and the bug
		// it pins was about FILES.
		for name, path := range map[string]string{"pid": servePIDFile(), "log": serveLogFile()} {
			if strings.Contains(path, "orchicon-prod") {
				t.Errorf("serve %s file = %q — it follows the ambient override for ANOTHER instance", name, path)
			}
			if !strings.HasPrefix(path, got) {
				t.Errorf("serve %s file = %q, which is not under this instance's state dir %q", name, path, got)
			}
		}
	})
	t.Run("override wins", func(t *testing.T) {
		unsetInstance(t)
		t.Setenv(serveStateDirEnv, "/home/op/.local/share/orchicon-dev/serve")
		if got := serveStateDir(); got != "/home/op/.local/share/orchicon-dev/serve" {
			t.Errorf("got %q, want the override verbatim", got)
		}
	})
}

// The package vars are initialised before any test can set the env, so assert
// their SHAPE (the env-derived value is covered by TestServeStateDir above).
func TestServeStatePathsShape(t *testing.T) {
	// READ AT CALL TIME — these are functions precisely so a test can isolate the environment before
	// reading them (see servePath's comment in serve_state.go).
	t.Setenv("ORCHICON_INSTANCE", "")
	if err := os.Unsetenv("ORCHICON_INSTANCE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(serveStateDirEnv, "")
	if err := os.Unsetenv(serveStateDirEnv); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"pid": servePIDFile(), "log": serveLogFile()} {
		if got == "" || filepath.Dir(got) == "" {
			t.Fatalf("%s path looks unset: %q", name, got)
		}
	}
	if want := filepath.Join(".dev", "pids", "orchicon.pid"); servePIDFile() != want {
		t.Errorf("servePIDFile() = %q, want %q (the pre-change value)", servePIDFile(), want)
	}
	if want := filepath.Join(".dev", "logs", "orchicon.log"); serveLogFile() != want {
		t.Errorf("serveLogFile() = %q, want %q (the pre-change value)", serveLogFile(), want)
	}
}

// TestASandboxDataDirNeverWritesIntoAnotherInstance is the leak that produced an eight-hour orphan.
//
// A worker's sandbox plane pinned its own data dir and inherited ORCHICON_INSTANCE=prod. The instance
// name won, so the sandbox wrote its log into the operator's PRODUCTION log file and stayed there for
// eight hours, reparented to systemd, appending `127.0.0.1:55432 connection refused` into the evidence
// an operator reads when something breaks.
func TestASandboxDataDirNeverWritesIntoAnotherInstance(t *testing.T) {
	t.Setenv("ORCHICON_INSTANCE", "prod")
	t.Setenv("ORCHICON_DATA_DIR", "/tmp/orchicon/qa/data")
	t.Setenv(serveStateDirEnv, "")

	got := serveStateDir()
	if strings.Contains(got, "orchicon-prod") {
		t.Fatalf("serveStateDir() = %q — a sandbox with its OWN data dir is writing into the prod "+
			"instance's state. This is the leak that put a foreign plane's failures in the operator's "+
			"production log for eight hours.", got)
	}
	if !strings.HasPrefix(got, "/tmp/orchicon/qa/data") {
		t.Fatalf("serveStateDir() = %q, want the sandbox's own data dir", got)
	}
	for name, path := range map[string]string{"pid": servePIDFile(), "log": serveLogFile()} {
		if strings.Contains(path, "orchicon-prod") {
			t.Errorf("serve %s file = %q — the sandbox is writing the operator's prod state", name, path)
		}
	}
}

// AND A REAL PLANE STILL OBEYS ITS INSTANCE, even with a data dir set to that same instance's root.
// The narrow rule must not weaken the original collision guard.
func TestARealPlaneStillFollowsItsInstanceName(t *testing.T) {
	t.Setenv("ORCHICON_INSTANCE", "dev")
	t.Setenv("ORCHICON_DATA_DIR", hostDataDirFor("dev"))
	t.Setenv(serveStateDirEnv, "/home/op/.local/share/orchicon-prod/serve")

	got := serveStateDir()
	if strings.Contains(got, "orchicon-prod") {
		t.Errorf("serveStateDir() = %q — an ambient override for ANOTHER instance still decides where this "+
			"instance writes", got)
	}
	if !strings.Contains(got, "orchicon-dev") {
		t.Errorf("serveStateDir() = %q, want the dev instance's own state dir", got)
	}
}
