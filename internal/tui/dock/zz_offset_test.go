package dock

import (
	"strings"
	"testing"
)

func TestZZOffsetReconstruct(t *testing.T) {
	m := New()
	m.Width = 80
	m.Focus()
	m.SetValue(strings.Repeat("alpha beta gamma delta epsilon\n", 40))
	m.Height = 40
	t.Logf("InputRows=%d ta.Width=%d", m.InputRows(), m.ta.Width())
	rows := m.caretRows()
	t.Logf("caretRows=%d", len(rows))
	li := m.ta.LineInfo()
	t.Logf("ta.Line()=%d RowOffset=%d Height=%d", m.ta.Line(), li.RowOffset, li.Height)
	// Reconstruct cursorRow from public API.
	lines := strings.Split(m.Value(), "\n")
	cursorRow := 0
	for i := 0; i < m.ta.Line(); i++ {
		cursorRow += len(m.wrapLinePublic(lines[i]))
	}
	cursorRow += li.RowOffset
	t.Logf("reconstructed cursorRow=%d", cursorRow)
}

// wrapLinePublic mirrors what memoizedWrap must produce for a plain line.
func (m *Model) wrapLinePublic(s string) []string {
	w := m.ta.Width()
	var out []string
	for len(s) > w {
		out = append(out, s[:w])
		s = s[w:]
	}
	out = append(out, s)
	return out
}
