package tui

// reasoning_fold.go — THE COLLAPSIBLE REASONING BLOCK.
//
// The operator: "Yes I would like to get the collapsible reasoning block but I don't want it to look fully
// like the execution page. It should still have the same 'bubbles' it has now with the user and orchicon
// chat and reasoning should look similar but have a arrow on the left to expand and collapse."
//
// So the transcript keeps its bubble shapes and the reasoning block gains an ARROW — not the execution
// pane's card-and-cursor model. See chat.renderReasoningBlock for the drawing; this file owns the state and
// the gesture.

import (
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// foldedReasoning reports whether a block is folded, for the renderer. Never nil: the renderer calls it per
// reasoning item, and a nil map read is fine but a nil FUNC would panic.
func (m *App) foldedReasoning(key string) bool {
	if key == "" {
		return false
	}
	return m.reasoningFolded[key]
}

// toggleLastReasoningBlock folds or unfolds the NEWEST reasoning block in the open conversation.
//
// WHY THE NEWEST, AND WHY THERE IS NO CURSOR. The execution pane gives its transcript a per-block cursor,
// because there the operator navigates blocks deliberately. The Ask transcript has no cursor — the arrows
// SCROLL it — and introducing one would make every arrow ambiguous between scrolling and selecting, which
// is a worse trade than the limitation. The newest block is the one a reader wants to fold: it is the
// block a live turn is growing, and the one that has just pushed the conversation off the screen.
//
// It reports whether anything changed, so the caller only repaints on a real toggle.
func (m *App) toggleLastReasoningBlock() bool {
	if m.chatConvID == "" {
		return false
	}
	// The newest reasoning block, from the same item list the renderer draws.
	items := m.chatStore.snapshot(m.chatConvID)
	key := ""
	for _, it := range items {
		if it.Kind == chat.KindReasoning && it.Key != "" {
			key = it.Key // keep overwriting: the LAST one wins
		}
	}
	if key == "" {
		return false
	}
	if m.reasoningFolded == nil {
		m.reasoningFolded = map[string]bool{}
	}
	if m.reasoningFolded[key] {
		delete(m.reasoningFolded, key)
	} else {
		m.reasoningFolded[key] = true
	}
	return true
}
