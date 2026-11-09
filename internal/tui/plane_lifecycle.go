package tui

// plane_lifecycle.go — plane STATE and LIFECYCLE CONTROL inside the TUI.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/planebind"
	"github.com/beardedparrott/orchicon/internal/planecontrol"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// planeState is what the TUI SHOWS. Deliberately NOT planecontrol.DriftState: "down", "drifted" and
// "not local" are three different operator problems with three different remedies.
type planeState int

const (
	planeStateUnknown  planeState = iota // never probed
	planeStateUp                         // PID alive, healthy, running this build
	planeStateDown                       // no live PID, or alive but not answering /healthz
	planeStateDrifted                    // serving a superseded binary (planecontrol.Drifted)
	planeStateNotLocal                   // the connected profile is not a plane on this host
)

// label is the SHORT footer form. IT CARRIES THE STATE ONLY, NEVER THE REMEDY.
func (s planeState) label() string {
	switch s {
	case planeStateUp:
		return "plane up"
	case planeStateDown:
		return "plane down"
	case planeStateDrifted:
		return "plane drifted"
	case planeStateNotLocal:
		return "plane remote"
	default:
		return "plane …"
	}
}

// planeInfo is the probed state plus everything the messages need to name a remedy.
type planeInfo struct {
	State    planeState
	Instance string              // "" when not local
	Status   planecontrol.Status // zero value when not local
	Detail   string              // "no plane PID file", "not answering http://…/healthz"
	Err      string              // probe/op failure text (empty on a clean probe)
}

// planeHomeDir is a SEAM, not indirection for its own sake: a test must never read the operator's real
// $HOME, because discovering a REAL instance there is exactly how a test could act on a live plane.
var planeHomeDir = func() string { return filepath.Dir(planecontrol.HostDataDirFor("dev")) }

// localInstances lists the instance data dirs on this host (`<home>/.local/share/orchicon-<name>`),
// sorted so the selection below is deterministic.
func localInstances() []string {
	matches, err := filepath.Glob(filepath.Join(planeHomeDir(), "orchicon-*"))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimPrefix(filepath.Base(m), "orchicon-")
		if name == "" || name == filepath.Base(m) {
			continue
		}
		if fi, err := os.Stat(m); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// readPortRecord reads one instance's recorded ports FROM planeHomeDir, so the seam above holds for
// discovery too.
func readPortRecord(instance string) (map[string]int, error) {
	b, err := os.ReadFile(filepath.Join(planeHomeDir(), "orchicon-"+instance, "ports.json"))
	if err != nil {
		return nil, err
	}
	ports := map[string]int{}
	if err := json.Unmarshal(b, &ports); err != nil {
		return nil, err
	}
	return ports, nil
}

// profilePort is the TCP port of the connected profile's URL (80/443 for a bare http/https URL).
func profilePort(p *config.Profile) int {
	if p == nil {
		return 0
	}
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil {
		return 0
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return 0
		}
		return n
	}
	switch u.Scheme {
	case "http":
		return 80
	case "https":
		return 443
	}
	return 0
}

// instanceForProfile decides WHICH instance a lifecycle verb acts on.
func instanceForProfile(p *config.Profile) (string, bool) {
	if env := strings.TrimSpace(os.Getenv("ORCHICON_INSTANCE")); env != "" {
		return env, true
	}
	port := profilePort(p)
	if port == 0 {
		return "", false
	}
	for _, inst := range localInstances() {
		ports, err := readPortRecord(inst)
		if err != nil {
			continue
		}
		if ports["ORCHICON_CONTROL_PORT"] == port {
			return inst, true
		}
	}
	return "", false
}

// planeLogPath is the plane's log for an instance — the SAME path planecontrol's start-timeout error
// names, so the two surfaces can never send the operator to different files.
func planeLogPath(instance string) string {
	return filepath.Join(planecontrol.HostDataDirFor(instance), "serve", "logs", "orchicon.log")
}

