package containerengine

// ensure.go — MAKE THE ENGINE USABLE FROM A NON-INTERACTIVE CALLER.
//
// WHY THIS EXISTS. The full repair sequence already lived in cmd/orchicon/install_engine.go, but it
// is welded to the INSTALL path: it writes to os.Stdout, it may prompt on a terminal, and it reports
// through installPhase. So when the operator asked for "once a fetch is complete, Orchicon should
// validate that podman is set up, running, and in a usable state, if not, it should fix it", there
// was nothing to call — the logic existed and was unreachable from a request.
//
// WHAT IT CAN AND CANNOT DO, stated because the difference matters:
//
//	FIXES       a stopped or absent podman MACHINE (macOS, Windows). `podman machine init --now` /
//	            `machine start` need no elevation, so a fetch can genuinely repair that case — and it
//	            is the common one on a fresh desktop install.
//	REPORTS     an engine that is not installed at all. Installing podman needs root on Linux and a
//	            package manager on Windows/macOS; a server-stream RPC has no terminal to confirm on
//	            and must never escalate silently. The exact command is returned so the UI can show it.
//	REPORTS     a version below the minimum, a podman that cannot run a trivial container, and a
//	            machine that failed — each with its own actionable reason rather than a bare "no".
//
// So `Fixed` is true only when this call actually changed the host, which is what lets the caller say
// "started the podman machine" instead of implying an install it never performed.

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// EnsureResult is what a caller needs to report honestly.
type EnsureResult struct {
	Status Status
	// Fixed is true when THIS call changed the host (a machine was created or started).
	Fixed bool
	// InstallCommand is the command an operator would run to install podman, set only when the
	// engine is absent. Empty otherwise.
	InstallCommand string
	// Err is the reason the engine is not usable. Nil when Status.Available.
	Err error
}

// EnsureReady detects the engine, brings a podman machine up if that is what is missing, and proves
// the result by running a trivial container. Output (the engine's own messages and ours) is written
// to out so a streaming RPC can relay it verbatim.
//
// It never prompts and never elevates.
func EnsureReady(ctx context.Context, p Probe, out io.Writer) EnsureResult {
	if out == nil {
		out = io.Discard
	}
	res := EnsureResult{Status: Status{Engine: EngineNone, Minimum: MinimumFor(p), State: StateUnavailable}}

	e, err := Detect(p)
	if err != nil {
		res.Err = err
		res.Status.Reason = err.Error()
		return res
	}
	if e.Name == EngineNone {
		// ABSENT. Report the install command rather than attempting it: see the file comment.
		plan, planErr := planInstall(p, e)
		if planErr == nil {
			res.InstallCommand = plan.String()
		}
		res.Err = fmt.Errorf("podman is not installed on this host%s", installHint(res.InstallCommand))
		res.Status.Reason = res.Err.Error()
		return res
	}
	if v, verr := Version(p, e); verr == nil {
		e = v
	}
	res.Status.Engine, res.Status.Bin, res.Status.Version = e.Name, e.Bin, e.Version

	if err := CheckMinimum(p, e); err != nil {
		res.Err = err
		res.Status.Reason = err.Error()
		return res
	}

	// THE MACHINE STEP IS THE ONE THAT ACTUALLY FIXES SOMETHING on a desktop host. Recorded so the
	// caller can say what it did — a repair nobody reports is indistinguishable from a repair that
	// did not happen.
	before, _, _ := MachineStatus(ctx, p, e)
	ms, merr := EnsureMachine(ctx, p, e, out)
	res.Status.Machine = ms
	if merr != nil {
		res.Err = merr
		res.Status.Reason = merr.Error()
		return res
	}
	if before != ms && (ms == MachineRunning) {
		res.Fixed = true
	}

	// "INSTALLED" IS NOT "WORKING" — the same rung the installer insists on.
	verifyImage := strings.TrimSpace(p.getenv("ORCHICON_ENGINE_VERIFY_IMAGE"))
	if err := Verify(ctx, p, e, verifyImage, 0); err != nil {
		res.Err = err
		res.Status.Reason = err.Error()
		return res
	}

	res.Status.Available, res.Status.State, res.Status.Reason = true, StateReady, ""
	return res
}

// installHint renders the trailing "run: …" clause, or nothing when no command is known.
func installHint(cmd string) string {
	if strings.TrimSpace(cmd) == "" {
		return ""
	}
	return " — install it with: " + cmd
}
