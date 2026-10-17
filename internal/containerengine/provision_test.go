package containerengine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestPlanInstallPerGOOS pins the platform's installer (AC2/AC7): the user's package manager
// owns the install, so they own the updates.
func TestPlanInstallPerGOOS(t *testing.T) {
	bins := map[string]string{"brew": "/fake/brew", "winget": "/fake/winget", "apt-get": "/fake/apt-get"}
	noRun := func(bin string, args []string) (string, error) {
		t.Fatalf("planning must not run anything, ran %s %v", bin, args)
		return "", nil
	}

	darwin := testProbe("darwin", nil, bins, noRun)
	plan, err := planInstall(darwin, Engine{Name: EngineNone})
	if err != nil {
		t.Fatalf("darwin plan: %v", err)
	}
	if plan.String() != "brew install podman" {
		t.Fatalf("darwin plan: %q", plan.String())
	}

	windows := testProbe("windows", nil, bins, noRun)
	plan, err = planInstall(windows, Engine{Name: EngineNone})
	if err != nil {
		t.Fatalf("windows plan: %v", err)
	}
	if plan.Command != "winget" || !strings.Contains(plan.String(), "RedHat.Podman") {
		t.Fatalf("windows plan: %q", plan.String())
	}
	if plan.Fallback == "" {
		t.Fatal("windows must name a fallback package id (Podman Desktop ships the same binary)")
	}

	linux := testProbe("linux", nil, bins, noRun)
	plan, err = planInstall(linux, Engine{Name: EngineNone})
	if err != nil {
		t.Fatalf("linux plan: %v", err)
	}
	if plan.Command != "apt-get" || !plan.NeedsRoot {
		t.Fatalf("linux plan: %q needsRoot=%v", plan.String(), plan.NeedsRoot)
	}
	if !strings.HasPrefix(plan.String(), "sudo ") {
		t.Fatalf("a root-requiring plan must print the sudo line the user can paste: %q", plan.String())
	}
}

// TestPlanInstallWithoutAnInstaller is the actionable-error case: no brew/winget/package
// manager still tells the operator exactly where to get podman, and it is ErrDeclined so no
// install path treats it as fatal.
func TestPlanInstallWithoutAnInstaller(t *testing.T) {
	noRun := func(bin string, args []string) (string, error) {
		t.Fatalf("planning must not run anything, ran %s %v", bin, args)
		return "", nil
	}
	for _, goos := range []string{"darwin", "windows", "linux", "plan9"} {
		p := testProbe(goos, nil, map[string]string{}, noRun)
		_, err := planInstall(p, Engine{Name: EngineNone})
		if err == nil {
			t.Fatalf("%s with no installer must be an error", goos)
		}
		if !errors.Is(err, ErrDeclined) {
			t.Fatalf("%s: the failure must be ErrDeclined (warn + local mode), got %v", goos, err)
		}
		if !strings.Contains(err.Error(), "podman") {
			t.Fatalf("%s: the message must be actionable: %v", goos, err)
		}
	}
}

// TestProvisionNeverSilent is AC2's hard rule: with no terminal the command is PRINTED and
// nothing runs. A piped install must never install a VM image behind the operator's back.
func TestProvisionNeverSilent(t *testing.T) {
	p := testProbe("darwin", nil, map[string]string{"brew": "/fake/brew"}, func(bin string, args []string) (string, error) {
		t.Fatalf("no terminal must run nothing, ran %s %v", bin, args)
		return "", nil
	})
	var out bytes.Buffer
	_, err := Provision(context.Background(), p, &out, nil)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("want ErrDeclined, got %v", err)
	}
	if !strings.Contains(out.String(), "brew install podman") {
		t.Fatalf("the exact command must be visible even when declined:\n%s", out.String())
	}
}

// TestProvisionDeclinedRunsNothing: an explicit "no" is the same as no terminal — visible
// command, zero side effects.
func TestProvisionDeclinedRunsNothing(t *testing.T) {
	p := testProbe("linux", nil, map[string]string{"apt-get": "/fake/apt-get"}, func(bin string, args []string) (string, error) {
		t.Fatalf("a refusal must run nothing, ran %s %v", bin, args)
		return "", nil
	})
	p.IsRoot = func() bool { return true }
	asked := false
	_, err := Provision(context.Background(), p, nil, func(string) bool { asked = true; return false })
	if !asked {
		t.Fatal("the operator must be asked before anything is installed")
	}
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("want ErrDeclined, got %v", err)
	}
}

