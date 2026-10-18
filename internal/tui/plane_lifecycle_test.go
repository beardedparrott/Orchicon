package tui

// plane_lifecycle_test.go — the unit side of the TUI plane control surface.
//
// NOTHING HERE TOUCHES A REAL PLANE OR THE OPERATOR'S REAL $HOME. The one filesystem seam
// (planeHomeDir) is redirected at t.TempDir(), and the lifecycle verbs are only ever reached through
// the NOT-LOCAL refusal, which returns before calling internal/planecontrol at all. A test suite that
// could signal a real process would be a worse bug than the one this feature fixes.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
)

// tempPlaneHome points the instance-discovery seam at a scratch directory for the test's lifetime.
//
// ORCHICON_INSTANCE is cleared in the same breath: it is the FIRST precedence rule, so an operator
// shell that exports it would otherwise decide what these tests read.
func tempPlaneHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := planeHomeDir
	planeHomeDir = func() string { return home }
	t.Cleanup(func() { planeHomeDir = old })
	t.Setenv("ORCHICON_INSTANCE", "")
	return home
}

// writePorts records an instance's ports exactly as the installer does (state.go's ports.json).
func writePorts(t *testing.T, home, instance string, controlPort int) {
	t.Helper()
	dir := filepath.Join(home, "orchicon-"+instance)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir instance dir: %v", err)
	}
	body := `{"ORCHICON_CONTROL_PORT": ` + strconv.Itoa(controlPort) + `, "ORCHICON_POSTGRES_PORT": 5432}`
	if err := os.WriteFile(filepath.Join(dir, "ports.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write ports.json: %v", err)
	}
}

// THE COMMAND-DRIFT GATE (acceptance criterion 4).
//
// A command that EXISTS but is absent from an enumeration surface is the exact drift this repo has
// hit before — printHelp's comment in cmd/orchicon documents `install` missing from help for the same
// reason. So every new command is asserted against ALL THREE surfaces the TUI enumerates commands on:
// the registry (dispatch), /help (slashHelpText), and the palette rows the composer opens.
func TestPlaneCommandsRegisteredEverywhere(t *testing.T) {
	m := newDockTestApp()
	names := []string{"/plane", "/plane-start", "/plane-stop", "/plane-restart"}

	for _, name := range names {
		if m.slash.resolve(name) == nil {
			t.Errorf("%s: not in the slash registry (a typed command would be sent as chat)", name)
		}
	}

	help := m.slashHelpText()
	for _, name := range names {
		if !strings.Contains(help, name) {
			t.Errorf("%s: missing from /help output", name)
		}
	}

	// The palette filters the SAME registry, so a name missing here means the row generation skipped it.
	m.palette.query = "plane"
	m.refreshPalette()
	rows := map[string]bool{}
	for _, c := range m.palette.filter {
		rows[c.Name] = true
	}
	for _, name := range names {
		if !rows[name] {
			t.Errorf("%s: missing from the palette rows (%d rows for query %q)", name, len(m.palette.filter), m.palette.query)
		}
	}
}

