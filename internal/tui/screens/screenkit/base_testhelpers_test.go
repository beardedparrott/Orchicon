package screenkit

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Same-package test helpers. These are test-only (the file ends in
// _test.go), so they never ship in bin/orch. The CROSS-package hook
// (SourcesForTest) lives in testhooks.go, because Go excludes *_test.go
// symbols from other packages' test builds.

// SourceListForTest exposes one source's list state for tests (the
// source struct + List are unexported; the fetchedMsg→list wiring is
// exactly what the opacity-of-data tests pin).
func (b *Base) SourceListForTest(name string) *List {
	for _, s := range b.sources {
		if s.name == name {
			return &s.list
		}
	}
	return nil
}

// loadCmdsForTest returns the per-source load commands WITHOUT the
// tea.Batch wrapper (Load returns one merged Cmd; this returns the
// slice so tests can execute each fetch and feed its message back
// through Update).
func (b *Base) loadCmdsForTest() []tea.Cmd {
	var cmds []tea.Cmd
	for i := range b.sources {
		cmds = append(cmds, b.loadSource(i, ""))
	}
	return cmds
}
