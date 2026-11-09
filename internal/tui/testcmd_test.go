package tui

// testcmd_test.go — running a tea.Cmd the way the RUNTIME does.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// runCmdDelivering runs cmd and feeds each resulting message back into the model, unwrapping
// batches — which is what bubbletea does between the command and Update.
func runCmdDelivering(t *testing.T, m *App, cmd tea.Cmd) *App {
	t.Helper()
	return runCmdDeliveringDepth(t, m, cmd, 0)
}

// runCmdDeliveringDepth is runCmdDelivering with the recursion bounded.
func runCmdDeliveringDepth(t *testing.T, m *App, cmd tea.Cmd, depth int) *App {
	t.Helper()
	if cmd == nil || m == nil || depth > 6 {
		return m
	}
	switch msg := runCmdBounded(cmd, runCtxCmdBudget).(type) {
	case nil:
		return m
	case tea.BatchMsg:
		for _, c := range msg {
			m = runCmdDeliveringDepth(t, m, c, depth+1)
		}
		return m
	default:
		nm, _ := m.Update(msg)
		if nm == nil {
			return m
		}
		return nm.(*App)
	}
}
