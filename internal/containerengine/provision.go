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
		brew, ok := locatePackageManager(p, "brew")
		if !ok {
			return InstallPlan{}, fmt.Errorf("%w: Homebrew was not found, so podman cannot be installed automatically — install Homebrew (https://brew.sh) or podman itself (%s)", ErrDeclined, PodmanDocsURL)
		}
		return InstallPlan{Command: brew, Args: []string{"install", "podman"}}, nil

	case "windows":
		// WINGET IS LOOKED FOR IN ITS CONVENTIONAL LOCATIONS TOO, and that is the fix for a reported bug rather
		// than a tidy-up. It used to be `p.lookPath("winget")` and nothing else. Launched by the Windows MSI's
		// custom action, this process does not carry %LOCALAPPDATA%\Microsoft\WindowsApps on its PATH, so the
		// install concluded "winget is not available" and SKIPPED podman entirely — measured, from a real box:
		//
		//	orchicon: the container engine is installed but cannot run a container: ... (podman could not be
		//	installed instead: winget is not available, so podman cannot be installed automatically)
		//
		// Run by hand in PowerShell the same install provisioned podman every time, because PowerShell resolves
		// App Execution Aliases itself. The capability must not depend on who launched us.
		winget, ok := locatePackageManager(p, "winget")
		if !ok {
			return InstallPlan{}, fmt.Errorf("%w: winget was not found (not on PATH, and not at %%LOCALAPPDATA%%\\Microsoft\\WindowsApps), so podman cannot be installed automatically — install the App Installer (winget) or podman itself from %s", ErrDeclined, PodmanDocsURL)
		}
		// SILENT, AND THAT IS NOT COSMETIC.
		//
		// Without --silent, winget runs the package's own installer UI — a graphical window, on top of a console
		// install the operator is watching, that has to be clicked through. Reported: "it also installs via a
		// graphical interface." A GUI appearing mid-command reads as the install having gone wrong, and on a
		// headless or remote session there is nobody to click it.
		//
		// The agreement flags stay because a silent install cannot accept a prompt it cannot show.
		return InstallPlan{
			// THE RESOLVED PATH, not the bare name. The child inherits this process's PATH — the very PATH that
			// did not find winget — so a bare "winget" would fail to start even now that we know where it is.
			Command: winget,
			Args: []string{"install", "--id", "RedHat.Podman", "-e", "--silent",
				"--accept-source-agreements", "--accept-package-agreements"},
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
			// THE SAME RULE AS WINDOWS. A minimal image can install a package manager somewhere that is not on
			// this process's PATH (a service-manager environment is the usual way), and concluding "no supported
			// package manager" on a host that has dnf at /usr/bin/dnf is the same defect in a different costume.
			if bin, ok := locatePackageManager(p, cand.bin); ok {
				return InstallPlan{Command: bin, Args: cand.args, NeedsRoot: cand.needsRoot}, nil
			}
		}
		return InstallPlan{}, fmt.Errorf("%w: no supported package manager (dnf, apt-get, zypper, pacman, apk) was found anywhere — install podman from %s", ErrDeclined, PodmanDocsURL)
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
	return ProvisionEngine(ctx, p, out, confirm, EngineNone)
}

// ProvisionEngine installs ONE named engine, or (want == EngineNone) whatever is missing.
//
// WHY THE PARAMETER EXISTS, and it is a measured bug rather than an abstraction. A caller that has just found
// an engine DETECTED BUT UNUSABLE — a docker CLI whose daemon is dead, the commonest Windows machine there is
// — needs to install PODMAN. It used to call Provision, whose very first act is:
//
//	e, err := Detect(p)
//	if e.Name != EngineNone {
//	    return e, nil          // <- docker is detected, so: install nothing, return docker
//	}
//
// So the install printed "installing podman instead", called this, got docker straight back, and verified
// docker again — reporting the SAME failure. Measured on the operator's host:
//
//	orchicon: docker is installed but cannot run a container
//	== installing podman instead — it needs no pre-existing daemon
//	== checking the podman machine
//	== verifying podman — running a trivial container
//	orchicon: ...failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine
//
// Podman was never mentioned in the verification failure because podman was never installed.
//
// want == EngineNone keeps the original "install whatever is absent" behaviour, so an install with NO engine
// is unaffected.
func ProvisionEngine(ctx context.Context, p Probe, out io.Writer, confirm func(string) bool, want Name) (Engine, error) {
	// requireConfirm is TRUE here: ProvisionEngine is called by a caller that may genuinely want the question
	// (a ladder deciding whether to offer an engine). The install's own path passes consent through the
	// environment, so alreadyConsented short-circuits it.
	return provision(ctx, p, out, confirm, want, true)
}

// ProvisionEngineUnattended installs the named engine WITHOUT asking, for a caller that has already decided.
//
// IT EXISTS AS ITS OWN NAME so the decision is visible at the call site. A caller passing a nil confirm to
// ProvisionEngine looks like an oversight; a caller calling this says what it means.
func ProvisionEngineUnattended(ctx context.Context, p Probe, out io.Writer, want Name) (Engine, error) {
	return provision(ctx, p, out, nil, want, false)
}

