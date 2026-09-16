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
