package chat

// truncation_utf8_test.go — the transcript never emits a HALF-DECODED rune.
//
// THE DEFECT THIS PINS, twice over, both found by reading the mode-refused PTY capture and seeing `??`
// where an ellipsis belonged:
//
//  1. `firstLine` clipped with `s[:117]` — a BYTE offset. Any multi-byte glyph straddling byte 117 was cut
//     in half and printed as U+FFFD, which an operator reads as corruption in the tool row.
//  2. `wrapText`'s hard-split for a too-long word (a URL, a path, a command) did `line[:w]`, also bytes,
//     so the same cut could land mid-glyph — and it counted BYTES against a CELL budget, so a row carrying
//     the `⚙ ` prefix overflowed the pane by a column per wide glyph.
//
// A rune is not a cell and a byte is neither. These tests feed the exact agy strings that broke and assert
// on both properties: valid UTF-8 out, and never wider than the pane.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// agyToolRowInputs are the recorded argument payloads from the parity fixture, whose subjects carry the
// `…`-adjacent multibyte risks (a long test path, a URL) that a byte offset splits.
func agyToolRowInputs() []*ParsedTool {
	return []*ParsedTool{
		{ID: "tc-1", ToolName: "run_command",
			Input:  `{"CommandLine":"go test ./internal/tui/... -run Antigravity","Cwd":"/home/op/project"}`,
			Output: "ok  internal/tui/modelpick  0.006s"},
		{ID: "tc-3", ToolName: "read_url_content",
			Input:  `{"Url":"https://antigravity.google/download"}`,
			Output: "Download Antigravity (200)"},
		{ID: "tc-4", ToolName: "write_file",
			Input: `{"TargetFile":"/home/op/project/probe.txt","CodeContent":"ok\n"}`,
			Output: "REFUSED BY THE PLATFORM: write is not available in brainstorm mode, so it was NOT " +
				"executed. Brainstorm is for thinking and planning — it does not touch the filesystem.",
			Failed: true},
	}
}

// TestToolRowNeverEmitsAPartialRune sweeps every width a pane can plausibly take, because the bug only
// appeared at the widths where the cut happened to land inside a glyph.
func TestToolRowNeverEmitsAPartialRune(t *testing.T) {
	for _, tool := range agyToolRowInputs() {
		for w := 20; w <= 200; w++ {
			out := renderToolRow(tool, w)
			if !utf8.ValidString(out) {
				t.Fatalf("renderToolRow(%s, w=%d) emitted invalid UTF-8 — a byte-sliced rune reaches the "+
					"operator as U+FFFD, which reads as corruption:\n%q", tool.ToolName, w, out)
			}
			if strings.ContainsRune(out, utf8.RuneError) {
				t.Fatalf("renderToolRow(%s, w=%d) emitted U+FFFD:\n%q", tool.ToolName, w, out)
			}
		}
	}
}

// TestToolRowFitsItsPaneInCells is the other half: a row that is VALID but too wide is silently clipped by
// the pane, which is how the refusal body lost its last words.
func TestToolRowFitsItsPaneInCells(t *testing.T) {
	for _, tool := range agyToolRowInputs() {
		for w := 40; w <= 160; w++ {
			out := renderToolRow(tool, w)
			for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				if line == "" {
					continue
				}
				// THE HEADER IS TRUNCATED TO THE PANE; the refusal body WRAPS, so it is bounded by w too.
				// One cell of slack absorbs the trailing reset a style leaves behind.
				if got := lipgloss.Width(line); got > w+1 {
					t.Fatalf("renderToolRow(%s, w=%d) painted a %d-cell line into a %d-cell pane:\n%q",
						tool.ToolName, w, got, w, line)
				}
			}
		}
	}
}

// TestFirstLineClipsOnARuneBoundary pins the specific helper: 117 had to become a rune index.
func TestFirstLineClipsOnARuneBoundary(t *testing.T) {
	// 118 ASCII runes of prefix, then a run of 3-byte glyphs that a BYTE cut at 117 would land inside of
	// only if the prefix itself were multibyte — so the prefix carries them too.
	long := strings.Repeat("é", 200) // 2 bytes each: byte 117 is the SECOND byte of rune 58
	got := firstLine(long)
	if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("firstLine cut a rune in half: %q", got)
	}
	if r := []rune(got); len(r) != 118 { // 117 runes + the ellipsis
		t.Fatalf("firstLine returned %d runes, want 118 (117 + ellipsis): %q", len(r), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a clipped line must say it was clipped: %q", got)
	}
}

// TestWrapTextHardSplitIsRuneAndCellSafe covers the WORD-split path, which `renderToolRow` reaches only
// through the refusal body: a long URL or command has no spaces, so it takes the hard-split branch.
func TestWrapTextHardSplitIsRuneAndCellSafe(t *testing.T) {
	for w := 8; w <= 60; w++ {
		// A single unbroken run of 3-byte glyphs: every byte offset inside it is invalid, and every rune is
		// one cell, so a byte-counting width overflows and a byte-slicing split corrupts.
		got := wrapText(strings.Repeat("→", 120), w)
		if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("wrapText(w=%d) emitted invalid UTF-8:\n%q", w, got)
		}
		for _, line := range strings.Split(got, "\n") {
			if lipgloss.Width(line) > w {
				t.Fatalf("wrapText(w=%d) painted a %d-cell line: %q", w, lipgloss.Width(line), line)
			}
		}
	}
}
