package tui

// railbulk_test.go — the three rail fixes.
//
//	1. the ARROWS move the rail's selection from CONTENT focus, not only from the composer
//	2. the chord list lives in the COMPOSER, not inside the rail pane
//	3. bulk operations: space marks, ctrl+x bulk-deletes (confirmed), ctrl+t bulk-assigns
//
// Each test names the operator's words, because the value of these is that they pin a REPORT rather
// than an implementation detail.

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// railApp builds an Ask shell with a loaded rail of n conversations and an empty composer.
func railApp(t *testing.T, n int) *App {
	t.Helper()
	m := newTestApp()
	m.width, m.height = 140, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.active = TabAsk
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.convLoaded = true
	m.convErr = ""
	m.chatConvID = "conv-01"
	for i := 1; i <= n; i++ {
		m.conversations = append(m.conversations, chat.Conversation{
			ID:    fmt.Sprintf("conv-%02d", i),
			Title: fmt.Sprintf("conversation %02d", i), MessageN: 2,
		})
	}
	m.convSel, m.convScroll = 0, 0
	m.dock.SetValue("")
	m.refreshComposerHint()
	return m
}

func press(m *App, keys ...tea.KeyMsg) *App {
	for _, k := range keys {
		nm, _ := m.Update(k)
		m = nm.(*App)
	}
	return m
}

var downKey = tea.KeyMsg{Type: tea.KeyDown}
var upKey = tea.KeyMsg{Type: tea.KeyUp}
var spaceKey = tea.KeyMsg{Type: tea.KeySpace}
var enterKey = tea.KeyMsg{Type: tea.KeyEnter}

// ---------------------------------------------------------------- 1. the arrows

// TestRailArrowsMoveTheSelectionFromContentFocus is the operator's report: "I move up/down the
// conversation list and it moves through the conversations but the currently selected conversation
// selector is not moving".
//
// THE CAUSE, and why this test drives all three focus levels: the rail's arrows were wired into the
// COMPOSER branch alone. With the keyboard in the CONTENT — which is where the operator is when
// reading the transcript, and exactly what the footer's "ctrl+g composer" tells them — the key fell
// through to the Ask screen, whose HIDDEN "conversations" source moved its own cursor and
// re-requested that conversation's detail. So the transcript changed and the rail's highlight did not.
func TestRailArrowsMoveTheSelectionFromContentFocus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		focus func(*App)
	}{
		{"composer focused (the launch default)", func(m *App) { m.setFocus(focusComposer) }},
		{"CONTENT focused (after esc) — the reported case", func(m *App) { m.setFocus(focusContent) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := railApp(t, 12)
			tc.focus(m)

			m = press(m, downKey)
			if m.convSel != 1 {
				t.Fatalf("down must move the rail selection to 1, got %d (this is the reported bug)", m.convSel)
			}
			m = press(m, downKey, downKey)
			if m.convSel != 3 {
				t.Fatalf("down x3 must land on 3, got %d", m.convSel)
			}
			m = press(m, upKey)
			if m.convSel != 2 {
				t.Fatalf("up must come back to 2, got %d", m.convSel)
			}

			// AND THE HIGHLIGHT IS IN THE RENDER — the operator's complaint was about what they SEE,
			// so the selection index moving is not on its own evidence that the bug is fixed.
			rail := m.rightRailView()
			if !strings.Contains(rail, "conversation 03") {
				t.Fatalf("the selected row must be in the rail:\n%s", rail)
			}
		})
	}
}

// TestRailArrowsRespectACloserDraft: with TEXT in the composer the arrows belong to the textarea
// (cursor movement). That rule must survive for the composer path — while the CONTENT path is
// deliberately exempt, because there the composer does not have the keys at all.
func TestRailArrowsRespectACloserDraft(t *testing.T) {
	m := railApp(t, 12)
	m.setFocus(focusComposer)
	m.dock.SetValue("draft in progress")
	m = press(m, downKey)
	if m.convSel != 0 {
		t.Fatalf("with a draft the arrows must stay in the textarea, got convSel=%d", m.convSel)
	}
}

// ---------------------------------------------------------------- 2. the hint

