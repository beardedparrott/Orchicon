package containerengine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture reads a file from testdata — every engine output in these tests comes from disk or
// from an injected func, so nothing here touches a real podman, a real PATH, or the network.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// runFunc is the injected process boundary: bin+args in, output+error out.
func runFunc(f func(bin string, args []string) (string, error)) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, bin string, args ...string) (string, error) { return f(bin, args) }
}

// testProbe is a Probe that can only see what the test gives it: `bins` is PATH (plus the
// package managers), every conventional location does not exist, and Run is whatever the
// test says. That is the same posture cmd/orchicon/install_host_test.go takes with a fake
// binary in a t.TempDir(), made stricter so a developer machine WITH podman installed still
// runs the absent cases.
func testProbe(goos string, env map[string]string, bins map[string]string, run func(bin string, args []string) (string, error)) Probe {
	return Probe{
		LookPath: func(name string) (string, error) {
			if p, ok := bins[name]; ok {
				return p, nil
			}
			return "", errors.New("exec: \"" + name + "\": executable file not found in $PATH")
		},
		Run:    runFunc(run),
		Stat:   func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		GOOS:   goos,
		Getenv: func(k string) string { return env[k] },
		IsRoot: func() bool { return false },
		Sleep:  func(time.Duration) {},
	}
}

// TestDetectPresent is AC1's first half: a host WITH podman is detected, its version is
// read, and it is inside the documented minimum.
func TestDetectPresent(t *testing.T) {
	script := fixture(t, "version-5.2.1.txt")
	p := testProbe("linux", nil, map[string]string{"podman": "/fake/bin/podman"}, func(bin string, args []string) (string, error) {
		if bin != "/fake/bin/podman" || len(args) != 1 || args[0] != "--version" {
			t.Fatalf("unexpected engine call: %s %v", bin, args)
		}
		return script, nil
	})

	e, err := Detect(p)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if e.Name != EnginePodman {
		t.Fatalf("want podman, got %q", e.Name)
	}
	e, err = Version(p, e)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if e.Version != "5.2.1" {
		t.Fatalf("want version 5.2.1, got %q", e.Version)
	}
	if err := CheckMinimum(p, e); err != nil {
		t.Fatalf("5.2.1 is inside the minimum %s: %v", MinimumVersion, err)
	}
}

// TestDetectAbsent is the mode that must never be an error: no engine, no failure.
func TestDetectAbsent(t *testing.T) {
	p := testProbe("linux", nil, nil, func(bin string, args []string) (string, error) {
		t.Fatalf("absent engine must run no command, ran %s %v", bin, args)
		return "", nil
	})
	e, err := Detect(p)
	if err != nil {
		t.Fatalf("absence must not be an error: %v", err)
	}
	if e.Name != EngineNone {
		t.Fatalf("want none, got %q", e.Name)
	}
}

// TestDetectPrefersPodman pins the search order: podman first, docker second — an existing
// docker user keeps working, a podman-capable host gets the engine we can provision.
func TestDetectPrefersPodman(t *testing.T) {
	p := testProbe("linux", nil, map[string]string{"podman": "/fake/podman", "docker": "/fake/docker"}, func(bin string, _ []string) (string, error) {
		if bin != "/fake/podman" {
			t.Fatalf("want the podman binary, ran %s", bin)
		}
		return fixture(t, "version-5.2.1.txt"), nil
	})
	e, err := Detect(p)
	if err != nil || e.Name != EnginePodman {
		t.Fatalf("want podman first, got %q (%v)", e.Name, err)
	}
	if e.Version != "" {
		t.Fatalf("Detect must stay an execution-free probe, but it read a version (%q)", e.Version)
	}
	e, err = Version(p, e)
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	dockerOnly := testProbe("linux", nil, map[string]string{"docker": "/fake/docker"}, func(string, []string) (string, error) {
		return "Docker version 27.1.1, build deadbeef", nil
	})
	e, err = Detect(dockerOnly)
	if err != nil || e.Name != EngineDocker {
		t.Fatalf("want docker second, got %q (%v)", e.Name, err)
	}
	e, _ = Version(dockerOnly, e)
	if e.Version != "27.1.1" {
		t.Fatalf("docker version parse: %q", e.Version)
	}
}

