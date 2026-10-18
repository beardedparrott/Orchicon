package main

// reclaim_test.go — taking back the ports an instance's OWN leftovers hold, and NOT touching anyone else's.
//
// THE FAILURE THESE PIN. `make rebuild-dev` could not complete on a host that had ever run a container-era
// instance:
//
//	orchicon services: dev did not release postgres :5432 and nats :4222
//	  — an orphaned process still holds the port (find it with `ss -ltnp` / `lsof -i :5432` ...)
//
// printed while `orchicon-cnt-dev` — a container THIS PRODUCT created, under OUR naming convention — was
// running and publishing both ports. The teardown only knew about host services, so nothing would ever stop
// it: the host was permanently stuck behind its own container, with an error telling the operator to go
// look for it.
//
// The other half of the rule is just as important, and it is why this is not a "kill whatever holds the
// port" feature: 5432 is a postgres port. The holder may be the operator's own database, a sibling
// instance, or a stranger's service. So the tests below pin BOTH halves — that our own container is
// reclaimed, and that identification stays narrow enough to never touch something else.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The container name is a CONVENTION shared with the installer and the lifecycle verbs; a near-match is a
// different instance, and reclaim must never widen it into a prefix or pattern match.
func TestInstanceContainerNameIsExact(t *testing.T) {
	if got := instanceContainerName("dev"); got != "orchicon-cnt-dev" {
		t.Errorf("instanceContainerName(dev) = %q, want the installer's convention", got)
	}
	// Two instances must never collide: stopping "test" must not compute "dev"'s name.
	for _, pair := range [][2]string{{"dev", "prod"}, {"prod", "test"}, {"test", "dev"}} {
		a, b := instanceContainerName(pair[0]), instanceContainerName(pair[1])
		if a == b {
			t.Errorf("instances %q and %q share container name %q", pair[0], pair[1], a)
		}
	}
}

// isOurBinary is the FIRST half of the identity check. It must not accept a postgres binary, or any binary
// that merely contains the product name.
func TestIsOurBinaryAcceptsOnlyOurExecutables(t *testing.T) {
	for _, yes := range []string{
		"/home/u/.local/bin/orchicon",
		"/home/u/projects/Orchicon/bin/orchicon (deleted)", // a plane whose binary was rebuilt underneath it
		"/home/u/.local/bin/orch",
	} {
		if !isOurBinary(yes) {
			t.Errorf("isOurBinary(%q) = false; that is one of our own executables", yes)
		}
	}
	for _, no := range []string{
		"/usr/lib/postgresql/16/bin/postgres",
		"/usr/bin/docker",
		"/usr/bin/passt.avx2",
		"/opt/orchicon-wrapper/other-binary",
		"",
	} {
		if isOurBinary(no) {
			t.Errorf("isOurBinary(%q) = true; reclaiming that would kill something this product does not own", no)
		}
	}
}

// ourInstanceProcesses requires BOTH signals — our binary AND this instance's name. Either alone is too
// wide: our binary alone matches a SIBLING instance's plane (stopping test must not take down dev), and a
// name alone matches anything that happens to mention it.
//
// Exercised against a real process: this test spawns a short-lived sleep with a fake /proc-visible identity
// is not possible without root, so the assertion is on the DECISION FUNCTION's inputs instead — that is
// where the bug would be, and it is pure.
func TestOurInstanceProcessesNeedsOurBinaryAndTheInstanceName(t *testing.T) {
	// The matching rule, stated once so a future edit has to change it deliberately.
	match := func(exe, environ, args, instance string) bool {
		if !isOurBinary(exe) {
			return false
		}
		return strings.Contains(environ, "ORCHICON_INSTANCE="+instance) || strings.Contains(args, instance)
	}

	if !match("/b/orchicon", "ORCHICON_INSTANCE=dev\x00", "orchicon serve", "dev") {
		t.Error("our own plane for dev was not recognised as dev's process")
	}
	// A SIBLING INSTANCE. This is the case that makes the name check mandatory.
	if match("/b/orchicon", "ORCHICON_INSTANCE=test\x00", "orchicon serve", "dev") {
		t.Error("a TEST plane matched a request to reclaim DEV — stopping one instance would kill the other")
	}
	// NOT OURS AT ALL — the case that makes the binary check mandatory.
	if match("/usr/lib/postgresql/16/bin/postgres", "ORCHICON_INSTANCE=dev\x00", "-D /data", "dev") {
		t.Error("an unrelated postgres carrying the instance name in its environment matched; reclaim must " +
			"never kill a database this product did not start")
	}
}

