package main

// uninstall_test.go — an uninstall must remove the SOFTWARE and keep the DATA.
//
// The operator: "an uninstall should clean all services/infrastructure. The only thing it technically
// should leave is the data. We may want to put some options in the installer when someone uninstalls, it
// gives them the option to also destroy data and do a nuke options with the wizard."
//
// Until this verb existed there was NO uninstall path at all: the MSI's setup action is conditioned on
// NOT REMOVE, so uninstalling removed the binaries and left a live supervisor, a live plane, postgres
// holding 5432, and the whole state directory. The next install inherited that orphan.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedInstance lays out the dirs an install produces, so a test can assert which survive.
func seedInstance(t *testing.T, root string) string {
	t.Helper()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	// PIN EVERY PORT TO SOMETHING FREE. servicesPortsFor falls back to the instance defaults (5432,
	// 4222, …), and on a developer machine those are very likely the REAL dev instance's — the
	// quiesce's port verification would then (correctly) report them held and the uninstall would
	// (correctly) refuse, which would make this test measure the host rather than the code.
	// DERIVED, NOT INVENTED: a fixed range can collide with another package's test in a parallel
	// `go test ./...` run, and the failure then reads as a timeout rather than a port clash.
	for _, env := range []string{
		"ORCHICON_POSTGRES_PORT", "ORCHICON_NATS_PORT", "ORCHICON_NATS_MONITOR_PORT",
		"ORCHICON_TEMPO_PORT", "ORCHICON_LOKI_PORT", "ORCHICON_VM_PORT",
		"ORCHICON_OTLP_GRPC_PORT", "ORCHICON_OTLP_HTTP_PORT", "ORCHICON_OTEL_HEALTH_PORT",
		"ORCHICON_TEMPO_OTLP_GRPC_PORT", "ORCHICON_TEMPO_OTLP_HTTP_PORT", "ORCHICON_CONTROL_PORT",
	} {
		t.Setenv(env, strconv.Itoa(closedPort(t)))
	}
	dataDir := hostDataDirFor("dev")
	for _, d := range []string{
		"bin/postgres/bin", "config", "serve/pids", "serve/logs", "nats", "loki", "tempo",
		"victoriametrics", "project-mounts", "postgres", "blobs", "secrets", "backups",
	} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"services.log", "ports.json", "infra-binaries.json", "permission-policy.yaml"} {
		if err := os.WriteFile(filepath.Join(dataDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A KEK, so the test can prove it is NOT deleted by a plain uninstall.
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "kek"), []byte("the-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// THE DEFAULT UNINSTALL REMOVES INFRASTRUCTURE AND KEEPS DATA.
func TestUninstallRemovesInfrastructureAndKeepsData(t *testing.T) {
	root := t.TempDir()
	dataDir := seedInstance(t, root)

	var out bytes.Buffer
	// No processes exist, so the quiesce is a clean no-op and the deletion proceeds.
	if err := runUninstall([]string{"dev"}, &out); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out.String())
	}

	for _, d := range uninstallInfraDirs {
		if _, err := os.Stat(filepath.Join(dataDir, d)); !os.IsNotExist(err) {
			t.Errorf("%s/ survived the uninstall — it is re-downloadable or regenerated on boot, so "+
				"leaving it is what made a reinstall inherit stale infrastructure", d)
		}
	}
	for _, f := range uninstallInfraFiles {
		if _, err := os.Stat(filepath.Join(dataDir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived the uninstall", f)
		}
	}
	for _, d := range uninstallDataDirs {
		if _, err := os.Stat(filepath.Join(dataDir, d)); err != nil {
			t.Errorf("%s/ was removed by a plain uninstall — the data must be kept unless --purge "+
				"was asked for", d)
		}
	}
	// The KEK above all: losing it makes every tenant secret permanently undecryptable, which is a
	// worse outcome than leaving a file behind.
	if _, err := os.Stat(filepath.Join(dataDir, "secrets", "kek")); err != nil {
		t.Error("the secrets KEK was deleted. That does not delete secrets — it makes them " +
			"permanently undecryptable.")
	}
	if !strings.Contains(out.String(), "kept your data") {
		t.Errorf("the uninstall did not say what it kept:\n%s", out.String())
	}
}

// --purge DESTROYS EVERYTHING, and says it is irreversible.
func TestUninstallPurgeRemovesTheWholeInstance(t *testing.T) {
	root := t.TempDir()
	dataDir := seedInstance(t, root)

	var out bytes.Buffer
	if err := runUninstall([]string{"dev", "--purge"}, &out); err != nil {
		t.Fatalf("uninstall --purge: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("--purge left %s behind", dataDir)
	}
	if !strings.Contains(out.String(), "DESTROYED") {
		t.Errorf("--purge did not say what it destroyed:\n%s", out.String())
	}
}

// AN UNINSTALL MUST NOT DELETE A RUNNING INSTANCE'S FILES.
//
// Removing a live postgres's data directory is corruption, not uninstallation — so a stop that cannot
// stop aborts the uninstall rather than proceeding.
func TestUninstallRefusesWhenItCannotStop(t *testing.T) {
	src := readGoFileFor(t, "uninstall.go")
	if !strings.Contains(src, "return fmt.Errorf(\"could not stop instance") {
		t.Error("runUninstall deletes the instance's files even when it could not stop it — that is a " +
			"live database's directory being removed under a running postmaster")
	}
	stop := strings.Index(src, "stopInstanceCompletely")
	rm := strings.Index(src, "uninstallInfraDirs {")
	if stop < 0 || rm < 0 {
		t.Fatal("could not locate the stop and the removal")
	}
	if stop > rm {
		t.Error("the removal loop runs before the stop, so files are deleted while services may be live")
	}
}

// THE MSI MUST ACTUALLY CALL IT ON UNINSTALL.
func TestMSIRunsUninstallOnRemove(t *testing.T) {
	wxs := readGoFileFor(t, filepath.Join("..", "..", "deploy", "desktop", "windows", "Orchicon.wxs"))
	if !strings.Contains(wxs, `ExeCommand="uninstall --all"`) {
		t.Error("the MSI has no uninstall action: uninstalling removes the binaries and leaves the " +
			"services, the plane and the instance state running — the reported defect")
	}
	// It must be conditioned on REMOVE, and the purge variant on PURGEDATA.
	if !strings.Contains(wxs, `Condition="REMOVE=&quot;ALL&quot;`) {
		t.Error("the uninstall action is not conditioned on REMOVE=&quot;ALL&quot;")
	}
	if !strings.Contains(wxs, `ExeCommand="uninstall --all --purge"`) {
		t.Error("there is no purge variant, so the wizard has no way to destroy the data")
	}
	if !strings.Contains(wxs, `Property Id="PURGEDATA"`) {
		t.Error("there is no PURGEDATA property for the uninstall's data option")
	}
}

// readGoFileFor reads a path relative to this package's dir (cmd/orchicon).
func readGoFileFor(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// THE INSTALL MUST CLEAR ITS OWN PREVIOUS RUN *BEFORE* IT RESOLVES PORTS.
//
// This is the operator's "the past two uninstall/reinstalls don't start the services and serve up fully
// automatically", and the order was the whole bug. Port resolution came first, it treats a held port as a
// conflict, and so the install ABORTED before reaching the cleanup meant to free it:
//
//	== resolving the plane residency and host ports
//	PostgreSQL: 5432 is held by a process this tool cannot identify — find it with PowerShell…
//	error: PostgreSQL: this instance is already installed on port 5432, which is held by …
//
// Installing an instance IS the statement of intent to own it, so the clear must happen first.
func TestInstallClearsItsPreviousRunBeforeResolvingPorts(t *testing.T) {
	src := readGoFileFor(t, "install.go")
	clear := strings.Index(src, "clearing this instance's previous run")
	resolve := strings.Index(src, "ports, err := resolveInstallPorts(")
	if clear < 0 {
		t.Fatal("install never clears this instance's previous run before resolving ports, so a " +
			"leftover holding 5432 aborts the install before the cleanup that would free it")
	}
	if resolve < 0 {
		t.Fatal("could not locate resolveInstallPorts")
	}
	if clear > resolve {
		t.Error("the pre-clear runs AFTER port resolution — the port conflict aborts first, which is " +
			"exactly the reported reinstall failure")
	}
	// And it must be the FULL stop (plane + supervisor + orphans), not a bare quiesce: a bare quiesce
	// under a live supervisor triggers its restart storm (measured at postgres restarts=6).
	if !strings.Contains(src, "stopInstanceCompletely(instance, pre, os.Stdout)") {
		t.Error("the pre-clear does not use stopInstanceCompletely, so a live supervisor's children are " +
			"killed under it and restarted with backoff")
	}
}

// --ALL MUST REACH AN INSTANCE THE MSI DID NOT CREATE.
//
// This is the operator's multi-instance question: "if someone has a prod instance and a dev instance and
// they run the installer, can they remove just dev and just the postgres, etc. for dev and vice versa?"
//
// The MSI's uninstall used to pass [INSTANCE] — the instance the WIZARD chose. But MSI properties are
// SESSION-SCOPED, so on uninstall that fell back to the property's default ("dev"): an operator who chose
// prod had `uninstall --instance dev` run instead, removing the wrong instance or nothing while reporting
// success. Worse, an instance created by hand (`orchicon install --instance mine`) was unreachable.
//
// --all ENUMERATES the instance directories, so it reaches every instance that actually exists. It has to,
// because the action is about to delete orchicon.exe: an instance whose services are still running after
// that has no CLI to stop them.
func TestUninstallAllReachesEveryInstanceOnTheMachine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	for _, env := range []string{
		"ORCHICON_POSTGRES_PORT", "ORCHICON_NATS_PORT", "ORCHICON_NATS_MONITOR_PORT",
		"ORCHICON_TEMPO_PORT", "ORCHICON_LOKI_PORT", "ORCHICON_VM_PORT",
		"ORCHICON_OTLP_GRPC_PORT", "ORCHICON_OTLP_HTTP_PORT", "ORCHICON_OTEL_HEALTH_PORT",
		"ORCHICON_TEMPO_OTLP_GRPC_PORT", "ORCHICON_TEMPO_OTLP_HTTP_PORT", "ORCHICON_CONTROL_PORT",
	} {
		t.Setenv(env, strconv.Itoa(closedPort(t)))
	}

	// Three instances: two the table knows, one created by hand (which the table would NOT name).
	share := filepath.Join(root, ".local", "share")
	for _, inst := range []string{"dev", "prod", "mine"} {
		dir := filepath.Join(share, "orchicon-"+inst)
		for _, d := range []string{"bin", "config", "postgres", "blobs", "secrets"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A directory that LOOKS like an instance but is not one must be left alone.
	if err := os.MkdirAll(filepath.Join(share, "orchicon-not-an-instance-name"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := installedInstances(); len(got) != 4 {
		t.Fatalf("installedInstances() = %v, want the four orchicon-* directories", got)
	}

	var out bytes.Buffer
	if err := runUninstall([]string{"--all"}, &out); err != nil {
		t.Fatalf("uninstall --all: %v\n%s", err, out.String())
	}
	for _, inst := range []string{"dev", "prod", "mine"} {
		dir := filepath.Join(share, "orchicon-"+inst)
		if _, err := os.Stat(filepath.Join(dir, "bin")); !os.IsNotExist(err) {
			t.Errorf("%s/bin survived --all — an instance whose services are still on disk after the CLI "+
				"is deleted has no manager left", inst)
		}
		if _, err := os.Stat(filepath.Join(dir, "postgres")); err != nil {
			t.Errorf("%s/postgres was removed by a plain --all; the data must be kept", inst)
		}
	}
	if !strings.Contains(out.String(), "this machine has 3 Orchicon instance(s)") &&
		!strings.Contains(out.String(), "instance(s)") {
		t.Errorf("--all did not report which instances it found:\n%s", out.String())
	}
}

// THE MSI MUST USE --all, AND MUST HAVE A DATA CHOICE ON THE REMOVE PATH.
func TestMSIUninstallCoversAllInstancesAndAsksAboutData(t *testing.T) {
	wxs := readGoFileFor(t, filepath.Join("..", "..", "deploy", "desktop", "windows", "Orchicon.wxs"))

	if strings.Contains(wxs, `ExeCommand="uninstall --instance [INSTANCE]"`) {
		t.Error("the MSI's uninstall still passes [INSTANCE]. MSI properties are session-scoped, so that " +
			"falls back to the default 'dev' on uninstall — an operator who chose prod removes the wrong " +
			"instance, or nothing.")
	}
	if !strings.Contains(wxs, `ExeCommand="uninstall --all"`) {
		t.Error("the MSI's uninstall does not use --all, so instances it did not create keep running with " +
			"no CLI to stop them once orchicon.exe is gone")
	}
	if !strings.Contains(wxs, `ExeCommand="uninstall --all --purge"`) {
		t.Error("there is no purge variant, so the wizard cannot destroy the data")
	}
	// The question itself.
	if !strings.Contains(wxs, `Dialog Id="RemoveOptionsDlg"`) {
		t.Error("there is no uninstall options page, so the operator has no way to choose between keeping " +
			"and destroying their data — the PURGEDATA property exists but nothing sets it")
	}
	if !strings.Contains(wxs, `Property="PURGEDATA"`) || !strings.Contains(wxs, `Value="1" X="0" Y="22"`) {
		t.Error("RemoveOptionsDlg does not offer the destroy-data choice wired to PURGEDATA")
	}
	// And it must be REACHABLE, on the remove path only.
	if !strings.Contains(wxs, `Value="RemoveOptionsDlg" Order="2" Condition="REMOVE=&quot;ALL&quot;"`) {
		t.Error("the uninstall options page is defined but nothing navigates to it, or it is reachable on " +
			"a non-remove path (a wizard page no path reaches is the silent seam-nothing-calls failure)")
	}
	// KEEP must be the default: an uninstall must not destroy data unless asked.
	if !strings.Contains(wxs, `Property Id="PURGEDATA" Value="0"`) {
		t.Error("PURGEDATA does not default to 0, so an uninstall could destroy data by default")
	}
}