// provision is the implementation, with requireConfirm explicit so a caller can declare that consent is already
// settled rather than relying on an environment variable to say so.
func provision(ctx context.Context, p Probe, out io.Writer, confirm func(string) bool, want Name, requireConfirm bool) (Engine, error) {
	if out == nil {
		out = io.Discard
	}
	e, err := Detect(p)
	// WHEN A SPECIFIC ENGINE IS DEMANDED, THE DETECTED ONE IS NOT AN ANSWER. Provision it even though something
	// else exists — that is the whole point.
	if want != EngineNone {
		if e.Name == want {
			return e, nil
		}
		// Fall through with e reset so planInstall plans for the ABSENT case.
		e = Engine{Name: EngineNone}
		if err != nil {
			// A detection ERROR (not merely "nothing found") still matters for planning.
			err = nil
		}
	} else if e.Name != EngineNone {
		return e, nil
	}
	if err != nil {
		return Engine{Name: EngineNone}, err
	}
	if want != EngineNone {
		e.Name = want
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

	// PROVISIONING IS CONSENTED BY ASKING FOR CONTAINERS, SO THERE IS NO PROMPT.
	//
	// THE REPORTED FAILURE, and it survived several rounds because the symptom was a HANG rather than an
	// error. The operator ran `orchicon install dev` and saw:
	//
	//	== installing podman instead — it needs no pre-existing daemon
	//	orchicon: podman is not installed — it can be installed with: winget install --id RedHat.Podman …
	//	                                                                                        <cursor stops>
	//
	// — and nothing happened until they pressed Enter, at which point podman installed. There was a prompt; it
	// was written to a handle nobody could read (on Windows promptTerminal returns stdin OPENED FOR INPUT, so
	// writing to it goes nowhere while the read still blocks). Both halves of that are fixed here: the prompt
	// is gone, and so is its dependence on a stream that may be un-writable.
	//
	// THE ARGUMENT IS THE PRODUCT DECISION, not a shortcut. Containers are provisioned by default; the operator
	// asked for them by running the installer; and a step that stops to ask a second time for the thing already
	// requested is the step that hangs. requireConfirm is now explicit about that, and a caller that genuinely
	// needs to ask (a first-time interactive choice on a host where containers were NOT requested) can still
	// pass it.
	if requireConfirm && !alreadyConsented(p) {
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
	// A FRESH INSTALL IS NOT ON THIS PROCESS'S PATH YET, so the re-detect above can legitimately say "none".
	//
	// winget and brew commit to the REGISTRY (or the shell profile), not to the environment of the process that
	// ran them, so the value we just watched install can be invisible to the very next probe. Concluding
	// "reported success but no podman binary is on PATH" from that is how a working install gets reported as a
	// failure and the operator is sent to open a new shell for nothing.
	//
	// WaitForBinary re-probes for a bounded window and only trusts a CONVENTIONAL location, so a winget stub
	// that appears early cannot masquerade as a working engine (see isConventionalInstall).
	if e.Name == EngineNone {
		if re, ok := e.WaitForBinary(p); ok {
			fmt.Fprintf(out, "orchicon: podman appeared at %s after the install (PATH is refreshed at logon)\n", re.Bin)
			e = re
		}
	}
	if e.Name == EngineNone {
		return Engine{Name: EngineNone}, fmt.Errorf("%w: the installer reported success but no podman binary was found — it may be installed somewhere this shell cannot see. Open a NEW terminal and re-run `orchicon install`", ErrDeclined)
	}
	// The version is a separate client call (Detect stays execution-free), and the "installed"
	// line is much more useful with it — so ask once, here.
	if v, verr := Version(p, e); verr == nil {
		e = v
	}
	fmt.Fprintf(out, "orchicon: podman %s installed at %s\n", e.Version, e.Bin)
	return e, nil
}

// alreadyConsented reports whether this run has been told to provision without being asked.
//
// IT IS AN ALLOW-LIST OF VALUES THAT MEAN YES, never a denial of everything else: an operator who sets the
// variable to something unrecognised gets the prompt (in an interactive run) rather than a surprise install.
// ORCHICON_PROVISION_ENGINE=0 is handled earlier as a hard refusal.
func alreadyConsented(p Probe) bool {
	switch strings.ToLower(strings.TrimSpace(p.getenv(ProvisionEnv))) {
	case "1", "yes", "on", "true":
		return true
	}
	// NOTHING ELSE IMPLIES CONSENT, and this used to guess that it did.
	//
	// It read ORCHICON_INSTALL_CONTAINERS and treated "unset" as yes — which turned `orchicon install` run by
	// hand into an unattended provisioner nobody had asked to be unattended. The installer is what sets that
	// variable (and sets it to "1"), so its absence means no installer decided anything: fall back to a prompt
	// where there is a terminal, and to a visible refusal where there is not. Both of those say what to run.
	return false
}

// ContainersOptInEnv is the installer's container consent, set to "0" by -NoContainers / --no-containers.
//
// It lives here so the provisioner and the installers cannot disagree about what "the operator asked for
// containers" is called.
const ContainersOptInEnv = "ORCHICON_INSTALL_CONTAINERS"
