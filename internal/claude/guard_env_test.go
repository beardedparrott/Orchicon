package claude

// guard_env_test.go — T5: the OS-level guard shim sits on the path that executes
// a claude worker's subprocess commands.
//
// This is the criterion that makes a destructive command impossible INSIDE the
// runtime container too: the shim is prepended to the child's PATH, so every
// process the session (or anything it spawns) resolves a guarded binary through
// the shim, no matter how the command is spelled or where it is spawned from.
//
// The targets are TEMP SPACE — the failure mode of a bad verdict is a directory
// this test owns (the rule internal/guard/guard_test.go documents).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// childEnvFor builds a session over one manifest — the transport selection is the
// only thing that differs between the local and container cases.
func childEnvFor(t *testing.T, m scheduler.ExecutionManifest) ([]string, func()) {
	t.Helper()
	s := newSession(&Bridge{}, "exec-1", "tenant-1", m, nil)
	return s.childEnv()
}

func TestChildEnvPutsTheGuardShimFirstOnPath(t *testing.T) {
	project := t.TempDir()
	env, cleanup := childEnvFor(t, scheduler.ExecutionManifest{ProjectDir: project})
	defer cleanup()

	path := envValue(env, "PATH")
	if path == "" {
		t.Fatal("childEnv produced no PATH")
	}
	shimDir := strings.SplitN(path, string(os.PathListSeparator), 2)[0]
	if !strings.Contains(filepath.Base(shimDir), "orchicon-guard-") {
		t.Fatalf("PATH does not begin with a guard shim directory: %q", path)
	}
	if got := envValue(env, ProjectDirEnv); got != project {
		t.Fatalf("%s = %q, want the project dir %q — the hook would judge a different boundary", ProjectDirEnv, got, project)
	}
	if got := envValue(env, HookBinEnv); got == "" {
		t.Fatalf("%s is unset — the hook command would not resolve the product binary", HookBinEnv)
	}

	// A destructive command on a path OUTSIDE the project is refused by the shim
	// THAT THIS ENVIRONMENT PREPENDS — i.e. by the shim the claude child gets.
	outside := t.TempDir()
	cmd := exec.Command(filepath.Join(shimDir, "rm"), "-rf", outside)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the shim on the claude child's PATH ALLOWED `rm -rf %s`: %q", outside, out)
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("running the shim: %v", err)
	}
	if !strings.Contains(strings.ToUpper(string(out)), "ORCHICON GUARD") {
		t.Errorf("the refusal does not name the guard: %q", out)
	}

	// Ordinary in-project cleanup is allowed (the shim was not made useless).
	build := filepath.Join(project, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ok := exec.Command(filepath.Join(shimDir, "rm"), "-rf", "build")
	ok.Dir = project
	ok.Env = env
	if out, err := ok.CombinedOutput(); err != nil {
		t.Fatalf("the shim refused in-project cleanup `rm -rf build`: %v (%q)", err, out)
	}
	if _, err := os.Stat(build); !os.IsNotExist(err) {
		t.Fatal("in-project cleanup did not happen")
	}
}

// Closing the session's environment cleanup must remove the shim directory: a
// shim that outlives its execution would keep guarding a path nothing owns.
func TestChildEnvCleanupRemovesTheShimDir(t *testing.T) {
	project := t.TempDir()
	env, cleanup := childEnvFor(t, scheduler.ExecutionManifest{ProjectDir: project})
	shimDir := strings.SplitN(envValue(env, "PATH"), string(os.PathListSeparator), 2)[0]
	if _, err := os.Stat(shimDir); err != nil {
		t.Fatalf("the shim directory is not present while the session is live: %v", err)
	}
	cleanup()
	if _, err := os.Stat(shimDir); !os.IsNotExist(err) {
		t.Fatalf("the shim directory survived the session cleanup: %v", err)
	}
}