// TestProvisionDisabledByEnv: ORCHICON_PROVISION_ENGINE=0 is an explicit refusal that does
// not even ask.
func TestProvisionDisabledByEnv(t *testing.T) {
	p := testProbe("darwin", map[string]string{ProvisionEnv: "0"}, map[string]string{"brew": "/fake/brew"}, func(bin string, args []string) (string, error) {
		t.Fatalf("disabled provisioning must run nothing, ran %s %v", bin, args)
		return "", nil
	})
	_, err := Provision(context.Background(), p, nil, func(string) bool { t.Fatal("must not ask when disabled"); return true })
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("want ErrDeclined, got %v", err)
	}
}

// TestProvisionRootRequirementIsReported not escalated: an installer that needs root is
// PRINTED for the user to run, never sudo-ed by us.
func TestProvisionRootRequirementIsReported(t *testing.T) {
	p := testProbe("linux", nil, map[string]string{"dnf": "/fake/dnf"}, func(bin string, args []string) (string, error) {
		t.Fatalf("we must never escalate, ran %s %v", bin, args)
		return "", nil
	})
	p.IsRoot = func() bool { return false }
	var out bytes.Buffer
	_, err := Provision(context.Background(), p, &out, func(string) bool { return true })
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("want ErrDeclined, got %v", err)
	}
	if !strings.Contains(err.Error(), "sudo dnf install -y podman") {
		t.Fatalf("the message must carry the exact command to run: %v", err)
	}
}

// TestProvisionInstallsThenDetects is the happy path end to end: absent → confirm → run the
// package manager → re-detect finds podman. Re-detection is the point: an installer's exit
// code is not proof the binary is on PATH.
func TestProvisionInstallsThenDetects(t *testing.T) {
	installed := false
	run := func(bin string, args []string) (string, error) {
		switch bin {
		case "brew":
			installed = true
			return "", nil
		case fakePodman:
			return fixture(t, "version-5.2.1.txt"), nil
		}
		t.Fatalf("unexpected run %s %v", bin, args)
		return "", nil
	}
	p := testProbe("darwin", nil, map[string]string{"brew": "/fake/brew"}, run)
	baseLook := p.LookPath
	p.LookPath = func(name string) (string, error) {
		if name == "podman" {
			if installed {
				return fakePodman, nil
			}
			return "", errors.New("not found")
		}
		return baseLook(name)
	}

	var out bytes.Buffer
	e, err := Provision(context.Background(), p, &out, func(string) bool { return true })
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if e.Name != EnginePodman || e.Version != "5.2.1" {
		t.Fatalf("want podman 5.2.1 after install, got %+v", e)
	}
	if !strings.Contains(out.String(), "running: brew install podman") {
		t.Fatalf("the action must be visible:\n%s", out.String())
	}
}

// TestProvisionLeavesAPresentEngineAlone: never reinstall over a working engine.
func TestProvisionLeavesAPresentEngineAlone(t *testing.T) {
	p := testProbe("linux", nil, map[string]string{"podman": fakePodman, "dnf": "/fake/dnf"}, func(bin string, args []string) (string, error) {
		if bin != fakePodman {
			t.Fatalf("an installed engine must not be reinstalled, ran %s %v", bin, args)
		}
		return fixture(t, "version-5.2.1.txt"), nil
	})
	e, err := Provision(context.Background(), p, nil, func(string) bool { t.Fatal("nothing to ask"); return false })
	if err != nil || e.Name != EnginePodman {
		t.Fatalf("want the existing podman, got %+v (%v)", e, err)
	}
}

// TestProvisionMarkersTheInstall: while the installer runs, the marker file exists, which is
// what makes the UI's "provisioning" state truthful (AC5) instead of a guess.
func TestProvisionMarkersTheInstall(t *testing.T) {
	dir := t.TempDir()
	var sawMarker bool
	podInstalled := false

	var p Probe
	p = testProbe("darwin", map[string]string{"ORCHICON_DATA_DIR": dir, ProvisionEnv: "1"}, nil, func(bin string, args []string) (string, error) {
		switch bin {
		case "brew":
			// The installer runs WHILE the marker exists.
			sawMarker = IsProvisioning(p)
			podInstalled = true
			return "", nil
		case fakePodman:
			return fixture(t, "version-5.2.1.txt"), nil
		}
		t.Fatalf("unexpected run %s %v", bin, args)
		return "", nil
	})
	p.Stat = os.Stat
	p.LookPath = func(name string) (string, error) {
		switch name {
		case "brew":
			return "/fake/brew", nil
		case "podman":
			if podInstalled {
				return fakePodman, nil
			}
		}
		return "", errors.New("not found")
	}
	if _, err := Provision(context.Background(), p, nil, nil); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !sawMarker {
		t.Fatal("the provisioning marker must exist while the installer runs")
	}
	if IsProvisioning(p) {
		t.Fatal("the marker must be removed when provisioning finishes (or fails)")
	}
}
