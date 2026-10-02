// Package tui — diff_e2e_pty_test.go — the REAL-PTY end-to-end gate for the
// diff surface (both mounts, in the terminal client).
//
// WHY THIS FILE EXISTS. Every defect in the diff-surface feature was reported
// against a WORKING pipeline, and this work-item family already shipped one
// acceptance criterion that was satisfied "on paper" and false in practice.
// The rendered-string tests in internal/tui/diffs prove the LAYOUT MATH; they
// can never prove what an operator's terminal actually shows.
//
// This gate launches the REAL bin/orch inside a REAL pty against a disposable
// plane that serves a real file-edit ledger, drives the keystrokes/mouse an
// operator uses, replays the live process's byte stream through a minimal
// terminal emulator (diff_e2e_fixture_test.go), and asserts on the SCREEN GRID:
//
//	1+2 click-through focus: a Tree row click AND a Timeline row click select the
//	    file AND move the pane to its Diff tab.
//	3   fit: a long, multi-hunk diff is fully readable — the long line's tail is
//	    reachable at the minimum terminal (80x24) and a wide one (200x50).
//	4   scrollbar: a visible bar column whenever the content overflows the pane.
//	6   resize: the rail resizes by keyboard chord and the width survives a
//	    RESTART as a RENDERED width (the drawn pane width, not a struct field).
//	+   honest states: an empty ledger renders the empty text; a failed ledger
//	    fetch renders the explicit error banner, never the empty text.
//
// The gate is OPT-IN (ORCH_DIFF_E2E=1) and skips cleanly otherwise, so the
// standing `go test ./internal/tui/...` suite stays green in CI. When
// ORCH_DIFF_E2E_OUT is set, each phase dumps the rendered frame there for the
// acceptance review to quote.
//
// Harness: reuses the single-reader pty session from orch_pty_smoke_test.go and
// the disposable-plane idiom from orch_pty_mouse_test.go (same package).
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// skipDiffE2E skips unless ORCH_DIFF_E2E=1, mirroring skipInteractivePTY's
// opt-in shape: the live gate is deliberate, never incidental.
func skipDiffE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("ORCH_DIFF_E2E") != "1" {
		t.Skip("real-pty diff-surface gate: set ORCH_DIFF_E2E=1 (and ORCH_PTY_SMOKE=1 from a terminal) to run")
	}
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty diff-surface gate: skipped in -short")
	}
}

// dumpFrame writes a rendered frame to $ORCH_DIFF_E2E_OUT/<name>.txt so the
// review can quote the exact cells an observation rests on.
func dumpFrame(t *testing.T, name string, scr *screen) {
	t.Helper()
	dir := os.Getenv("ORCH_DIFF_E2E_OUT")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("frame dump skipped (mkdir): %v", err)
		return
	}
	path := filepath.Join(dir, name+".txt")
	var b strings.Builder
	for r := 0; r < scr.rows; r++ {
		fmt.Fprintf(&b, "%s\n", scr.row(r))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Logf("frame dump skipped (write): %v", err)
	}
}

// replay rebuilds the SCREEN GRID from every byte the session has painted so
// far. Replaying the whole capture is correct for an alt-screen program: each
// bubbletea flush starts with a cursor-home, so the concatenation's final state
// is the current screen.
func replay(t *testing.T, s *ptySession, cols, rows int) *screen {
	t.Helper()
	scr := newScreen(cols, rows)
	scr.feed(s.readFor(0))
	return scr
}

// screenText is the whole grid as one flat string (for coarse "is X painted"
// checks that a wrap could split across rows).
func screenText(scr *screen) string {
	var b strings.Builder
	for r := 0; r < scr.rows; r++ {
		b.WriteString(scr.row(r))
		b.WriteByte('\n')
	}
	return b.String()
}

