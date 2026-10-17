package planecontrol

// seams_test.go — the fake every lifecycle test drives. These seams exist because the properties this task
// must guarantee (a plane that cannot be identified is left ALONE; a stop is BOUNDED; two instances never
// share state) are about DECISIONS, and a test that had to race real processes and real sleeps to observe them
// would be slow, flaky and would prove less.

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeRun struct {
	args []string
	env  []string
}

type fakeSeams struct {
	home      string
	drift     DriftState
	healthy   bool
	alive     func(instance string) bool
	binary    string
	// runErr is what the re-exec seam returns: `serve --stop` exits 1 when there is nothing to stop, and a
	// stop that FAILED must be distinguishable from one that had nothing to do.
	runErr    error
	runs      []fakeRun
	sleeps    int
	aliveCall int
	driftCall int
}

func newFakeSeams(t *testing.T) *fakeSeams {
	t.Helper()
	f := &fakeSeams{
		home:    t.TempDir(),
		drift:   Unknown,
		healthy: false,
		binary:  "/opt/orchicon/bin/orchicon",
	}
	f.alive = func(string) bool { return false }

	oldRun, oldSleep, oldNow := runCommand, sleep, now
	oldHealthy, oldDrift, oldAlive := healthyProbe, driftProbe, planeAlive
	oldExe, oldBin, oldHome := executable, binaryOf, userHomeDir
	t.Cleanup(func() {
		runCommand, sleep, now = oldRun, oldSleep, oldNow
		healthyProbe, driftProbe, planeAlive = oldHealthy, oldDrift, oldAlive
		executable, binaryOf, userHomeDir = oldExe, oldBin, oldHome
	})

	userHomeDir = func() (string, error) { return f.home, nil }
	executable = func() (string, error) { return f.binary, nil }
	binaryOf = func(pid int) (string, error) { return f.binary, nil }
	driftProbe = func(instance string, out io.Writer) DriftState { f.driftCall++; return f.drift }
	healthyProbe = func(url string, timeout time.Duration) bool { return f.healthy }
	planeAlive = func(instance string) bool { f.aliveCall++; return f.alive(instance) }
	runCommand = func(args []string, env []string, out io.Writer) error {
		f.runs = append(f.runs, fakeRun{args: args, env: env})
		return f.runErr
	}
	// A clock that advances one second per reading, matching startHealthPoll: every bounded loop in this package
	// therefore ends because of its OWN bound (60 readings) rather than because the test sat through a minute.
	var clock time.Time
	now = func() time.Time { clock = clock.Add(time.Second); return clock }
	sleep = func(time.Duration) { f.sleeps++ }

	// No container engine is consulted by these tests: the bridge bind is a separate concern and answering it
	// from the host would make the assertions depend on whether Docker happens to be installed.
	t.Setenv("ORCHICON_RUNTIME_CONTAINERS", "0")
	return f
}

// envValue reads one key out of a recorded child environment.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if len(kv) > len(key) && kv[:len(key)+1] == key+"=" {
			return kv[len(key)+1:]
		}
	}
	return ""
}

// writePortRecord lays down the ports.json an install would have written for the instance.
func (f *fakeSeams) writePortRecord(t *testing.T, instance string, ports map[string]int) {
	t.Helper()
	path, err := PortsRecordPath(instance)
	if err != nil {
		t.Fatalf("ports record path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "{\n"
	first := true
	for k, v := range ports {
		if !first {
			body += ",\n"
		}
		first = false
		body += "  \"" + k + "\": " + itoa(v)
	}
	body += "\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write ports record: %v", err)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

// writePIDFile records a PID for the instance, the way `serve --detach` does.
func (f *fakeSeams) writePIDFile(t *testing.T, instance string, pid int) {
	t.Helper()
	path := ServePIDFile(instance)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir pid dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
}