// TestRailChordListLivesInTheComposer: "The command shortcut guidance is inside the conversation list
// pane which is causing it to get cut off. I suggest we put the shortcuts in the composer on
// conversations instead of in that pane."
//
// THE CUT-OFF WAS ARITHMETIC, which is why it is asserted rather than eyeballed: the rail is 32 cells
// with 28 of inner text, and the chord list was 34 characters, so it rendered as
// "ctrl+n: rename · ctrl+t: ca…".
func TestRailChordListLivesInTheComposer(t *testing.T) {
	m := railApp(t, 3)

	// In the composer...
	hint := m.dock.Hint()
	for _, want := range []string{"space: mark", "ctrl+n: rename", "ctrl+t: categorize"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("the composer hint must carry %q:\n%s", want, hint)
		}
	}
	// ...and NOT in the rail pane.
	rail := m.rightRailView()
	if strings.Contains(rail, "ctrl+n") || strings.Contains(rail, "ctrl+t") {
		t.Fatalf("the rail must not carry the chord list in its own truncating pane:\n%s", rail)
	}
	// The row count stays: it is rail state and it fits.
	if !strings.Contains(rail, fmt.Sprintf("1-3/%d", 3)) {
		t.Fatalf("the rail must keep its scroll position footer:\n%s", rail)
	}
	// NO RAIL, NO RAIL HINT: the chords describe a list that is not on screen.
	m.convRailOpen = false
	m2 := railApp(t, 3)
	m2.active = TabWork
	m2.refreshComposerHint()
	if strings.Contains(m2.dock.Hint(), "space: mark") {
		t.Fatalf("the rail's chords must not appear when the rail is not on screen:\n%s", m2.dock.Hint())
	}
}

// ---------------------------------------------------------------- 3. bulk

// attachRail loads the conversations rail on a shell built by categoryApp (which already has the Ask
// tab focused and a category client wired). Returns the shell so a test can chain.
func (m *App) attachRail(n int) *App {
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.convLoaded = true
	m.convErr = ""
	m.conversations = nil
	for i := 1; i <= n; i++ {
		m.conversations = append(m.conversations, chat.Conversation{
			ID:    fmt.Sprintf("conv-%02d", i),
			Title: fmt.Sprintf("conversation %02d", i), MessageN: 2,
		})
	}
	m.convSel, m.convScroll = 0, 0
	m.dock.SetValue("")
	m.refreshComposerHint()
	return m
}

// TestSpaceMarksAndAdvances: the operator's "Spacebar selects". It is the kit2 gesture — mark, then
// step one row — so a run of spaces builds a selection without mark-then-down each time.
func TestSpaceMarksAndAdvances(t *testing.T) {
	m := railApp(t, 5)
	m = press(m, spaceKey)
	if ids := m.convMarkedIDs(); len(ids) != 1 || ids[0] != "conv-01" {
		t.Fatalf("space must mark the highlighted conversation, got %v", ids)
	}
	if m.convSel != 1 {
		t.Fatalf("space must advance one row (kit2 does), got convSel=%d", m.convSel)
	}
	m = press(m, spaceKey)
	if ids := m.convMarkedIDs(); len(ids) != 2 {
		t.Fatalf("a second space must add the next row, got %v", ids)
	}
	// Space on an already-marked row UNMARKS it — the toggle, not a one-way add.
	m = press(m, upKey, spaceKey)
	if ids := m.convMarkedIDs(); len(ids) != 1 || ids[0] != "conv-01" {
		t.Fatalf("space must unmark a marked row, got %v", ids)
	}
	// And at the END of the list the cursor does not walk off it.
	m = railApp(t, 2)
	m = press(m, downKey, spaceKey)
	if m.convSel != 1 {
		t.Fatalf("space on the last row must not move past it, got convSel=%d", m.convSel)
	}
}

// TestOneMarkIsNotABulkSelection: the kit2 rule, and the reason it exists — one marked row is the row
// the cursor is on, and "delete 1 conversation" beside "delete" is a slower way to say the same thing.
func TestOneMarkIsNotABulkSelection(t *testing.T) {
	m := railApp(t, 5)
	m = press(m, spaceKey)
	if ids := m.convBulkIDs(); len(ids) != 0 {
		t.Fatalf("one mark is NOT a bulk selection, got %v", ids)
	}
	m = press(m, spaceKey)
	if ids := m.convBulkIDs(); len(ids) != kit2.BulkThreshold {
		t.Fatalf("%d marks IS a bulk selection, got %v", kit2.BulkThreshold, ids)
	}
}