// EVERY STATE IS VISIBLE, and DRIFTED IS NOT DOWN (acceptance criterion 2).
func TestPlaneStateRendersEveryState(t *testing.T) {
	cases := []struct {
		state planeState
		want  string
	}{
		{planeStateUp, "plane up"},
		{planeStateDown, "plane down"},
		{planeStateDrifted, "plane drifted"},
		{planeStateNotLocal, "plane remote"},
		{planeStateUnknown, "plane …"},
	}
	seen := map[string]planeState{}
	for _, tc := range cases {
		f := footerModel{
			PlaneState:    tc.state,
			PlaneWired:    true,
			PlaneInstance: "dev",
			StreamStatus:  stream.StatusOpen,
			Width:         200,
		}
		view := f.View()
		if !strings.Contains(view, tc.want) {
			t.Errorf("state %v: footer does not render %q\n%s", tc.state, tc.want, view)
		}
		seen[tc.want] = tc.state
	}
	// The drifted chip must be DISTINGUISHABLE from the down one: a drifted plane is answering, and
	// telling the operator to start it would be wrong.
	down := footerModel{PlaneState: planeStateDown, PlaneWired: true, StreamStatus: stream.StatusOpen, Width: 200}.View()
	drifted := footerModel{PlaneState: planeStateDrifted, PlaneWired: true, StreamStatus: stream.StatusOpen, Width: 200}.View()
	if down == drifted {
		t.Fatal("drifted and down render identically — the operator cannot tell them apart")
	}
	// The chip names the STATE; the remedy (which names the command) belongs on the strip, so the
	// footer keeps its one row on a narrow terminal — see planeState.label.
	if !strings.Contains(drifted, "plane drifted") {
		t.Errorf("drifted chip must say drifted: %s", drifted)
	}
	// An unwired footer (a test or embedder that never probes) renders no chip at all.
	unwired := footerModel{PlaneState: planeStateDown, StreamStatus: stream.StatusOpen, Width: 200}.View()
	if strings.Contains(unwired, "plane") {
		t.Errorf("an unwired footer must render no plane chip: %s", unwired)
	}
}

