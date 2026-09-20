package execution

// execution_delete_test.go — the Executions pane's DELETE, single and bulk.
//
// The operator, mid-live-test: "Executions and Workflow Runs do not have a delete operation (single
// and bulk)."
//
// THE EXECUTIONS HALF WAS A PURE CLIENT GAP. DeleteExecution AND BatchDeleteExecutions have existed in
// the API all along (proto/orchicon/api/v1/execution_service.proto:52,55, implemented at
// internal/execution/service.go:226,270) — the TUI simply never called either. Workers and Workflows
// had delete; Executions did not. So the property worth pinning is NOT "the action exists" (an action
// in the bar that its chord cannot reach is the failure this pane's own history is made of): it is
// that the delete chord resolves to a real, runnable delete on this pane, and that its confirm tells
// the truth about what it destroys.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// executionsPane builds a screen focused on the Executions pane with the given rows loaded.
func executionsPane(t *testing.T, rows ...screenkit.Item) *Model {
	t.Helper()
	m := newModel(t, &fakePlane{})
	if !m.Base.SelectSource(srcExecutions) {
		t.Fatal("fixture: could not focus the Executions pane")
	}
	m.Base.LoadItems(srcExecutions, rows, "")
	return m
}

// labelFor returns the action whose label is exactly want.
func labelFor(m *Model, want string) (kit2.Action, bool) {
	for _, a := range m.actionsForSelection() {
		if a.Label == want {
			return a, true
		}
	}
	return kit2.Action{}, false
}

// deleteAction is the SINGLE-row delete as the pane builds it.
func deleteAction(t *testing.T, m *Model) kit2.Action {
	t.Helper()
	a, ok := m.actionByKey(keyDelete)
	if !ok {
		t.Fatalf("ctrl+x resolved to no action on the Executions pane — the pane offers no delete. "+
			"Actions present: %v", actionLabels(m.actionsForSelection()))
	}
	return a
}

// THE CHORD REACHES A REAL, DESTRUCTIVE DELETE.
func TestExecutionDeleteChordBuildsADeleteAction(t *testing.T) {
	m := executionsPane(t, screenkit.Item{ID: "exec-1", Title: "wf · item", Meta: "succeeded"})

	a := deleteAction(t, m)
	if !a.Danger {
		t.Error("the delete must be marked Danger, so the bar and the confirm treat it as destructive")
	}
	if !strings.Contains(a.Confirm, "exec-1") {
		t.Errorf("the confirm does not name the execution it destroys: %q", a.Confirm)
	}
	if !strings.Contains(strings.ToUpper(a.Confirm), "CANNOT BE UNDONE") {
		t.Errorf("the confirm does not warn that it is irreversible: %q", a.Confirm)
	}
	if a.Do == nil {
		t.Fatal("the delete has no write — the action would remove the row locally and delete nothing")
	}
}

