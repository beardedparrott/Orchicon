package execution

// bulkactions.go — BULK actions for the Workers and Workflows panes.

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// entityBulkActions builds the bulk action set for one source.
func (m *Model) entityBulkActions(source, key, noun string, ids []string, del func(context.Context, string) error) []kit2.Action {
	n := len(ids)
	return []kit2.Action{
		{
			Label: fmt.Sprintf("delete %d selected", n), Key: key, Danger: true, Source: source,
			Confirm: fmt.Sprintf("Delete %d %ss?\n", n, noun) +
				"This removes each one AND every version of it, and cannot be undone.\n" +
				"Each write goes out in turn, and the first refusal is counted rather than swallowed.",
			Do: func(ctx context.Context) error {
				failed := 0
				for _, id := range ids {
					if err := del(ctx, id); err != nil {
						failed++
					}
				}
				if failed > 0 {
					return fmt.Errorf("deleted %d of %d — %d failed", n-failed, n, failed)
				}
				return nil
			},
		},
		{
			Label: "clear selection", Key: "esc", Source: source,
			// No confirm and no write: it is the escape hatch the operator named, and offering it
			// as an action makes it discoverable from the bar as well as the key.
			Apply: func() {
				m.Base.ClearMarks()
				m.notice = "selection cleared"
			},
			Do: func(context.Context) error { return nil },
		},
	}
}

// executionBulkDeleteActions is the Executions pane's bulk delete.
func (m *Model) executionBulkDeleteActions(ids []string) []kit2.Action {
	n := len(ids)
	live := 0
	for _, id := range ids {
		if it, ok := m.Base.SourceItem(srcExecutions, id); ok && isLiveExecution(it.Meta) {
			live++
		}
	}
	warn := "This REMOVES each execution and its transcript, and cannot be undone."
	if live > 0 {
		verb := "are"
		if live == 1 {
			verb = "is"
		}
		warn += fmt.Sprintf("\n%d of them %s STILL RUNNING and will be stopped first.", live, verb)
	}
	return []kit2.Action{
		{
			Label: fmt.Sprintf("delete %d selected", n), Key: keyDelete, Danger: true, Source: srcExecutions,
			Confirm: fmt.Sprintf("Delete %d executions?\n", n) + warn +
				"\nEach write goes out in turn, and failures are counted rather than swallowed.",
			Do: func(ctx context.Context) error {
				failed := 0
				for _, id := range ids {
					if err := m.rpcDeleteExecution(ctx, id); err != nil {
						failed++
					}
				}
				if failed > 0 {
					return fmt.Errorf("deleted %d of %d — %d failed", n-failed, n, failed)
				}
				return nil
			},
		},
		{
			Label: "clear selection", Key: "esc", Source: srcExecutions,
			Apply: func() {
				m.Base.ClearMarks()
				m.notice = "selection cleared"
			},
			Do: func(context.Context) error { return nil },
		},
	}
}

// liveNote is the extra sentence a single-row delete confirm carries when the row is STILL RUNNING.
func liveNote(meta string) string {
	if !isLiveExecution(meta) {
		return ""
	}
	return "\nThis execution is STILL RUNNING — it will be stopped first."
}

// runBulkDeleteActions is the Runs pane's bulk delete.
func (m *Model) runBulkDeleteActions(ids []string) []kit2.Action {
	n := len(ids)
	live := 0
	for _, id := range ids {
		if it, ok := m.Base.SourceItem(srcRuns, id); ok && isLiveRun(it.Meta) {
			live++
		}
	}
	warn := "This REMOVES each run and its step runs, and cannot be undone."
	if live > 0 {
		verb := "are"
		if live == 1 {
			verb = "is"
		}
		warn += fmt.Sprintf("\n%d of them %s STILL RUNNING and will be aborted first.", live, verb)
	}
	return []kit2.Action{
		{
			Label: fmt.Sprintf("delete %d selected", n), Key: keyDelete, Danger: true, Source: srcRuns,
			Confirm: fmt.Sprintf("Delete %d workflow runs?\n", n) + warn +
				"\nEach write goes out in turn, and failures are counted rather than swallowed.",
			Do: func(ctx context.Context) error {
				failed := 0
				for _, id := range ids {
					if err := m.rpcDeleteRun(ctx, id); err != nil {
						failed++
					}
				}
				if failed > 0 {
					return fmt.Errorf("deleted %d of %d — %d failed", n-failed, n, failed)
				}
				return nil
			},
		},
		{
			Label: "clear selection", Key: "esc", Source: srcRuns,
			Apply: func() {
				m.Base.ClearMarks()
				m.notice = "selection cleared"
			},
			Do: func(context.Context) error { return nil },
		},
	}
}

// isLiveRun reports whether a run row is non-terminal.
func isLiveRun(meta string) bool {
	w := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(meta)), "workflow_run_status_")
	switch w {
	case "pending", "running", "paused":
		return true
	}
	return false
}

// runLiveNote is the runs equivalent of liveNote.
func runLiveNote(meta string) string {
	if !isLiveRun(meta) {
		return ""
	}
	return "\nThis run is STILL RUNNING — it will be aborted first."
}

// markableIDs returns the marked ids that are REAL entities.
func (m *Model) markableIDs() []string {
	ids := m.Base.MarkedIDs()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || screenkit.IsGroupRow(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// DeleteChord is the pane's delete chord, EXPORTED so the shell can assert its own copy of the same
// value agrees.
func DeleteChord() string { return keyDelete }
