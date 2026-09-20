package tui

// categories_hooks.go — the App side of the screens' categorize chord.
//
// The screens own their selection; the shell owns the category cache and the modal. This is the one
// method they call, and it is deliberately the ONLY thing they need to know about categories: the
// entity id, a label for the modal's title, and which kind of grouping applies.

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// OpenAssignCategory is the shell hook the Workers and Workflows panes call (via the optional
// interface in execution/categories.go) when the operator presses `C`.
//
// It is exported on purpose — it is part of the shell's contract with its screens, alongside
// DockError / DockNotice, not an internal detail.
func (m *App) OpenAssignCategory(entityID, entityLabel string, target apiv1.CategoryTargetType) {
	_ = entityLabel // the modal's title names the TARGET TYPE, which is what the picker is about
	m.openAssignCategory(entityID, entityLabel, target)
}

// OpenRenameCategory / OpenDeleteCategory are the shell's MANAGE hooks, called by a pane whose cursor
// is on a category row. They are the TUI equivalent of the GUI's per-folder rename and delete
// (CategoryFolder's onRename / onDelete), which is what lets the same two actions be reachable from
// every grouped list instead of from one special section.
func (m *App) OpenRenameCategory(categoryID string) { m.openRenameCategory(categoryID) }

// OpenDeleteCategory opens the delete confirm for a grouping.
func (m *App) OpenDeleteCategory(categoryID string) { m.openDeleteCategory(categoryID) }

// conversationCategorizeChord and conversationRenameChord are named here so the rail's footer and the
// help overlay cannot drift from the bindings — the same reason the tab chords are derived rather than
// spelled out (row 288: the chords were written down in four places).
const (
	conversationRenameChord     = "ctrl+n"
	conversationCategorizeChord = "ctrl+t"
)

// categoryRenameChord / categoryDeleteChord are the chords a CATEGORY ROW answers, on any surface that
// shows one. They are the surface's OWN item keys — `e` edits what the cursor is on, and the shared
// delete chord deletes it — re-pointed by whether that is an item or a grouping, which is how the GUI
// reads too (the folder row carries the rename and delete affordances itself).
//
// categoryDeleteChord is `ctrl+x`, the SAME chord the Workers and Workflows panes use for every delete
// (single, bulk, and a folder). The execution package names that chord itself because it cannot import
// the shell — so a test asserts the two agree rather than trusting them to (`TestDeleteChordMatchesThePanes`),
// which is the only way this particular drift can be caught.
const (
	categoryRenameChord = "e"
	categoryDeleteChord = "ctrl+x"
)
