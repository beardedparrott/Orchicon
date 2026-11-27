package tui

// antigravity_pty_capture_test.go — the TUI half of AC3's ten captures: a REAL `orch` process on a REAL
// pty, painting each of the five antigravity states.
//
// WHY A REAL PTY AND NOT A RENDERED-STRING ASSERTION. docs/tui-pty-verification-gate.md records nine
// regressions that string assertions could not see, because a footer can promise an interaction that does
// nothing. These captures are the same gate applied to a NEW adapter kind: the claim is that an operator
// watching a terminal SEES these words, so the evidence has to be the terminal's own painted cells.
//
// THE PLANE IS THE FIXTURE (internal/testfixtures/antigravityparity), which answers exactly the five AC3
// shapes. That is deliberate: "agy not installed", "auth error" and "stale snapshot" are not states a
// healthy plane can be driven into one after another, and a capture whose state depends on breaking a real
// plane is a capture nobody can reproduce.
//
// RUN IT (it writes its frames into $AGY_PARITY_OUT):
//
//	AGY_PARITY_OUT=$PWD/qa-evidence/antigravity-parity/tui \
//	  go test ./internal/tui/ -run TestPTYAntigravityParity -v -count=1
//
// The frames land as plain text, one file per state, so a reviewer can read the exact cells.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/testfixtures/antigravityparity"
)

// agyParityOut is where the frames are written. Unset means the test still ASSERTS and only skips the
// dump, so it is useful in CI without littering the tree.
func agyParityOut() string { return os.Getenv("AGY_PARITY_OUT") }

// startAgyParityPlane serves one state on an ephemeral loopback port.
func startAgyParityPlane(t *testing.T, state antigravityparity.State) string {
	t.Helper()
	srv := httptest.NewServer(antigravityparity.Mux(antigravityparity.New(state)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// dumpAgyFrame writes one painted frame to the evidence directory.
func dumpAgyFrame(t *testing.T, name string, scr *screen) {
	t.Helper()
	dir := agyParityOut()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("frame dump skipped (mkdir): %v", err)
		return
	}
	var b strings.Builder
	for r := 0; r < scr.rows; r++ {
		b.WriteString(strings.TrimRight(scr.row(r), " "))
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, name+".txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Logf("frame dump skipped (write): %v", err)
		return
	}
	t.Logf("frame: %s", path)
}

// openAgyModelsPicker launches orch against the fixture and opens /models.
//
// THE PICKER OPENS ALREADY SEEDED ON THE agy TIER. The fixture's default ask model is
// `antigravity/google/gemini-4-neon-low`, so /models opens with kind/provider/model all set, the model
// tier focused, and its load already issued. That is the state an operator on an agy-defaulted plane
// actually lands in — and it means the capture observes a state rather than driving one, so no key
// presses can accidentally COMMIT a model and close the box (which is what an enter does on the model
// tier).
func openAgyModelsPicker(t *testing.T, planeURL string, cols, rows int) *ptySession {
	t.Helper()
	bin := orchBinPath(t)
	home := t.TempDir()
	writeOrchConfig(t, home, planeURL)
	s := startOrchPtySized(t, bin, planeURL, home, cols, rows)
	t.Cleanup(s.close)

	waitForFrame(t, s, cols, rows, 10*time.Second, "❯")
	// The shell runs its capability handshake and first loads on start; keys sent in that window are
	// dropped (the same reason every other gate in this package waits).
	time.Sleep(4 * time.Second)
	_, _ = s.tty.WriteString("/models\r")
	waitForFrame(t, s, cols, rows, 15*time.Second, "ADAPTER")
	return s
}

// agyPickerFrame waits for the ADAPTER tier to be painted with antigravity selected, so every capture
// starts from the same known picker scope.
func agyPickerFrame(t *testing.T, s *ptySession, cols, rows int, want ...string) *screen {
	t.Helper()
	markers := append([]string{"ADAPTER", "▸ antigravity"}, want...)
	scr := waitForFrame(t, s, cols, rows, 15*time.Second, markers...)
	return scr
}

// TestPTYAntigravityParityCaptureLive is state 1 of 5: the picker on a LIVE agy listing — the adapter
// pills, `google` as its only provider, and agy's own model ids with their 1M window.
func TestPTYAntigravityParityCaptureLive(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	const cols, rows = 140, 44
	planeURL := startAgyParityPlane(t, antigravityparity.StateLive)
	s := openAgyModelsPicker(t, planeURL, cols, rows)

	scr := agyPickerFrame(t, s, cols, rows, "gemini-4-carbon-high")
	dumpAgyFrame(t, "01-live-model-list", scr)

	text := screenText(scr)
	for _, want := range []string{"antigravity", "ADAPTER", "PROVIDER", "Google",
		"gemini-4-carbon-high", "gemini-4-neon-low", "1.0M ctx", "reasoning", "live",
		"antigravity/google/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the live picker never painted %q — the frame is the proof an operator can pick an "+
				"agy model, and it does not:\n%s", want, tailOfPlain(text, 3000))
		}
	}
	if strings.Contains(text, "may be stale") {
		t.Errorf("a LIVE listing must not claim it may be stale:\n%s", tailOfPlain(text, 2000))
	}
}

