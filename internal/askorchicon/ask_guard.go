package askorchicon

import (
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/beardedparrott/orchicon/internal/guard"
)

// ask_guard.go: the Ask path's destructive-command backstop.
//
// The worker path applies the execution guard (internal/guard) by
// prepending a shim dir to the agent's PATH (internal/runtime/agent.go:
// guard.MakeGuard + prependGuard): any process the worker spawns resolves
// `rm`, `sudo`, `dd`, `mkfs`, … through the shim, which refuses
// destructive or out-of-project targets. The Ask path runs bash
// IN-PROCESS on the host container (HostTools.execBash with a nil
// containerExec), so the same shim must ride the bash tool's environment —
// PATH first — or an Ask conversation could `rm -rf` outside the project
// while a worker would be refused.
//
// The guard is the OS-level backstop, not the primary containment: the
// file tools stay engine-scoped (worktree.Base), and the persona/prompt
// rails ride the system prompt. This shim is the hard refuse for the
// "a subprocess did it" hole the 2026-07-30 /home wipe went through.

// askGuardLog is the package-level logger the guard warns on. Nil-safe.
var askGuardLog atomic.Pointer[slog.Logger]

// SetAskGuardLogger wires the instance logger (server.Mount). Nil-safe.
func SetAskGuardLogger(log *slog.Logger) {
	if log != nil {
		askGuardLog.Store(log)
	}
}

// askGuardState holds the process-wide guard shim: one dir per daemon,
// created lazily on first Ask bash execution, closed at daemon shutdown.
//
// It is built with an EMPTY projectDir (guard.NewExecutionGuard("")) —
// the shared host-serve mode: every absolute target outside the
// sanctioned scratch dir is refused (guard.go's blocked_path: empty
// PROJECT_DIR → every absolute target is outside scope). Ask sessions are
// not pre-bound to one project dir (the boundary resolves per call via
// AskFileRoot), and the file tools' engine boundary (worktree.Base)
// scopes writes to the project dir regardless — the shim is the
// destructive-binary backstop, not the containment boundary.
var askGuardState = struct {
	sync.Mutex
	g    *guard.Guard
	err  error
	init bool
}{}

// askGuardForExec returns the process-wide guard, creating it lazily on
// first bash execution and retrying after an earlier failure (a temp-dir
// failure is transient). Never returns (nil, nil).
func askGuardForExec() (*guard.Guard, error) {
	askGuardState.Lock()
	defer askGuardState.Unlock()
	if askGuardState.init {
		return askGuardState.g, askGuardState.err
	}
	askGuardState.init = true
	g, err := guard.NewExecutionGuard("")
	if err != nil {
		if log := askGuardLog.Load(); log != nil {
			log.Warn("askorchicon: guard not applied to Ask bash", "error", err)
		} else {
			slog.Warn("askorchicon: guard not applied to Ask bash", "error", err)
		}
	}
	askGuardState.g = g
	askGuardState.err = err
	return g, err
}

// AskGuardEnviron returns the bash environment for Ask-path HostTools
// execution: the guard shim dir FIRST on PATH (mirroring
// internal/runtime/agent.go's prependGuard), then os.Environ(). The guard
// is the worker path's OS-level destructive-command backstop
// (internal/guard): without it, an Ask conversation could `rm -rf`
// outside the project while a worker would be refused.
func AskGuardEnviron() []string {
	env := os.Environ()
	g, err := askGuardForExec()
	if err != nil || g == nil {
		// No guard → env unchanged. The failure is already warned at
		// creation; degrading to unguarded bash matches the worker
		// supervisor's behavior (warn + continue, never block).
		return env
	}
	return g.Apply(env)
}

// CloseAskGuard removes the shim dir at daemon shutdown. Safe on nil /
// double call.
func CloseAskGuard() {
	askGuardState.Lock()
	defer askGuardState.Unlock()
	if askGuardState.g != nil {
		askGuardState.g.Close()
	}
	// Reset so a later call (test re-mount) rebuilds fresh.
	askGuardState.g = nil
	askGuardState.err = nil
	askGuardState.init = false
}