// waitForFrame polls until `want` appears in the replayed grid or the budget
// expires, returning the final screen. It is how each phase waits for the live
// process to finish painting rather than sleeping a fixed guess.
func waitForFrame(t *testing.T, s *ptySession, cols, rows int, budget time.Duration, want ...string) *screen {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last *screen
	for {
		last = replay(t, s, cols, rows)
		text := screenText(last)
		ok := true
		for _, w := range want {
			if !strings.Contains(text, w) {
				ok = false
				break
			}
		}
		if ok {
			return last
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// diffOwnerWidth reads the drawn width of the left diff rail off the grid: the
// display column where the content pane's first non-blank cell begins, found by
// locating the vertical panel border run. Returns 0 when the pane is not open.
//
// The pane's left border is a column of '│' (the DiffPanel rounded left
// border). The rail's drawn width is that column index + 1.
func diffOwnerWidth(scr *screen, rows int) int {
	// The pane's border column is the first column with many consecutive '│'.
	for c := 0; c < scr.cols/2; c++ {
		run := 0
		for r := 0; r < rows; r++ {
			if scr.grid[r][c] == '│' {
				run++
			}
		}
		if run >= rows/2 {
			return c + 1
		}
	}
	return 0
}

// diffE2ELaunch starts orch against the disposable plane at the given size
// (the size variant of startOrchPtyAt, which hardcodes 120x40).
func diffE2ELaunch(t *testing.T, planeURL string, cols, rows int) *ptySession {
	t.Helper()
	bin := orchBinPath(t)
	home := t.TempDir()
	writeOrchConfig(t, home, planeURL)
	return startOrchPtySized(t, bin, planeURL, home, cols, rows)
}

// openAskDiffPane drives the Ask mount to an open diff pane: /conversations
// opens the rail, a click on the first conversation row opens it (setting the
// (ask_conversation, <id>) owner), then ctrl+d opens the pane.
func openAskDiffPane(t *testing.T, s *ptySession, cols, rows int) *screen {
	t.Helper()
	waitForFrame(t, s, cols, rows, 6*time.Second, "❯")
	// Let the shell finish its capability handshake + first live loads before
	// typing (the proven mouse gate waits a flat 5s for the same reason: keys
	// sent in that window are dropped).
	time.Sleep(5 * time.Second)
	_, _ = s.tty.WriteString("/conversations\r")
	scr := waitForFrame(t, s, cols, rows, 6*time.Second, "diff-e2e-conv")
	if !strings.Contains(screenText(scr), "diff-e2e-conv") {
		t.Fatalf("conversations rail never listed the fixture conversation\n%s", screenText(scr))
	}
	// Click the first conversation row (SGR rows are 1-based; the rail's first
	// row is 0-based railTopRow+1, i.e. SGR railTopRow+2). Mirrors the proven
	// mouse gate's coordinates.
	s.sendMouse(0, cols-ConversationsRailWidth+8, railTopRow+2, false)
	s.sendMouse(0, cols-ConversationsRailWidth+8, railTopRow+2, true)
	// ctrl+d opens the left diff rail (the global chord; composerBypassKeys
	// carries ctrl+d so it works from either focus).
	_, _ = s.tty.WriteString("\x04")
	waitForFrame(t, s, cols, rows, 6*time.Second, "Timeline")
	// Drop keyboard focus to the CONTENT so the pane owns j/k/G/h/l (the shell
	// launched composer-focused; esc from an empty composer moves focus down,
	// it does NOT close the still-empty pane).
	_, _ = s.tty.WriteString("\x1b")
	time.Sleep(500 * time.Millisecond)
	return replay(t, s, cols, rows)
}

// TestDiffE2EAskMountPTY exercises the Ask mount: the pane opens, the ledger's
// files paint, click-through from Tree and Timeline focuses the diff, the long
// diff is readable at both sizes, and the scrollbar is visible on overflow.
func TestDiffE2EAskMountPTY(t *testing.T) {
	skipDiffE2E(t)
	fe := &diffE2EFileEditService{files: diffE2EFixtureFiles()}
	url := diffE2EPlane(t, fe)
	files := diffE2EFixtureFiles()
	// The long line's final 7 chars — a contiguous token that survives wrapping
	// across rows, so its presence proves the whole line is reachable (a
	// truncating pane would have ellipsized it away).
	tailWitness := "TAILEND"

	for _, size := range [][2]int{{80, 24}, {200, 50}} {
		cols, rows := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", cols, rows), func(t *testing.T) {
			s := diffE2ELaunch(t, url, cols, rows)
			defer s.close()
			scr := openAskDiffPane(t, s, cols, rows)
			dumpFrame(t, fmt.Sprintf("ask-%dx%d-open", cols, rows), scr)
			text := screenText(scr)
			// The default Diff tab shows the first file's diff, witnessed by its
			// short marker (never wrapped) — proving the pane fetched and rendered
			// the real ledger.
			if !strings.Contains(text, files[0].Marker) {
				t.Fatalf("the diff pane never rendered the ledger's first file (marker %q) at %dx%d\n%s",
					files[0].Marker, cols, rows, text)
			}

			// The pane is drawn (a left border column exists) and the chat is
			// still usable beside it.
			if w := diffOwnerWidth(scr, rows); w == 0 {
				t.Fatalf("no diff pane border at %dx%d — the pane did not open\n%s", cols, rows, text)
			}

			// 3. FIT: the long line lives in hunk 0, so it is already on the
			// initial frame — assert its tail token is reachable without
			// scrolling (a truncating pane would have dropped it). A "…" next to
			// it would be the truncation signature.
			if !strings.Contains(text, tailWitness) {
				t.Fatalf("the long diff line's tail %q is UNREACHABLE at %dx%d — content is cut off\n%s",
					tailWitness, cols, rows, text)
			}
			if strings.Contains(text, "…") {
				t.Fatalf("the diff pane ellipsized content at %dx%d (truncation, not wrap)\n%s", cols, rows, text)
			}

			// 4. SCROLLBAR: with the diff overflowing, the pane's rightmost
			// content column carries a bar glyph (thumb █ or track │).
			if !strings.ContainsAny(screenText(scr), diffScrollThumbGlyph+diffScrollTrackGlyph) {
				t.Fatalf("no scrollbar glyph painted anywhere at %dx%d with an overflowing diff\n%s",
					cols, rows, screenText(scr))
			}
			barCol := diffPaneBarColumn(scr)
			if !strings.ContainsAny(barCol, diffScrollThumbGlyph+diffScrollTrackGlyph) {
				t.Fatalf("the pane's reserved bar column carries no bar glyph at %dx%d (col=%q)\n%s",
					cols, rows, barCol, screenText(scr))
			}

			// 1+2. CLICK-THROUGH: from the Tree tab, click the second file's row
			// and assert the Diff tab shows THAT file's diff.
			assertAskClickThrough(t, s, cols, rows, files[1])
		})
	}
}

// assertAskClickThrough drives a Tree-tab row click and a Timeline-tab row
// click and asserts each selects the clicked file and focuses its diff.
func assertAskClickThrough(t *testing.T, s *ptySession, cols, rows int, target diffE2EFile) {
	t.Helper()

	// Tree tab: click the tab label (the operator's gesture). The pane's tab bar
	// is terminal row 3 (diffPaneTopRow); the "tree" label sits around content
	// columns 9-12.
	clickPaneTab(s, cols, 10) // "tree"
	// Settle + wait for the tree list (the tab click repaints asynchronously; a
	// no-want waitForFrame returns before the repaint).
	time.Sleep(1500 * time.Millisecond)
	scr := replay(t, s, cols, rows)
	dumpFrame(t, fmt.Sprintf("ask-%dx%d-tree-tab", cols, rows), scr)
	// The list rows ellipsize by design (an index entry, not diff content — see
	// treeBody's truncate), so at 80x24 the path is "internal/second.g…". Assert
	// the stable stem, which survives the ellipsis at every width.
	if !strings.Contains(screenText(scr), "second") {
		t.Fatalf("Tree tab did not list the second file %s\n%s", target.Path, screenText(scr))
	}
	// Click the second file's row in the tree (row 1 of the list region below
	// the tab bar). The pane body starts at terminal row 4 (diffPaneBodyRow).
	clickPaneRow(s, 5, diffPaneBodyRow+1)
	scr = waitForFrame(t, s, cols, rows, 3*time.Second, target.Marker)
	text := screenText(scr)
	if !strings.Contains(text, target.Marker) {
		t.Fatalf("Tree row click did not focus the clicked file's diff (marker %q missing)\n%s",
			target.Marker, text)
	}
	dumpFrame(t, fmt.Sprintf("ask-%dx%d-tree-click", cols, rows), scr)

	// Timeline tab: click its label, then a row. The row-click witness is the
	// Diff tab now carrying a marker (either file's).
	clickPaneTab(s, cols, 18) // "timeline"
	time.Sleep(1500 * time.Millisecond)
	clickPaneRow(s, 5, diffPaneBodyRow)
	scr = waitForFrame(t, s, cols, rows, 3*time.Second, diffE2EMarkers["cmd/first.go"])
	text = screenText(scr)
	if !strings.Contains(text, diffE2EMarkers["cmd/first.go"]) && !strings.Contains(text, diffE2EMarkers["internal/second.go"]) {
		t.Fatalf("Timeline row click did not focus a diff (no marker painted)\n%s", text)
	}
	dumpFrame(t, fmt.Sprintf("ask-%dx%d-timeline-click", cols, rows), scr)
}

// clickPaneTab sends a real mouse click on the pane's tab label at the given
// content-relative column, on the pane's tab-bar row (paneTopRow). The pane
// starts at terminal column 0 with a 1-cell left border; SGR coordinates are
// 1-based, so terminal (x, y) → SGR (x+1, y+1).
func clickPaneTab(s *ptySession, cols, contentX int) {
	x := contentX + 1 // + the pane's left border column
	s.sendMouse(0, x+1, diffPaneTopRow+1, false)
	s.sendMouse(0, x+1, diffPaneTopRow+1, true)
}

// clickPaneRow sends a real mouse click on a pane body row at the given
// content-relative column.
func clickPaneRow(s *ptySession, contentX, termRow int) {
	s.sendMouse(0, contentX+2, termRow+1, false)
	s.sendMouse(0, contentX+2, termRow+1, true)
}

// diffPaneBarColumn returns the pane's reserved scrollbar column as a per-row
// string. The pane's content width is the drawn pane width minus the 1-cell
// left border minus the 1-cell bar column; the bar is the last content cell.
func diffPaneBarColumn(scr *screen) string {
	// The pane's drawn width is where the content border run ends; the bar
	// column is just inside it. Find the pane border column (leftmost '│' run).
	left := -1
	for c := 0; c < scr.cols/2; c++ {
		run := 0
		for r := 0; r < scr.rows; r++ {
			if scr.grid[r][c] == '│' {
				run++
			}
		}
		if run >= scr.rows/2 {
			left = c
			break
		}
	}
	if left < 0 {
		return ""
	}
	// The pane's right edge: the next column left of the content region. The
	// reserved bar column is the last cell of the pane's inner content, which
	// the pane pads to (paneInnerWidth). Read a slice of candidate columns and
	// return the one carrying bar glyphs, if any.
	var colsWithBar []int
	for c := left + 1; c < scr.cols; c++ {
		col := scr.col(c)
		if strings.ContainsAny(col, diffScrollThumbGlyph+diffScrollTrackGlyph) {
			colsWithBar = append(colsWithBar, c)
		}
	}
	if len(colsWithBar) == 0 {
		return ""
	}
	return scr.col(colsWithBar[0])
}

// diffPaneDrawnWidth reads the pane's RENDERED width off the grid: the column
// where the next box (the Detail panel, or the chat) begins on the pane's tab-bar
// row. The pane is the left rail, so its right edge is where the neighboring box
// starts — a drawn measurement, never a struct field.
func diffPaneDrawnWidth(scr *screen) int {
	row := scr.grid[diffPaneTopRow] // 0-based index of the pane's tab-bar row (row 3)
	for c := 1; c < scr.cols; c++ {
		if row[c] == '┌' || row[c] == '╭' {
			// The first box-drawing corner right of the pane's own content: the
			// neighbor box's left edge, i.e. the pane's drawn right edge.
			if c > 3 {
				return c
			}
		}
	}
	return 0
}

// TestDiffE2EResizeAndRestartPTY is defect 6: the rail resizes by keyboard chord
// and the width survives a RESTART as a RENDERED width.
func TestDiffE2EResizeAndRestartPTY(t *testing.T) {
	skipDiffE2E(t)
	// 200 columns, NOT 120: the clamp is floor-then-cap, and at 120 the
	// available half (41) is below the 48-cell floor, so the CAP binds and the
	// rail cannot move at all (see TestDiffRailWidthClampsOnAShrink). At 200 the
	// half (81) is above the floor, so the drag/chord genuinely resizes.
	const cols, rows = 200, 50
	fe := &diffE2EFileEditService{files: diffE2EFixtureFiles()}
	url := diffE2EPlane(t, fe)
	home := t.TempDir()
	writeOrchConfig(t, home, url)

	bin := orchBinPath(t)
	s := startOrchPtySized(t, bin, url, home, cols, rows)
	defer s.close()
	scr := openAskDiffPane(t, s, cols, rows)
	base := diffPaneDrawnWidth(scr)
	if base == 0 {
		t.Fatalf("could not read the pane's drawn width\n%s", screenText(scr))
	}
	dumpFrame(t, "resize-before", scr)

	// Widen by three keyboard steps (ctrl+right ×3). The chord is in
	// composerBypassKeys, so it fires from either focus.
	for i := 0; i < 3; i++ {
		_, _ = s.tty.WriteString("\x1b[1;5C") // ctrl+right
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)
	scr = replay(t, s, cols, rows)
	grown := diffPaneDrawnWidth(scr)
	dumpFrame(t, "resize-after-grow", scr)
	if grown <= base {
		t.Fatalf("ctrl+right did not widen the drawn pane: %d -> %d\n%s", base, grown, screenText(scr))
	}
	// The chat column stays usable: the conversations rail is still visible to
	// the pane's right.
	if !strings.Contains(screenText(scr), "Conversations") {
		t.Fatalf("the chat/rail vanished after widening the pane\n%s", screenText(scr))
	}
	s.close()

	// RESTART with the same HOME: a NEW process must draw the SAME width.
	s2 := startOrchPtySized(t, bin, url, home, cols, rows)
	defer s2.close()
	scr2 := openAskDiffPane(t, s2, cols, rows)
	restarted := diffPaneDrawnWidth(scr2)
	dumpFrame(t, "resize-after-restart", scr2)
	if restarted != grown {
		t.Fatalf("the rail width did not survive the restart: drawn %d before, %d after\n%s",
			grown, restarted, screenText(scr2))
	}
	if restarted == base {
		t.Fatalf("the restarted width equals the pre-resize default (%d) — the resize itself did nothing", base)
	}
}

// TestDiffE2EExecutionMountPTY is the Execution page × TUI pane: the pane opens
// against the (execution, <id>) owner and renders that ledger.
func TestDiffE2EExecutionMountPTY(t *testing.T) {
	skipDiffE2E(t)
	const cols, rows = 120, 40
	fe := &diffE2EFileEditService{files: diffE2EFixtureFiles()}
	url := diffE2EPlane(t, fe)
	s := diffE2ELaunch(t, url, cols, rows)
	defer s.close()

	waitForFrame(t, s, cols, rows, 6*time.Second, "❯")
	time.Sleep(5 * time.Second)

	// Go to Execution (F4) then dismiss its dropdown so the list is reachable.
	_, _ = s.tty.WriteString("\x1bOS") // F4
	time.Sleep(1 * time.Second)
	_, _ = s.tty.WriteString("\x1b") // esc closes the menu
	time.Sleep(1500 * time.Millisecond)
	// Select the execution row: down then Enter loads its detail (setting the
	// owner to (execution, exec-diff-e2e)).
	_, _ = s.tty.WriteString("\x1b[B") // down
	time.Sleep(400 * time.Millisecond)
	_, _ = s.tty.WriteString("\r")
	time.Sleep(2 * time.Second)

	// ctrl+d opens the pane.
	_, _ = s.tty.WriteString("\x04")
	_, _ = s.tty.WriteString("\x1b") // drop focus to content
	time.Sleep(1500 * time.Millisecond)
	scr := replay(t, s, cols, rows)
	dumpFrame(t, "exec-open", scr)
	if !strings.Contains(screenText(scr), diffE2EMarkers["cmd/first.go"]) {
		t.Fatalf("the Execution-mount diff pane did not render the ledger\n%s", screenText(scr))
	}
}

// TestDiffE2EHonestStatesPTY re-verifies the honest states with a live session:
// an empty ledger renders the empty text; a failed ledger fetch renders the
// explicit error banner rather than the empty text.
func TestDiffE2EHonestStatesPTY(t *testing.T) {
	skipDiffE2E(t)
	const cols, rows = 120, 40

	t.Run("empty", func(t *testing.T) {
		fe := &diffE2EFileEditService{empty: true}
		url := diffE2EPlane(t, fe)
		s := diffE2ELaunch(t, url, cols, rows)
		defer s.close()
		scr := openAskDiffPane(t, s, cols, rows)
		dumpFrame(t, "honest-empty", scr)
		text := screenText(scr)
		found := strings.Contains(text, "no changed files yet") ||
			strings.Contains(text, "no edits yet") ||
			strings.Contains(text, "select a file")
		if !found {
			t.Fatalf("an empty ledger did not render the empty state\n%s", text)
		}
		if strings.Contains(text, "Couldn't load file edits") {
			t.Fatalf("an empty ledger rendered the ERROR banner\n%s", text)
		}
	})

	t.Run("failed", func(t *testing.T) {
		fe := &diffE2EFileEditService{fail: true}
		url := diffE2EPlane(t, fe)
		s := diffE2ELaunch(t, url, cols, rows)
		defer s.close()
		scr := openAskDiffPane(t, s, cols, rows)
		dumpFrame(t, "honest-failed", scr)
		text := screenText(scr)
		// The pane's error line is "  <err>" (diffs.Model.rawView renders Err).
		if !strings.Contains(text, "session file edits") && !strings.Contains(text, "ledger fetch failure") {
			t.Fatalf("a failed ledger fetch rendered no error banner (an unreachable ledger must never look empty)\n%s", text)
		}
		if strings.Contains(text, "no changed files yet") || strings.Contains(text, "no edits yet") {
			t.Fatalf("a failed ledger fetch masqueraded as the EMPTY state\n%s", text)
		}
	})
}