// TestPTYAntigravityParityCaptureNotInstalled is state 2 of 5: no `agy` on the host. The model tier is
// EMPTY and the picker says what to install, in the server's own words.
func TestPTYAntigravityParityCaptureNotInstalled(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	const cols, rows = 140, 44
	planeURL := startAgyParityPlane(t, antigravityparity.StateNotInstalled)
	s := openAgyModelsPicker(t, planeURL, cols, rows)

	scr := agyPickerFrame(t, s, cols, rows, "Antigravity CLI")
	dumpAgyFrame(t, "02-not-installed", scr)

	text := screenText(scr)
	for _, want := range []string{"Antigravity CLI (`agy`) not found", "install from"} {
		if !strings.Contains(text, want) {
			t.Fatalf("an absent agy must say %q on the painted screen:\n%s", want, tailOfPlain(text, 3000))
		}
	}
	if strings.Contains(text, "(no models)") {
		t.Errorf("the empty tier fell back to the generic text instead of the install sentence:\n%s",
			tailOfPlain(text, 2000))
	}
}

// TestPTYAntigravityParityCaptureAuthError is state 3 of 5: agy is installed but signed out. The picker
// surfaces the actionable auth sentence and offers `r` to retry.
func TestPTYAntigravityParityCaptureAuthError(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	const cols, rows = 140, 44
	planeURL := startAgyParityPlane(t, antigravityparity.StateAuthError)
	s := openAgyModelsPicker(t, planeURL, cols, rows)

	scr := agyPickerFrame(t, s, cols, rows, "not signed in")
	dumpAgyFrame(t, "03-auth-error", scr)

	text := screenText(scr)
	for _, want := range []string{"not signed in", "GEMINI_API_KEY"} {
		if !strings.Contains(text, want) {
			t.Fatalf("an auth failure must paint %q — the operator needs the fix, not just the fault:\n%s",
				want, tailOfPlain(text, 3000))
		}
	}
	// THE RETRY AFFORDANCE: a failed load must offer `r`, or the operator's only route out is a restart.
	if !strings.Contains(text, "press r") && !strings.Contains(text, "r: retry") {
		t.Errorf("a failed load must advertise the retry key:\n%s", tailOfPlain(text, 2000))
	}
}

// TestPTYAntigravityParityCaptureStale is state 4 of 5: the vendored snapshot answered. The rows are
// selectable AND the list says it may be out of date.
func TestPTYAntigravityParityCaptureStale(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	const cols, rows = 140, 44
	planeURL := startAgyParityPlane(t, antigravityparity.StateStale)
	s := openAgyModelsPicker(t, planeURL, cols, rows)

	scr := agyPickerFrame(t, s, cols, rows, "may be stale")
	dumpAgyFrame(t, "04-stale-snapshot", scr)

	text := screenText(scr)
	for _, want := range []string{"gemini-4-carbon-high", "may be stale", "antigravity-snapshot"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the snapshot fallback must paint %q — a stale list that looked live would be worse "+
				"than a short one:\n%s", want, tailOfPlain(text, 3000))
		}
	}
}

// TestPTYAntigravityParityCaptureModeRefused is state 5 of 5: a Brainstorm turn in which the platform
// refused a write. The transcript shows agy's own tool rows (with their subjects) and the refusal.
func TestPTYAntigravityParityCaptureModeRefused(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	const cols, rows = 150, 46
	planeURL := startAgyParityPlane(t, antigravityparity.StateModeRefused)

	bin := orchBinPath(t)
	home := t.TempDir()
	writeOrchConfig(t, home, planeURL)
	s := startOrchPtySized(t, bin, planeURL, home, cols, rows)
	defer s.close()

	waitForFrame(t, s, cols, rows, 10*time.Second, "❯")
	time.Sleep(4 * time.Second)
	_, _ = s.tty.WriteString("/conversations\r")
	waitForFrame(t, s, cols, rows, 12*time.Second, antigravityparity.ConvTitle)

	// Open the conversation by clicking its rail row (SGR rows are 1-based; the rail's first row is
	// railTopRow+2 in SGR terms — the same addressing orch_pty_mouse_test.go and openActivityConversation
	// use).
	s.sendMouse(0, cols-ConversationsRailWidth+8, railTopRow+2, false)
	s.sendMouse(0, cols-ConversationsRailWidth+8, railTopRow+2, true)
	scr := waitForFrame(t, s, cols, rows, 15*time.Second, "REFUSED BY THE PLATFORM")
	dumpAgyFrame(t, "05-mode-refused", scr)

	text := screenText(scr)
	// THE TOOL ROWS, with the SUBJECT from agy's own argument keys — the parity claim this item makes.
	for _, want := range []string{
		"run_command", "go test ./internal/tui/... -run Antigravity",
		"view_file", "/home/op/project/README.md",
		"read_url_content", "https://antigravity.google/download",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the transcript never painted %q — an agy tool row without its subject is the JSON "+
				"dump this contract exists to prevent:\n%s", want, tailOfPlain(text, 4000))
		}
	}
	// THE REFUSAL, and its reason.
	for _, want := range []string{"REFUSED BY THE PLATFORM", "not available in brainstorm mode", "write_file"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the mode refusal must paint %q:\n%s", want, tailOfPlain(text, 4000))
		}
	}
	// NO CARD WAS RAISED for the refused tool — the mode gate runs before the card.
	if strings.Contains(text, "Allow once") {
		t.Errorf("a mode-refused tool must never raise a consent card:\n%s", tailOfPlain(text, 3000))
	}
}
