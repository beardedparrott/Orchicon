package askorchicon

import (
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
)

// ask_guard.go: the Ask path's destructive-command backstop.

// askGuardLog is the package-level logger the guard warns on. Nil-safe.
var askGuardLog atomic.Pointer[slog.Logger]

// SetAskGuardLogger wires the instance logger (server.Mount). Nil-safe.
func SetAskGuardLogger(log *slog.Logger) {
	if log != nil {
		askGuardLog.Store(log)
	}
}

// askGuardState holds the process-wide guard shim: one dir per daemon, created lazily on first Ask
// bash execution, closed at daemon shutdown.
var askGuardState = struct {
	sync.Mutex
	g    *guard.Guard
	err  error
	init bool
}{}

// askGuardForExec returns the process-wide guard, creating it lazily on first bash execution and
// retrying after an earlier failure (a temp-dir failure is transient).
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

// AskGuardEnvironFor returns the bash environment for an Ask-path execution: the guard shim dir
// FIRST on PATH (mirroring internal/runtime/agent.go's prependGuard), then os.Environ().
func AskGuardEnvironFor(policyPath, projectDir string, grants, once []string, fullsend bool) []string {
	env := os.Environ()
	g, err := askGuardForExec()
	if err != nil || g == nil {
		// No guard → env unchanged.
		return env
	}
	return append(g.Apply(env), guard.InteractiveEnviron(policyPath, projectDir, grants, once, fullsend)...)
}

// AskGuardEnviron is the conversation-less form (kept for callers that want the
// shim on PATH without a conversation's project, grants or once-targets).
func AskGuardEnviron() []string {
	return AskGuardEnvironFor(permpolicy.DefaultPath(), "", nil, nil, false)
}

// askGuardEnviron is the PER-CONVERSATION environment factory handed to the host tool suite
// (orchicon.HostTools.SetBashEnviron): the shim, plus the conversation's project dir.
func (s *Service) askGuardEnviron(scope AskFileScope, convID string) func() []string {
	return func() []string {
		var grants, once []string
		fullsend := false
		if s != nil {
			grants, once = s.grants.Roots(convID), s.once.Targets(convID)
			fullsend = s.fullsend.Enabled(convID)
		}
		return AskGuardEnvironFor(permpolicy.DefaultPath(), scope.Dir, grants, once, fullsend)
	}
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
