package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// engineCLI is the binary the INSTALL PATH drives for image and container operations,
// set once by resolveContainerEngine when the ladder reached "ready".
//
// It used to be the literal "docker" in every call site, which made "containers available"
// mean "docker installed" no matter what the resolver said. The resolver now owns the
// decision (podman first, docker second).
var engineCLI string

// engineResolved / engineMu memoise the LAZY resolution below (see engineBinary).
var (
	engineMu       sync.Mutex
	engineResolved string
)

// engineBinary is the engine binary to invoke.
//
// THE INSTALL'S DECISION WINS (engineCLI, set by resolveContainerEngine). Everything else —
// `orchicon restart` on a recorded container shape, the bridge probe, the port read-back —
// never runs the ladder, and those paths must NOT default to the literal "docker": on the
// podman-only host this feature exists to produce, `docker restart` is a command-not-found
// and an instance that cannot be restarted. So the default is resolved LAZILY with the same
// rules (podman first, then docker), memoised because a restart asks more than once, and
// still "docker" when Detect cannot see anything — which is exactly what those paths did
// before this change.
func engineBinary() string {
	if engineCLI != "" {
		return engineCLI
	}
	engineMu.Lock()
	defer engineMu.Unlock()
	if engineResolved != "" {
		return engineResolved
	}
	engineResolved = "docker"
	if e, err := containerengine.Detect(containerengine.DefaultProbe()); err == nil && e.Bin != "" {
		engineResolved = e.Bin
	}
	return engineResolved
}

// engineCommandTimeout bounds every engine invocation the INSTALL makes.
//
// WHY A TIMEOUT IS NOT OPTIONAL HERE. `exec.Command` has no deadline, and on Windows a Docker
// Desktop / podman pipe that EXISTS but whose daemon is wedged does not fail — it BLOCKS. An
// operator hit exactly that: the install printed
//
//	== resolving the plane residency and host ports
//
// and then simply stopped. That phase's first act is rung 0, which asks the engine whether this
// instance is already running (`docker inspect`) — an unbounded call against a dead pipe. The
// install had no way to recover and no way to say what it was waiting for.
//
// Bounded at 20s: generous for a healthy daemon (tens of milliseconds), short enough that a
// broken one degrades to "no container is running" instead of hanging the install for ever.
// The probe that decides AVAILABILITY is separate and already bounded (internal/containerengine),
// so a timeout here can only move the install onto the host path — which is the supported shape.
const engineCommandTimeout = 20 * time.Second

// engineCommand is an engine invocation through the resolved binary, with a deadline.
func engineCommand(args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), engineCommandTimeout)
	// Cancel when the process is waited on; exec.Cmd does not expose a completion hook, so the
	// timer is released by the deadline expiring at worst. The leak window is one timer per
	// call, and calls are few and sequential.
	_ = cancel
	// quietCommandContext, because the install makes MANY engine calls and runs with no console
	// of its own (instancectl spawns it detached). Each bare one was another window.
	return procwin.CommandContext(ctx, engineBinary(), args...)
}

// engineStatus is the availability decision the install MADE, cached for the summary. It is a
// decision, not a re-probe: an install summary that re-detected could describe a carrier the
// install never created (the same defect printInstallInfo's containersUsed comment records).
//
// The plane's /enginez is a SEPARATE, live probe (internal/api/engine_status.go) — the two
// agree because both go through internal/containerengine, not because they share a file.
var engineStatus containerengine.Status

