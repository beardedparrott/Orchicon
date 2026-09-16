package execution

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestZZProbeLongLineWidth: does the execution transcript wrap a long prose line to the pane width,
// or does the detail viewport truncate it?
func TestZZProbeLongLineWidth(t *testing.T) {
	long := strings.Repeat("alpha bravo charlie delta echo foxtrot golf hotel india juliet ", 6)
	items := []chat.ChatItem{{Kind: chat.KindText, Text: long, Key: "m1"}}
	for _, w := range []int{60, 100} {
		blocks := blocksFromItems(items, w)
		body, _ := renderBlocks(blocks, &blockState{}, w, transcriptCursor{idx: -1})
		max := 0
		for _, l := range strings.Split(body, "\n") {
			if n := len([]rune(l)); n > max {
				max = n
			}
		}
		t.Logf("width=%d  longest rendered line=%d  (overflow=%d)", w, max, max-w)
	}
}
