package containerengine

// provision_locate_test.go — AN INSTALLER MUST NOT DEPEND ON WHO LAUNCHED US.
//
// Reported from a real Windows box, from the MSI's last step:
//
//	orchicon: docker is installed but cannot run a container
//	== installing podman instead — it needs no pre-existing daemon
//	orchicon: ... (podman could not be installed instead: winget is not available, so podman cannot be
//	           installed automatically — install podman from https://podman.io/docs/installation)
//
// And in the same run, three lines earlier, the install HAD found the opencode adapter — because
// internal/adapter's resolver knows about %LOCALAPPDATA%\Microsoft\WindowsApps and %APPDATA%\npm, while this
// package's installer check asked exec.LookPath and nothing else. Run by hand in PowerShell the same install
// provisioned podman every time: PowerShell resolves App Execution Aliases itself, and a process launched by an
// installer custom action does not inherit the PATH that would make LookPath succeed.
//
// The engine resolvers had always searched conventional locations (find). The installer resolvers had not. That
// asymmetry IS the bug, and these tests are its shape.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeWithFiles is testProbe with a Stat that reports the given paths as existing FILES, so conventional
// locations can be exercised without a Windows machine.
func probeWithFiles(goos string, env map[string]string, files []string, bins map[string]string,
	run func(bin string, args []string) (string, error)) Probe {
	p := testProbe(goos, env, bins, run)
	p.Stat = func(name string) (os.FileInfo, error) {
		for _, f := range files {
			if filepath.Clean(f) == filepath.Clean(name) {
				return fakeInfo{}, nil
			}
		}
		return nil, os.ErrNotExist
	}
	return p
}

// TestWingetIsFoundWhereWindowsPutsIt is THE reported case: winget is present at its conventional location and
// absent from PATH, which is the state an MSI-launched install runs in.
func TestWingetIsFoundWhereWindowsPutsIt(t *testing.T) {
	localAppData := `C:\Users\operator\AppData\Local`
	winget := filepath.Join(localAppData, `Microsoft\WindowsApps\winget.exe`)

	env := map[string]string{"LOCALAPPDATA": localAppData, "USERPROFILE": `C:\Users\operator`}
	p := probeWithFiles("windows", env, []string{winget},
		// PATH says NOTHING — this is the whole point: LookPath finds no winget.
		map[string]string{},
		func(bin string, args []string) (string, error) { return "", nil })

	plan, err := planInstall(p, Engine{Name: EngineNone})
	if err != nil {
		t.Fatalf("winget exists at %s but the install claimed it was unavailable: %v.\n"+
			"This is the reported bug: the engine resolvers searched conventional locations and the installer "+
			"resolvers did not.", winget, err)
	}
	if filepath.Clean(plan.Command) != filepath.Clean(winget) {
		t.Fatalf("the plan must run winget by its RESOLVED path (the child inherits our PATH, which cannot find "+
			"it): got %q want %q", plan.Command, winget)
	}
	for _, want := range []string{"RedHat.Podman", "--silent"} {
		if !strings.Contains(strings.Join(plan.Args, " "), want) {
			t.Errorf("the plan lost %q: %v", want, plan.Args)
		}
	}
}

// TestAnAbsentInstallerIsStillAnHonestError: the fallback must not turn "nothing found" into a fake plan.
func TestAnAbsentInstallerIsStillAnHonestError(t *testing.T) {
	p := probeWithFiles("windows", map[string]string{"LOCALAPPDATA": `C:\nope`}, nil, map[string]string{},
		func(bin string, args []string) (string, error) { return "", nil })
	_, err := planInstall(p, Engine{Name: EngineNone})
	if err == nil {
		t.Fatal("no winget anywhere must still be an error")
	}
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("must stay ErrDeclined so the install degrades instead of failing: %v", err)
	}
	// It must not claim to have LOOKED nowhere: the message names where it looked.
	if !strings.Contains(err.Error(), "WindowsApps") {
		t.Errorf("the error should say where winget was sought, so the operator can check: %v", err)
	}
}

// TestBrewAndPackageManagersUseTheirConventionalPaths is the same rule on the other platforms — the fix is not a
// Windows special case, it is the removal of a Windows-shaped assumption.
func TestBrewAndPackageManagersUseTheirConventionalPaths(t *testing.T) {
	for _, c := range []struct {
		name  string
		goos  string
		file  string
		wants string
	}{
		{"brew on Apple silicon", "darwin", "/opt/homebrew/bin/brew", "/opt/homebrew/bin/brew"},
		{"dnf on a minimal image", "linux", "/usr/bin/dnf", "/usr/bin/dnf"},
		{"apt where only apt-get exists", "linux", "/usr/bin/apt-get", "/usr/bin/apt-get"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := probeWithFiles(c.goos, nil, []string{c.file}, map[string]string{},
				func(bin string, args []string) (string, error) { return "", nil })
			plan, err := planInstall(p, Engine{Name: EngineNone})
			if err != nil {
				t.Fatalf("%s exists at %s but was not found: %v", c.name, c.file, err)
			}
			if filepath.Clean(plan.Command) != filepath.Clean(c.wants) {
				t.Fatalf("plan.Command = %q, want %q", plan.Command, c.wants)
			}
		})
	}
}

