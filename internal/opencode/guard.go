package opencode

import "github.com/beardedparrott/orchicon/internal/guard"

// newExecutionGuard creates the OS-level safety shim for one in-process
// execution. The containerized path builds the same shim inside the
// runtime container via guard.MakeGuard (see internal/runtime/agent.go).
func newExecutionGuard(projectDir string) (*guard.Guard, error) {
	return guard.NewExecutionGuard(projectDir)
}
