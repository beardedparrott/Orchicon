package planecontrol

// status_test.go — AC7: status reports the INSTANCE and the BINARY serving, not merely that something is
// listening.

import (
	"os"
	"strings"
	"testing"
)

func TestStatusReportsTheInstanceAndTheBinaryServing(t *testing.T) {
	f := newFakeSeams(t)
	f.writePortRecord(t, "dev", map[string]int{"ORCHICON_CONTROL_PORT": 18121})
	f.writePIDFile(t, "dev", os.Getpid())
	if err := os.MkdirAll(HostDataDirFor("dev"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(f.home+"/.local/share/orchicon-dev/residency", []byte("host\n"), 0o644); err != nil {
		t.Fatalf("write residency: %v", err)
	}
	f.alive = func(string) bool { return true }
	f.drift = Current
	f.healthy = true

	st, err := StatusOf(Options{Instance: "dev"})
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if st.Instance != "dev" || st.Residency != ResidencyHost {
		t.Errorf("status must name the instance and its recorded shape: %+v", st)
	}
	if st.PID != os.Getpid() || !st.Alive {
		t.Errorf("status must report the PID-file process: %+v", st)
	}
	if st.Binary != f.binary {
		t.Errorf("status must report the BINARY serving (%q), not just a listening port; got %q", f.binary, st.Binary)
	}
	if st.Drift != Current {
		t.Errorf("status must carry the drift verdict: %v", st.Drift)
	}
	line := st.String()
	for _, want := range []string{"dev", f.binary, "18121"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line must contain %q; got:\n%s", want, line)
		}
	}
}

func TestStatusWithoutAPIDFileStillNamesTheInstance(t *testing.T) {
	f := newFakeSeams(t)
	f.writePortRecord(t, "prod", map[string]int{"ORCHICON_CONTROL_PORT": 18122})

	st, err := StatusOf(Options{Instance: "prod"})
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if st.Alive || st.PID != 0 || st.Binary != "" {
		t.Errorf("no PID file must read as not running, with no binary claimed: %+v", st)
	}
	line := st.String()
	for _, want := range []string{"prod", HostDataDirFor("prod"), "http://localhost:18122/healthz"} {
		if !strings.Contains(line, want) {
			t.Errorf("status line must still name %q; got:\n%s", want, line)
		}
	}
	_ = f
}

// A LIVE PID FILE IS ENOUGH TO REPORT THE PID AND THE BINARY, even when the liveness seam says the plane is
// not alive. That is not a contradiction — it is exactly what makes `plane status` FIND a plane on a platform
// whose liveness probe is weaker than Unix's: `StatusOf` gates the PID and the binary on `ServingPlanePID`
// (the PID file naming a process the OS confirms exists) and only then asks `planeAlive` for the verdict. Pin
// it, because the opposite arrangement (gate on liveness) turns a weak probe into "no plane here".
func TestStatusReportsThePIDFileEvenWhenTheLivenessSeamSaysNo(t *testing.T) {
	f := newFakeSeams(t)
	f.writePortRecord(t, "dev", map[string]int{"ORCHICON_CONTROL_PORT": 18122})
	f.writePIDFile(t, "dev", os.Getpid())
	f.alive = func(string) bool { return false }

	st, err := StatusOf(Options{Instance: "dev"})
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if st.PID != os.Getpid() {
		t.Errorf("status PID = %d, want %d — the PID file names this running process", st.PID, os.Getpid())
	}
	if st.Alive {
		t.Error("status Alive = true while the liveness seam reports false")
	}
	if st.Binary != f.binary {
		t.Errorf("status Binary = %q, want %q — the binary must be reported from the PID file, not from liveness",
			st.Binary, f.binary)
	}
}
