package containerengine

// PROVISIONING IS DELIBERATELY DELEGATED TO THE PLATFORM'S OWN INSTALLER (AC7).
//
// Podman is Apache-2.0, so bundling a runtime binary in our archive would be permitted by
// the licence — we do not do it anyway. We install through the user's package manager
// (`brew`, `winget`, or the distro's `dnf`/`apt-get`/`zypper`/`pacman`/`apk`) for three
// reasons: (1) their package manager owns upgrades and security patches, so a Podman we
// installed today is not an orphan tomorrow; (2) we never ship a second copy of a runtime
// that can drift from the host's, which is the same "mount, never bake" posture the
// adapter CLIs already follow; (3) nothing we redistribute can be complained about later.
// Docker is DETECTED and never installed: Docker Desktop's licensing is not ours to accept
// on a user's behalf.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrDeclined means provisioning did not happen — declined by the operator, disabled by
// policy, impossible on this host, or the installer failed. It is an EXPECTED outcome, not
// a crash: every caller treats it as "containers unavailable, install continues in local
// mode" (AC6). Callers test with errors.Is; the wrapping message says why.
var ErrDeclined = errors.New("container engine provisioning was not performed")

// ProvisionMarkerFile is written in the instance data dir while an installer runs, so a
// status surface can report "provisioning" truthfully instead of showing a spinner that
// may be a lie (AC5's third state).
const ProvisionMarkerFile = "engine-provisioning"

// PodmanDocsURL is the manual path we point at when no supported installer is available.
const PodmanDocsURL = "https://podman.io/docs/installation"

// ProvisioningMarkerPath is <instance data dir>/engine-provisioning, or "" when no data
// dir is configured (then nothing is marked and IsProvisioning answers false).
func ProvisioningMarkerPath(p Probe) string {
	dir := strings.TrimSpace(p.getenv("ORCHICON_DATA_DIR"))
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ProvisionMarkerFile)
}

// IsProvisioning reports whether an installer is running right now.
func IsProvisioning(p Probe) bool {
	path := ProvisioningMarkerPath(p)
	if path == "" {
		return false
	}
	_, err := p.stat(path)
	return err == nil
}

func markProvisioning(p Probe) {
	path := ProvisioningMarkerPath(p)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("installing\n"), 0o644)
}

func clearProvisioning(p Probe) {
	if path := ProvisioningMarkerPath(p); path != "" {
		_ = os.Remove(path)
	}
}

// InstallPlan is the command that would install Podman on this host. It is data, not a
// side effect, so the exact command can be PRINTED before anything runs (AC2: the user
// always sees the action, even when they decline it).
type InstallPlan struct {
	Command string
	Args    []string
	// NeedsRoot marks a package manager that must run as root. We report those with the
	// `sudo` prefix and never escalate ourselves: silently sudo-ing inside an installer is
	// exactly the kind of invisible host modification this feature must not do.
	NeedsRoot bool
	// Fallback is a secondary package id when the primary is unavailable (Windows: the
	// Podman Desktop package ships the same binary).
	Fallback string
}

// String is the printable command line, `sudo `-prefixed when root is required.
func (pl InstallPlan) String() string {
	line := pl.Command
	if len(pl.Args) > 0 {
		line += " " + strings.Join(pl.Args, " ")
	}
	if pl.NeedsRoot {
		line = "sudo " + line
	}
	return line
}

// planInstall picks the platform's installer. It returns ErrDeclined-wrapped errors for
// "nothing to install with" so the caller has ONE failure test.
func planInstall(p Probe, e Engine) (InstallPlan, error) {
	if e.Name != EngineNone && e.Name != EnginePodman {
		return InstallPlan{}, fmt.Errorf("%w: %s is detected but Orchicon only installs podman — install podman yourself (or %s=none for local mode)",
			ErrDeclined, e.Name, EnginePinEnv)
	}

	switch p.goos() {
	case "darwin":
		if _, err := p.lookPath("brew"); err != nil {
			return InstallPlan{}, fmt.Errorf("%w: Homebrew is not on PATH, so podman cannot be installed automatically — install Homebrew (https://brew.sh) or podman itself (%s)", ErrDeclined, PodmanDocsURL)
		}
		return InstallPlan{Command: "brew", Args: []string{"install", "podman"}}, nil

	case "windows":
		if _, err := p.lookPath("winget"); err != nil {
			return InstallPlan{}, fmt.Errorf("%w: winget is not available, so podman cannot be installed automatically — install podman from %s", ErrDeclined, PodmanDocsURL)
		}
		return InstallPlan{
			Command:  "winget",
			Args:     []string{"install", "--id", "RedHat.Podman", "-e", "--accept-source-agreements", "--accept-package-agreements"},
			Fallback: "RedHat.Podman-Desktop",
		}, nil

	case "linux":
		// The first package manager that exists wins. Order is by base-image popularity, so the
		// common case is one LookPath.
		for _, cand := range []struct {
			bin       string
			args      []string
			needsRoot bool
		}{
			{"dnf", []string{"install", "-y", "podman"}, true},
			{"apt-get", []string{"install", "-y", "podman"}, true},
			{"zypper", []string{"install", "-y", "podman"}, true},
			{"pacman", []string{"-S", "--noconfirm", "podman"}, false},
			{"apk", []string{"add", "podman"}, false},
		} {
			if _, err := p.lookPath(cand.bin); err == nil {
				return InstallPlan{Command: cand.bin, Args: cand.args, NeedsRoot: cand.needsRoot}, nil
			}
		}
		return InstallPlan{}, fmt.Errorf("%w: no supported package manager (dnf, apt-get, zypper, pacman, apk) was found — install podman from %s", ErrDeclined, PodmanDocsURL)
	}

	return InstallPlan{}, fmt.Errorf("%w: no supported installer for %s — install podman from %s", ErrDeclined, p.goos(), PodmanDocsURL)
}

