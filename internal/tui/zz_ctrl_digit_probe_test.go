package tui

// zz_ctrl_digit_probe_test.go — TEMPORARY PROBE. What does a terminal's ctrl+<digit> actually
// deliver? Asked before binding tab chords to ctrl+1..ctrl+7, because bubbletea's byte→key table maps
// the control bytes to ctrl+LETTERS, and a wrong binding would collide with esc and backspace.

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// probeKeys feeds raw bytes to a real bubbletea input reader and reports the KeyMsg names it
// produces. This is the same parser the running program uses.
func probeKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	pr, pw := io.Pipe()
	var got []string
	seen := make(chan struct{})
	done := make(chan struct{})

	m := probeModel{onKey: func(k tea.KeyMsg) {
		got = append(got, k.String())
		select {
		case <-seen:
		default:
			close(seen)
		}
	}}
	p := tea.NewProgram(m,
		tea.WithInput(pr),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)
	go func() { _, _ = p.Run() }()
	go func() {
		defer close(done)
		time.Sleep(150 * time.Millisecond)
		_, _ = pw.Write(raw)
		select {
		case <-seen:
		case <-time.After(400 * time.Millisecond):
		}
		p.Quit()
	}()
	<-done
	time.Sleep(80 * time.Millisecond)
	return got
}

type probeModel struct{ onKey func(tea.KeyMsg) }

func (m probeModel) Init() tea.Cmd { return nil }
func (m probeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && m.onKey != nil {
		m.onKey(k)
	}
	return m, nil
}
func (m probeModel) View() string { return "" }

// The probe: what each byte a terminal sends for ctrl+1..ctrl+9 produces, plus the two bytes that
// would be catastrophic to hijack (esc and backspace).
func TestProbeCtrlDigitBytes(t *testing.T) {
	cases := []struct {
		label string
		raw   []byte
	}{
		{"ctrl+1 byte 0x11", []byte{0x11}},
		{"ctrl+2 byte 0x00", []byte{0x00}},
		{"ctrl+3 byte 0x1b", []byte{0x1b}},
		{"ctrl+4 byte 0x1c", []byte{0x1c}},
		{"ctrl+5 byte 0x1d", []byte{0x1d}},
		{"ctrl+6 byte 0x1e", []byte{0x1e}},
		{"ctrl+7 byte 0x1f", []byte{0x1f}},
		{"ctrl+8 byte 0x7f", []byte{0x7f}},
		{"xterm modifyOtherKeys: ctrl+1 = ESC[27;5;49~", []byte("\x1b[27;5;49~")},
		{"kitty CSI-u: ctrl+1 = ESC[49;5u", []byte("\x1b[49;5u")},
	}
	for _, c := range cases {
		got := probeKeys(t, c.raw)
		t.Logf("%-46s -> %v", c.label, got)
	}
	// And what the tab chords use TODAY must be unaffected.
	for _, p := range []struct {
		label string
		raw   []byte
	}{{"ctrl+o (0x0f)", []byte{0x0f}}, {"ctrl+t (0x14)", []byte{0x14}}} {
		got := probeKeys(t, p.raw)
		t.Logf("%-46s -> %v", p.label, got)
	}
	// THE ALTERNATIVE: alt+<digit> is ESC-prefixed, which legacy terminals DO send distinctly.
	for i := 1; i <= 7; i++ {
		raw := []byte{0x1b, byte('0' + i)}
		got := probeKeys(t, raw)
		t.Logf("%-46s -> %v", "alt+"+string(rune('0'+i))+" (ESC digit)", got)
	}
}

// Sanity: the probe harness itself works — a plain letter is delivered.
func TestProbeHarnessDeliversLetters(t *testing.T) {
	got := probeKeys(t, []byte("a"))
	if len(got) == 0 || !strings.Contains(strings.Join(got, ","), "a") {
		t.Fatalf("the probe harness delivered %v for \"a\" — the harness is broken, so its other results prove nothing", got)
	}
}
