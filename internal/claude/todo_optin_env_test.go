package claude

// todo_optin_env_test.go — the ENV half of the todo/task opt-in (AC1).
//
// The launch-shape half (the PreToolUse matcher + permissions.allow) is pinned
// by TestTodoTrackToolsAreOptedIn. This file pins the half that decides whether
// the tools exist at ALL: claude 2.1.261 REMOVES the todo/task family from the
// tool registry on Sonnet 5 / Opus 4.8 / Fable 5 / Mythos 5 and newer unless
// CLAUDE_CODE_ENABLE_TODO_TOOLS is set (verified in the installed binary's own
// release notes and its `todoToolsEnabled()` gate — see TodoToolsEnv). A
// permission allow cannot conjure a tool the CLI never offers, so omitting this
// env var makes the whole parity feature silently inert on the models orchicon
// runs claude workers with.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestChildEnvOptsIntoTheTodoToolFamily pins the env opt-in on BOTH transports:
// the local host subprocess and the in-container child (the runtime daemon
// forwards spec.Env into the container, so the same env must carry it).
func TestChildEnvOptsIntoTheTodoToolFamily(t *testing.T) {
	local, localCleanup := childEnvFor(t, scheduler.ExecutionManifest{ProjectDir: t.TempDir()})
	defer localCleanup()
	if got := envValue(local, TodoToolsEnv); got != "1" {
		t.Fatalf("local child %s = %q, want \"1\" — without it claude offers no todo/task tool and the parity feature is inert", TodoToolsEnv, got)
	}

	container := newSession(&Bridge{rt: &runtime.Client{}}, "exec-1", "tenant-1", scheduler.ExecutionManifest{
		ProjectDir:        t.TempDir(),
		RuntimeWorkflowID: "wf-1",
		ExecutionMode:     "workflow",
	}, nil)
	env, cleanup := container.childEnv()
	defer cleanup()
	if got := envValue(env, TodoToolsEnv); got != "1" {
		t.Fatalf("container child %s = %q, want \"1\"", TodoToolsEnv, got)
	}
}

// TestChildEnvForcesTheTodoOptInOn pins that an AMBIENT toggle can never turn
// the family off for an orchicon claude worker: the supervisor/container env is
// not the operator's, so a stray `=0` must be overridden rather than inherited.
func TestChildEnvForcesTheTodoOptInOn(t *testing.T) {
	t.Setenv(TodoToolsEnv, "0")
	env, cleanup := childEnvFor(t, scheduler.ExecutionManifest{ProjectDir: t.TempDir()})
	defer cleanup()
	if got := envValue(env, TodoToolsEnv); got != "1" {
		t.Fatalf("child %s = %q with an ambient \"0\", want \"1\"", TodoToolsEnv, got)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, TodoToolsEnv+"=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s appears %d times in the child env, want exactly 1", TodoToolsEnv, n)
	}
}

// TestTodoOptInIsNotAPermissionBypass pins that the opt-in stays an opt-in: no
// bypass spelling rides the env or the launch argv alongside it.
func TestTodoOptInIsNotAPermissionBypass(t *testing.T) {
	env, cleanup := childEnvFor(t, scheduler.ExecutionManifest{ProjectDir: t.TempDir()})
	defer cleanup()
	for _, kv := range env {
		if strings.Contains(kv, "dangerously") {
			t.Fatalf("child env carries a bypass spelling: %q", kv)
		}
	}
}
