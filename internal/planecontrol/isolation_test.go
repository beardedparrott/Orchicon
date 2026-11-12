package planecontrol

// isolation_test.go — AC6: two instances are never controllable as one. Every path derives from
// HostDataDir(instance), and the child environment of a stop names THAT instance's state dir.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTwoInstancesNeverShareState(t *testing.T) {
	newFakeSeams(t)

	dev, err := HostDataDir("dev")
	if err != nil {
		t.Fatalf("HostDataDir(dev): %v", err)
	}
	prod, err := HostDataDir("prod")
	if err != nil {
		t.Fatalf("HostDataDir(prod): %v", err)
	}
	if dev == prod || strings.HasSuffix(dev, "/orchicon-dev") == false || strings.HasSuffix(prod, "/orchicon-prod") == false {
		t.Fatalf("the two instances must have distinct data dirs derived from their names: dev=%q prod=%q", dev, prod)
	}
	if ServePIDFile("dev") == ServePIDFile("prod") {
		t.Fatal("two instances must never share a PID file — a stop would kill the sibling's plane")
	}
	devPorts, _ := PortsRecordPath("dev")
	prodPorts, _ := PortsRecordPath("prod")
	if devPorts == prodPorts {
		t.Errorf("the port records must be per instance: %q", devPorts)
	}
	devRes, _ := ResidencyRecordPath("dev")
	prodRes, _ := ResidencyRecordPath("prod")
	if devRes == prodRes {
		t.Errorf("the residency records must be per instance: %q", devRes)
	}
	if want := filepath.Join(dev, "serve", "pids", "orchicon.pid"); ServePIDFile("dev") != want {
		t.Errorf("the PID file must sit under THIS instance's state dir: got %q want %q", ServePIDFile("dev"), want)
	}
}

func TestStopOnlySignalsItsOwnInstance(t *testing.T) {
	f := newFakeSeams(t)
	f.writePIDFile(t, "dev", os.Getpid())
	f.writePIDFile(t, "prod", os.Getpid())

	if err := Stop(Options{Instance: "dev"}); err != nil {
		t.Fatalf("Stop(dev): %v", err)
	}
	if len(f.runs) != 1 {
		t.Fatalf("exactly one re-exec expected, got %v", f.runs)
	}
	want := filepath.Join(HostDataDirFor("dev"), "serve")
	if got := envValue(f.runs[0].env, ServeStateDirEnv); got != want {
		t.Fatalf("the stop must be scoped to dev's state dir %q, got %q", want, got)
	}
	if strings.Contains(envValue(f.runs[0].env, ServeStateDirEnv), "orchicon-prod") {
		t.Fatal("the stop child must never be pointed at the sibling instance")
	}
	// The sibling's PID file is untouched: nothing wrote to prod.
	if _, err := os.Stat(ServePIDFile("prod")); err != nil {
		t.Errorf("prod's PID file must survive a dev stop: %v", err)
	}
}
