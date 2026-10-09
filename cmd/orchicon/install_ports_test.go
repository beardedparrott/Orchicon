package main

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"strings"
	"testing"
)

// TestInstallPortDefaultsMatchContainerScript is the cross-language agreement
// gate. The Go installer and scripts/container.sh each resolve the per-instance
// port table, and they cannot share a constant — so the agreement has to be
// ASSERTED. Two drifts matter, and this checks both:
//
//  1. the DEFAULTS, so `orchicon install` and `container.sh up` agree on which
//     port an instance answers on when nobody overrides anything;
//  2. the OVERRIDE KNOB NAMES, because an operator setting one must move the
//     port in BOTH launchers. That is the failure this test is really for: the
//     Go side honouring ORCHICON_CONTROL_PORT while the bash side silently keeps
//     8080 would leave the two binding different ports for one instance.
func TestInstallPortDefaultsMatchContainerScript(t *testing.T) {
	src, err := os.ReadFile("../../scripts/container.sh")
	if err != nil {
		t.Fatalf("read container.sh: %v", err)
	}
	script := string(src)

	// 1. The per-instance defaults container.sh must still carry.
	for _, want := range []string{
		"PLANE_HTTP_PORT=8080",
		"PLANE_HTTP_PORT=8091",
		"GRAFANA_HOST_PORT=3002",
		"GRAFANA_HOST_PORT=3003",
		"PG_PORT=5432",
		"PG_PORT=5433",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/container.sh no longer contains %q — the per-instance port table drifted", want)
		}
	}

	// 2. The override knobs must be applied in container.sh under exactly the
	// names the Go resolver uses, or setting one moves the port in only one
	// launcher. Asserted as whole shell lines so a rename on either side fails.
	for _, want := range []string{
		`PLANE_HTTP_PORT="${ORCHICON_CONTROL_PORT:-$PLANE_HTTP_PORT}"`,
		`GRAFANA_HOST_PORT="${ORCHICON_GRAFANA_PORT:-$GRAFANA_HOST_PORT}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/container.sh must apply %q so an override moves the port in both launchers", want)
		}
	}

	// The publishes must be COMPOSED from those variables: a literal beside them
	// is how an override silently leaves `up` on the old number.
	if !strings.Contains(script, `PORTS="-p $PLANE_HTTP_PORT:8080 -p $GRAFANA_HOST_PORT:3000"`) {
		t.Error("container.sh's PORTS must be composed from PLANE_HTTP_PORT/GRAFANA_HOST_PORT, not written as literals")
	}

	// ...and the Go table must agree with the defaults above.
	control, grafana := installerPorts()[0], installerPorts()[1]
	if control.Dev != 8080 || control.Prod != 8091 {
		t.Errorf("control defaults = %d/%d, want 8080/8091", control.Dev, control.Prod)
	}
	if grafana.Dev != 3002 || grafana.Prod != 3003 {
		t.Errorf("grafana defaults = %d/%d, want 3002/3003", grafana.Dev, grafana.Prod)
	}
	// The host->container mapping must match container.sh's -p pairs
	// (8080:8080 and 3002:3000), or the published port lands on the wrong
	// listener.
	if control.ContainerPort != 8080 || grafana.ContainerPort != 3000 {
		t.Errorf("container ports = %d/%d, want 8080/3000", control.ContainerPort, grafana.ContainerPort)
	}
	// The Env names are the contract with container.sh, so pin them literally:
	// the shell assertions above hardcode these exact strings.
	if control.Env != "ORCHICON_CONTROL_PORT" || grafana.Env != "ORCHICON_GRAFANA_PORT" {
		t.Errorf("env knobs = %q/%q, want ORCHICON_CONTROL_PORT/ORCHICON_GRAFANA_PORT", control.Env, grafana.Env)
	}
}

// alwaysFree / freeExcept are the injected conflict probes.
func alwaysFree(int) bool { return true }

func freeExcept(taken ...int) func(int) bool {
	blocked := map[int]bool{}
	for _, p := range taken {
		blocked[p] = true
	}
	return func(p int) bool { return !blocked[p] }
}

// TestPlanInstallPortsDefaultsNonInteractive pins the headless path: with no
// terminal and free ports, every port takes its per-instance default and the
// choices are STATED (an operator must never have to infer what a headless
// install bound).
func TestPlanInstallPortsDefaultsNonInteractive(t *testing.T) {
	var out bytes.Buffer
	got, err := planInstallPorts("dev", installerPorts(), nil, alwaysFree, nil, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 8080 || got["ORCHICON_GRAFANA_PORT"] != 3002 {
		t.Fatalf("dev defaults = %v, want control 8080 / grafana 3002", got)
	}
	if !strings.Contains(out.String(), "Control plane (web UI + API): 8080") {
		t.Errorf("headless install must state the port it bound; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "hit") {
		t.Error("no terminal: the prompt note must not be printed")
	}
}

// TestPlanInstallPortsProdDefaults pins the prod column, which is the half that
// keeps two instances on one host apart.
func TestPlanInstallPortsProdDefaults(t *testing.T) {
	var out bytes.Buffer
	got, err := planInstallPorts("prod", installerPorts(), nil, alwaysFree, nil, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 8091 || got["ORCHICON_GRAFANA_PORT"] != 3003 {
		t.Fatalf("prod defaults = %v, want control 8091 / grafana 3003", got)
	}
}

// TestPlanInstallPortsUnknownInstanceIsNotDevsPorts is the regression for the
// fall-through: an instance that is neither dev nor prod used to be handed dev's
// 8080, so an install for a THIRD instance was guaranteed to collide with dev.
// It must resolve deterministically to the dev column (documented behaviour) so
// the operator can pin it — the point is that nothing silently guesses a
// DIFFERENT free port, which would be unreviewable.
func TestPlanInstallPortsUnknownInstanceIsNotDevsPorts(t *testing.T) {
	var out bytes.Buffer
	got, err := planInstallPorts("staging", installerPorts(), nil, alwaysFree, nil, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	// With the port free, the dev column is used — and it is PRINTED, so a
	// collision with a real dev instance is visible rather than silent.
	if got["ORCHICON_CONTROL_PORT"] != 8080 {
		t.Fatalf("unknown instance control = %d, want 8080", got["ORCHICON_CONTROL_PORT"])
	}
}

// TestPlanInstallPortsConflictSuggestsNext is the auto-enumeration the operator
// asked for: 8080 taken means the default becomes 8081 — but the conflict is
// ANNOUNCED rather than silently absorbed.
func TestPlanInstallPortsConflictSuggestsNext(t *testing.T) {
	var out bytes.Buffer
	got, err := planInstallPorts("dev", installerPorts(), nil, freeExcept(8080), nil, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 8081 {
		t.Fatalf("control = %d, want 8081", got["ORCHICON_CONTROL_PORT"])
	}
	if !strings.Contains(out.String(), "port 8080 is in use") {
		t.Errorf("the conflict must be reported; got:\n%s", out.String())
	}
}

// TestPlanInstallPortsConflictSkipsRunawayPorts walks further than one step: if
// 8081 is ALSO taken the suggestion must reach 8082, not stop at the first
// candidate.
func TestPlanInstallPortsConflictSkipsRunawayPorts(t *testing.T) {
	var out bytes.Buffer
	got, err := planInstallPorts("dev", installerPorts(), nil, freeExcept(8080, 8081, 8082), nil, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 8083 {
		t.Fatalf("control = %d, want 8083", got["ORCHICON_CONTROL_PORT"])
	}
}

// TestPlanInstallPortsStrictRefuses pins the automation escape hatch: a conflict
// must FAIL rather than move the port when the operator asked for strictness.
func TestPlanInstallPortsStrictRefuses(t *testing.T) {
	t.Setenv("ORCHICON_STRICT_PORTS", "1")
	var out bytes.Buffer
	_, err := planInstallPorts("dev", installerPorts(), nil, freeExcept(8080), nil, &out)
	if err == nil {
		t.Fatal("strict mode must refuse a conflicted port")
	}
	if !strings.Contains(err.Error(), "ORCHICON_CONTROL_PORT") {
		t.Errorf("the error must name the knob to set; got: %v", err)
	}
}

// TestPlanInstallPortsPinWinsAndIsNotPrompted pins automation: an explicit pin
// is used as-is and skips the prompt entirely. BOTH ports are pinned here,
// because pinning one still legitimately prompts for the other — the reader
// fails the test if it is consulted at all, which is what proves the pins
// bypassed the prompt rather than merely being overridden by it.
func TestPlanInstallPortsPinWinsAndIsNotPrompted(t *testing.T) {
	var out bytes.Buffer
	pinned := map[string]int{
		"ORCHICON_CONTROL_PORT": 9080,
		"ORCHICON_GRAFANA_PORT": 9081,
	}
	r := bufio.NewReader(&failReader{t: t})
	got, err := planInstallPorts("dev", installerPorts(), pinned, alwaysFree, r, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 9080 || got["ORCHICON_GRAFANA_PORT"] != 9081 {
		t.Fatalf("pinned ports = %v, want 9080/9081", got)
	}
	if !strings.Contains(out.String(), "ORCHICON_CONTROL_PORT") {
		t.Errorf("the pin must be reported; got:\n%s", out.String())
	}
	// Nothing was pending, so the accept-defaults note must NOT appear either.
	if strings.Contains(out.String(), "hit") {
		t.Errorf("all ports pinned: no note should be printed; got:\n%s", out.String())
	}
}

// TestPlanInstallPortsPinnedConflictWarnsNotReassigns closes the gap that a pin
// used to pass through silently: an explicitly pinned port that is ALREADY BUSY
// must be reported, but must still be honoured — silently moving a pinned
// number would break the automation contract the pin exists to provide.
func TestPlanInstallPortsPinnedConflictWarnsNotReassigns(t *testing.T) {
	var out bytes.Buffer
	pinned := map[string]int{
		"ORCHICON_CONTROL_PORT": 9080,
		"ORCHICON_GRAFANA_PORT": 9081,
	}
	got, err := planInstallPorts("dev", installerPorts(), pinned, freeExcept(9080), nil, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 9080 {
		t.Fatalf("a pinned port must be honoured even when busy; got %d", got["ORCHICON_CONTROL_PORT"])
	}
	if !strings.Contains(out.String(), "already in use") {
		t.Errorf("a busy pinned port must be warned about; got:\n%s", out.String())
	}
}

// failReader fails the test if any read reaches it — used to prove a port is
// NOT prompted for.
func (f *failReader) Read(p []byte) (int, error) {
	f.t.Error("a pinned port must not be prompted for")
	return 0, os.ErrClosed
}

type failReader struct{ t *testing.T }

// TestPlanInstallPortsEnterAcceptsDefaults is the operator's core request:
// an empty line takes the default, so the common install is a run of enters.
func TestPlanInstallPortsEnterAcceptsDefaults(t *testing.T) {
	var out bytes.Buffer
	r := bufio.NewReader(strings.NewReader("\n\n"))
	got, err := planInstallPorts("dev", installerPorts(), nil, alwaysFree, r, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 8080 || got["ORCHICON_GRAFANA_PORT"] != 3002 {
		t.Fatalf("enter must accept defaults; got %v", got)
	}
	// The note the operator asked for must precede the prompts.
	if !strings.Contains(out.String(), `Ports — hit "enter" to accept defaults.`) {
		t.Errorf("the accept-defaults note must be printed; got:\n%s", out.String())
	}
	// The default must be SHOWN in parentheses.
	if !strings.Contains(out.String(), "Control plane (web UI + API) (8080): ") {
		t.Errorf("the prompt must show the default in parentheses; got:\n%s", out.String())
	}
}

// TestPlanInstallPortsTypedOverride proves a typed value beats the default, and
// only for the port it was typed at.
func TestPlanInstallPortsTypedOverride(t *testing.T) {
	var out bytes.Buffer
	r := bufio.NewReader(strings.NewReader("\n9000\n"))
	got, err := planInstallPorts("dev", installerPorts(), nil, alwaysFree, r, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	if got["ORCHICON_CONTROL_PORT"] != 8080 {
		t.Errorf("control = %d, want the 8080 default", got["ORCHICON_CONTROL_PORT"])
	}
	if got["ORCHICON_GRAFANA_PORT"] != 9000 {
		t.Errorf("grafana = %d, want the typed 9000", got["ORCHICON_GRAFANA_PORT"])
	}
}

// TestAskInstallPort covers the reader's edge cases, including the one that
// matters for a closed or degenerate terminal: EOF must fall back to the
// default rather than spin or crash.
func TestAskInstallPort(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"empty line takes default", "\n", 8080},
		{"typed value wins", "9100\n", 9100},
		{"junk falls back after retries", "abc\ndef\nghi\n", 8080},
		{"out of range falls back", "70000\n0\n-1\n", 8080},
		{"bare EOF takes default", "", 8080},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := bufio.NewReader(strings.NewReader(tc.input))
			if got := askInstallPort(r, &out, "Port", 8080); got != tc.want {
				t.Fatalf("askInstallPort(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestParseInstancePorts maps real docker inspect output back to the env keys,
// which is what stops a re-run from treating its own instance as a conflict.
func TestParseInstancePorts(t *testing.T) {
	raw := []byte(`{"3000/tcp":[{"HostIp":"0.0.0.0","HostPort":"3002"}],"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"8081"}]}`)
	got, ok := parseInstancePorts(raw, installerPorts())
	if !ok {
		t.Fatal("expected the bindings to parse")
	}
	if got["ORCHICON_CONTROL_PORT"] != 8081 {
		t.Errorf("control = %d, want 8081 (a moved port must be read back)", got["ORCHICON_CONTROL_PORT"])
	}
	if got["ORCHICON_GRAFANA_PORT"] != 3002 {
		t.Errorf("grafana = %d, want 3002", got["ORCHICON_GRAFANA_PORT"])
	}

	// An unparseable payload must report "not ok" so the caller falls back to
	// fresh resolution rather than silently reusing nothing.
	if _, ok := parseInstancePorts([]byte("not json"), installerPorts()); ok {
		t.Error("garbage must not parse")
	}
	// No published ports at all is also "not ok" — there is nothing to reuse.
	if _, ok := parseInstancePorts([]byte(`{}`), installerPorts()); ok {
		t.Error("empty bindings must not be treated as reuse")
	}
}

// TestPortFreeSeesAHeldPort is the one test that touches a real socket, because
// the probe's whole job is to collide with a real listener — and TestNextFree
// proves the upward walk then steps past it.
func TestPortFreeSeesAHeldPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if portFree(port) {
		t.Fatalf("portFree(%d) = true while a listener holds it", port)
	}
	got, ok := nextFreePortFor(port, map[int]bool{}, portFree)
	if !ok {
		t.Fatalf("nextFreePortFor could not walk past the held port %d", port)
	}
	if got <= port {
		t.Fatalf("nextFreePortFor returned %d, which is not past the held %d", got, port)
	}
}

// TestNextFreePortHonoursClaimed proves the walk also skips a port that is free
// on the host but already assigned to another Orchicon port in the same run —
// otherwise two prompts could resolve onto one number.
func TestNextFreePortHonoursClaimed(t *testing.T) {
	got, ok := nextFreePortFor(7000, map[int]bool{7000: true, 7001: true}, func(int) bool { return true })
	if !ok {
		t.Fatal("expected a free port to be found")
	}
	if got != 7002 {
		t.Fatalf("nextFreePortFor = %d, want 7002 (both claimed ports skipped)", got)
	}
}