// THE WRITE GOES TO DeleteExecution for the focused id.
func TestExecutionDeleteWritesTheFocusedID(t *testing.T) {
	m := executionsPane(t, screenkit.Item{ID: "exec-42", Title: "wf", Meta: "succeeded"})
	var got []string
	m.rpcDeleteExecution = func(_ context.Context, id string) error {
		got = append(got, id)
		return nil
	}
	a := deleteAction(t, m)
	if err := a.Do(context.Background()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(got) != 1 || got[0] != "exec-42" {
		t.Errorf("deleted %v, want exactly [exec-42] — the row under the cursor", got)
	}
}

// DELETING A *RUNNING* EXECUTION SAYS SO FIRST.
//
// The server cancels a still-running execution before removing it (execution/service.go), so the
// operator's run dies as a side effect — and an operator who expected it to keep going must not learn
// that by watching it stop.
func TestExecutionDeleteWarnsWhenTheRunIsStillLive(t *testing.T) {
	live := executionsPane(t, screenkit.Item{ID: "exec-live", Title: "wf", Meta: "running · worker"})
	a := deleteAction(t, live)
	if !strings.Contains(a.Confirm, "STILL RUNNING") {
		t.Errorf("the confirm does not warn that a live execution will be stopped first: %q", a.Confirm)
	}

	// And the warning is NOT blanket: a blanket warning is noise the operator learns to skip, and it
	// would make the live case indistinguishable from the safe one.
	done := executionsPane(t, screenkit.Item{ID: "exec-done", Title: "wf", Meta: "succeeded"})
	a2 := deleteAction(t, done)
	if strings.Contains(a2.Confirm, "STILL RUNNING") {
		t.Errorf("a FINISHED execution's confirm claims it is still running: %q", a2.Confirm)
	}
}

// BULK: marking above the shared threshold replaces the single-row actions with a counted delete.
func TestExecutionBulkDeleteAppearsForASelection(t *testing.T) {
	m := executionsPane(t,
		screenkit.Item{ID: "exec-1", Title: "wf a", Meta: "succeeded"},
		screenkit.Item{ID: "exec-2", Title: "wf b", Meta: "failed"},
		screenkit.Item{ID: "exec-3", Title: "wf c", Meta: "running"},
	)
	m.Base.SelectItem(srcExecutions, "exec-1")
	press(t, m, " ")
	press(t, m, " ")
	press(t, m, " ")

	a, ok := labelFor(m, "delete 3 selected")
	if !ok {
		t.Fatalf("marking three executions produced no bulk delete. Actions: %v",
			actionLabels(m.actionsForSelection()))
	}
	if a.Key != keyDelete {
		t.Errorf("the bulk delete answers %q, want the shared %q (single and bulk share one chord)",
			a.Key, keyDelete)
	}
	if !strings.Contains(a.Confirm, "3 executions") {
		t.Errorf("the bulk confirm does not say how many it destroys: %q", a.Confirm)
	}
	// ONE of the three is live, and the confirm must say so — the fact this pane has that
	// Workers/Workflows do not.
	if !strings.Contains(a.Confirm, "STILL RUNNING") {
		t.Errorf("the bulk confirm misses that one of the marked runs is live: %q", a.Confirm)
	}
}

// THE BULK WRITE GOES OUT once per id, and a PARTIAL FAILURE is COUNTED rather than swallowed.
//
// This is why the pane writes the single-id RPC per row rather than calling BatchDeleteExecutions: the
// batch returns only a count, so "3 of 5 failed" would arrive as an opaque number. A partial success
// reported as one is what lets the operator know which rows are still there.
func TestExecutionBulkDeleteCountsPartialFailures(t *testing.T) {
	m := executionsPane(t,
		screenkit.Item{ID: "exec-1", Title: "a", Meta: "succeeded"},
		screenkit.Item{ID: "exec-2", Title: "b", Meta: "succeeded"},
		screenkit.Item{ID: "exec-3", Title: "c", Meta: "succeeded"},
	)
	m.Base.SelectItem(srcExecutions, "exec-1")
	press(t, m, " ")
	press(t, m, " ")
	press(t, m, " ")

	var got []string
	m.rpcDeleteExecution = func(_ context.Context, id string) error {
		got = append(got, id)
		if id == "exec-2" {
			return errors.New("execution is locked")
		}
		return nil
	}

	a, ok := labelFor(m, "delete 3 selected")
	if !ok {
		t.Fatal("no bulk delete to run")
	}
	err := a.Do(context.Background())
	if len(got) != 3 {
		t.Fatalf("wrote %d deletes (%v), want one per marked row", len(got), got)
	}
	if err == nil {
		t.Fatal("a partial failure returned nil — the operator would believe all three were removed")
	}
	if !strings.Contains(err.Error(), "2 of 3") {
		t.Errorf("the failure does not report the partial success (want \"2 of 3\"): %v", err)
	}
}

// ONE marked row is NOT bulk — it is the row the cursor is on, so the single-row delete stands alone.
func TestExecutionSingleMarkIsNotBulk(t *testing.T) {
	m := executionsPane(t,
		screenkit.Item{ID: "exec-1", Title: "a", Meta: "succeeded"},
		screenkit.Item{ID: "exec-2", Title: "b", Meta: "succeeded"},
	)
	m.Base.SelectItem(srcExecutions, "exec-1")
	press(t, m, " ")

	for _, a := range m.actionsForSelection() {
		if strings.Contains(a.Label, "1 selected") {
			t.Fatalf("one marked row produced a BULK action (%q) — one mark IS the cursor row", a.Label)
		}
	}
}

// THE PANE ADVERTISES IT. A delete the hint row never names is a feature the operator has to be told
// about — which is exactly how the bulk delete on Workers was found missing before.
func TestExecutionHintNamesTheDelete(t *testing.T) {
	m := executionsPane(t, screenkit.Item{ID: "exec-1", Title: "wf", Meta: "succeeded"})
	if h := m.HintLine(); !strings.Contains(h, keyDelete) {
		t.Errorf("the Executions hint does not name %s: %q", keyDelete, h)
	}
	if h := m.HintLine(); !strings.Contains(h, "mark") {
		t.Errorf("the Executions hint does not say how to reach the bulk gesture: %q", h)
	}
}

// actionLabels lists the bar's labels, for failure messages.
func actionLabels(acts []kit2.Action) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Label)
	}
	return out
}
