package tui

// testcmd_test.go — running a tea.Cmd the way the RUNTIME does.
//
// WHY THIS EXISTS. `tea.Batch` does not run the commands it is given: it returns a closure whose result
// is a `tea.BatchMsg`, which is a `[]Cmd` — the runtime then calls each of those. A test that simply
// called `cmd()` therefore got the batch back and ran NOTHING.
//
// That is a false-pass waiting to happen, and it bit for real: once the assign modal began reloading the
// category list on open, the shell staged that load alongside the write and the returned command became
// a batch — so two tests that had passed for months started reporting "AssignToCategory got ("", "")"
// even though their code had not changed. The production path was always fine (bubbletea expands the
// batch); the tests were wrong to assume a single closure.
//
// Every test that runs a command which may have been batched should go through here.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// runCmdDelivering runs cmd and feeds each resulting message back into the model, unwrapping batches —
// which is what bubbletea does between the command and Update. It returns the updated App.
//
// A nil command, or one that produces nothing, returns the model unchanged.
//
// IT IS BOUNDED, because it is the helper most tests reach for and it used to be unbounded TWICE: the recursion had
// no depth cap, and each member was invoked INLINE — so a command that BLOCKS hung the suite while the depth stayed
// low. That is the shape that already cost this package a ten-minute timeout once (a conversations reload on the
// refresh tick put waitChat, a blocking waiter, into a tick's command tree). A test helper's failure mode is a
// hang rather than a failure, so the only real defence is to bound it — see runCmdBounded and runCtx.
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
