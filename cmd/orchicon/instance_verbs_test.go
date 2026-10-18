package main

// instance_verbs_test.go — `orchicon start|stop [instance]`, and instance targeting across the CLI.
//
// TWO THINGS ARE PINNED HERE, and they are the two that were actually wrong:
//
//  1. ORDER. start and stop are compositions, so their whole correctness is the order of the parts.
//     start must bring the services up before the plane (the plane migrates on boot, and migrations
//     must not race a half-started Postgres); stop must take the plane down before the services (a
//     plane left running over a dead database holds its port, serves nothing, and makes the next
//     start fail with a bind error that names no cause). No behavioural test can see ordering — the
//     parts are real processes — so this is a source-level pin, the same technique
//     postgres_legacy_volume_test.go uses for "the guard runs before initdb".
//
//  2. TARGETING. `install` and `restart` read only ORCHICON_INSTANCE while `plane` and `services`
//     took a positional instance, so `orchicon restart prod` restarted DEV and said nothing. Every
//     instance-addressed verb now goes through ONE parser, and this pins that none of them drifts
//     back to reading the environment alone.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstanceArgReadsAPositionalInstance(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "no argument leaves the choice to the caller", args: nil, want: ""},
		{name: "a bare instance", args: []string{"prod"}, want: "prod"},
		{name: "surrounding space is ignored", args: []string{"  test  "}, want: "test"},
		{name: "trailing arguments do not change the instance", args: []string{"dev", "extra"}, want: "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := instanceArg(c.args, "start")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("instanceArg(%q) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

// A FLAG MUST BE REFUSED, NOT IGNORED. `orchicon stop -i prod` silently acting on dev is the exact
// failure this whole change removes: the operator sees prod keep running and has no reason to suspect
// the argument was dropped.
func TestInstanceArgRefusesAFlag(t *testing.T) {
	for _, args := range [][]string{{"-i", "prod"}, {"--instance", "prod"}} {
		t.Run(args[0], func(t *testing.T) {
			got, err := instanceArg(args, "stop")
			if err == nil {
				t.Fatalf("instanceArg(%q) accepted a flag and returned %q — it must say it takes an instance name", args, got)
			}
			// The refusal has to teach the right spelling, or the operator just tries another flag.
			if !strings.Contains(err.Error(), "usage: orchicon stop [dev|prod|test]") {
				t.Errorf("refusal does not show the usage line: %v", err)
			}
			if !strings.Contains(err.Error(), "ORCHICON_INSTANCE") {
				t.Errorf("refusal does not name the environment variable alternative: %v", err)
			}
		})
	}
}

// The instance name becomes a DIRECTORY NAME, so a name that can climb out of the state dir is refused
// rather than turned into a path. Every verb shares this guard.
func TestValidInstanceNameRefusesAPathClimb(t *testing.T) {
	for _, ok := range []string{"dev", "prod", "test", "my-instance"} {
		if err := validInstanceName(ok); err != nil {
			t.Errorf("validInstanceName(%q) refused a legitimate name: %v", ok, err)
		}
	}
	for _, bad := range []string{"../etc", "a/b", `a\b`, "..", "x/../y"} {
		if err := validInstanceName(bad); err == nil {
			t.Errorf("validInstanceName(%q) allowed a name that could climb out of the instance data dir", bad)
		}
	}
}

// An instance that was never installed has nothing to start or stop, and the answer has to say how to
// install it — otherwise `orchicon start prod` on a fresh host reads as a bug in start.
//
// It also proves the record is what decides the SHAPE, which is the thing that makes one verb able to
// drive either residency.
func TestRequireInstalledReadsTheRecordedShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the instance data dir is derived from the home directory, which this test relocates with HOME")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Nothing installed yet.
	if _, err := requireInstalled("prod", "start"); err == nil {
		t.Fatal("requireInstalled accepted an instance with no install record — start would then try to " +
			"bring up a stack that was never created")
	} else {
		for _, want := range []string{`"prod"`, "orchicon install prod"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the no-record refusal must contain %q; got: %v", want, err)
			}
		}
	}

	// Record a host install, exactly as `orchicon install` does, and it resolves.
	dir := filepath.Join(home, ".local", "share", "orchicon-prod")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "residency"), []byte("host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := requireInstalled("prod", "start")
	if err != nil {
		t.Fatalf("requireInstalled refused an instance that IS installed: %v", err)
	}
	if got != residencyHost {
		t.Errorf("requireInstalled returned residency %q, want %q — the shape decides which arm of "+
			"start/stop runs, so reading it wrong starts the wrong half", got, residencyHost)
	}

	// And a neighbouring instance is still unknown: the record is PER INSTANCE, never shared.
	if _, err := requireInstalled("dev", "start"); err == nil {
		t.Error("requireInstalled found dev's record while only prod's was written — the record must be " +
			"per instance or start/stop could drive the wrong stack")
	}
}