// stateFromStatus maps a probe onto what the operator should SEE.
func stateFromStatus(st planecontrol.Status) planeState {
	if st.Healthy {
		if st.Drift == planecontrol.Drifted && st.Alive {
			return planeStateDrifted
		}
		return planeStateUp
	}
	// Not answering: down, whether or not a PID file exists. A live PID that serves nothing is still a
	// failure, and reporting it as "up" would hide the thing the operator needs to fix.
	return planeStateDown
}

// probePlane reads the plane's state.
func probePlane(p *config.Profile) planeInfo {
	inst, ok := instanceForProfile(p)
	if !ok {
		detail := "no connection profile"
		if p != nil && p.URL != "" {
			detail = p.URL
		}
		return planeInfo{State: planeStateNotLocal, Detail: detail}
	}
	st, err := planecontrol.StatusOf(planecontrol.Defaults(inst, io.Discard))
	if err != nil {
		return planeInfo{State: planeStateDown, Instance: inst, Err: err.Error(), Detail: err.Error()}
	}
	info := planeInfo{State: stateFromStatus(st), Instance: inst, Status: st}
	info.Detail = planeDetail(info)
	return info
}

// planeDetail is the one-line "why" the dock shows beside the remedy.
func planeDetail(info planeInfo) string {
	switch info.State {
	case planeStateUp:
		return "pid " + strconv.Itoa(info.Status.PID) + " healthy at " + info.Status.HealthURL
	case planeStateDrifted:
		running := info.Status.Binary
		if running == "" {
			running = "an older binary"
		}
		return "pid " + strconv.Itoa(info.Status.PID) + " serving " + running + " (superseded)"
	case planeStateDown:
		switch {
		case info.Err != "":
			return info.Err
		case info.Status.PID == 0:
			return "no plane PID file for instance " + info.Instance
		case !info.Status.Alive:
			return "pid " + strconv.Itoa(info.Status.PID) + " is not running"
		default:
			return "pid " + strconv.Itoa(info.Status.PID) + " is not answering " + info.Status.HealthURL
		}
	default:
		return info.Detail
	}
}

// planeRemedy is THE actionable line: one function feeds both the footer chip's follow-up and the dock
// strip, so the two surfaces cannot disagree about the remedy.
func planeRemedy(info planeInfo) string {
	switch info.State {
	case planeStateDown:
		return "the plane for instance " + info.Instance +
			" is down — /plane-start to start it, or read " + planeLogPath(info.Instance)
	case planeStateDrifted:
		return "the plane for instance " + info.Instance +
			" is serving a superseded binary — /plane-restart to swap it; it is still answering at " +
			info.Status.HealthURL
	case planeStateNotLocal:
		return info.Detail + " is not a plane on this host — lifecycle control works only for a local instance"
	case planeStateUp:
		return "plane up (instance " + info.Instance + ", pid " + strconv.Itoa(info.Status.PID) + ")"
	default:
		return "plane state not probed yet — /plane probes it"
	}
}

// planeChip renders the footer chip for the current state.
func planeChip(f footerModel) string {
	if !f.PlaneWired {
		return ""
	}
	label, glyph, style := f.PlaneState.chipParts()
	// NARROW TERMINALS GET THE BARE STATE.
	if f.Width > 0 && f.Width < 100 {
		label = strings.TrimPrefix(label, "plane ")
	}
	if f.PlaneState == planeStateUp && f.PlaneInstance != "" && f.Width >= 100 {
		label += " (" + f.PlaneInstance + ")"
	}
	return style.Render(glyph + " " + label)
}

// chipParts maps a state onto its glyph, its word and its THEME STYLE — the only colour source a
// footer part may use (footer.go's EVERY PART IS STYLED note.
func (s planeState) chipParts() (label, glyph string, style lipgloss.Style) {
	switch s {
	case planeStateUp:
		return "plane up", "●", theme.StatusOK
	case planeStateDown:
		return "plane down", "○", theme.StatusErr
	case planeStateDrifted:
		return "plane drifted", "⚠", theme.FooterVersionDrift
	case planeStateNotLocal:
		return "plane remote", "·", theme.HintText
	default:
		return "plane …", "·", theme.HintText
	}
}

