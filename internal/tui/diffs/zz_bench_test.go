package diffs

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// A diff the size a real session produces: 400 hunks.
func bigDiffModel(b *testing.B) *Model {
	var sb strings.Builder
	sb.WriteString("--- a/src/main.go\n+++ b/src/main.go\n")
	for i := 0; i < 400; i++ {
		sb.WriteString("@@ -1,2 +1,3 @@\n")
		sb.WriteString("-old line " + strings.Repeat("x", 20) + "\n")
		sb.WriteString("+new line " + strings.Repeat("y", 20) + "\n")
		sb.WriteString(" context\n")
	}
	m := NewModel(nil, nil)
	m.Width, m.Height = 80, 40
	m.groups = GroupByFile([]*apiv1.FileEdit{{Id: "1", Path: "src/main.go", Kind: "modify", Tool: "edit", Seq: 1, UnifiedDiff: sb.String()}})
	m.SelectedPath = "src/main.go"
	m.setRows(m.rowsForSelected())
	m.Tab = TabDiff
	return m
}

// A scroll step is clamp + body render — what a single mouse-motion event costs.
func BenchmarkScrollStep(b *testing.B) {
	m := bigDiffModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Scroll(1)
		_ = m.diffBody()
	}
}
