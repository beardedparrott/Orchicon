// Package tui — activity_e2e_pty_test.go — the REAL-PTY end-to-end gate for the ACTIVITY LINE.
package tui

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/testfixtures/activitye2e"
	"github.com/beardedparrott/orchicon/internal/toolclass"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// skipActivityE2E skips unless ORCH_ACTIVITY_E2E=1. The live gate is deliberate, never incidental.
func skipActivityE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("ORCH_ACTIVITY_E2E") != "1" {
		t.Skip("real-pty activity-line E2E: set ORCH_ACTIVITY_E2E=1 " +
			"(and ORCH_PTY_SMOKE=1 from a terminal) to run")
	}
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty activity-line E2E: skipped in -short")
	}
}

// activityE2EOut is the evidence directory (frames + the cross-client handshake file). Empty means
// "write nothing" — the gate still asserts.
func activityE2EOut() string { return os.Getenv("ORCH_ACTIVITY_E2E_OUT") }

// activityE2EAddr is where the fixture plane binds.
func activityE2EAddr() string {
	if a := os.Getenv("ORCH_ACTIVITY_E2E_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8080"
}

// activityE2EPlane starts the fixture plane — the address the SPA's dev proxy targets, so the
// Playwright half can be pointed at the SAME plane process.
func activityE2EPlane(t *testing.T) (string, *activitye2e.Plane, func()) {
	t.Helper()
	addr := activityE2EAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("activity-line E2E: %s is already in use (%v) — either something else is "+
			"serving there (the container's own sandbox plane, which would make every observation "+
			"here a statement about the WRONG plane) or a previous run leaked. Stop it, or set "+
			"ORCH_ACTIVITY_E2E_ADDR to a free address", addr, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	plane := activitye2e.New()
	srv := &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: activitye2e.Mux(plane, &activitye2e.Sessions{}, cwd)},
	}
	srv.Start()
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	// kill DROPS every connection the plane holds — the live stream included — which is the harness's
	// "the connection died mid-turn" control.
	return srv.URL, plane, srv.CloseClientConnections
}

// e2eDumpFrame writes a rendered frame to $ORCH_ACTIVITY_E2E_OUT/<name>.txt so the review can quote
// the exact cells an observation rests on.
func e2eDumpFrame(t *testing.T, name string, scr *screen) {
	t.Helper()
	dir := activityE2EOut()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("frame dump skipped (mkdir): %v", err)
		return
	}
	var b strings.Builder
	for r := 0; r < scr.rows; r++ {
		fmt.Fprintf(&b, "%s\n", scr.row(r))
	}
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(b.String()), 0o644); err != nil {
		t.Logf("frame dump skipped (write): %v", err)
	}
}

// footerRow returns the FIRST painted row that carries the activity line, or "" when the footer
// holds none.
func footerRow(scr *screen) string {
	for r := 0; r < scr.rows; r++ {
		if row := strings.TrimSpace(scr.row(r)); strings.Contains(row, "Orchicon is ") {
			return row
		}
	}
	return ""
}

// footerRowIndex is the frame row the activity line was painted on, or -1.
func footerRowIndex(scr *screen) int {
	for r := 0; r < scr.rows; r++ {
		if strings.Contains(scr.row(r), "Orchicon is ") {
			return r
		}
	}
	return -1
}

// witnessRowIndex is the frame row of the LAST painted transcript witness line.
func witnessRowIndex(scr *screen) int {
	idx := -1
	for r := 0; r < scr.rows; r++ {
		if strings.Contains(scr.row(r), "E2EWITNESSFINAL") {
			idx = r
		}
	}
	return idx
}

// footerCounter extracts the count phrase from a painted footer row: everything after the verb's
// own "…" separator, minus the frame's own box glyphs.
func footerCounter(row string) string {
	i := strings.Index(row, "…")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(row[i+len("…"):])
	rest = strings.TrimPrefix(rest, "·")
	// The line is the pane's LAST content row, so the host Panel's right border follows it. Strip the
	// box glyphs and padding BEFORE the emptiness test, or a bare "Orchicon is …" reads as a counter.
	rest = strings.Trim(rest, " \t│┃|+-")
	if strings.HasPrefix(rest, "no output for") || strings.HasPrefix(rest, "last activity") ||
		strings.HasPrefix(rest, "the stream will re-attach") {
		return ""
	}
	return rest
}

