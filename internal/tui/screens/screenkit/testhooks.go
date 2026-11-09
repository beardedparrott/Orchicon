package screenkit

import (
	"context"
)

// TestSource is the cross-package test view of one source pane: its registered name, whether a
// fetch func is wired, and the current list items.
type TestSource struct {
	Name  string
	Fetch func(ctx context.Context, pageToken string) ([]Item, string, error)
	Items []Item
}

// SourcesForTest exposes the registered sources for CROSS-PACKAGE tests only.
func (b *Base) SourcesForTest() []TestSource {
	out := make([]TestSource, 0, len(b.sources))
	for _, s := range b.sources {
		out = append(out, TestSource{Name: s.name, Fetch: s.fetch, Items: s.list.Items})
	}
	return out
}
