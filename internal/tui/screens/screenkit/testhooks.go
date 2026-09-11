package screenkit

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSource is the cross-package test view of one source pane: its
// registered name, whether a fetch func is wired, and the current list
// items. It exists so tests in OTHER packages (e.g. control's populate
// test) can assert source registration without reaching into the
// unexported source struct.
type TestSource struct {
	Name  string
	Fetch func(ctx context.Context, pageToken string) ([]Item, string, error)
	Items []Item
}

// SourcesForTest exposes the registered sources for CROSS-PACKAGE tests
// only. This lives in a non-test file because Go excludes *_test.go
// symbols from other packages' test builds; the same-package test helpers
// (SourceListForTest, loadCmdsForTest) stay in base_testhelpers_test.go.
//
// Not part of the production API: no non-test code may call this.
func (b *Base) SourcesForTest() []TestSource {
	out := make([]TestSource, 0, len(b.sources))
	for _, s := range b.sources {
		out = append(out, TestSource{Name: s.name, Fetch: s.fetch, Items: s.list.Items})
	}
	return out
}

// FetchedMsgForTest builds the exact message a source fetch produces, so a
// screen's CROSS-PACKAGE test can drive the fetched→list wiring (the
// message type is unexported; a test must never re-implement the state
// machine). Feed the result to the screen's Update.
//
// Not part of the production API.
func FetchedMsgForTest(src string, items []Item, next string, err error) tea.Msg {
	return fetchedMsg{src: src, items: items, next: next, err: err}
}