// The CONTAINER transport must NOT ship a host-built shim path into the
// container: the supervisor's stdio path builds and prepends its own shim inside
// the container (guard.MakeGuard + prependGuard), and a host path would point at
// a directory that does not exist there.
func TestContainerTransportDoesNotShipAHostShimPath(t *testing.T) {
	project := t.TempDir()
	// A container execution: a runtime client is wired (that is what makes
	// spawn() — and therefore isContainer — choose the container child), the
	// execution carries a workflow id, and the mode is not local.
	s := newSession(&Bridge{rt: &runtime.Client{}}, "exec-1", "tenant-1", scheduler.ExecutionManifest{
		ProjectDir:        project,
		RuntimeWorkflowID: "wf-1",
		ExecutionMode:     "workflow",
	}, nil)
	env, cleanup := s.childEnv()
	defer cleanup()
	// PATH must be UNTOUCHED: the supervisor builds and prepends the shim INSIDE
	// the container, and a host-built path would not exist there. (The harness's
	// own PATH may legitimately already carry a guard entry, so the assertion is
	// that THIS adapter changed nothing.)
	base := envValue(os.Environ(), "PATH")
	if got := envValue(env, "PATH"); got != base {
		t.Fatalf("the container transport modified PATH (host shim shipped in?): %q, base %q", got, base)
	}
	// The boundary the hook reads is still set — both transports get it.
	if got := envValue(env, ProjectDirEnv); got != project {
		t.Fatalf("%s = %q, want %q", ProjectDirEnv, got, project)
	}
}

// The empty-runtime case (local mode) keeps the shim even when a workflow id is
// present, because spawn() selects the host subprocess there.
func TestLocalExecutionModeStillGetsTheShim(t *testing.T) {
	project := t.TempDir()
	env, cleanup := childEnvFor(t, scheduler.ExecutionManifest{
		ProjectDir:        project,
		RuntimeWorkflowID: "wf-1",
		ExecutionMode:     "local",
	})
	defer cleanup()
	base := envValue(os.Environ(), "PATH")
	got := envValue(env, "PATH")
	if got == base {
		t.Fatalf("a local-mode execution did not get a guard shim prepended: %q", got)
	}
	shimDir := strings.SplitN(got, string(os.PathListSeparator), 2)[0]
	if !strings.Contains(filepath.Base(shimDir), "orchicon-guard-") {
		t.Fatalf("the local transport's PATH does not begin with a shim dir: %q", got)
	}
	if shimDir == strings.SplitN(base, string(os.PathListSeparator), 2)[0] {
		t.Fatalf("the local transport re-used the ambient guard dir instead of its own: %q", got)
	}
}

// A CONTAINER session's hook command must name the in-container bind mount, not
// the control plane's own (host) executable. The argv — and therefore the
// settings document naming the hook — is built on the HOST and executed inside
// the run's container, where the host path does not exist; a hook that cannot
// launch silently removes the authority layer (protected paths, the carve-outs,
// the operator policy) from every container run.
func TestHookBinaryFollowsTheTransport(t *testing.T) {
	project := t.TempDir()
	// The operator override names a HOST path — exactly what must never reach a
	// container hook command.
	t.Setenv(HookBinEnv, "/host/only/orchicon")
	settingsOf := func(args []string) string {
		for i, a := range args {
			if a == "--settings" && i+1 < len(args) {
				return args[i+1]
			}
		}
		t.Fatal("the launch argv carries no --settings document")
		return ""
	}

	container := newSession(&Bridge{rt: &runtime.Client{}}, "exec-1", "tenant-1", scheduler.ExecutionManifest{
		ProjectDir:        project,
		RuntimeWorkflowID: "wf-1",
		ExecutionMode:     "workflow",
	}, nil)
	args, err := PermissionArgs(container.permissionOptions())
	if err != nil {
		t.Fatalf("PermissionArgs: %v", err)
	}
	if got := SettingsHookBinary(settingsOf(args)); got != HookBinaryContainerPath {
		t.Fatalf("the container hook command names %q, want the in-container bind mount %q", got, HookBinaryContainerPath)
	}
	env, cleanup := container.childEnv()
	defer cleanup()
	if got := envValue(env, HookBinEnv); got != HookBinaryContainerPath {
		t.Fatalf("the container child's %s = %q, want %q", HookBinEnv, got, HookBinaryContainerPath)
	}

	// The LOCAL transport keeps resolving the operator's host binary: there the
	// child IS a host process, a sibling of this one.
	local := newSession(&Bridge{}, "exec-1", "tenant-1", scheduler.ExecutionManifest{ProjectDir: project}, nil)
	args, err = PermissionArgs(local.permissionOptions())
	if err != nil {
		t.Fatalf("PermissionArgs: %v", err)
	}
	if got := SettingsHookBinary(settingsOf(args)); got != "/host/only/orchicon" {
		t.Fatalf("the local hook command names %q, want the host override %q", got, "/host/only/orchicon")
	}
}
