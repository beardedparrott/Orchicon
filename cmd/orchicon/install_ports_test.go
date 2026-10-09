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

	// 1. The per-instance defaults container.sh must still carry. The service
	// entries matter as much as the plane ones now: host residency PUBLISHES them,
	// so a drift there means the two launchers bind different loopback ports.
	for _, want := range []string{
		"PLANE_HTTP_PORT=8080",
		"PLANE_HTTP_PORT=8091",
		"PLANE_HTTP_PORT=8092",
		"GRAFANA_HOST_PORT=3002",
		"GRAFANA_HOST_PORT=3003",
		"GRAFANA_HOST_PORT=3004",
		"PG_PORT=5432",
		"PG_PORT=5433",
		"PG_PORT=5434",
		"NATS_PORT=4222",
		"NATS_PORT=4223",
		"NATS_PORT=4224",
		"NATS_MON_PORT=8222",
		"NATS_MON_PORT=8223",
		"NATS_MON_PORT=8224",
		"OTLP_GRPC_PORT=4317",
		"OTLP_GRPC_PORT=4319",
		"OTLP_GRPC_PORT=4321",
		"OTLP_HTTP_PORT=4318",
		"OTLP_HTTP_PORT=4320",
		"OTLP_HTTP_PORT=4322",
		"TEMPO_PORT=3200",
		"TEMPO_PORT=3201",
		"TEMPO_PORT=3202",
		"LOKI_PORT=3100",
		"LOKI_PORT=3101",
		"LOKI_PORT=3102",
		"VM_PORT=8428",
		"VM_PORT=8429",
		"VM_PORT=8430",
		// The host state dir carries the KEK. Two launchers disagreeing about it is
		// the one disagreement that silently orphans every tenant secret.
		`HOST_DATA_DIR="$HOME/.local/share/orchicon-dev"`,
		`HOST_DATA_DIR="$HOME/.local/share/orchicon-prod"`,
		`HOST_DATA_DIR="$HOME/.local/share/orchicon-test"`,
		// The third instance must actually exist on the bash side, or the Go table
		// would define a column the launcher refuses.
		"    test)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/container.sh no longer contains %q — the per-instance port table drifted", want)
		}
	}

	// The container-side ports this table mirrors must match container.sh's
	// SERVICE_PORTS composition: these are the in-container listeners a host
	// publish maps ONTO, so a wrong pair sends a host port to the wrong service.
	for _, want := range []string{
		`SERVICE_PORTS="-p 127.0.0.1:$PG_PORT:5432"`,
		`-p 127.0.0.1:$NATS_PORT:4222 -p 127.0.0.1:$NATS_MON_PORT:8222`,
		`-p 127.0.0.1:$OTLP_GRPC_PORT:4317 -p 127.0.0.1:$OTLP_HTTP_PORT:4318`,
		`-p 127.0.0.1:$TEMPO_PORT:3200 -p 127.0.0.1:$LOKI_PORT:3100`,
		`-p 127.0.0.1:$VM_PORT:8428 -p 127.0.0.1:$GRAFANA_HOST_PORT:3000`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scripts/container.sh's SERVICE_PORTS no longer contains %q — the host→container mapping drifted", want)
		}
	}

	// ...and EVERY entry of the Go table must agree with those defaults. The
	// expected map is the gate: an entry added to installerPorts without being
	// listed here fails, so a new port cannot skip the cross-language check.
	type wantCase struct{ dev, prod, test, container int }
	wantDefaults := map[string]wantCase{
		"ORCHICON_CONTROL_PORT":      {8080, 8091, 8092, 8080},
		"ORCHICON_GRAFANA_PORT":      {3002, 3003, 3004, 3000},
		"ORCHICON_POSTGRES_PORT":     {5432, 5433, 5434, 5432},
		"ORCHICON_NATS_PORT":         {4222, 4223, 4224, 4222},
		"ORCHICON_NATS_MONITOR_PORT": {8222, 8223, 8224, 8222},
		"ORCHICON_OTLP_GRPC_PORT":    {4317, 4319, 4321, 4317},
		"ORCHICON_OTLP_HTTP_PORT":    {4318, 4320, 4322, 4318},
		"ORCHICON_TEMPO_PORT":        {3200, 3201, 3202, 3200},
		"ORCHICON_LOKI_PORT":         {3100, 3101, 3102, 3100},
		"ORCHICON_VM_PORT":           {8428, 8429, 8430, 8428},
	}
	for _, p := range installerPorts() {
		w, ok := wantDefaults[p.Env]
		if !ok {
			t.Errorf("installerPorts() entry %s is not in this test's table — add it so the drift check covers it", p.Env)
			continue
		}
		if p.Defaults["dev"] != w.dev || p.Defaults["prod"] != w.prod || p.Defaults["test"] != w.test {
			t.Errorf("%s defaults = %d/%d/%d, want %d/%d/%d (container.sh's values)",
				p.Env, p.Defaults["dev"], p.Defaults["prod"], p.Defaults["test"], w.dev, w.prod, w.test)
		}
		if p.ContainerPort != w.container {
			t.Errorf("%s container port = %d, want %d (container.sh's -p pair)", p.Env, p.ContainerPort, w.container)
		}
		delete(wantDefaults, p.Env)
	}
	for env := range wantDefaults {
		t.Errorf("installerPorts() is missing %s", env)
	}

	// STRUCTURAL: every row must define EVERY column. This is the check that
	// catches a row gaining a column on one side only, and it is what a third
	// instance makes easy to get wrong — a row copied without its new entry looks
	// complete and resolves to a zero default.
	for _, p := range installerPorts() {
		for _, inst := range instanceOrder {
			if _, ok := p.Defaults[inst]; !ok {
				t.Errorf("%s has no default for instance %q — every column must be defined", p.Env, inst)
			}
		}
	}

	// DISJOINTNESS: no two instances may share a port on the same row. The whole
	// point of the table is that one host can run these side by side, and a shared
	// port is exactly the collision it exists to prevent — easy to introduce by
	// copying a row and forgetting to increment.
	for _, p := range installerPorts() {
		seen := map[int]string{}
		for _, inst := range instanceOrder {
			v := p.Defaults[inst]
			if prev, dup := seen[v]; dup {
				t.Errorf("%s: instances %s and %s share port %d — the columns must stay disjoint", p.Env, prev, inst, v)
			}
			seen[v] = inst
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
	if control.Defaults["dev"] != 8080 || control.Defaults["prod"] != 8091 || control.Defaults["test"] != 8092 {
		t.Errorf("control defaults = %v, want dev 8080 / prod 8091 / test 8092", control.Defaults)
	}
	if grafana.Defaults["dev"] != 3002 || grafana.Defaults["prod"] != 3003 || grafana.Defaults["test"] != 3004 {
		t.Errorf("grafana defaults = %v, want dev 3002 / prod 3003 / test 3004", grafana.Defaults)
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

// TestPlanInstallPortsUnknownInstanceIsRefused replaces the old
// fall-through-to-dev behaviour — which is precisely the bug the table now
// prevents. An instance the table does not define was handed dev's 8080, so
// adding a THIRD instance silently collided with dev on every port. It must now
// be an explicit refusal that names the knob to set.
func TestPlanInstallPortsUnknownInstanceIsRefused(t *testing.T) {
	var out bytes.Buffer
	_, err := planInstallPorts("staging", installerPorts(), nil, alwaysFree, nil, &out)
	if err == nil {
		t.Fatal("an unknown instance must be refused, not silently given another instance's ports")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("the error must name the instance; got: %v", err)
	}
	if !strings.Contains(err.Error(), "ORCHICON_CONTROL_PORT") {
		t.Errorf("the error must name the knob to pin; got: %v", err)
	}
}

// TestResolveInstallPortsRefusesUnknownInstance pins the refusal at the resolver,
// BEFORE anything is probed or asked — so a typo cannot walk the operator through
// a prompt sequence and then fail.
func TestResolveInstallPortsRefusesUnknownInstance(t *testing.T) {
	var out bytes.Buffer
	if _, err := resolveInstallPorts("staging", residencyHost, &out, nil); err == nil {
		t.Fatal("resolveInstallPorts must refuse an instance the table does not define")
	}
}

// TestValidateInstanceAcceptsExactlyTheTableColumns pins that every documented
// column resolves and nothing else does.
func TestValidateInstanceAcceptsExactlyTheTableColumns(t *testing.T) {
	for _, inst := range instanceOrder {
		if err := validateInstance(inst); err != nil {
			t.Errorf("validateInstance(%q): %v", inst, err)
		}
	}
	if err := validateInstance("nope"); err == nil {
		t.Error("validateInstance must reject an instance outside instanceOrder")
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
// is used as-is and skips the prompt entirely. EVERY active port is pinned here,
// because a pin covers one port and the others would still legitimately prompt —
// the reader fails the test if it is consulted at all, which is what proves the
// pins bypassed the prompt rather than merely being overridden by it.
func TestPlanInstallPortsPinWinsAndIsNotPrompted(t *testing.T) {
	specs := activeInstallPorts(residencyHost)
	var out bytes.Buffer
	pinned := map[string]int{}
	want := 9080
	for _, p := range specs {
		pinned[p.Env] = want
		want++
	}
	r := bufio.NewReader(&failReader{t: t})
	got, err := planInstallPorts("dev", specs, pinned, alwaysFree, r, &out)
	if err != nil {
		t.Fatalf("planInstallPorts: %v", err)
	}
	for _, p := range specs {
		if got[p.Env] != pinned[p.Env] {
			t.Fatalf("%s = %d, want the pinned %d", p.Env, got[p.Env], pinned[p.Env])
		}
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

// TestParseInstancePortsRealHostResidencyBindings uses the binding set observed
// on a REAL host-resident instance (captured from `docker inspect` on a live dev
// instance), because the shape the parser will actually meet is the one worth
// pinning.
//
// The `8080/tcp` entry is `null` — exposed but NOT published, because the host
// plane owns that port — and the whole point is that it must NOT be read back as
// a value: reporting it would invent a container-published port that does not
// exist, and the control port's real value has to come from resolving the
// override instead.
func TestParseInstancePortsRealHostResidencyBindings(t *testing.T) {
	raw := []byte(`{
		"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"3002"}],
		"3100/tcp":[{"HostIp":"127.0.0.1","HostPort":"3100"}],
		"3200/tcp":[{"HostIp":"127.0.0.1","HostPort":"3200"}],
		"4222/tcp":[{"HostIp":"127.0.0.1","HostPort":"4222"}],
		"4317/tcp":[{"HostIp":"127.0.0.1","HostPort":"4317"}],
		"4318/tcp":[{"HostIp":"127.0.0.1","HostPort":"4318"}],
		"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5432"}],
		"8080/tcp":null,
		"8222/tcp":[{"HostIp":"127.0.0.1","HostPort":"8222"}],
		"8428/tcp":[{"HostIp":"127.0.0.1","HostPort":"8428"}]
	}`)
	got, ok := parseInstancePorts(raw, activeInstallPorts(residencyHost))
	if !ok {
		t.Fatal("the real binding set must parse")
	}

	// The plane port is exposed-but-null in host residency: not a read-back value.
	if _, invented := got["ORCHICON_CONTROL_PORT"]; invented {
		t.Errorf("the plane port is not published in host residency; it must not be read back (got %d)", got["ORCHICON_CONTROL_PORT"])
	}

	// Every published service port must be read back, keyed by env var.
	for env, want := range map[string]int{
		"ORCHICON_GRAFANA_PORT":      3002,
		"ORCHICON_POSTGRES_PORT":     5432,
		"ORCHICON_NATS_PORT":         4222,
		"ORCHICON_NATS_MONITOR_PORT": 8222,
		"ORCHICON_OTLP_GRPC_PORT":    4317,
		"ORCHICON_OTLP_HTTP_PORT":    4318,
		"ORCHICON_TEMPO_PORT":        3200,
		"ORCHICON_LOKI_PORT":         3100,
		"ORCHICON_VM_PORT":           8428,
	} {
		if got[env] != want {
			t.Errorf("%s read back as %d, want %d", env, got[env], want)
		}
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