// TestDetectPin covers the three pin values: a named engine, the explicit none, and a pin
// that is NOT installed (an error, because a pin is a requirement, not a preference).
func TestDetectPin(t *testing.T) {
	bins := map[string]string{"podman": "/fake/podman", "docker": "/fake/docker"}
	run := func(string, []string) (string, error) { return fixture(t, "version-5.2.1.txt"), nil }

	p := testProbe("linux", map[string]string{EnginePinEnv: "docker"}, bins, func(bin string, _ []string) (string, error) {
		if bin != "/fake/docker" {
			t.Fatalf("a docker pin must select docker, ran %s", bin)
		}
		return "Docker version 27.1.1, commit x", nil
	})
	if e, err := Detect(p); err != nil || e.Name != EngineDocker {
		t.Fatalf("docker pin: %q %v", e.Name, err)
	}

	none := testProbe("linux", map[string]string{EnginePinEnv: "none"}, bins, run)
	if e, err := Detect(none); err != nil || e.Name != EngineNone {
		t.Fatalf("none pin must disable containers with no error: %q %v", e.Name, err)
	}

	missing := testProbe("linux", map[string]string{EnginePinEnv: "podman"}, map[string]string{}, run)
	if _, err := Detect(missing); err == nil {
		t.Fatal("a pinned engine that is not installed must be an error, not a silent fall-through")
	}
}

// TestDetectTooOld is AC1's second half: a version below the minimum is REPORTED with the
// upgrade command, and it is reported rather than attempted.
func TestDetectTooOld(t *testing.T) {
	p := testProbe("linux", nil, map[string]string{"podman": "/fake/podman"}, func(string, []string) (string, error) {
		return fixture(t, "version-3.4.4.txt"), nil
	})
	e, err := Detect(p)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	e, _ = Version(p, e)
	if e.Version != "3.4.4" {
		t.Fatalf("version: %q", e.Version)
	}
	err = CheckMinimum(p, e)
	if err == nil {
		t.Fatal("3.4.4 is below the minimum and must be reported")
	}
	if !strings.Contains(err.Error(), "3.4.4") || !strings.Contains(err.Error(), MinimumVersion) {
		t.Fatalf("the error must name both versions: %v", err)
	}
	if !strings.Contains(err.Error(), "upgrade with") {
		t.Fatalf("the error must carry the upgrade command: %v", err)
	}

	// The minimum is overridable so an operator (and this test) can accept an older engine.
	low := testProbe("linux", map[string]string{MinVersionEnv: "3.0.0"}, map[string]string{"podman": "/fake/podman"}, func(string, []string) (string, error) {
		return fixture(t, "version-3.4.4.txt"), nil
	})
	e, _ = Detect(low)
	e, _ = Version(low, e)
	if err := CheckMinimum(low, e); err != nil {
		t.Fatalf("%s=3.0.0 must accept 3.4.4: %v", MinVersionEnv, err)
	}
}

func TestParseVersionAndCompare(t *testing.T) {
	for in, want := range map[string]string{
		"podman version 5.2.1":              "5.2.1",
		"podman version 4.9.4-dev":          "4.9.4",
		"Docker version 27.1.1, build dead": "27.1.1",
		"podman version 4.0.0":              "4.0.0",
		"no version here":                   "",
	} {
		if got := ParseVersion(in); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"5.2.1", "4.0.0", 1},
		{"3.4.4", "4.0.0", -1},
		{"4.0.0", "4.0.0", 0},
		{"4.0", "4.0.0", 0},
		{"4.1", "4.0.9", 1},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestInspectStates is AC5's data: the three states the UI renders, from the same rules the
// installer acted on.
func TestInspectStates(t *testing.T) {
	ready := testProbe("linux", nil, map[string]string{"podman": "/fake/podman"}, func(_ string, args []string) (string, error) {
		if args[0] == "--version" {
			return fixture(t, "version-5.2.1.txt"), nil
		}
		return "amd64", nil
	})
	st := Inspect(context.Background(), ready)
	if !st.Available || st.State != StateReady || st.Engine != EnginePodman || st.Version != "5.2.1" {
		t.Fatalf("ready status: %+v", st)
	}

	none := testProbe("linux", nil, nil, func(string, []string) (string, error) { return "", nil })
	st = Inspect(context.Background(), none)
	if st.Available || st.State != StateUnavailable || st.Reason == "" {
		t.Fatalf("no engine must be unavailable WITH a reason: %+v", st)
	}

	// The provisioning state comes from the marker file the installer writes, so the UI can
	// say "being installed" without guessing.
	dir := t.TempDir()
	marker := filepath.Join(dir, ProvisionMarkerFile)
	if err := os.WriteFile(marker, []byte("installing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provisioning := testProbe("linux", map[string]string{"ORCHICON_DATA_DIR": dir}, nil, func(string, []string) (string, error) { return "", nil })
	provisioning.Stat = os.Stat
	st = Inspect(context.Background(), provisioning)
	if st.State != StateProvisioning || st.Available {
		t.Fatalf("marker present must report provisioning: %+v", st)
	}
}
