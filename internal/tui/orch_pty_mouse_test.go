// Package tui — orch_pty_mouse_test.go — the Phase-2c REAL-PTY MOUSE
// verification (operator finding #7: PR #515 shipped unit-level mouse
// handlers while the footer promised "Mouse Enabled" and real clicks did
// nothing).
//
// This test injects REAL SGR mouse sequences into the RUNNING program
// (a live `bin/orch` on a real pty, pointed at a disposable plane
// fixture) and asserts what the live process PAINTS afterwards:
//
//  1. the conversations rail is up and POPULATED from the live API;
//  2. clicking the rail header collapses it (the header text leaves the
//     screen);
//  3. ctrl+r re-opens it;
//  4. wheel-down over the rail scrolls the list (the visible range moves);
//  5. clicking a tab opens its dropdown submenu;
//  6. clicking a rail ROW opens that conversation (its transcript paints);
//  7. clicking the composer focuses it (typed characters then echo);
//  8. /theme switches the theme in place.
//
// Rendered-string (unit) assertions are INSUFFICIENT — the predecessor run
// verified via strings and missed every operator finding. This file is the
// mouse half of the standing real-pty verification gate (see
// docs/tui-pty-verification-gate.md).
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// mouseAsk is the fixture Ask service: 60 conversations (more than the rail
// can show, so wheel scrolling is observable) and a durable transcript with
// a known marker string (so a rail-row click is observable).
const mouseConvCount = 60
const mouseTranscriptMarker = "rail-transcript-alpha"

type mouseAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
}

func (f *mouseAsk) ListConversations(ctx context.Context, req *connect.Request[v1.ListConversationsRequest]) (*connect.Response[v1.ListConversationsResponse], error) {
	out := &v1.ListConversationsResponse{}
	for i := 0; i < mouseConvCount; i++ {
		out.Conversations = append(out.Conversations, &v1.Conversation{
			Id:           fmt.Sprintf("conv-%02d", i),
			Title:        fmt.Sprintf("rail-conv-%02d", i),
			MessageCount: int32(i + 1),
			// THE CONVERSATIONS BELONG TO THE SEEDED PROJECT. mousePlaneFixture makes the launch
			// directory resolve to project "p1" (that is what keeps the launch prompt quiet), and
			// applyLaunchDirScope then points the rail at that workspace. A conversation with no
			// project_id is filtered OUT of the scoped rail, so the fixture's rows must carry p1 or
			// the rail renders empty and every row assertion in this gate is vacuous.
			ProjectId: "p1",
		})
	}
	return connect.NewResponse(out), nil
}

func (f *mouseAsk) ListMessages(ctx context.Context, req *connect.Request[v1.ListMessagesRequest]) (*connect.Response[v1.ListMessagesResponse], error) {
	out := &v1.ListMessagesResponse{}
	out.Messages = append(out.Messages, &v1.ChatMessage{
		Id:             "m1",
		ConversationId: req.Msg.GetConversationId(),
		Role:           "user",
		Content:        mouseTranscriptMarker,
	})
	return connect.NewResponse(out), nil
}

func (f *mouseAsk) GetConversation(ctx context.Context, req *connect.Request[v1.GetConversationRequest]) (*connect.Response[v1.GetConversationResponse], error) {
	return connect.NewResponse(&v1.GetConversationResponse{Conversation: &v1.Conversation{
		Id: req.Msg.GetId(), Title: "detail-of-" + req.Msg.GetId(), MessageCount: 2,
	}}), nil
}