// TestPathStillWinsOverTheConventionalLocation keeps the operator in charge: if they have a binary on PATH, that
// is the one that runs — the conventional search is a FALLBACK, never an override.
func TestPathStillWinsOverTheConventionalLocation(t *testing.T) {
	localAppData := `C:\Users\operator\AppData\Local`
	conventional := filepath.Join(localAppData, `Microsoft\WindowsApps\winget.exe`)
	onPath := `D:\tools\winget.exe`

	p := probeWithFiles("windows", map[string]string{"LOCALAPPDATA": localAppData},
		[]string{conventional}, map[string]string{"winget": onPath},
		func(bin string, args []string) (string, error) { return "", nil })

	plan, err := planInstall(p, Engine{Name: EngineNone})
	if err != nil {
		t.Fatalf("planInstall: %v", err)
	}
	if plan.Command != onPath {
		t.Fatalf("the binary on PATH must win, got %q want %q", plan.Command, onPath)
	}
}

// TestProvisionWaitsForTheBinaryItJustInstalled is the second half of the same defect.
//
// winget and brew commit to the REGISTRY, not to this process's environment, so the re-detect right after a
// SUCCESSFUL install can still say "no podman". Concluding failure from that sends the operator to open a new
// shell for an install that worked. Provision must re-probe, and must then use the engine it found.
func TestProvisionWaitsForTheBinaryItJustInstalled(t *testing.T) {
	localAppData := `C:\Users\operator\AppData\Local`
	winget := filepath.Join(localAppData, `Microsoft\WindowsApps\winget.exe`)
	podman := filepath.Join(`C:\Program Files`, `RedHat\Podman\podman.exe`)

	ran := false
	p := probeWithFiles("windows", map[string]string{
		"LOCALAPPDATA": localAppData, "ProgramFiles": `C:\Program Files`, ProvisionEnv: "1",
	}, []string{winget}, map[string]string{}, func(bin string, args []string) (string, error) {
		ran = true
		return "", nil
	})

	// THE INSTALLED BINARY APPEARS ONLY AFTER winget RUNS — and never on PATH. This is the exact state that made
	// the old code report "the installer reported success but no podman binary is on PATH".
	p.Stat = func(name string) (os.FileInfo, error) {
		if filepath.Clean(name) == filepath.Clean(winget) {
			return fakeInfo{}, nil
		}
		if ran && filepath.Clean(name) == filepath.Clean(podman) {
			return fakeInfo{}, nil
		}
		return nil, os.ErrNotExist
	}
	// Everything the process runs goes through here — the INSTALLER and the --version probe after it — so `ran`
	// must be set on the installer call, which is what makes the installed binary appear in Stat.
	p.Run = func(ctx context.Context, bin string, args ...string) (string, error) {
		if filepath.Clean(bin) == filepath.Clean(podman) {
			return "podman version 5.8.3", nil
		}
		ran = true
		return "", nil
	}

	var out strings.Builder
	e, err := ProvisionEngineUnattended(context.Background(), p, &out, EnginePodman)
	if err != nil {
		t.Fatalf("a successful install reported as a failure:\n%v\noutput:\n%s", err, out.String())
	}
	if !ran {
		t.Fatal("the installer never ran")
	}
	if e.Name != EnginePodman {
		t.Fatalf("want the freshly installed podman, got %+v", e)
	}
	if filepath.Clean(e.Bin) != filepath.Clean(podman) {
		t.Fatalf("resolved binary = %q, want %q", e.Bin, podman)
	}
}

// TestNoCandidateIsEverRelative pins the rule that cost real debugging: a candidate built from an environment
// variable that did not resolve must be DROPPED, never joined onto an empty base.
//
// Measured before this was fixed: with no %USERPROFILE% the podman table produced
// "AppData\Local/Programs/Podman/podman.exe" — a RELATIVE path, which os.Stat resolves against the working
// directory. A resolver that can look in the current directory is a resolver that can match the wrong file, and
// that is the class of bug that hides on CI and bites on a real machine.
//
// ABSOLUTENESS IS CHECKED PER PLATFORM, NOT WITH filepath.IsAbs. These tables describe OTHER operating systems
// than the one running the test, and filepath.IsAbs applies the RUNNING system's rules — on Linux it says a
// `C:\...` path is relative. (The test failed exactly that way first, which is this comment's provenance: the
// assertion was wrong, not the table.) A Windows candidate is absolute when it starts with a drive letter.
func isAbsFor(goos, path string) bool {
	if goos == "windows" {
		if len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
			return true
		}
		return strings.HasPrefix(path, `\\`) // a UNC share
	}
	return strings.HasPrefix(path, "/")
}

func TestNoCandidateIsEverRelative(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		bare := testProbe(goos, nil, nil, nil) // no environment at all
		for name, cands := range conventionalPaths(bare) {
			for _, c := range cands {
				if !isAbsFor(goos, c) {
					t.Errorf("%s engine %s: candidate %q is RELATIVE — it would resolve against the working "+
						"directory", goos, name, c)
				}
			}
		}
		for pm, cands := range packageManagerPaths(bare) {
			for _, c := range cands {
				if !isAbsFor(goos, c) {
					t.Errorf("%s package manager %s: candidate %q is RELATIVE", goos, pm, c)
				}
			}
		}
	}
}

// TestConventionalPathsHonourTheProbeEnv is the seam that made the table untestable, and the reason the
// relative-path bug survived: it read os.Getenv directly, so no test could describe a Windows machine.
func TestConventionalPathsHonourTheProbeEnv(t *testing.T) {
	p := testProbe("windows", map[string]string{"ProgramFiles": `D:\Apps`}, nil, nil)
	found := false
	for _, c := range conventionalPaths(p)[EnginePodman] {
		if strings.HasPrefix(c, `D:\Apps`) {
			found = true
		}
	}
	if !found {
		t.Errorf("conventionalPaths ignored the probe's %%ProgramFiles%%: %v", conventionalPaths(p)[EnginePodman])
	}
}
