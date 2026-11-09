package tui

// reasoning_fold.go — THE COLLAPSIBLE REASONING BLOCK.

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