// mousePlaneFixture serves just enough of the plane for the TUI to launch
// with a populated rail: /versionz (footer), ListProjects (auth probe) and
// the Ask service (conversations + transcript).
func mousePlaneFixture(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v9.9.9-ptymouse"})
	})
	// THE PROJECT CARRIES A DIRECTORY, and it is the directory orch is launched from
	// (the package dir — what startOrchPtyAt's child inherits). Without it the plane
	// reports an UNATTACHED launch directory, the launch-time project prompt fires, and
	// its full-frame question REPLACES the shell — so this gate waiting for the composer
	// (❯) never sees one and fails as "❯ never painted". This is the same trap
	// connectPlaneFixture documents; an empty &ptyProjects{} is not enough.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve cwd: %v", err)
	}
	pp, ph := apiv1connect.NewProjectServiceHandler(&ptyProjects{dir: cwd})
	mux.Handle(pp, ph)
	ap, ah := apiv1connect.NewAskOrchiconServiceHandler(&mouseAsk{})
	mux.Handle(ap, ah)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sendMouse writes a REAL SGR mouse press/release for the running program.
func (s *ptySession) sendMouse(btn, col, row int, release bool) {
	fin := "M"
	if release {
		fin = "m"
	}
	_, _ = s.tty.WriteString(fmt.Sprintf("\x1b[<%d;%d;%d%s", btn, col, row, fin))
}

// railRange parses the rail's visible-range indicator (" 1-38/60").
var railRangeRE = regexp.MustCompile(`(\d+)-(\d+)/` + strconv.Itoa(mouseConvCount))

func railRangeStart(t *testing.T, s string) int {
	t.Helper()
	m := railRangeRE.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		t.Fatalf("rail range indicator never painted: %s", tailOf(s, 1500))
	}
	start, err := strconv.Atoi(m[len(m)-1][1])
	if err != nil {
		t.Fatalf("rail range start parse: %v", err)
	}
	return start
}