// portHolders must be silent rather than wrong when /proc cannot answer, and must not invent a holder for a
// port nothing is listening on. Both are properties of the FAILURE MESSAGE: naming the wrong pid is worse
// than naming none.
func TestPortHoldersFindsNothingForAFreePort(t *testing.T) {
	// A port in the ephemeral range that this test certainly does not hold.
	if got := portHolders(1); len(got) != 0 {
		t.Errorf("portHolders(1) reported %d holder(s) for a port nothing listens on: %+v", len(got), got)
	}
	// And a nonsense port must not panic or scan forever.
	if got := portHolders(0, -5); len(got) != 0 {
		t.Errorf("portHolders with invalid ports returned %+v", got)
	}
}

// THE RECORDED ANSWER. A rebuild runs install, and install used to ask which runtime images to fetch EVERY
// time — so a developer who had declined once was asked again on every rebuild, and a script or CI had
// nobody to answer. The answer is now recorded per instance, like ports.json and the residency record.
func TestRuntimeImagesAnswerIsRecordedNotReasked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A HUMAN IS PRESENT, stated rather than assumed. This test is about the RECORD, so it must reach the
	// prompt at all — and the install now refuses to prompt when its stdout is not a terminal (a test's
	// stdout is a pipe), which is the fix in interactive_gate_test.go. Declaring interactivity is the same
	// thing install.ps1 does for the Windows wrapper.
	t.Setenv("ORCHICON_INSTALL_INTERACTIVE", "1")
	dir := filepath.Join(home, ".local", "share", "orchicon-dev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// First run: nothing recorded, so the prompt is reached. Stdin says "none".
	var out bytes.Buffer
	got, err := resolveInstallRuntimeImagesFor("dev", &out, strings.NewReader("\n"), "latest", false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("first run with an empty answer fetched %d image(s), want none", len(got))
	}
	if !strings.Contains(out.String(), "Download any runtime images now?") {
		t.Error("the first run never asked — the record must not exist yet")
	}

	// Record the decline, as the install does.
	recordRuntimeImagesAnswer("dev", "none")

	// Second run: must NOT ask, and must say why.
	out.Reset()
	got, err = resolveInstallRuntimeImagesFor("dev", &out, strings.NewReader("\n"), "latest", false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if strings.Contains(out.String(), "Download any runtime images now?") {
		t.Errorf("the install asked again on a rebuild it had already answered:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "not asking again") {
		t.Errorf("the second run did not say the question was already answered:\n%s", out.String())
	}
	if len(got) != 0 {
		t.Errorf("a recorded 'none' still fetched %d image(s)", len(got))
	}

	// --force (the env override) must still reach the prompt, so a deliberate change is possible.
	out.Reset()
	if _, err := resolveInstallRuntimeImagesFor("dev", &out, strings.NewReader("\n"), "latest", true); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if !strings.Contains(out.String(), "Download any runtime images now?") {
		t.Error("ORCHICON_RUNTIME_IMAGES_ASK must force the question back — otherwise the record is a trap")
	}

	// And the record is PER INSTANCE: answering dev must not answer prod.
	if p, err := runtimeImagesAnsweredPath("prod"); err == nil {
		if _, statErr := os.Stat(p); statErr == nil {
			t.Error("recording dev's answer created prod's record — the prompt would then be skipped on an " +
				"instance that was never asked")
		}
	}
}

// THE WIRING. The functions above are only useful if the teardown actually calls them — the original defect
// was not a wrong decision, it was a decision nothing made. Source-level pins, because driving servicesDown
// end to end needs real services bound to real ports.
func TestTeardownReclaimsItsOwnLeftovers(t *testing.T) {
	for _, tc := range []struct {
		file string
		fn   string
		want string
		why  string
	}{
		{"services.go", "func servicesDown(instance string, force bool) int {", "stopInstanceContainer(instance",
			"`services down` (what make instance-down and instance-rebuild call) must stop this instance's own\n" +
				"container when the ports do not come free — otherwise a container-era instance can never be rebuilt"},
		{"services.go", "func servicesDown(instance string, force bool) int {", "forceKillOurLeftovers(instance",
			"`--force` must be able to terminate this instance's leftover processes"},
		{"lifecycle.go", "func runStop(args []string) error {", "stopInstanceContainer(instance",
			"`orchicon stop <instance>` must leave the instance's ports free, which means stopping its own container"},
	} {
		body := functionBody(t, tc.file, tc.fn)
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s\n  (%s does not call %s)", tc.why, tc.fn, tc.want)
		}
	}

	// AND THE BOUNDARY MUST HOLD IN THE CALL SITE: the force path may only reach our own leftovers, never a
	// port number. Asserted as an absence — `portHolders` may be REPORTED, never killed through.
	body := functionBody(t, "services.go", "func servicesDown(instance string, force bool) int {")
	if strings.Contains(body, "terminateProcess(") {
		t.Error("servicesDown calls terminateProcess directly — every kill must go through forceKillOurLeftovers,\n" +
			"which checks the process is ours first; a direct call could signal anything")
	}
}