// The counter vocabulary is the summarizer's OWN bucket words (toolclass.go countLabel), kept in
// step with the shared contract: `other tools?` is the catch-all the under-report fix added.
var counterRE = regexp.MustCompile(`\d+ (modif(?:y|ies)|reads?|bash|other tools?)`)

// hasCounter reports whether a painted footer row carries a TOOL COUNT.
func hasCounter(row string) bool { return counterRE.MatchString(row) }

// verbWord extracts the rotating word from a painted footer row ("Orchicon is <word>…").
func verbWord(row string) string {
	const prefix = "Orchicon is "
	i := strings.Index(row, prefix)
	if i < 0 {
		return ""
	}
	rest := row[i+len(prefix):]
	if j := strings.IndexAny(rest, " …·"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// countHalf drops the summarizer's trailing age ("· newest call Ns ago"), which is measured from
// each client's own clock and is therefore the ONE token the two clients may legitimately differ
// on.
func countHalf(counter string) string {
	if i := strings.LastIndex(counter, "· newest call "); i >= 0 {
		return strings.TrimSpace(counter[:i])
	}
	return counter
}

// waitFooter polls the replayed grid until the activity-line row satisfies want, or the budget
// expires. It returns the last footer row seen (so a failing assertion can quote what WAS painted).
func waitFooter(t *testing.T, s *ptySession, cols, rows int, budget time.Duration, want func(string) bool) (string, *screen) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var lastRow string
	var lastScr *screen
	for {
		lastScr = replay(t, s, cols, rows)
		lastRow = footerRow(lastScr)
		if lastRow != "" && want(lastRow) {
			return lastRow, lastScr
		}
		if time.Now().After(deadline) {
			return lastRow, lastScr
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// openActivityConversation drives the Ask mount to an OPEN conversation: /conversations opens the
// rail, a click on the first row opens it (setting chatConvID).
func openActivityConversation(t *testing.T, s *ptySession, cols, rows int) *screen {
	t.Helper()
	waitForFrame(t, s, cols, rows, 8*time.Second, "❯")
	// Let the shell finish its capability handshake + first live loads before typing (keys sent in
	// that window are dropped) — the same flat 5s the proven mouse/diff gates wait.
	time.Sleep(5 * time.Second)
	_, _ = s.tty.WriteString("/conversations\r")
	scr := waitForFrame(t, s, cols, rows, 8*time.Second, activitye2e.Title)
	if !strings.Contains(screenText(scr), activitye2e.Title) {
		t.Fatalf("the conversations rail never listed the fixture conversation %q\n%s",
			activitye2e.Title, screenText(scr))
	}
	// SGR rows are 1-based; the rail's first row is 0-based railTopRow+1, i.e. SGR railTopRow+2.
	s.sendMouse(0, cols-ConversationsRailWidth+8, railTopRow+2, false)
	s.sendMouse(0, cols-ConversationsRailWidth+8, railTopRow+2, true)
	time.Sleep(2 * time.Second)
	// FOCUS THE COMPOSER with ctrl+g — the gate's own documented focus chord (the same one
	// orch_pty_phase3_test.go uses).
	_, _ = s.tty.WriteString("\x07")
	time.Sleep(700 * time.Millisecond)
	return replay(t, s, cols, rows)
}

// sendPrompt types the prompt into the composer and sends it.
func sendPrompt(s *ptySession, text string) {
	_, _ = s.tty.WriteString(text)
	_, _ = s.tty.WriteString("\r")
}

// crossClient is the TUI -> GUI handshake file (AC7).
type crossClient struct {
	Stamp   int64  `json:"stamp"`
	Word    string `json:"word"`
	Counter string `json:"counter"`
	Line    string `json:"line"`
}

func writeCrossClient(t *testing.T, cc crossClient) {
	t.Helper()
	dir := activityE2EOut()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("cross-client dump skipped (mkdir): %v", err)
		return
	}
	b, _ := json.MarshalIndent(cc, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "tui-cross-client.json"), append(b, '\n'), 0o644); err != nil {
		t.Logf("cross-client dump skipped (write): %v", err)
	}
}

// serverCounter is AC4's independent read: the SERVER's own render of the ledger the client is
// counting, over the shared rolling window.
func serverCounter(plane *activitye2e.Plane) string {
	return toolclass.SummarizeCalls(plane.Calls(), time.Now(), toolclass.DefaultWindow)
}

// e2eFooterRow carries the with-tools turn's FOOTER row index across the two tests in this file, so
// the zero-tool-call leg can assert the pane spent the SAME number of rows on chrome (AC6.
var e2eFooterRow = -1

var _ = chat.VerbAt