// TestPTYMouseGate is the standing REAL-PTY mouse gate (finding #7): mouse
// events injected through the RUNNING program must drive tabs, rail rows,
// rail scrolling, and composer focus — not just unit-level handlers.
func TestPTYMouseGate(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty mouse gate: skipped in -short")
	}
	bin := orchBinPath(t)
	plane := mousePlaneFixture(t)
	home := t.TempDir()
	writeOrchConfig(t, home, plane.URL)
	s := startOrchPtyAt(t, bin, plane.URL, home)
	defer s.close()

	// 1. LAUNCH: composer focused. The launch page is the HERO view ("New", no
	// rail — see askMode/welcomeMode), so the conversations rail is reached by
	// its VERB, exactly as an operator reaches it: `/conversations`. Asserting a
	// rail straight off the launch frame was the stale half of this gate.
	out := s.readFor(5 * time.Second)
	if !strings.Contains(out, "❯") {
		t.Fatalf("launch: composer (❯) never painted (%d bytes)\n%s", len(out), tailOf(out, 2500))
	}
	_, _ = s.tty.WriteString("/conversations\r")
	out = s.readFor(5 * time.Second)
	for _, want := range []string{"❯", "Conversations", "rail-conv-00"} {
		if !strings.Contains(out, want) {
			t.Fatalf("launch: %q never painted after /conversations (%d bytes)\n%s", want, len(out), tailOf(out, 2500))
		}
	}
	if got := railRangeStart(t, out); got != 1 {
		t.Fatalf("launch: rail range starts at %d, want 1", got)
	}

	// 2. CLICK THE RAIL HEADER (absolute row 2 → SGR row 3) collapses it: the
	// header text must leave the frame.
	//
	// Assert on the repaint DELTA from the click, like step 3 below. A tail window of a whole-frame
	// repaint is vacuous here: the collapse repaints the entire shell, and the rail header sits on the
	// frame's FIRST body row, so it is never in the last few KB whether or not the rail left. That
	// vacuity is exactly what let a no-op ctrl+r/toggle look green.
	beforeCollapse := len(s.readFor(0))
	s.sendMouse(0, 120-ConversationsRailWidth+5, railTopRow+1, false)
	s.sendMouse(0, 120-ConversationsRailWidth+5, railTopRow+1, true)
	collapsed := s.readFor(2 * time.Second)
	if strings.Contains(stripCSI(collapsed[beforeCollapse:]), "Conversations") {
		t.Fatal("mouse click on the rail header did not collapse the rail")
	}

	// 3. ctrl+r re-opens it.
	//
	// Assert on the repaint DELTA, not on a byte window of the whole capture. Re-opening repaints the
	// WHOLE frame (the shell draws in alt-screen), and the rail's header sits on the frame's FIRST body
	// row — so a tail window can only find it if the window happens to be larger than the frame, which is
	// why the previous `tailOf(reopened, 20000)` went flaky as soon as the frame grew past 20KB. The
	// delta is the exact bytes this key press produced, and the header is always in it.
	before := len(collapsed)
	_, _ = s.tty.WriteString("\x12")
	reopened := s.readFor(2 * time.Second)
	if !strings.Contains(reopened[before:], "Conversations") {
		t.Fatalf("ctrl+r did not re-open the conversations rail (no header in this key press's %d-byte "+
			"repaint)\n%s", len(reopened)-before, tailOfPlain(stripCSI(reopened), 2000))
	}

	// 4. WHEEL over the rail scrolls the list.
	for i := 0; i < 30; i++ {
		s.sendMouse(65, 120-5, railTopRow+3, false) // wheel down (a rail list row)
	}
	scrolled := s.readFor(2 * time.Second)
	firstRow := railRangeStart(t, tailOf(scrolled, 8000))
	if firstRow <= 1 {
		t.Fatalf("wheel over the rail did not scroll (range still starts at %d)", firstRow)
	}
	// Some terminals/bubbletea builds can leak a stray tail of a mouse
	// sequence into the composer as text; clear the input so later phases
	// (which type commands) start from an empty buffer.
	_, _ = s.tty.WriteString(strings.Repeat("\x7f", 40))

	// 5. CLICK A RAIL ROW opens that conversation: its detail paints.
	//
	// F1 is a no-op navigation here (the shell launched on Ask and never left), but the tab
	// CHORD is openTabWithMenu now: pressing the OPEN tab's chord drops its dropdown down
	// (shell.go), and the open menu owns the next click — so the rail-row press below landed on
	// the menu, not the rail. The LONE esc that step 6 already relies on dismisses it (the menu
	// owns esc), returning the frame to the rail before the click.
	_, _ = s.tty.WriteString("\x1bOP") // F1 → Ask (opens its submenu)
	s.readFor(1500 * time.Millisecond)
	_, _ = s.tty.WriteString("\x1b") // dismiss the submenu so the rail is clickable
	s.readFor(700 * time.Millisecond)
	// SGR rows are 1-based: the FIRST conversation row (0-based railTopRow+1)
	// is SGR row railTopRow+2.
	s.sendMouse(0, 120-ConversationsRailWidth+8, railTopRow+2, false)
	s.sendMouse(0, 120-ConversationsRailWidth+8, railTopRow+2, true)
	// The ASSERTION is the conversation the click OPENED, read off the detail pane:
	// its "Conversation: <title>" header and its "id conv-NN" row. The fixture's older
	// "detail-of-<id>" marker is no longer what the pane draws (the detail header comes from the
	// LIST row that was clicked), so keying on it made a WORKING click look broken.
	opened := s.readFor(3 * time.Second)
	wantID := fmt.Sprintf("id conv-%02d", firstRow-1)
	// stripCSI, not stripANSI: stripANSI stops at the '[', so a parameterised colour like
	// "\x1b[38;5;231m" leaves "38;5;231m" in the text and SPLITS the needle. The painted
	// stream here is full of those (the shell paints 256-colour cells), so the strip has to
	// consume the whole CSI sequence.
	if !strings.Contains(stripCSI(opened), wantID) {
		t.Fatalf("clicking the rail row did not open its conversation (want %q in the painted stream)\n%s", wantID, tailOfPlain(stripCSI(tailOf(opened, 12000)), 2500))
	}

	// 6. CLICK TABS: a click on the tab bar opens that tab's dropdown
	// submenu (the header is "<Tab> ▾").
	tabMarkers := []string{"Ask Orchicon ▾", "Work ▾", "Execution ▾", "Automation ▾", "Enforcement ▾", "Control ▾"}
	menu := ""
	for _, col := range []int{40, 60, 80} {
		s.sendMouse(0, col, 1, false)
		s.sendMouse(0, col, 1, true)
		menu += s.readFor(1200 * time.Millisecond)
		if hasAny(menu, tabMarkers) {
			break
		}
	}
	if !hasAny(menu, tabMarkers) {
		t.Fatalf("mouse click on the tab bar never opened a tab submenu\n%s", tailOf(menu, 2000))
	}

	// Close any open tab dropdown (a LONE esc — the menu owns esc), then
	// GUARANTEE composer focus with ctrl+g. Both are separate writes so
	// bubbletea parses each as its own key (a concatenated ESC ESC is an
	// alt-sequence, not two escs).
	_, _ = s.tty.WriteString("\x1b")
	s.readFor(700 * time.Millisecond)
	_, _ = s.tty.WriteString("\x07")
	s.readFor(700 * time.Millisecond)

	// 7. THEME SWITCH in place (the gate's theme step): re-focus the composer
	// with a REAL mouse click (a stray esc in the menu phase may have dropped
	// keyboard focus to content), clear any leaked bytes, then drive /theme
	// through the palette (name selection) + its argument.
	s.sendMouse(0, 20, 40-1, false)
	s.sendMouse(0, 20, 40-1, true)
	s.readFor(1 * time.Second)
	_, _ = s.tty.WriteString(strings.Repeat("\x7f", 40))
	_, _ = s.tty.WriteString("/theme\r")
	themed := s.readFor(2 * time.Second)
	tail := stripCSI(tailOf(themed, 6000))
	// The /theme command surface runs IN the running shell (palette select +
	// live repaint). The switch itself is asserted at shell level by
	// TestThemeStepSequence (palette -> "/theme light" -> enter), which drives
	// the identical sequence deterministically.
	//
	// The markers are the notice's CURRENT text (its palette list header and its
	// switching hint). An earlier gate keyed on a bare "themes:", which the notice has
	// not printed since it grew its "(TUI palettes)" clause — slash.go's runThemeCmd —
	// so a WORKING /theme read as broken.
	for _, want := range []string{"themes (TUI palettes)", "/theme <name> switches"} {
		if !strings.Contains(tail, want) {
			t.Fatalf("/theme never ran in the running shell: %q missing\n%s", want, tailOf(tail, 1500))
		}
	}

	// 8. CLICK THE COMPOSER focuses it: esc drops focus to content, the click
	// restores it, and the next keystrokes ECHO in the composer (finding 7).
	_, _ = s.tty.WriteString("\x1b")
	s.readFor(1 * time.Second)
	s.sendMouse(0, 20, 40-1, false)
	s.sendMouse(0, 20, 40-1, true)
	s.readFor(1 * time.Second)
	_, _ = s.tty.WriteString("zk")
	focused := s.readFor(2 * time.Second)
	if !strings.Contains(focused, "zk") {
		t.Fatalf("clicking the composer did not focus it — typed chars never echoed\n%s", tailOf(focused, 2500))
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// stripCSI removes whole ANSI CSI sequences — "\x1b[" plus any parameter bytes up to and including
// the final byte. It is the strip the PAINTED-FRAME assertions need: stripANSI above stops at the
// first byte in the escape body, so a 256-colour cell like "\x1b[38;5;231m" leaves the literal
// "38;5;231m" in the text and splits any needle that spans a style boundary. The real-pty frames are
// almost entirely such cells, so a substring that looks present to the eye can be un-findable to
// strings.Contains.
var csiSeqRE = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

func stripCSI(s string) string { return csiSeqRE.ReplaceAllString(s, "") }

func hasAny(hay string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}
