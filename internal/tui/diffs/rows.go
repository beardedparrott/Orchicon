// Package diffs is the TUI's side-by-side diff pane.
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

// Row is one side-by-side diff row.
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
