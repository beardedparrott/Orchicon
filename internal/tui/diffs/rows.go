// Package diffs is the TUI's side-by-side diff pane. It is the TUI sibling
// of the GUI DiffSidebar: it consumes the SAME file-edit ledger (FileEdit
// RPCs) and renders it with the SAME logical layout as the GUI's
// frontend/src/lib/diff/sideBySide.ts, so the pipeline truth (Go engine),
// the GUI renderer (TS), and the TUI renderer (Go) all agree.
//
// The pure parsing/emphasis/grouping functions in this package are a
// faithful port of sideBySide.ts's parseUnifiedDiff / emphasizeTokens /
// groupByFile and are the cross-language contract. The Go tests drive them
// over the SAME internal/testfixtures/fileedit/*.json vectors that both the
// Go engine's diff_test.go and the TS sideBySide.test.ts consume.
package diffs

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// Kind is a row's visual classification (maps 1:1 to the diff marker).
type Kind string

// The four row kinds (the GUI's RowKind union).
const (
	KindAdd  Kind = "add"
	KindDel  Kind = "del"
	KindCtx  Kind = "ctx"
	KindHunk Kind = "hunk" // never emitted as a rendered row; state only
)

// EmphasisSpan is a word-level intra-line emphasis range (byte offsets
// into OldText/NewText). Mirrors the GUI's EmphasisSpan contract.
type EmphasisSpan struct {
	Start int
	End   int
	Type  string // "add" | "del"
}

// Row is one side-by-side diff row. It mirrors the GUI's SideBySideRow:
// paired line numbers (null = no line on that side; an added line has no
// old line number), the sign, the old/new text, and optional word-level
// emphasis spans.
//
// HasOld/HasNew represent the "null line number" cases (Go has no null int;
// a zero line number would be ambiguous with line 1).
type Row struct {
	LineNoOld int
	LineNoNew int
	HasOld    bool
	HasNew    bool
	Sign      string // "+" | "-" | " " | ""
	OldText   string
	NewText   string
	Kind      Kind
	OldSpans  []EmphasisSpan
	NewSpans  []EmphasisSpan
}

// FileGroup is one path's grouped edit history, mirroring the GUI's
// FileGroup. Adds/Dels are tallied from the LATEST edit's unified diff.
type FileGroup struct {
	Path     string
	Kind     string
	Edits    []*apiv1.FileEdit
	Adds     int
	Dels     int
	LastTool string
	LastAt   int64
}
