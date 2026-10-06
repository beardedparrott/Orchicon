// Package tui — activity_e2e_pty_test.go — the REAL-PTY end-to-end gate for the ACTIVITY LINE.
//
// WHY THIS FILE EXISTS. Five component tasks each proved a part of the feature: the server stamps
// time, the summarizer formats, the list rotates, the TUI paints, the GUI paints. Nothing in that
// set proves the parts work TOGETHER, and correctly-built components that were never proven
// together is how a feature ships green and broken. This gate drives ONE real turn through the
// REAL binary in a REAL pty and reads the activity line off the REPLAYED SCREEN GRID — the layer
// the operator is actually looking at.
//
// WHAT IS REAL AND WHAT IS SUBSTITUTED. One layer is substituted: the provider behind ChatStream.
// A genuinely model-driven turn cannot be run in this container — exec.LookPath("opencode") is
// empty and the serve-host fallback probe misses — so the fixture plane
// (internal/testfixtures/activitye2e, the SAME implementation the Playwright half talks to) scripts
// the turn's events. Every layer above it is the production one: the Connect/HTTP transport, the
// real chat.Controller state machine (heartbeat -> serverTimeMs, pageToolCalls -> chatStore), the
// real App.onChatWake -> ask pane -> frame pipeline in a real pty, and the real summarizer.
//
// The tool calls the counter counts are the FIXTURE'S OWN LEDGER ROWS, served over the same
// ListMessages page the production client polls, so the count reconciliation (AC4) is still a
// genuine client-vs-server comparison rather than a client-vs-itself one.
//
// AC3 IS THE REGRESSION THIS FEATURE EXISTS TO FIX, and it is asserted from ONE frame: the reply is
// PARTIALLY RENDERED in the body AND the activity line is still present in the footer. On the
// pre-change build the gate was "before the first token", so the line was gone the moment content
// arrived — the operator: "After the initial 'Orchicon is thinking...', streaming started and the
// 'Orchicon is thinking...' went away and never came back."
//
// The gate is OPT-IN (ORCH_ACTIVITY_E2E=1) and skips cleanly otherwise, so the standing
// `go test ./internal/tui/...` suite stays green in CI — the same shape as ORCH_DIFF_E2E.
//
// Harness: reuses the single-reader pty session from orch_pty_smoke_test.go, the disposable-plane
// idiom from orch_pty_mouse_test.go, and the terminal emulator from diff_e2e_fixture_test.go.
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

// activityE2EAddr is where the fixture plane binds. The default is the address the SPA's dev proxy
// targets, so the SAME process can serve both clients; ORCH_ACTIVITY_E2E_ADDR moves it when that
// port is already taken by the container's own sandbox plane (which is NOT the plane this gate may
// observe — it has no agent behind it).
func activityE2EAddr() string {
	if a := os.Getenv("ORCH_ACTIVITY_E2E_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8080"
}

// activityE2EPlane starts the fixture plane — the address the SPA's dev proxy targets, so the
// Playwright half can be pointed at the SAME plane process.
//
// The port is CONTENDED with the container's own sandbox plane. A silent attach to the wrong plane
// would invalidate every observation, so an already-bound address is a loud failure rather than a
// fallback (set ORCH_ACTIVITY_E2E_ADDR to move it).
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
	// "the connection died mid-turn" control: a REAL socket teardown rather than a status the client
	// is told to believe.
	//
	// CloseClientConnections, NOT Close: Close waits for outstanding requests to finish, and this
	// fixture's ChatStream deliberately never returns (a turn streams until the client or the plane
	// ends it), so Close would deadlock on the very socket the harness is trying to kill.
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
// holds none. The line is the pane's FIXED FOOTER (App.transcriptStatusLine -> DetailFooter), so
// finding it by its own opening words is finding it where the operator reads it.
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

// witnessRowIndex is the frame row of the LAST painted transcript witness line. Comparing it across
// the with-tools and the zero-tool-call turns is AC6's row budget measured directly: if the counter
// cost the body a row, this index moves.
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
// own "…" separator, minus the frame's own box glyphs. The escalation bands live behind a "·" of
// their own and are never mistaken for counts.
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
// step with the shared contract: `other tools?` is the catch-all the under-report fix added, so a
// turn whose only counted work is product tools still reads as a counter here. Mirrored byte-for-byte
// by COUNTER_RE in frontend/tests/activity-line-e2e.spec.ts.
var counterRE = regexp.MustCompile(`\d+ (modif(?:y|ies)|reads?|bash|other tools?)`)

// hasCounter reports whether a painted footer row carries a TOOL COUNT. It is deliberately narrow
// (a digit followed by a count word) so the watchdog's own "last activity 4s ago" — which also
// carries a digit — is not mistaken for work.
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

// countHalf drops the summarizer's trailing age ("· newest call Ns ago"), which is measured from each client's
// own clock and is therefore the ONE token the two clients may legitimately differ on. Everything
// before it is the work, and the work must agree exactly.
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
// rail, a click on the first row opens it (setting chatConvID), and esc drops keyboard focus to the
// content so the composer is reachable. Mirrors the coordinates the proven mouse gate uses.
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
	// orch_pty_phase3_test.go uses). esc would drop focus to CONTENT, where typing lands nowhere and
	// Enter never dispatches the send, which is a green-looking run that observes nothing.
	_, _ = s.tty.WriteString("\x07")
	time.Sleep(700 * time.Millisecond)
	return replay(t, s, cols, rows)
}

// sendPrompt types the prompt into the composer and sends it.
func sendPrompt(s *ptySession, text string) {
	_, _ = s.tty.WriteString(text)
	_, _ = s.tty.WriteString("\r")
}

// crossClient is the TUI -> GUI handshake file (AC7). Writing it to disk keeps the Go and browser
// runtimes decoupled: the Playwright spec reads the TUI's own observed word and counts and asserts
// the GUI's DOM carries the same ones.
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
// counting, over the shared rolling window. Both halves come from the SAME fixture rows the
// ListMessages page carried, so a client that invented, dropped or mis-windowed a call fails here.
func serverCounter(plane *activitye2e.Plane) string {
	return toolclass.SummarizeCalls(plane.Calls(), time.Now(), toolclass.DefaultWindow)
}

// e2eFooterRow carries the with-tools turn's FOOTER row index across the two tests in this file, so
// the zero-tool-call leg can assert the pane spent the SAME number of rows on chrome (AC6: "the
// transcript body does not lose or gain a row"). The tests run sequentially in one package, so a
// plain package variable is enough.
var e2eFooterRow = -1

var _ = chat.VerbAt