// ── source-level pins ────────────────────────────────────────────────────────────────────────────

// verbBody returns the source of one function in a file of this package.
func verbBody(t *testing.T, file, signature string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(b)
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("%s not found in %s — update this test's anchor", signature, file)
	}
	body := src[start:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	return body
}

// START BRINGS THE SERVICES UP BEFORE THE PLANE. The plane runs migrations on boot, so a plane started
// beside a half-started Postgres migrates against a database that is not accepting connections yet.
func TestStartStartsServicesBeforeThePlane(t *testing.T) {
	body := verbBody(t, "instance_verbs.go", "func runStart(args []string) int {")

	services := strings.Index(body, "servicesUp(instance)")
	plane := strings.Index(body, "planecontrol.Start(")
	daemon := strings.Index(body, "ensureInstallDaemon(")

	if services < 0 || plane < 0 {
		t.Fatalf("runStart no longer calls both servicesUp and planecontrol.Start (services at %d, plane at %d)", services, plane)
	}
	if services > plane {
		t.Errorf("runStart starts the plane BEFORE the services (services at %d, plane at %d) — the plane "+
			"migrates on boot and would race a Postgres that is not ready", services, plane)
	}
	// The daemon is what a dispatched run needs; an instance reported as started whose work cannot
	// execute is a half-up.
	if daemon < 0 {
		t.Error("runStart does not start the runtime daemon — after a reboot a workflow run would have " +
			"nowhere to execute, while start reports success")
	}
	if daemon > services {
		t.Errorf("runStart starts the runtime daemon AFTER the services (daemon at %d, services at %d)",
			daemon, services)
	}
}

// STOP TAKES THE PLANE DOWN BEFORE THE SERVICES — the reverse of start, and for the reason
// scripts/orchicon.sh's stop_host documents: a plane still running over a dead database holds its port
// and makes the next start fail with a bind error that names no cause.
func TestStopStopsThePlaneBeforeTheServices(t *testing.T) {
	body := verbBody(t, "instance_verbs.go", "func runStop(args []string) int {")

	plane := strings.Index(body, "planecontrol.Stop(")
	services := strings.Index(body, "servicesDown(instance)")

	if plane < 0 || services < 0 {
		t.Fatalf("runStop no longer calls both planecontrol.Stop and servicesDown (plane at %d, services at %d)", plane, services)
	}
	if plane > services {
		t.Errorf("runStop stops the services BEFORE the plane (plane at %d, services at %d) — a plane left "+
			"running over a dead database is what makes a status check lie", plane, services)
	}
}

// EVERY INSTANCE-ADDRESSED VERB TAKES A POSITIONAL INSTANCE. This is the drift that existed: `plane`
// and `services` parsed one, `install` and `restart` read only the environment. A verb that silently
// ignores its argument acts on dev while the operator waits for another instance to change.
func TestEveryInstanceAddressedVerbParsesAPositionalInstance(t *testing.T) {
	for _, c := range []struct{ file, signature, verb string }{
		{"instance_verbs.go", "func runStart(args []string) int {", "start"},
		{"instance_verbs.go", "func runStop(args []string) int {", "stop"},
		{"restart.go", "func runRestart(args []string, log *slog.Logger) error {", "restart"},
		{"install.go", "func runInstall(args []string, log *slog.Logger) error {", "install"},
	} {
		t.Run(c.verb, func(t *testing.T) {
			body := verbBody(t, c.file, c.signature)
			// The PARSER is the requirement, not its spelling: two verbs pass the verb name as a
			// variable for their error text and two inline it, and pinning the literal would be a
			// test about formatting.
			if !strings.Contains(body, "instanceArg(args,") {
				t.Errorf("run%s does not consume a positional instance through instanceArg — it reads "+
					"only ORCHICON_INSTANCE, so `orchicon %s <name>` would silently act on dev",
					strings.ToUpper(c.verb[:1])+c.verb[1:], c.verb)
			}
		})
	}
}

// And the shared name guard is used by every one of them, so the path-climb rule cannot be relaxed in
// one verb and forgotten in another.
func TestEveryInstanceAddressedVerbSharesTheNameGuard(t *testing.T) {
	for _, c := range []struct{ file, signature string }{
		{"instance_verbs.go", "func runStart(args []string) int {"},
		{"instance_verbs.go", "func runStop(args []string) int {"},
		{"restart.go", "func runRestart(args []string, log *slog.Logger) error {"},
		{"install.go", "func runInstall(args []string, log *slog.Logger) error {"},
		{"plane.go", "func runPlane(args []string) error {"},
	} {
		body := verbBody(t, c.file, c.signature)
		if !strings.Contains(body, "validInstanceName(") {
			t.Errorf("%s does not use validInstanceName — the instance name becomes a directory name, and "+
				"an inline guard in one verb is how the rule drifts between them", c.signature)
		}
	}
}