// word is the bare state name, for sentences (an op result should not read "plane down — /plane-start").
func (s planeState) word() string {
	switch s {
	case planeStateUp:
		return "up"
	case planeStateDown:
		return "down"
	case planeStateDrifted:
		return "drifted"
	case planeStateNotLocal:
		return "not local"
	default:
		return "unknown"
	}
}

// planeStateMsg carries a completed probe back to the tea loop.
type planeStateMsg struct{ info planeInfo }

// planeReprobeCooldown bounds how often a DISCONNECTED shell re-probes the plane.
const planeReprobeCooldown = 15 * time.Second

// planeReprobeCmd re-probes the plane AFTER the connection to it has failed.
func (m *App) planeReprobeCmd() tea.Cmd {
	if !m.planeProbed {
		return nil // no shell layout yet: the first-layout probe is still the one to arm
	}
	if m.streamStatus() == openStatus {
		m.planeReprobeAt = time.Time{}
		return nil
	}
	if !m.planeReprobeAt.IsZero() && time.Since(m.planeReprobeAt) < planeReprobeCooldown {
		return nil
	}
	m.planeReprobeAt = time.Now()
	return m.planeProbeCmd()
}

// planeOpMsg carries a completed lifecycle verb back to the tea loop — SUCCESS OR FAILURE, with the
// verb's output, so a stop that refuses is reported rather than looking like a no-op.
type planeOpMsg struct {
	op       string
	instance string
	out      string
	err      error
	info     planeInfo
}

// planeURL names the connected profile's URL for a message, without ever inventing one.
func planeURL(p *config.Profile) string {
	if p == nil || strings.TrimSpace(p.URL) == "" {
		return "this shell's connection"
	}
	return p.URL
}

// oneLine collapses a multi-line mechanism error into the one line the dock strip can render. The
// full text (with its indented remedies) belongs in the log, not in a one-row strip.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// planeProbeCmd probes the plane OFF the tea loop: StatusOf dials /healthz with a 2s timeout, which on
// the loop is a visible stall.
func (m *App) planeProbeCmd() tea.Cmd {
	profile := m.profile
	return func() tea.Msg { return planeStateMsg{info: probePlane(profile)} }
}

// planeOpCmd runs a lifecycle verb OFF the tea loop and re-probes afterwards.
func (m *App) planeOpCmd(op string) tea.Cmd {
	profile := m.profile
	return func() tea.Msg {
		inst, ok := instanceForProfile(profile)
		if !ok {
			url := planeURL(profile)
			return planeOpMsg{
				op:  op,
				err: fmt.Errorf("%s is not a plane on this host — lifecycle control works only for a local instance", url),
				info: planeInfo{
					State:  planeStateNotLocal,
					Detail: url,
				},
			}
		}
		// THE VERBS GO THROUGH internal/planebind, NOT planecontrol.
		buf := &bytes.Buffer{}
		svc := &planebind.Service{Out: buf}
		var err error
		switch op {
		case "start":
			_, err = svc.Start(inst)
		case "stop":
			// Stop is IDEMPOTENT (planecontrol.Stop: "nothing to stop" is success). It stays success
			// here too — a second /plane-stop must never read as a failure.
			_, err = svc.Stop(inst)
		case "restart":
			_, err = svc.Restart(inst)
		default:
			err = fmt.Errorf("unknown plane operation %q (use start, stop or restart)", op)
		}
		return planeOpMsg{op: op, instance: inst, out: oneLine(buf.String()), err: err, info: probePlane(profile)}
	}
}

