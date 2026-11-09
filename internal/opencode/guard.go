package opencode

import "github.com/beardedparrott/orchicon/internal/guard"

// newExecutionGuard creates the OS-level safety shim for one in-process execution.
func newExecutionGuard(projectDir string) (*guard.Guard, error) {
	return guard.NewExecutionGuard(projectDir)
}