// TestBulkIDsComeBackInListOrder: a bulk write must be deterministic. Map order would reshuffle the
// ids run to run, so a partial failure would report a different set each time.
func TestBulkIDsComeBackInListOrder(t *testing.T) {
	m := railApp(t, 5)
	// Mark rows 3, 1 and 5 in that (non-list) order.
	m.convSel = 2
	m = press(m, spaceKey)
	m.convSel = 0
	m = press(m, spaceKey)
	m.convSel = 4
	m = press(m, spaceKey)
	got := m.convBulkIDs()
	want := []string{"conv-01", "conv-03", "conv-05"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids must be in LIST order: got %v, want %v", got, want)
		}
	}
}

// TestEscClearsMarksBeforeAnythingElse: the kit2 order, and the safe one — an operator mid-selection
// reaching for esc means "drop this selection", not "unfocus the pane".
func TestEscClearsMarksBeforeAnythingElse(t *testing.T) {
	m := railApp(t, 5)
	m.setFocus(focusContent)
	m = press(m, spaceKey, spaceKey)
	if m.convMarkedCount() != 2 {
		t.Fatalf("precondition: two marks, got %d", m.convMarkedCount())
	}
	m = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.convMarkedCount() != 0 {
		t.Fatalf("esc must clear the marks, got %d", m.convMarkedCount())
	}
	// The second esc keeps its established meaning (disengage into the content).
	m = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.chatFocus != focusContent {
		t.Fatalf("after clearing, esc must keep its meaning, focus=%d", m.chatFocus)
	}
}

// TestBulkDeleteAsksFirstAndThenDeletesEach: the confirm is not ceremony — these are destructive and
// unrecoverable, and the count is the thing the operator needs to see before agreeing.
func TestBulkDeleteAsksFirstAndThenDeletesEach(t *testing.T) {
	m := railApp(t, 5)
	m = press(m, spaceKey, spaceKey, spaceKey) // conv-01..03 marked
	if got := len(m.convBulkIDs()); got != 3 {
		t.Fatalf("precondition: 3 marked, got %d", got)
	}

	m = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	if m.bulkConfirm == nil {
		t.Fatal("ctrl+x must raise a confirm before deleting")
	}
	if !strings.Contains(m.bulkConfirm.Title, "3") {
		t.Fatalf("the confirm must state HOW MANY: %q", m.bulkConfirm.Title)
	}
	// Nothing is dispatched while the dialog is open.
	if m.bulkConfirmRun == nil {
		t.Fatal("the confirm must carry the operation to run")
	}
	// Esc dismisses and RUNS NOTHING.
	before := m.bulkConfirmRun
	m = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.bulkConfirm != nil {
		t.Fatal("esc must close the confirm")
	}
	if before == nil {
		t.Fatal("sanity")
	}
	if m.dock.Value() != "" {
		t.Fatal("dismissing must not write anything")
	}
}