// THE STATE MAPPING: drifted is drifted, a live-and-healthy plane is up, everything else is down.
func TestPlaneStateFromStatus(t *testing.T) {
	cases := []struct {
		name string
		st   planecontrol.Status
		want planeState
	}{
		{"healthy current", planecontrol.Status{PID: 42, Alive: true, Healthy: true, Drift: planecontrol.Current}, planeStateUp},
		{"superseded but answering", planecontrol.Status{PID: 42, Alive: true, Healthy: true, Drift: planecontrol.Drifted}, planeStateDrifted},
		// A drifted VERDICT about a process that is gone is not drifted: the remedy for a dead plane is
		// to start it, and /plane-restart would be the wrong instruction.
		{"drifted verdict, dead process", planecontrol.Status{PID: 42, Alive: false, Drift: planecontrol.Drifted}, planeStateDown},
		{"no pid file", planecontrol.Status{}, planeStateDown},
		{"pid but dead", planecontrol.Status{PID: 42, Alive: false}, planeStateDown},
		{"alive but not answering", planecontrol.Status{PID: 42, Alive: true, Healthy: false, Drift: planecontrol.Current}, planeStateDown},
	}
	for _, tc := range cases {
		if got := stateFromStatus(tc.st); got != tc.want {
			t.Errorf("%s: stateFromStatus = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE REMEDY IS NAMED (acceptance criterion 3): a down plane puts a command AND the log path in front
// of the operator, not just "connection refused".
func TestPlaneDownMessageNamesTheRemedy(t *testing.T) {
	m := newDockTestApp()
	info := planeInfo{
		State:    planeStateDown,
		Instance: "dev",
		Detail:   "no plane PID file for instance dev",
	}
	m.applyPlaneState(info)

	if !strings.Contains(m.dock.Err, "/plane-start") {
		t.Errorf("dock error does not name the start command: %q", m.dock.Err)
	}
	if !strings.Contains(m.dock.Err, filepath.Join("serve", "logs", "orchicon.log")) {
		t.Errorf("dock error does not name the log path: %q", m.dock.Err)
	}
	if !strings.Contains(m.dock.Err, "dev") {
		t.Errorf("dock error does not name the instance: %q", m.dock.Err)
	}
	// The footer chip reflects the same state, so the two surfaces cannot disagree.
	if m.footer.PlaneState != planeStateDown || !m.footer.PlaneWired {
		t.Errorf("footer not wired to the probed state: state=%v wired=%v", m.footer.PlaneState, m.footer.PlaneWired)
	}

	// A REPEATED PROBE DOES NOT DUPLICATE THE LINE — it re-asserts the same one.
	m.applyPlaneState(info)
	if n := strings.Count(m.dock.Err, "is down"); n != 1 {
		t.Errorf("the down notice repeated on a repeated probe (%d times): %q", n, m.dock.Err)
	}
	// AND THE STRIP SELF-HEALS: every slash command clears it (dispatchSlash), so a strip that came
	// back empty while the plane is STILL down must carry the remedy again on the next probe. Without
	// this, `/plane` reported "plane down" in the footer and nothing at all on the strip.
	m.dock.SetError("")
	m.applyPlaneState(info)
	if !strings.Contains(m.dock.Err, "/plane-start") {
		t.Errorf("the remedy did not return after the strip was cleared: %q", m.dock.Err)
	}
	// A DRIFTED probe replaces the remedy (restart, not start) rather than leaving the down one.
	m.applyPlaneState(planeInfo{State: planeStateDrifted, Instance: "dev",
		Status: planecontrol.Status{HealthURL: "http://localhost:18080/healthz"}})
	if strings.Contains(m.dock.Err, "/plane-start") {
		t.Errorf("a drifted plane kept the down remedy on the strip: %q", m.dock.Err)
	}
}

// surfaceStreamError carries the remedy on the SAME strip as the connection failure.
func TestPlaneDownRemedySurfacesOnStreamError(t *testing.T) {
	m := newDockTestApp()
	m.plane = planeInfo{State: planeStateDown, Instance: "dev"}
	remedy := planeRemedy(m.plane)
	if !strings.Contains(remedy, "/plane-start") || !strings.Contains(remedy, "orchicon.log") {
		t.Fatalf("planeRemedy must name the command and the log path: %q", remedy)
	}
	// A drifted plane's remedy points at restart, never at start.
	drifted := planeRemedy(planeInfo{State: planeStateDrifted, Instance: "dev",
		Status: planecontrol.Status{HealthURL: "http://localhost:8080/healthz"}})
	if !strings.Contains(drifted, "/plane-restart") || strings.Contains(drifted, "/plane-start") {
		t.Errorf("drifted remedy must point at /plane-restart only: %q", drifted)
	}
	// A not-local profile states the fact and offers no command, because there is none to offer.
	notLocal := planeRemedy(planeInfo{State: planeStateNotLocal, Detail: "https://remote.example.com"})
	if !strings.Contains(notLocal, "https://remote.example.com") || !strings.Contains(notLocal, "local instance") {
		t.Errorf("not-local remedy must name the URL and the reason: %q", notLocal)
	}
}

// WHICH INSTANCE? — the connected profile's port selects the local instance, and no match means
// NOT LOCAL rather than a guess.
func TestPlaneInstanceForProfileMatchesControlPort(t *testing.T) {
	home := tempPlaneHome(t)
	writePorts(t, home, "dev", 18080)
	writePorts(t, home, "prod", 18090)

	if got := localInstances(); len(got) != 2 || got[0] != "dev" || got[1] != "prod" {
		t.Fatalf("localInstances = %v, want [dev prod]", got)
	}

	cases := []struct {
		url      string
		wantInst string
		wantOK   bool
	}{
		{"http://localhost:18080", "dev", true},
		{"http://127.0.0.1:18090", "prod", true},
		// No local port record matches → NOT LOCAL. Guessing "dev" here is how a shell pointed at a
		// remote plane would stop a local one the operator was not looking at.
		{"https://remote.example.com", "", false},
		{"http://localhost:19999", "", false},
	}
	for _, tc := range cases {
		inst, ok := instanceForProfile(&config.Profile{Name: "default", URL: tc.url})
		if inst != tc.wantInst || ok != tc.wantOK {
			t.Errorf("%s: instanceForProfile = (%q, %v), want (%q, %v)", tc.url, inst, ok, tc.wantInst, tc.wantOK)
		}
	}

	// ORCHICON_INSTANCE is EXPLICIT operator intent and outranks the port match.
	t.Setenv("ORCHICON_INSTANCE", "prod")
	if inst, ok := instanceForProfile(&config.Profile{URL: "http://localhost:18080"}); !ok || inst != "prod" {
		t.Errorf("ORCHICON_INSTANCE must win: got (%q, %v)", inst, ok)
	}
}

// A NON-LOCAL INSTANCE REFUSES LOUDLY (acceptance criterion 1's failure half): the verb reports a
// specific failure naming the URL, and never reaches internal/planecontrol at all.
func TestPlaneOpRefusesNonLocalInstance(t *testing.T) {
	tempPlaneHome(t)
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://remote.example.com"}, "v9.9.9")

	for _, op := range []string{"start", "stop", "restart"} {
		cmd := m.planeOpCmd(op)
		if cmd == nil {
			t.Fatalf("planeOpCmd(%q) returned no command", op)
		}
		msg, ok := cmd().(planeOpMsg)
		if !ok {
			t.Fatalf("planeOpCmd(%q) did not produce a planeOpMsg", op)
		}
		if msg.err == nil {
			t.Fatalf("planeOpCmd(%q) must FAIL for a non-local instance, not silently succeed", op)
		}
		if !strings.Contains(msg.err.Error(), "remote.example.com") {
			t.Errorf("planeOpCmd(%q) error must name the profile URL: %q", op, msg.err.Error())
		}
		if msg.info.State != planeStateNotLocal {
			t.Errorf("planeOpCmd(%q) info state = %v, want not-local", op, msg.info.State)
		}
	}
}

// A COMPLETED VERB IS REPORTED, SUCCESS OR FAILURE (acceptance criterion 1) — never a silent no-op.
func TestPlaneOpReportedToDock(t *testing.T) {
	m := newDockTestApp()

	// Failure: the dock says which verb, why, and what to do about it.
	m.applyPlaneOp(planeOpMsg{
		op:       "start",
		instance: "dev",
		err:      errString("the host plane did not answer http://localhost:18080/healthz within 60s"),
		info:     planeInfo{State: planeStateDown, Instance: "dev"},
	})
	if !strings.Contains(m.dock.Err, "plane start failed") {
		t.Errorf("a failed verb must be reported with its name: %q", m.dock.Err)
	}
	if !strings.Contains(m.dock.Err, "did not answer") || !strings.Contains(m.dock.Err, "/plane-start") {
		t.Errorf("a failed verb must carry the cause and the remedy: %q", m.dock.Err)
	}

	// Success: a notice naming the verb, the instance and the resulting state.
	m.dock.SetError("")
	m.applyPlaneOp(planeOpMsg{
		op:       "stop",
		instance: "dev",
		out:      "serve stopped",
		info:     planeInfo{State: planeStateDown, Instance: "dev"},
	})
	if !strings.Contains(m.dock.Notice, "✓ plane stop") || !strings.Contains(m.dock.Notice, "down") {
		t.Errorf("a successful verb must be reported: %q", m.dock.Notice)
	}
	if m.footer.PlaneState != planeStateDown {
		t.Errorf("the footer must reflect the post-op state, got %v", m.footer.PlaneState)
	}
}

// errString is a minimal error for the message tests (no fmt dependency in the assertion).
type errString string

func (e errString) Error() string { return string(e) }

// The registry-driven /plane dispatch: no verb probes and reports; a verb refuses loudly when the
// profile is not a local plane.
func TestPlaneCommandDispatch(t *testing.T) {
	tempPlaneHome(t)
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://remote.example.com"}, "v9.9.9")

	// No verb: a probe is armed (never a blocking call).
	if cmd := m.planeCommand(""); cmd == nil {
		t.Fatal("/plane with no verb must arm a probe")
	}
	// A verb against a non-local profile: refused in the dock, nothing invoked.
	m.dock.SetError("")
	if cmd := m.planeCommand("stop"); cmd != nil {
		t.Error("/plane stop against a non-local plane must not arm a lifecycle call")
	}
	if !strings.Contains(m.dock.Err, "refused") || !strings.Contains(m.dock.Err, "local instance") {
		t.Errorf("the refusal must be loud and explained: %q", m.dock.Err)
	}
}

// THE STATE INDICATOR MUST NOT GO STALE (acceptance criteria 2 and 3).
//
// The first-layout probe answers "was there a plane when the shell started". A plane stopped from
// another surface (the CLI, the desktop app) left the chip saying "plane up" and the strip with only
// "connection refused" — the two failures this feature exists to remove. The rolling refresh tick is
// where a failed connection becomes a re-probe.
func TestPlaneReprobeWhenConnectionDrops(t *testing.T) {
	tempPlaneHome(t)
	m := newDockTestApp()
	m.planeProbed = true

	// CONNECTED: nothing is armed — a healthy plane is not polled by a state indicator.
	if cmd := m.planeReprobeCmd(); cmd != nil {
		t.Fatal("a connected shell must not re-probe the plane on every tick")
	}
	// A FAILED STREAM IS THE EVIDENCE: the tick arms a probe.
	m.reg.ReportStatusForTest("plane-gate", string(stream.StatusError))
	if cmd := m.planeReprobeCmd(); cmd == nil {
		t.Fatal("a disconnected shell must re-probe the plane")
	}
	// THROTTLED: one probe per cooldown, not one per tick.
	if cmd := m.planeReprobeCmd(); cmd != nil {
		t.Error("the re-probe must be throttled — a down plane must not be probed every tick")
	}
	// RECONNECTING CLEARS THE COOLDOWN, so the NEXT outage probes immediately.
	m.planeReprobeAt = time.Now()
	m.reg.ReportStatusForTest("plane-gate", string(stream.StatusOpen))
	if cmd := m.planeReprobeCmd(); cmd != nil {
		t.Fatal("a connected shell must not probe")
	}
	if !m.planeReprobeAt.IsZero() {
		t.Error("a healthy connection must reset the cooldown, or the next outage waits for it")
	}
	// NO SHELL LAYOUT YET: the first-layout probe owns the first probe.
	m.planeProbed = false
	m.reg.ReportStatusForTest("plane-gate", string(stream.StatusError))
	if cmd := m.planeReprobeCmd(); cmd != nil {
		t.Error("before the first layout the reprobe must arm nothing")
	}
}

// A PLANE THAT COMES BACK CLEARS THE DOWN MESSAGE — but only the text this surface wrote.
func TestPlaneRecoveryClearsTheDownMessage(t *testing.T) {
	m := newDockTestApp()
	down := planeInfo{State: planeStateDown, Instance: "dev"}
	up := planeInfo{State: planeStateUp, Instance: "dev"}

	m.applyPlaneState(down)
	if m.dock.Err == "" {
		t.Fatal("the down state must put the remedy on the strip")
	}
	m.applyPlaneState(up)
	if m.dock.Err != "" {
		t.Errorf("a plane that came back left the down message on the strip: %q", m.dock.Err)
	}

	// SOMEONE ELSE'S ERROR IS NOT OURS TO CLEAR.
	m.applyPlaneState(down)
	m.dock.SetError("some other failure")
	m.applyPlaneState(up)
	if m.dock.Err != "some other failure" {
		t.Errorf("recovery must not clear an unrelated error: %q", m.dock.Err)
	}
}

// A MISTYPED VERB IS REPORTED, NOT SILENTLY TURNED INTO A PROBE (acceptance criterion 1's "not a
// silent no-op"), and "status" is accepted because `orchicon plane status` is.
func TestPlaneUnknownVerbReported(t *testing.T) {
	m := newDockTestApp()

	if cmd := m.planeCommand("strat"); cmd != nil {
		t.Error("a mistyped verb must not arm a probe — it must report")
	}
	if !strings.Contains(m.dock.Err, "not a plane verb") || !strings.Contains(m.dock.Err, "strat") {
		t.Errorf("a mistyped verb must be named in the strip: %q", m.dock.Err)
	}
	if cmd := m.planeCommand("status"); cmd == nil {
		t.Error("/plane status must probe, matching `orchicon plane status`")
	}
}