// Provision installs Podman when it is absent, and ONLY when the operator opted in.
//
// ORDER, and why: detect (never reinstall over a working engine) → decide the plan → PRINT
// THE EXACT COMMAND → ask → run → re-detect (the installer's exit code is not proof the
// binary is on PATH) → CheckMinimum. Machine bring-up and the trivial-container proof are
// the caller's next rungs, because the installer must report BETWEEN them ("installed, now
// starting the VM") and because a machine failure is a different message from a failed
// install.
//
// A nil `confirm` means THERE IS NO TERMINAL: the command is printed and nothing runs,
// which is the same posture cmd/orchicon/install_ports.go takes for the port prompt. That is
// what makes a piped install safe — it can never install a VM image silently.
func Provision(ctx context.Context, p Probe, out io.Writer, confirm func(string) bool) (Engine, error) {
	if out == nil {
		out = io.Discard
	}
	e, err := Detect(p)
	if e.Name != EngineNone {
		return e, nil
	}
	if err != nil {
		return Engine{Name: EngineNone}, err
	}

	switch strings.ToLower(strings.TrimSpace(p.getenv(ProvisionEnv))) {
	case "0", "off", "no", "false", "none":
		return Engine{Name: EngineNone}, fmt.Errorf("%w: disabled by %s", ErrDeclined, ProvisionEnv)
	}

	plan, planErr := planInstall(p, e)
	if planErr != nil {
		return Engine{Name: EngineNone}, planErr
	}

	// THE COMMAND IS PRINTED BEFORE ANY DECISION, so declining still leaves the operator with
	// the exact thing the installer would have run.
	fmt.Fprintf(out, "orchicon: podman is not installed — it can be installed with: %s\n", plan.String())

	if plan.NeedsRoot && !p.isRoot() {
		return Engine{Name: EngineNone}, fmt.Errorf("%w: installing podman needs root on this host — run `%s` yourself, then re-run `orchicon install`", ErrDeclined, plan.String())
	}

	if mode := strings.ToLower(strings.TrimSpace(p.getenv(ProvisionEnv))); mode != "1" && mode != "yes" && mode != "on" && mode != "true" {
		if confirm == nil {
			return Engine{Name: EngineNone}, fmt.Errorf("%w: no terminal to ask on — run `%s` yourself, or set %s=1 to allow it", ErrDeclined, plan.String(), ProvisionEnv)
		}
		if !confirm(fmt.Sprintf("Install podman now with `%s`?", plan.String())) {
			return Engine{Name: EngineNone}, fmt.Errorf("%w: declined — containers stay unavailable and runs execute locally", ErrDeclined)
		}
	}

	markProvisioning(p)
	defer clearProvisioning(p)

	fmt.Fprintf(out, "orchicon: running: %s\n", plan.String())
	if runOut, runErr := p.Run(ctx, plan.Command, plan.Args...); runErr != nil {
		detail := firstLine(runOut)
		if detail == "" {
			detail = runErr.Error()
		}
		return Engine{Name: EngineNone}, fmt.Errorf("%w: `%s` failed: %s — install podman manually from %s, or set %s=none for local mode",
			ErrDeclined, plan.String(), detail, PodmanDocsURL, EnginePinEnv)
	}

	e, err = Detect(p)
	if err != nil {
		return Engine{Name: EngineNone}, err
	}
	if e.Name == EngineNone {
		return Engine{Name: EngineNone}, fmt.Errorf("%w: the installer reported success but no podman binary is on PATH — open a new shell (PATH is usually reloaded by the shell profile) and re-run `orchicon install`", ErrDeclined)
	}
	// The version is a separate client call (Detect stays execution-free), and the "installed"
	// line is much more useful with it — so ask once, here.
	if v, verr := Version(p, e); verr == nil {
		e = v
	}
	fmt.Fprintf(out, "orchicon: podman %s installed at %s\n", e.Version, e.Bin)
	return e, nil
}