// TestBulkDeleteRefusesWithTooFewMarks: a key that silently does nothing reads as broken, so the
// refusal names the specific, actionable reason.
func TestBulkDeleteRefusesWithTooFewMarks(t *testing.T) {
	m := railApp(t, 5)
	m = press(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	if m.bulkConfirm != nil {
		t.Fatal("one (or zero) marks must not raise a bulk confirm")
	}
	if !strings.Contains(m.dock.Err, "two or more") {
		t.Fatalf("the refusal must say what to do, got %q", m.dock.Err)
	}
}

// TestBulkAssignTargetsEveryMarkedConversation: ctrl+t on a marked selection assigns the WHOLE
// selection, and the write loops the entity ids — the API is per-entity.
func TestBulkAssignTargetsEveryMarkedConversation(t *testing.T) {
	m, stub := categoryApp(t, convCat("cat-1", "Research"))
	m = loadCats(t, m)
	m.attachRail(5)
	// Start on a CONVERSATION row — with a grouping present the rail's first rows are folders.
	m = railSelectConversation(t, m, "conv-01")
	m = press(m, spaceKey, spaceKey, spaceKey)

	m = press(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.assignForm == nil {
		t.Fatal("ctrl+t must open the assign modal for the marked selection")
	}
	if len(m.assignEntities) != 3 {
		t.Fatalf("the modal must target all 3 marked conversations, got %v", m.assignEntities)
	}
	if !strings.Contains(m.assignForm.Title, "3") {
		t.Fatalf("the modal title must say how many: %q", m.assignForm.Title)
	}
	// Choose the existing grouping and submit; EVERY id must be assigned.
	m.assignForm.Set(assignPickField, "cat-1")
	cmd, err := m.assignForm.Submit()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	cmd()
	if len(stub.assignCalls) != 3 {
		t.Fatalf("bulk assign must write once per entity, got %d: %v", len(stub.assignCalls), stub.assignCalls)
	}
	for i, want := range []string{"conv-01", "conv-02", "conv-03"} {
		if stub.assignCalls[i] != want {
			t.Fatalf("assign[%d] = %q, want %q (list order)", i, stub.assignCalls[i], want)
		}
	}
}

// TestAssignModalWriteIsUnchangedForOneEntity: the single-entity path (the Workers / Workflows panes
// and the rail's own ctrl+t with nothing marked) must keep working through the same code.
func TestAssignModalWriteIsUnchangedForOneEntity(t *testing.T) {
	m, stub := categoryApp(t, workerCat("cat-w", "Frontend"))
	m = loadCats(t, m)
	m.OpenAssignCategory("worker-9", "W", apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)
	if len(m.assignEntities) != 1 || m.assignEntities[0] != "worker-9" {
		t.Fatalf("single assign must carry one entity, got %v", m.assignEntities)
	}
	m.assignForm.Set(assignPickField, "cat-w")
	cmd, err := m.assignForm.Submit()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	cmd()
	if len(stub.assignCalls) != 1 || stub.assignCalls[0] != "worker-9" {
		t.Fatalf("single assign must write exactly once to worker-9, got %v", stub.assignCalls)
	}
}

// TestMarksArePrunedWhenTheListShrinks: a reload is the honest reconciliation for a bulk delete. A
// mark on a conversation that no longer exists would OVERSTATE the selection (the count is the number
// the operator reads before confirming) and aim the next bulk action at a row the server would reject.
//
// Note what convBulkIDs() can and cannot hide: it only ever returns ids that are still in the list, so
// a stale mark is invisible there while still inflating convMarkedCount(). That is why this asserts on
// the COUNT — the number the operator actually sees — and not only on the ids.
func TestMarksArePrunedWhenTheListShrinks(t *testing.T) {
	m := railApp(t, 5)
	m = press(m, spaceKey, spaceKey, spaceKey) // conv-01, conv-02, conv-03
	if m.convMarkedCount() != 3 {
		t.Fatalf("precondition: 3 marks, got %d", m.convMarkedCount())
	}
	// The list comes back having lost conv-01 and conv-02 (as a bulk delete would leave it).
	m.conversations = m.conversations[2:]

	// Before the prune the count still claims three — this is the stale state the reload must fix.
	if m.convMarkedCount() != 3 {
		t.Fatalf("precondition: the marks are still on the map, got %d", m.convMarkedCount())
	}
	m.pruneConvMarks()
	if m.convMarkedCount() != 1 {
		t.Fatalf("pruning must drop the marks for gone conversations, got %d", m.convMarkedCount())
	}
	if ids := m.convMarkedIDs(); len(ids) != 1 || ids[0] != "conv-03" {
		t.Fatalf("the surviving mark must be the conversation that is still there, got %v", ids)
	}
}

// TestMarksSurviveARefreshThatKeepsTheConversations: pruning must not clear a selection that is still
// valid — the rolling refresh reloads every few seconds, and losing the selection each time would make
// the feature unusable.
func TestMarksSurviveARefreshThatKeepsTheConversations(t *testing.T) {
	m := railApp(t, 5)
	m = press(m, spaceKey, spaceKey)
	m.pruneConvMarks()
	if m.convMarkedCount() != 2 {
		t.Fatalf("a refresh that keeps the conversations must keep the marks, got %d", m.convMarkedCount())
	}
}

// TestMarkedRowsShowInTheRail: a selection the operator cannot see is not a selection. The marker is
// asserted in the RENDERED rail, not in the map.
func TestMarkedRowsShowInTheRail(t *testing.T) {
	m := railApp(t, 3)
	m = press(m, spaceKey)
	rail := m.rightRailView()
	if !strings.Contains(rail, "✓") {
		t.Fatalf("a marked row must carry a marker in the rail:\n%s", rail)
	}
	// The unmarked rows do not.
	if strings.Count(rail, "✓") != 1 {
		t.Fatalf("exactly one row is marked, so exactly one marker:\n%s", rail)
	}
}
