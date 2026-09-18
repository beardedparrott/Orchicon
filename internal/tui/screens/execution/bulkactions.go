package execution

// bulkactions.go — BULK actions for the Workers and Workflows panes.
//
// THE OPERATOR: "There doesn't seem to be a bulk delete on workflows or workers. We should have the ctrl+x
// there as well."
//
// The mechanism was already here and already shared: kit2 marks rows with SPACE (any list), `BulkIDs`
// reports a selection above the shared threshold, and `actionsForSelection` lets a source REPLACE its
// single-row actions with bulk ones — which is exactly what the Schedules pane does. What was missing was
// the branch, so marking workers or workflows did nothing but highlight rows.
//
// The shape matches the Work pane's own bulk delete: one action, named with the count, behind a confirm,
// writing each id in turn and COUNTING the refusals rather than stopping at the first — a partial success
// reported as one ("deleted 3 of 5 — 2 failed") is far more useful than an error that hides the rest.

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// entityBulkActions builds the bulk action set for one source.
//
// key is the source's OWN delete chord (`x` on Workers, `X` on Workflows, where lowercase `x` belongs to
// the step editor) so a bulk delete answers the same key its single-row delete does — the rule every other
// multi-select list in this client follows.
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
//
// It is its own function rather than a call to entityBulkActions because the CONFIRM has to describe
// executions: the shared builder promises "every version of it", which is a worker/workflow fact. What
// these rows do have that those do not is a LIVE state — a marked run may still be executing — so the
// confirm says so and names how many, rather than letting the operator discover it afterwards.
//
// The write itself is the same single-id thunk, called once per row, so a partial failure is counted
// and reported as "deleted 2 of 3 — 1 failed" instead of a bare count (see entityBulkActions' own note
// for why the batch RPC is not used).
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
//
// It is a helper because the same wording is needed wherever a delete meets a live execution, and
// because saying it is the whole point: deleting a running execution stops it, and an operator who
// expected the run to continue in the background must not find out by watching it die.
func liveNote(meta string) string {
	if !isLiveExecution(meta) {
		return ""
	}
	return "\nThis execution is STILL RUNNING — it will be stopped first."
}

// runBulkDeleteActions is the Runs pane's bulk delete. It mirrors executionBulkDeleteActions: the
// shared entityBulkActions cannot be used because its confirm promises "every version of it", which is
// a worker/workflow fact, and a run has none.
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

// isLiveRun reports whether a run row is non-terminal. A run that is pending, running or paused has not
// finished, so deleting it aborts it first (see DeleteWorkflowRun).
//
// It reads the bare status word, which is exactly what the row's Meta holds (workflowRunStatusWord) — and
// accepting the raw enum form too costs one TrimPrefix, which is worth it so a caller cannot pass "the
// other" spelling and silently get false for every live run. That silent false is precisely the bug
// that had disabled the retry and force-progress chords.
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
//
// A category FOLDER can be marked like any other row (it is a row), and its id is synthetic — so a bulk
// delete must skip it. The count in the confirm is the count of what will actually be written, which is
// the number the operator is agreeing to.
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
//
// The two have to live in different packages — the shell draws the conversations rail and its folder
// rows, the execution screen draws the Workers and Workflows panes, and the shell imports the screen
// rather than the other way round — so the literal necessarily appears twice. That is exactly how the
// Worker and Workflow delete chords drifted apart in the first place (one pane on `x`, the other on
// `shift+x`), so rather than add a third comment asking people to keep them in step, a test compares
// them: see the shell's TestDeleteChordMatchesThePanes.
func DeleteChord() string { return keyDelete }