// resolveContainerEngine is the whole engine decision, as SIX RUNGS whose every failure is
// a warning that falls through to the local shape (AC6): detect → minimum version → opt-in
// provisioning → `podman machine` → verify-by-running-a-container → available.
//
// The ONE fatal case is an explicit ORCHICON_CONTAINER_ENGINE pin: an operator who named the
// engine asked for it, so quietly installing in local mode would be ignoring them.
func resolveContainerEngine() (bool, error) {
	p := containerengine.DefaultProbe()
	pin := strings.ToLower(strings.TrimSpace(os.Getenv(containerengine.EnginePinEnv)))
	pinned := pin != "" && pin != string(containerengine.EngineNone) && pin != "0" && pin != "off"

	st := containerengine.Status{
		Engine:  containerengine.EngineNone,
		Minimum: containerengine.MinimumFor(p),
		State:   containerengine.StateUnavailable,
	}

	// unavailable records WHY containers are off, prints it, and returns the install's answer.
	// A pinned engine turns the same reason into the install's error.
	unavailable := func(format string, args ...any) (bool, error) {
		msg := fmt.Sprintf(format, args...)
		st.Available, st.State, st.Reason = false, containerengine.StateUnavailable, msg
		engineStatus = st
		if pinned {
			return false, errors.New(msg)
		}
		fmt.Printf("orchicon: %s\n", msg)
		fmt.Println("orchicon: runtime containers are unavailable — the services run as HOST processes and work runs in-process")
		return false, nil
	}

	// 1. DETECT (AC1).
	installPhase("checking the container engine")
	e, err := containerengine.Detect(p)
	if err != nil {
		return unavailable("no container engine: %v", err)
	}
	// A BRAND-NEW ENGINE INSTALL IS NOT ON THIS PROCESS'S PATH YET.
	//
	// winget/brew/npm commit their changes to the REGISTRY (or the shell profile), not to the environment of the
	// process that ran them. So the first detect after installing podman reports "none" and every later rung
	// fails on a host where podman is, in fact, installed — the operator ends up with no engine recorded, no
	// runtime daemon (install.go skips it when no engine is available), and a container fetch that fails with a
	// socket error, having done nothing wrong.
	//
	// The narrowing is deliberate: only a fast re-probe, and only when the binary has appeared at its
	// CONVENTIONAL location too, so a winget stub that happens to be on PATH cannot masquerade as a working
	// engine. If it is still not there, we fall through to the normal "no engine" path.
	if e.Name == containerengine.EngineNone {
		if re, ok := e.WaitForBinary(p); ok {
			e = re
			installPhase("container engine found after install")
		}
	}

	// 2. ABSENT → OPT-IN PROVISIONING (AC2). The command is always printed; the prompt goes
	// to the controlling terminal and a run with no terminal prints and declines, so a piped
	// install can never install a VM image silently.
	if e.Name == containerengine.EngineNone {
		installPhase("no container engine found — podman can be installed for you")
		var confirm func(string) bool
		if tty, owned := promptTerminal(); tty != nil {
			if owned {
				defer tty.Close()
			}
			confirm = func(q string) bool { return askEngineInstall(tty, tty, q) }
		}
		prov, perr := containerengine.Provision(context.Background(), p, os.Stdout, confirm)
		if perr != nil {
			if errors.Is(perr, containerengine.ErrDeclined) {
				return unavailable("podman is not installed: %s", declinedReason(perr))
			}
			return unavailable("podman could not be installed: %v", perr)
		}
		e = prov
	}
	// The version is a separate client call (Detect stays execution-free), and the minimum
	// check needs it — so this is the one place the installer asks.
	if v, verr := containerengine.Version(p, e); verr == nil {
		e = v
	}
	st.Engine, st.Bin, st.Version = e.Name, e.Bin, e.Version

	// 3. MINIMUM VERSION (AC1) — reported, never attempted.
	if err := containerengine.CheckMinimum(p, e); err != nil {
		return unavailable("%v", err)
	}

	// 4. `podman machine` on macOS/Windows (AC3): absent → init, stopped → start, running →
	// nothing, failed → the engine's own reason plus two remedies.
	installPhase("checking the podman machine")
	ms, merr := containerengine.EnsureMachine(context.Background(), p, e, os.Stdout)
	st.Machine = ms
	if merr != nil {
		return unavailable("%v", merr)
	}

	// 5. VERIFY (AC4): "installed" is not "working". A trivial container has to exit 0 before
	// this install is allowed to claim runtime containers.
	installPhase("verifying the container engine — running a trivial container")
	verifyImage := strings.TrimSpace(os.Getenv("ORCHICON_ENGINE_VERIFY_IMAGE"))
	if err := containerengine.Verify(context.Background(), p, e, verifyImage, 0); err != nil {
		return unavailable("%v", err)
	}

	// 6. READY.
	engineCLI = e.Bin
	st.Available, st.State, st.Reason = true, containerengine.StateReady, ""
	engineStatus = st
	fmt.Fprintf(os.Stdout, "orchicon: container engine ready: %s %s (%s)\n", e.Name, e.Version, e.Bin)
	return true, nil
}

// declinedReason strips the ErrDeclined prefix so the warn line reads as a sentence.
func declinedReason(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, containerengine.ErrDeclined.Error()+": ")
	return msg
}

// askEngineInstall is the opt-in question for installing podman. It lives here (rather than
// reusing askInstallPort) because the answer is yes/no, and because the SAFE default for a
// broken terminal is NO: an EOF with no content takes the refusal, so an unattended install
// never installs an engine it could not ask about.
func askEngineInstall(r io.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [Y/n]: ", question)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// SAY SO. This used to print a bare newline, so an EOF (a terminal that closed, a stream that is not a
		// terminal after all) turned "install podman?" into a silent NO — and the operator's only clue was a
		// container fetch failing much later. A prompt that could not be answered is a fact worth reading.
		fmt.Fprintln(out)
		fmt.Fprintln(out, "orchicon: no answer was read — NOT installing podman (containers stay unavailable).")
		fmt.Fprintln(out, "orchicon: re-run with -Containers to provision it without being asked.")
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// describeEngineStatus renders the availability decision as ONE line for the install summary:
// ready (with the version and the machine state), provisioning, or unavailable WITH the
// reason. A bare "unavailable" is exactly the "green but broken" summary this repo removes.
func describeEngineStatus(st containerengine.Status) string {
	if st.Available {
		machine := ""
		if st.Machine != "" && st.Machine != containerengine.MachineNotApplicable {
			machine = fmt.Sprintf(", machine %s", st.Machine)
		}
		return fmt.Sprintf("%s %s%s — runtime containers available", st.Engine, st.Version, machine)
	}
	if st.State == containerengine.StateProvisioning {
		return "provisioning (podman is being installed)"
	}
	if st.Reason != "" {
		return "unavailable — " + st.Reason
	}
	// NO REASON RECORDED BUT NOT AVAILABLE is the silent case this summary exists to prevent: nothing decided
	// anything, so the operator would read "install succeeded" and discover the gap as a socket error later.
	return "unavailable — the container engine was not detected, so there are no runtime containers and no " +
		"runtime daemon; re-run with -Containers to provision one"
}