// planeCommand is the ONE entry point every /plane* command uses: no verb re-probes and reports the
// state with usage; a verb runs the mechanism.
func (m *App) planeCommand(verb string) tea.Cmd {
	switch verb {
	case "start", "stop", "restart":
		inst, ok := instanceForProfile(m.profile)
		if !ok {
			url := planeURL(m.profile)
			info := planeInfo{State: planeStateNotLocal, Detail: url}
			m.dock.SetError("plane " + verb + " refused: " + planeRemedy(info))
			m.applyPlaneState(info)
			return nil
		}
		m.dock.SetNotice("plane " + verb + " (instance " + inst + ") …")
		return m.planeOpCmd(verb)
	case "", "status":
		m.dock.SetNotice("probing the plane …")
		return m.planeProbeCmd()
	}
	// A MISTYPED VERB IS REPORTED, NOT SWALLOWED. "/plane strat" silently probing the plane reads as
	// "the command worked" — the class of silent no-op acceptance criterion 1 is about.
	m.dock.SetError("plane " + verb + " is not a plane verb — usage: /plane [start | stop | restart]")
	return nil
}

// refreshPlaneLabel pushes the probed state onto the footer chip.
func (m *App) refreshPlaneLabel() {
	m.footer.PlaneState = m.plane.State
	m.footer.PlaneInstance = m.plane.Instance
	// The TUI is one of the surfaces that CAN start a plane (it does not depend on the plane serving
	// it), so the chip is always wired — including "remote".
	m.footer.PlaneWired = true
}

// applyPlaneState records a probe and reflects it in the footer, raising ONE dock notice per transition
// into "down" (a periodic probe must not spam the strip; it re-arms when the state leaves "down").
func (m *App) applyPlaneState(info planeInfo) tea.Cmd {
	m.plane = info
	// THE LABEL IS SET BEFORE ANY DOCK TEXT IS BUILT: surfaceStreamError reads m.plane.State to append
	// the remedy, and ordering this after it would render a stale state.
	m.refreshPlaneLabel()
	if info.State == planeStateDown {
		// THE STRIP CARRIES THE REMEDY FOR AS LONG AS THE PLANE IS DOWN — it is STATE, not a one-shot
		// notice.
		remedy := planeRemedy(info)
		// THREE CASES ASSERT IT, one does not: - the strip is EMPTY (a slash command just cleared it) →
		// assert.
		if m.planeDownErrText != remedy || m.dock.Err == "" || m.dock.Err == m.planeDownErrText {
			m.dock.SetError(remedy)
		}
		m.planeDownErrText = remedy
		return nil
	}
	// LEAVING "down" CLEARS THE STRIP THIS FUNCTION WROTE. It clears ONLY the exact text it wrote; any
	// other error on the strip belongs to someone else and is left alone.
	if m.planeDownErrText != "" && m.dock.Err == m.planeDownErrText {
		m.dock.SetError("")
	}
	m.planeDownErrText = ""
	return nil
}

// assertPlaneRemedy RE-ASSERTS the down remedy on the strip WITHOUT probing.
func (m *App) assertPlaneRemedy() {
	if m.plane.State != planeStateDown {
		return
	}
	remedy := planeRemedy(m.plane)
	m.planeDownErrText = remedy
	if m.dock.Err == "" || m.dock.Err == remedy {
		m.dock.SetError(remedy)
	}
}

// applyPlaneOp reports a completed verb. SUCCESS AND FAILURE BOTH REACH THE OPERATOR — a lifecycle
// command that silently did nothing is the failure mode this surface exists to remove.
func (m *App) applyPlaneOp(msg planeOpMsg) tea.Cmd {
	m.applyPlaneState(msg.info)
	inst := msg.instance
	if inst == "" {
		inst = "local instance"
	}
	if msg.err != nil {
		m.dock.SetError("plane " + msg.op + " failed: " + oneLine(msg.err.Error()) + " — " + planeRemedy(msg.info))
		return nil
	}
	notice := "✓ plane " + msg.op + " (" + inst + ") — " + msg.info.State.word()
	if msg.out != "" {
		notice += " — " + msg.out
	}
	m.dock.SetNotice(notice)
	return nil
}
