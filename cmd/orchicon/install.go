package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"github.com/beardedparrott/orchicon/internal/instancectl"
	"github.com/beardedparrott/orchicon/internal/version"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// installProgress is the ACTIVE install's setup record, or nil when no install is running.
//
// A PACKAGE-LEVEL SEAM RATHER THAN A PARAMETER THREADED THROUGH EVERY CALL, and that is a deliberate
// trade: installPhase is called from ten-plus sites across three files, and adding an argument to each
// would be churn in the one file where churn is most dangerous (every line of it is an install path).
// The variable is written in exactly two places — set at the start of runInstall and cleared on the way
// out — so the lifetime is the install's, which is also the lifetime of the record it feeds.
//
// A nil tracker makes installPhase a no-op for progress, so a caller that is not an install (a test, or
// any future phase banner) keeps the plain banner.
var installProgress *setupTracker

// setupTracker writes the machine-readable progress record (design of record, D4) beside the instance's
// ports.json, which is the channel a GUI polls while a detached install runs.
type setupTracker struct {
	instance  string
	lastPhase string
	// startedAt is when THIS install began, carried through every write.
	//
	// LEARNED FROM A REAL RUN: the terminal records were written as fresh structs, so WriteSetupProgress
	// stamped started_at = now on each one and the record ended up saying the install STARTED at the moment
	// it FINISHED. A GUI showing "elapsed: 0s" for a six-minute download is the kind of small lie that makes
	// the whole record untrustworthy.
	startedAt string
	// pid is this install process, so "is setup actually running?" is answered by the PROCESS TABLE rather
	// than by guesswork about a file's age.
	//
	// ALSO FROM A REAL RUN: without it the record carried pid 0 on every write, so staleness fell back to
	// the timestamp alone — the weaker of the two signals the design specifies.
	pid int
}

// beginSetup starts a record for an instance: it CLEARS any previous one first, so a finished record from
// an older run cannot be mistaken for this run's progress.
func beginSetup(instance string) *setupTracker {
	_ = instancectl.ClearSetupProgress(instance)
	t := &setupTracker{instance: instance, pid: os.Getpid()}
	t.write(instancectl.SetupRunning, "starting", "Starting setup", 0, "")
	return t
}

// write is the ONE place a record is written, so every field that must persist (started_at, pid) persists
// on every state — which is exactly what the two real-run gaps were: fields set in one writer and dropped
// in another.
func (t *setupTracker) write(state, phase, label string, percent int, errText string) {
	if t == nil {
		return
	}
	if t.startedAt == "" {
		t.startedAt = nowRFC3339()
	}
	_ = instancectl.WriteSetupProgress(t.instance, instancectl.SetupProgress{
		State: state, Phase: phase, Label: label, Percent: percent,
		StartedAt: t.startedAt, PID: t.pid, Error: errText,
	})
}

// phase records a new step, so the GUI's label follows the install rather than showing one frozen line.
func (t *setupTracker) phase(msg string) {
	if t == nil {
		return
	}
	t.lastPhase = msg
	t.write(instancectl.SetupRunning, slug(msg), msg, 0, "")
}

// done records a clean finish. The record is KEPT (not cleared) so a GUI that was closed during setup can
// show "setup complete" once on reopen instead of an empty page.
func (t *setupTracker) done() {
	t.write(instancectl.SetupDone, "done", "Setup complete", 100, "")
}

// fail records the error, which is the whole point of the record on the failure path: "setup stopped" with
// no reason is the black box this channel exists to replace.
func (t *setupTracker) fail(err error) {
	if err == nil {
		return
	}
	t.write(instancectl.SetupFailed, "failed", "Setup failed", 0, oneLineProgress(err.Error()))
}

// nowRFC3339 is a seam so a test can pin the clock.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

// slug turns a phase banner into a stable machine key ("starting the host services" -> "starting-the-host-services").
// The KEY is what a GUI switches on; the LABEL is what it shows.
func slug(msg string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(msg) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// oneLineProgress collapses whitespace so a multi-line error still fits the record's single field.
func oneLineProgress(s string) string { return strings.Join(strings.Fields(s), " ") }

// installPhase prints a phase banner for `orchicon install`.
//
// It exists because the one-command install is minutes long and largely unattended:
// with no spine through its output, the operator's only evidence that it is working
// is that nothing has failed yet, and the only way to describe a failure is "during
// install". Naming each phase as it starts makes progress visible, and puts a failure
// under the step that caused it.
//
// IT ALSO FEEDS THE SETUP RECORD when an install is running (see installProgress), so the same banner that
// serves the terminal serves a GUI that has no terminal to read.
func installPhase(msg string) {
	fmt.Printf("== %s\n", msg)
	installProgress.phase(msg)
}

// runInstall implements `orchicon install` — the one-command setup for a
// fresh machine. It pulls the published images, starts the host-side
// runtime daemon, creates + starts the single-container instance, and
// prints connection / management info. The one-line installers
// (scripts/install.sh / install.ps1) call this after dropping the binary
// in place, so `curl https://orchicon.dev/install | bash` brings up the
// whole stack.
//
// It shells out to the Docker CLI (same trust model as container.sh) and
// is idempotent: re-running reports the existing instance instead of
// creating a duplicate.
func runInstall(args []string, log *slog.Logger) error {
	// THE SETUP MAY HAVE BEEN STARTED WITHOUT A CONSOLE, and the only caller that does that is
	// the Windows MSI's last step (a custom action has no console). Rather than manufacture one
	// through `cmd.exe /c start <wrapper>.cmd` — three processes and a command line that ate a
	// quote per hop — the program re-executes ITSELF in a new console and exits. On every other
	// platform this is a no-op.
	//
	// BEFORE the instance is parsed, deliberately: the relaunched child gets the same argv, so
	// parsing here as well would be harmless — but doing the work twice is not the point. This
	// is a hand-off, and a hand-off that has already started acting is not one.
	// A NON-INTERACTIVE RUN NEVER RELAUNCHES FOR A CONSOLE. It has nothing to ask, so opening a
	// window would be the one thing it must not do — that is the "brought up the script and asked
	// me everything again" report, and it is why this precedes the relaunch.
	// A RUN THAT MUST OUTLIVE ITS PARENT ASKS FOR A CONSOLE OF ITS OWN.
	//
	// `interactiveAllowed` is what the relaunched child will see: it must keep answering nothing when the
	// caller said --non-interactive. A console is a place to PRINT, not an invitation to ask.
	interactiveAllowed := !hasNonInteractiveFlag(args)
	//
	// THE MSI'S INSTALL WAS DYING WITH THE WIZARD. Its custom action is
	//     install --non-interactive --instance dev --launch-desktop
	// with Return="asyncNoWait", so the wizard finishes and exits immediately while this install is still
	// downloading host binaries — and the child dies with the console it was sharing. Reported, exactly:
	// "it runs through the entire install and then closes out at the window at the end."
	//
	// --non-interactive used to imply NO console at all, which conflated two different needs: "never ask a
	// question" and "do not open a window". The MSI run needs the second: an install with nobody asking still
	// has minutes of work to do, and the console is what keeps it alive after the wizard goes. It also gives the
	// operator something to watch instead of a wizard that vanishes.
	//
	// The two requirements are therefore separate flags, and neither implies the other:
	//     --non-interactive   ask nothing (unchanged meaning)
	//     --console           I am not attached to a console; give me my own and detach onto it
	// A plain `orchicon install` in a terminal still relaunches into its own console as before.
	wantConsole := !hasNonInteractiveFlag(args) || hasConsoleFlag(args)
	if wantConsole && ensureInteractiveConsole(interactiveAllowed) {
		log.Info("orchicon install: relaunched in its own console")
		return nil
	}
	// THE WIZARD'S ANSWERS COME IN AS FLAGS AND BECOME THE ENVIRONMENT THIS INSTALL ALREADY READS.
	//
	// The MSI's custom action runs this binary directly (FileRef — no shell, by design) and can
	// pass ARGUMENTS but cannot set a child's ENVIRONMENT. So the wizard's pages reach us here.
	//
	// THEY ARE TRANSLATED RATHER THAN THREADED THROUGH. Each flag sets the very variable the
	// install already consults (ORCHICON_<X>_PORT for a port, ORCHICON_INSTALL_RUNTIME_IMAGES
	// for the image choice), so there is ONE code path and one meaning per value — a second,
	// parallel way to reach the resolver is how the wizard and the CLI start disagreeing about
	// what was actually chosen.
	flags, rest, err := parseInstallFlags(args)
	if err != nil {
		return err
	}
	args = rest
	for env, port := range flags.Ports {
		if err := os.Setenv(env, strconv.Itoa(port)); err != nil {
			return fmt.Errorf("apply --port %s: %w", env, err)
		}
	}
	if flags.RuntimeImages != "" {
		if err := os.Setenv("ORCHICON_INSTALL_RUNTIME_IMAGES", flags.RuntimeImages); err != nil {
			return fmt.Errorf("apply --runtime-images: %w", err)
		}
	}
	// THE ARGUMENTS ARE PARSED, AND THAT IS A FIX RATHER THAN A TIDY-UP. `install` used to
	// accept `args` and never read a byte of them, so `orchicon install --instance prod`
	// installed (or REPAIRED) `dev` while the operator believed they had chosen prod. The
	// installer's own `--instance` flag worked, which made the binary's silence worse: the
	// flag existed one layer up and did nothing here.
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	// THE PROGRESS RECORD'S LIFETIME IS THIS INSTALL'S (design of record, D4).
	//
	// It starts here, once the instance is known, and is settled by the deferred pair below on BOTH exits:
	// a clean finish records "done", a returned error records "failed" WITH the reason. Without the failure
	// arm the record would be left claiming "running" by an install that gave up — the eternal-spinner state
	// the staleness rule exists to catch, which should never be the normal way a failure is reported.
	//
	// beginSetup CLEARS any older record first, so a finished one from a previous run cannot be read as this
	// run's progress.
	// THE TRACKER IS CAPTURED IN A LOCAL, and that is what preserves its fields for the settle path.
	//
	// The first version called a helper to rebuild a tracker after nil-ing the package variable, so the
	// terminal records were written by a FRESH tracker with no pid and no start time — the finished record
	// claimed pid 0 and stated that the install had started at the moment it finished. Measured on a real
	// run. Holding the original is both simpler and the only version that keeps its state.
	tracker := beginSetup(instance)
	installProgress = tracker
	// THE RESULT IS THE WHOLE POINT OF NAMING IT. A defer cannot see an unnamed return, and "did this install
	// fail?" is exactly what the record has to answer.
	settled := func() (err error) {
		defer func() {
			installProgress = nil
			if err != nil {
				tracker.fail(err)
			} else {
				tracker.done()
			}
		}()
		err = runInstallBody(args, log, instance, flags)
		return err
	}()
	return settled
}

// runInstallBody is runInstall's work, split out so the setup record can be settled around it.
func runInstallBody(args []string, log *slog.Logger, instance string, flags installFlags) error {
	imageTag := env("ORCHICON_IMAGE_TAG", "latest")
	runtimeImage := env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:"+imageTag)

	name := "orchicon-cnt-" + instance
	dataVolume := "orchicon-cnt-" + instance + "-data"
	socketDir := env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))

	// 1. THE CONTAINER ENGINE IS OPTIONAL, AND ITS ABSENCE IS A MODE, NOT A FAILURE.
	//
	// This used to call `docker version` unconditionally and return a hard error, so a machine with no Docker
	// could not install Orchicon at all. With the container removed from the install path there is a second,
	// complete shape: the services run as HOST processes of the invoking user and the plane runs on the host.
	//
	// The gate is a PATH PROBE (containerEngineAvailable), never an engine call — absence must be answerable
	// without a daemon. `docker version` runs ONLY when the binary is present, so a host with no engine never
	// sees a doomed docker invocation, and a host WITH one still gets the loud "installed but not answering".
	//
	// THE DECISION IS NOW SIX RUNGS, and every one of them falls through to that local shape
	// rather than failing the install (see resolveContainerEngine): detect → minimum version →
	// opt-in provisioning → `podman machine` → a trivial container run → available. The old
	// hard error for "an engine is installed but not answering" survives only as the
	// explicit-pin case, because an operator who set ORCHICON_CONTAINER_ENGINE asked for it.
	containersUsed, err := resolveContainerEngine()
	if err != nil {
		return err
	}

	// 1.5. An external adapter CLI is OPTIONAL. Orchicon ships its own runtime
	// engine, so this install is complete without one and nothing below needs
	// opencode on the host. Report it when present, so the operator knows it will
	// be used, and say nothing when it is absent — there is nothing to fix.
	//
	// THIS USED TO FAIL THE INSTALL. It called requireAdapterCLI("opencode") and
	// returned the error, so `orchicon install` refused to proceed on any host
	// without an external adapter — a hard prerequisite that stopped being true
	// once the plane gained its own engine. The binary is still mounted from the
	// host when present (below), which is what keeps the images redistributable,
	// but that is a capability rather than a requirement.
	if adapterCLIPresent("opencode") {
		reportAdapterCLI("opencode")
	} else {
		fmt.Println("orchicon: no external adapter CLI found — the built-in engine will run sessions (nothing to install)")
	}

	// 1.6. Plane residency, then host-side ports — both resolved BEFORE the image
	// pull, because the pull can take minutes and an operator must never be asked
	// a question after waiting for it. The residency comes first because it decides
	// WHICH ports the instance binds. The terminal is opened and closed here; a nil
	// tty means "no terminal", which takes the defaults rather than blocking a
	// headless install.
	installPhase("resolving the plane residency and host ports")
	residency, err := residencyForInstall()
	if err != nil {
		return err
	}
	// THE PLANE AND ITS SERVICES ALWAYS RUN ON THE HOST. A container is now used for exactly ONE thing:
	// a workflow's runtime container. It is no longer a carrier for the instance.
	//
	// This is a REFUSAL rather than a silent override, because silently installing the host shape for
	// someone who asked for a container would leave them reading a summary that describes something they
	// did not choose. The container shape is retired; saying so once, loudly, is the honest answer.
	if residency == residencyContainer {
		return fmt.Errorf("ORCHICON_PLANE_RESIDENCY=%s is no longer supported: the plane and its services now\n"+
			"  always run on the HOST. Containers are used only for a workflow's runtime container.\n"+
			"  Unset ORCHICON_PLANE_RESIDENCY (or set it to %s) and re-run.",
			residencyContainer, residencyHost)
	}
	residency = residencyHost
	// 1.6a. THE PRIVILEGE GUARD COMES BEFORE ANYTHING ELSE THAT COSTS ANYTHING.
	// Postgres refuses to run under an Administrators token on Windows, and an
	// elevated `orchicon install` would otherwise spend the image pull, write the
	// dirs, resolve the binaries and report success — and then be unable to start the
	// database. Failing here names the cause and the remedy while nothing has been
	// created yet. It is a no-op on Linux and macOS (privilege.go).
	if err := ensureHostPostgresPrivileges(); err != nil {
		return err
	}
	tty, ttyOwned := promptTerminal()
	if ttyOwned {
		defer tty.Close()
	}
	// 1.6a-bis. WHICH INSTANCE, ASKED OUT LOUD WHEN THERE IS SOMEBODY TO ASK.
	//
	// The instance decides the whole stack — its ports, its data dir, its plane — and until now
	// the only place it was ever PUT TO a person was scripts/install.ps1. A run driven by the MSI
	// (or by `orchicon install` straight from a fresh console) silently took the default, so an
	// operator who expected to be asked got `dev` without being told.
	//
	// THE INSTALLER IS SUPPOSED TO BE ALL ENCOMPASSING, so it asks here too. `instance` is
	// whatever the flag, the environment or the MSI property already decided, and it is offered
	// as the DEFAULT — pressing ENTER keeps it, which is what makes an unattended run and an
	// answered run agree. promptOut is used rather than stdout so the question lands on the
	// terminal even when stdout is captured.
	// AN INSTANCE THAT IS ALREADY SET UP IS NOT ASKED ABOUT AGAIN.
	//
	// REPORTED: the MSI runs `orchicon install`, and then the operator ran `orchicon install` themselves and was
	// taken through the whole interrogation a second time — "this should have happened automatically with the
	// install that gets kicked off with the MSI. Not sure how many people are going to want to do a second
	// install after another. It doesn't make logical sense."
	//
	// So a run whose instance ALREADY has host state (a data dir with this instance's KEK) and is already
	// answering skips the questions and keeps the recorded decisions. It is the same principle as ports being
	// record-first: an answered install stays answered.
	//
	// It does NOT skip the WORK — the install still converges what is there (an engine installed since the last
	// run is picked up, a stopped service is started). It skips only the ASKING, which is what a second
	// invocation has no business doing.
	alreadySetUp := instanceAlreadyProvisioned(instance)
	if tty != nil && !flags.NonInteractive && !alreadySetUp {
		if chosen, ok := askInstance(tty, instance); ok {
			instance = chosen
		}
	} else if alreadySetUp && tty != nil {
		fmt.Printf("orchicon: instance %q is already installed — keeping its recorded ports and settings "+
			"(pass --reconfigure to be asked again)\n", instance)
	}
	// THE PROMPT GOES TO A STREAM THAT CAN BE WRITTEN, WHICH IS NOT ALWAYS THE ONE WE READ.
	//
	// This used to say `if tty != nil { promptOut = tty }`, on the reasoning that a terminal is
	// a terminal. It is not, on Windows:
	//
	//   promptTerminal() tries /dev/tty, which does not exist there, and falls back to
	//   os.Stdin when it is a character device. So `tty` IS os.Stdin — a console INPUT handle.
	//   Writing the port banner to it goes nowhere, and resolveInstallPorts then blocks reading
	//   answers to a question nobody can see. The install hangs immediately after the instance
	//   prompt, which is exactly where it was reported to stop.
	//
	// `ttyOwned` is the distinction that matters: only a /dev/tty we OPENED OURSELVES is
	// read-write. In the os.Stdin fallback the console's OUTPUT is os.Stdout (CONOUT$ once the
	// setup has reattached its stdio), so that is where the prompt belongs.
	promptOut := io.Writer(os.Stdout)
	if ttyOwned {
		promptOut = tty
	}
	// A NON-INTERACTIVE RUN PASSES NO READER, so planInstallPorts takes the record or the
	// defaults and PRINTS which ports it used. That is the difference between "the wizard's
	// answers were honoured" and "a console asked again over the top of them".
	promptTTY := tty
	if flags.NonInteractive {
		promptTTY = nil
	}
	// CLEAR THIS INSTANCE'S OWN PREVIOUS RUN BEFORE RESOLVING PORTS, NOT AFTER.
	//
	// THE ORDER WAS THE BUG, and it is why a reinstall over a running (or half-dead) instance could not
	// converge. Port resolution came FIRST, and it treats a held port as a conflict:
	//
	//	== resolving the plane residency and host ports
	//	PostgreSQL: 5432 is held by a process this tool cannot identify — find it with PowerShell…
	//	error: PostgreSQL: this instance is already installed on port 5432, which is held by …
	//
	// The cleanup step existed, but it ran further down — so the install aborted BEFORE reaching the code
	// meant to free the port. Measured on the operator's machine: an orphaned supervisor's postgres held
	// 5432, the install failed at resolution, and the operator's own `Stop-Process` was the only thing
	// that ever cleared it. Their report was "the past two uninstall/reinstalls don't start the services
	// and serve up fully automatically".
	//
	// AND FORCE-CLOSING IS THE RIGHT BEHAVIOUR HERE, not a workaround. The operator asked for it
	// plainly: "If someone is actively installing to the dev instance, same ports, etc. then we should
	// automatically assume they are aware and don't care if their processes are force closed to ensure a
	// healthy run." Installing an instance IS the statement of intent to own it, so an install clears
	// what that instance already has running — its supervisor, its orphaned children — and only then
	// decides which ports it will use.
	//
	// IT ONLY TOUCHES WHAT PROVABLY BELONGS TO THIS INSTANCE (an executable inside its own data dir, or
	// its own recorded supervisor PID), so a sibling instance and the host's own PostgreSQL are safe.
	//
	// The port set for this pre-clear comes from the RECORD, not from the resolution below — the record
	// is what the previous run actually bound, which is exactly what has to be released.
	if pre, perr := servicesPortsFor(instance); perr == nil {
		installPhase("clearing this instance's previous run")
		if err := stopInstanceCompletely(instance, pre, os.Stdout); err != nil {
			// NOT FATAL: the resolution below reports honestly if a port is genuinely held by something
			// that is not ours, and the start after it reports what it finds.
			fmt.Printf("note: %v — continuing\n", err)
		}
	}

	ports, err := resolveInstallPorts(instance, residency, promptOut, promptTTY)
	if err != nil {
		return err
	}
	// WRITE THE PORTS DOWN, because the operator can CHANGE them and a wrapper that re-prints them has
	// nothing to read. This is the bug it fixes, seen on Windows: the operator answered 3001 for a port,
	// this installer printed 3001, and then scripts/install.ps1 — which re-prints the URLs after the setup
	// step — printed a hardcoded 3002, because the resolved values existed only as output on a stream it
	// had already consumed. It is the same class of defect the port table itself was introduced to fix
	// ("a URL pointing at a port this install never bound"): every port the operator is PROMPTED for is a
	// port a wrapper cannot guess.
	//
	// The instance's own state dir is the right home for it — it is the record of what THIS instance
	// bound. Failure is a warning rather than fatal: an unwritable home directory must not fail an install
	// that has already resolved everything, and the wrapper's fallback is the documented default.
	// THE RECORD IS COMPLETE, INCLUDING THE PORTS THIS SHAPE DOES NOT PUBLISH.
	//
	// `ports` above holds what the install PROMPTS for and publishes. The instance also BINDS
	// internal listeners that are never exposed — Tempo's gRPC port, its two OTLP receivers, the
	// collector's health extension and its self-metrics exporter — and those are exactly the ports a
	// readiness probe needs by number. Recording only the published half left every record
	// INCOMPLETE (portsRecordIsComplete answered false for every correct install, so the record was
	// never trusted and the defaults were silently re-derived), and anything asking the record for
	// ORCHICON_OTEL_HEALTH_PORT got 0 and reported a healthy collector as "not answering".
	//
	// The values are the SAME resolution the host-services child receives (servicesPortsFor's rule:
	// the record, else the instance defaults, overridden by an explicit ORCHICON_*_PORT), so the
	// written record and the child cannot disagree.
	writeResolvedPorts(instance, withUnpublishedPorts(instance, ports))

	// 1.7. RESOLVE THE INFRASTRUCTURE BINARIES. The services container needs a
	// PostgreSQL server and a NATS server, and there is no image carrying them any
	// more — so this is the step that decides whether a normal person can install
	// Orchicon at all.
	//
	// IT RUNS HERE, ON THE HOST, BEFORE THE CONTAINER EXISTS, for two reasons that
	// are both about the operator rather than about tidiness. First, the download is
	// the second-slowest part of a fresh install after the image pull, and an
	// operator must see it start before being asked nothing else. Second, the host
	// path needs NO ROOT: the build lands under the instance's own state dir, which
	// the operator already owns, so this works on a machine where `sudo` is not
	// available. (The services container used to mount that dir; the instance is host
	// the services container runs the SAME binaries rather than downloading a second
	// copy of them.)
	//
	// A failure here is fatal, deliberately: "the instance came up with no database"
	// is a much worse outcome than "the install stopped and said why".
	installPhase("resolving the infrastructure binaries (postgres, nats)")
	{
		opt := defaultAcquireOptions(hostDataDirFor(instance))
		infra, err := acquireInfraBinaries(context.Background(), opt)
		if err != nil {
			return fmt.Errorf("resolve the infrastructure binaries: %w", err)
		}
		fmt.Printf("infrastructure binaries: %s — postgres %s (%s), nats-server %s (%s)\n",
			infra.Source, infra.PostgresVersion, infra.Postgres, infra.NatsVersion, infra.NatsServer)
	}

	// 2. RUNTIME IMAGES ONLY. The instance no longer has an image: the plane and its services are host
	// processes. What remains is the per-workflow runtime container's image, and that is fetched ON DEMAND
	// (the Runtime Images page, or the moment a project opts into runtime containers) — never required by
	// an install. A project starts in execution_mode=local, so a user who never opts in needs no image at
	// all, and pulling multi-GB layers up front made every install pay for a feature it might not use.
	//
	// With no engine there is nothing to pull and nothing to run it in, so the step is skipped rather
	// than failed: absence of an engine is a supported mode, not a broken host.
	if containersUsed {
		installPhase("runtime images (optional)")
		// ASKED ONCE PER INSTANCE, then answered from the record. An install runs on every instance-up and
		// every rebuild, so re-asking made a rebuild an interactive command — and reboots/CI/scripts have no
		// one to answer. ORCHICON_RUNTIME_IMAGES_ASK=1 forces the question back for a deliberate change.
		forceAsk := strings.TrimSpace(os.Getenv("ORCHICON_RUNTIME_IMAGES_ASK")) != ""
		// THE ANSWER IS READ FROM THE TERMINAL, NOT FROM STDIN. Under `curl … | bash` (and this script's own
		// `curl … | bash -s -- …`) STDIN IS THE INSTALLER SCRIPT, already at EOF — so a prompt reading stdin
		// would render and then answer "none" by itself, without ever having asked. It is the same defect the
		// port prompt documents in install_ports.go promptTerminal, and the same fix: the tty opened above
		// when there is one, stdin only when there is genuinely nothing else.
		answerIn := io.Reader(os.Stdin)
		if tty != nil {
			answerIn = tty
		}
		installChoices, cerr := resolveInstallRuntimeImagesFor(instance, promptOut, answerIn, imageTag, forceAsk)
		if cerr != nil {
			fmt.Printf("warning: %v\n", cerr)
		}
		if len(installChoices) == 0 {
			if forceAsk {
				recordRuntimeImagesAnswer(instance, "none")
			}
		} else {
			recordRuntimeImagesAnswer(instance, selectionIndexes(installChoices))
			ensureInstallRuntimeImages(installChoices, os.Stdout)
		}
	} else {
		installPhase("skipping the image pull — no container engine")
		fmt.Println("orchicon: runtime images are only needed for per-workflow runtime containers, and there is nothing on this host to run them in")
	}

	// 3. Ensure the host-side runtime daemon is running (the container
	// mounts its socket directory; per-workflow runtime containers spawn
	// from the runtime image above).
	//
	// THE DAEMON IS OPTIONAL WHEN THERE IS NO ENGINE, and making it fatal was a defect that
	// stopped a whole install.
	//
	// The install has ALREADY decided, one step earlier, that this host has no container engine:
	//
	//	== skipping the image pull — no container engine
	//	orchicon: runtime images are only needed for per-workflow runtime containers, and there
	//	is nothing on this host to run them in
	//
	// …and then failed at this step, because the daemon cannot start without an engine either.
	// So the install said the feature was unavailable and then refused to finish because the
	// feature's helper was missing. MEASURED on a real Windows install: the install aborted
	// with "runtime daemon did not become ready", having already started nothing — no services,
	// no plane — and the operator was left with a desktop app that could not reach anything.
	//
	// AND THE PLANE DOES NOT NEED IT. The server's own comment says so:
	//
	//	When the socket is absent (headless `orchicon serve`), the lifecycle is disabled and
	//	execution stays in-process.
	//
	// The daemon exists for PER-WORKFLOW RUNTIME CONTAINERS — the one feature this host has no
	// engine for. Without an engine there is nothing it could do, so its absence is a
	// DEGRADATION TO REPORT, never a reason to refuse to install.
	installPhase("starting the runtime daemon")
	if err := ensureInstallDaemon(socketDir); err != nil {
		// THE DAEMON IS OPTIONAL WHENEVER CONTAINERS ARE UNAVAILABLE — AND "AN ENGINE IS INSTALLED" IS NOT
		// "CONTAINERS ARE AVAILABLE".
		//
		// This asked containerEngineAvailable(), which answers "is an engine BINARY present". MEASURED on
		// the affected Windows machine, Docker Desktop was installed but its daemon was not running, so:
		//
		//	== checking the container engine
		//	orchicon: the container engine is installed but cannot run a container: failed to connect to
		//	          the docker API at npipe:////./pipe/dockerDesktopLinuxEngine …
		//	orchicon: runtime containers are unavailable — … work runs in-process
		//	== starting the runtime daemon
		//	error: runtime daemon did not become ready            <-- AND THE WHOLE INSTALL DIED HERE
		//
		// "Installed but cannot run a container" is exactly the state the install had ALREADY decided to
		// carry on from one step earlier — and then it made that same state fatal here. The operator's
		// report was "it did NOT start orchicon after install", and this is why: the install aborted
		// BEFORE the data dir, the services and the plane, which all sit below this line.
		//
		// So the gate is the DECISION the install already made (engineStatus, set by
		// resolveContainerEngine), not a second, coarser probe. The daemon exists to serve per-workflow
		// runtime containers; when there is nothing to run them in, it has no job.
		if !engineStatus.Available {
			fmt.Printf("orchicon: the runtime daemon could not start (%v)\n", err)
			fmt.Println("orchicon: that only affects PER-WORKFLOW RUNTIME CONTAINERS; work runs in-process," +
				" and the plane, the web UI and the TUI are unaffected.")
			if reason := engineStatus.Reason; reason != "" {
				fmt.Printf("orchicon: containers are unavailable because: %s\n", reason)
			}
			fmt.Println("orchicon: to enable runtime containers later, start your container engine and re-run: " +
				"orchicon install")
		} else {
			return err
		}
	}

	// 4. THE HOST DATA DIR IS PREPARED BEFORE ANYTHING ELSE TOUCHES IT, and the order is load-bearing in TWO
	// ways.
	//
	// FIRST, the KEK. The plane loads <DataDir>/secrets/kek on boot, so this must precede the plane's start or
	// a fresh empty host dir would mint a NEW KEK and orphan every tenant secret.
	//
	// SECOND, and this was wrong before: it must precede the SERVICES, because a services boot writes its own
	// state (a postgres cluster, config/) into the data dir — so a guard that ran afterwards would inspect the
	// instance's OWN fresh boot state rather than what was there before. Observed on a real install: a
	// no-op migration dressed as a real one, moments after the volume it "migrated" had been created.
	//
	// It is resolved whenever the plane is host-resident, because that is the only shape that reads this dir
	// (the container shape mounts a volume instead).
	var hostDir string
	if residency == residencyHost {
		var err error
		hostDir, err = hostDataDir(instance)
		if err != nil {
			return err
		}
		if err := ensureHostDataDir(instance, hostDir, os.Stdout); err != nil {
			return err
		}
	}

	// 4b. BRING THE SERVICES UP — ALWAYS AS HOST PROCESSES.
	//
	// This used to branch on whether a container engine was present, which made the INSTANCE SHAPE depend on
	// an unrelated fact. Worse, it defeated a migration: an operator who had moved postgres to the host got a
	// brand-new empty cluster inside a container volume, with their real data sitting untouched beside it —
	// and, under podman's passt forwarding, a pg_hba rule naming the network GATEWAY while the connection
	// arrives from the host's own address, so the plane could not even connect. Host services are now the one
	// shape, and an engine no longer decides where the instance's database lives.
	// 4b-i. CLEAR THIS INSTANCE'S OWN LEFTOVERS FIRST.
	//
	// AN INSTALL MUST CONVERGE, INCLUDING OVER A RUNNING INSTANCE. This path used to start the services
	// unconditionally, so a reinstall over an instance whose supervisor was mid-restart could put a
	// SECOND supervisor on the same ports — and a supervisor killed by a Stop-Process (the operator's
	// own recovery attempt) leaves its children running, holding postgres's socket and the data
	// directory. The next postgres then refuses:
	//
	//	FATAL:  lock file "postmaster.pid" already exists
	//
	// The operator's message for exactly this was "it did NOT start orchicon after install", and it was
	// true: the install had no step that decided what was already running.
	//
	// The quiesce terminates only processes whose executable is inside this instance's own data dir, so
	// it cannot touch a sibling instance or the host's own PostgreSQL, and it WAITS for the ports — which
	// is what makes the start that follows bind reliably rather than win a race.
	// STOP WHAT THIS INSTANCE ALREADY HAS RUNNING, THE SAME WAY `orchicon stop` DOES.
	//
	// AN INSTALL MUST CONVERGE, INCLUDING OVER A RUNNING INSTANCE. Starting the services unconditionally
	// put a SECOND supervisor on the same ports when a reinstall met a live one; and a supervisor killed by
	// the operator's own Stop-Process leaves re-parented children holding postgres's data directory, which
	// is the "FATAL: lock file postmaster.pid already exists" the operator kept hitting.
	//
	// THIS IS stopInstanceCompletely, not a bare quiesce, and the difference matters: a bare quiesce would
	// kill a LIVE supervisor's children under it, and the supervisor would restart them all with backoff —
	// measured as postgres restarts=6, nats=4, loki=3 from a single install command. The shared function
	// stops the supervisor FIRST, so what the quiesce then finds is genuinely an orphan.
	installPhase("stopping anything already running for this instance")
	if err := stopInstanceCompletely(instance, ports, os.Stdout); err != nil {
		// NOT FATAL HERE: the instance may have been half-up, and `startHostServices` below reports what it
		// finds. A stop that could not finish is stated, and the start that follows fails honestly if it
		// must — which is better than aborting an install that is otherwise complete.
		fmt.Printf("note: %v — continuing the install over what is left\n", err)
	}

	installPhase("starting the host services")
	if err := startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout); err != nil {
		return err
	}

	// 4c. The services are up, so wait for them and then start the plane on THIS host. Waiting first keeps the
	// plane's migrations from racing a half-started postgres.
	{
		// THE RETURN VALUE IS THE POINT, AND IT USED TO BE THROWN AWAY.
		//
		// This read `waitServicesReady(instance, ports, os.Stdout)` and ignored the bool, so an
		// install whose DATABASE NEVER CAME UP went on to start the plane anyway, found nothing
		// to talk to, and reported success. The operator's experience was an installer that
		// finished cleanly and a product where nothing ran — with the real cause buried in a
		// services log nobody had been told about.
		//
		// A services stack that is not up makes the rest of the install meaningless, so this is
		// now fatal, and it names the log that says why.
		if !waitServicesReady(instance, ports, os.Stdout) {
			return servicesNotReadyError(instance, hostDataDirFor(instance))
		}
		if err := startHostPlane(instance, ports, hostDir, filepath.Join(socketDir, "runtime.sock"), os.Stdout); err != nil {
			return err
		}
		// THE HOST SHAPE IS RECORDED HERE. It is the ONLY shape now, and the record is still the thing
		// `orchicon restart` (and every other plane-lifecycle surface that reuses startHostPlane/stopHostPlane)
		// has to decide which carrier to bounce.
		//
		// WITHOUT THIS THE HOST-SERVICES PATH LEFT NO RECORD AT ALL. The record was written only by the
		// container arm, which a host with no engine never reaches: so an install with no engine SUCCEEDED,
		// printed a summary whose own restart recipe is `orchicon restart`, and that command then answered
		// "no install record for instance %q — nothing is installed for it" over the instance this very run
		// had just brought up. Observed by reading the flow, not by a user — the same class of defect as the
		// dead `docker inspect`, arriving through the command the installer recommends.
		recordResidency(instance, residency)
	}

	// 5. Wait for the control plane to serve health, ON THE PORT THIS INSTALL
	// PUBLISHED — read back from the resolved set rather than re-derived from
	// the instance name. Re-deriving is how a moved port gets probed at the old
	// one, and a neighbouring instance answering there would report a healthy
	// install that is not this one.
	controlPort := fmt.Sprintf("%d", ports["ORCHICON_CONTROL_PORT"])
	healthURL := "http://localhost:" + controlPort + "/healthz"
	healthWait := 90 * time.Second
	installPhase("waiting for the control plane")
	fmt.Printf("waiting for %s (up to %s) …\n", healthURL, healthWait)
	healthy := false
	deadline := time.Now().Add(healthWait)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(healthURL); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				healthy = true
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	// A TIMEOUT IS NOT SUCCESS, and reporting one as success is the worst outcome
	// available: the operator is told the install is running, opens a URL that does
	// not answer, and is given nothing to go on. Say what happened, say where the
	// reason is, and let the summary describe the install honestly — see
	// printInstallInfo's healthy flag.
	// ONE REPAIR ATTEMPT. An install that leaves a broken instance without trying to fix it is what the
	// operator hit — "said it was complete but no auth works" — so a plane that does not answer, or a
	// services stack that is not ready, gets one bounded restart before the install reports honestly.
	//
	// ONE, not a loop: a plane that cannot come up twice is a defect to report, not to retry, and an
	// installer that keeps bouncing an instance is worse than one that says what it found.
	if !healthy || !servicesReady(instance, ports) {
		if !healthy {
			fmt.Printf("warning: the control plane did not answer %s within %s\n", healthURL, healthWait)
		} else {
			fmt.Printf("warning: %s services are not ready\n", instance)
		}
		fmt.Println("attempting one repair: restarting the instance's services and plane …")

		// SERVICES FIRST — the plane talks to its Postgres and NATS.
		{
			// HOST SERVICES are stopped and started rather than restarted in place: there is no engine to ask
			// for a bounce, and a supervisor that is already up holds the ports the new one needs.
			if err := stopHostServices(instance, os.Stdout); err != nil {
				fmt.Printf("  could not stop the host services: %v\n", err)
			} else if err := startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout); err != nil {
				fmt.Printf("  could not start the host services: %v\n", err)
			} else if waitServicesReady(instance, ports, os.Stdout) {
				fmt.Printf("  %s services are ready after the restart\n", instance)
			}
		}
		// Then the plane, so it reconnects to a stack that has settled rather than racing its boot.
		if residency == residencyHost {
			if err := stopHostPlane(instance, os.Stdout); err == nil {
				if err := startHostPlane(instance, ports, hostDataDirFor(instance), filepath.Join(socketDir, "runtime.sock"), os.Stdout); err != nil {
					fmt.Printf("  could not restart the plane: %v\n", err)
				}
			}
		}
		deadline = time.Now().Add(healthWait)
		for time.Now().Before(deadline) {
			if resp, err := http.Get(healthURL); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					healthy = true
					break
				}
			}
			time.Sleep(2 * time.Second)
		}
		if healthy {
			fmt.Printf("  %s is answering after the repair\n", healthURL)
		} else {
			fmt.Println("  the repair did not bring it up")
		}
	}

	if !healthy {
		fmt.Printf("warning: the control plane did not answer %s within %s\n", healthURL, healthWait)
		// POINT AT THE LOG THAT EXISTS ON THIS HOST. `docker logs` is the right answer for exactly one of the
		// three combinations — a plane running in the instance container — and on a host with no engine it
		// names a binary that is not installed. That is the same defect this item removes from the install
		// path, so it is removed from the install's ADVICE too: the host plane writes to the instance state
		// dir, and the services write to services.log when they are host processes.
		fmt.Println("  the instance itself is up. find out why the plane is not:")
		fmt.Printf("    %s\n", filepath.Join(hostDataDirFor(instance), "serve", "logs", "orchicon.log"))
		fmt.Printf("    %s   (the host services)\n", filepath.Join(hostDataDirFor(instance), "services.log"))
		fmt.Println("  then re-run `orchicon install` — it is idempotent")
	}

	// 5b. SAY WHICH OPTIONAL COMPONENTS ARE NOT ANSWERING.
	//
	// This runs AFTER the plane is confirmed up, and that ordering is the point: a plane that
	// answers /healthz but has no Loki is a WORKING instance whose Logs view is dead, and that is
	// worth stating plainly — but it is not a failed install, so it must not turn into a non-zero
	// exit over the whole run.
	//
	// WHY IT IS SAID AT ALL. `services: up` used to mean only "postgres and nats answer", so an
	// install could bring up a stack whose entire telemetry plane had crash-looped and finish with
	// no indication of it. The operator found out by opening the Logs view and being told the
	// backend was unreachable. Four words here replace that hunt.
	if down := degradedServices(instance, ports); len(down) > 0 {
		fmt.Printf("note: %s is up, but these telemetry components are not answering: %s\n",
			instance, strings.Join(down, ", "))
		fmt.Println("      the UI and API work; the Logs/Traces/Metrics views for them will report")
		fmt.Println("      their backend as unreachable until they do. Why:")
		fmt.Printf("        %s\n", filepath.Join(hostDataDirFor(instance), "services.log"))
		fmt.Printf("      They are retried automatically while the services run, and `%s status %s` lists them.\n",
			filepath.Base(os.Args[0]), instance)
	}

	// 6. Install/refresh the `orch` companion launcher on PATH (the thin
	// remote TUI client). Non-fatal: a missing sibling binary or a clobber
	// guard only warns.
	installDir := env("ORCHICON_INSTALL_DIR", defaultInstallDir())
	installOrchLauncher(installDir)

	printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir, ports, containersUsed, healthy, engineStatus)

	// THE HANDOVER, AND IT HAPPENS HERE BECAUSE THE FACT IS KNOWN HERE.
	//
	// `healthy` above is the result of WAITING for the control plane to answer /healthz — so this is the one
	// place in the product that knows the environment is actually up. The wizard's Finish page never could:
	// WixUI owns that navigation, and three attempts to put a launch there failed (install_desktop.go records
	// them). The installer stops asking and the thing that did the work opens the app.
	//
	// ONLY ON A HEALTHY INSTALL. Opening a GUI onto a plane that is not answering would show the operator the
	// failure inside a window with no context — worse than not opening it.
	//
	// The flag OR the environment, so a shell script can request the same thing the MSI does.
	if (flags.LaunchDesktop || env(desktopLauncherEnv, "") == "1") && healthy {
		launchDesktop()
	}
	return nil
}

// resolvedPortsFile is where an install records the host ports it actually bound, as JSON keyed by the
// ORCHICON_*_PORT names. Read back by wrappers that print connection info (scripts/install.ps1) and by
// anything else that needs "what did this instance end up on" without re-deriving it.
//
// IT IS NAMED "ports.json" AND LIVES IN THE INSTANCE STATE DIR, which matters to uninstall: a state dir
// containing ONLY this file is not an instance, and the shell installer's instance_exists must not treat
// it as one — otherwise installing to a scratch dir would look like another instance still needed the
// binaries. See scripts/install.sh.
func resolvedPortsFile(instance string) (string, error) {
	dir, err := hostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ports.json"), nil
}

// writeResolvedPorts records the ports this install bound. Best-effort: it returns nothing and a failure
// is a warning, because the install has already succeeded by this point and losing the record costs the
// operator only a stale URL in a wrapper's summary.
func writeResolvedPorts(instance string, ports map[string]int) {
	path, err := resolvedPortsFile(instance)
	if err != nil {
		fmt.Printf("warning: could not locate the instance state dir to record the ports: %v\n", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Printf("warning: could not record the resolved ports: %v\n", err)
		return
	}
	// SORTED, STABLE OUTPUT: a map marshals in key order, and the file is read by humans and by a shell
	// one-liner, so a jumbled line would be needlessly hard to read.
	b, err := json.MarshalIndent(ports, "", "  ")
	if err != nil {
		fmt.Printf("warning: could not record the resolved ports: %v\n", err)
		return
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		fmt.Printf("warning: could not record the resolved ports: %v\n", err)
	}
}

// withUnpublishedPorts fills in the ports this instance BINDS but does not PUBLISH, so the record
// is a complete picture of the instance rather than only its public half.
//
// A port already in ports is kept as-is: the operator's answer for a published port is authoritative
// and must not be second-guessed by a default. Everything else comes from the same per-instance
// default table the install and the supervisor share.
func withUnpublishedPorts(instance string, ports map[string]int) map[string]int {
	out := make(map[string]int, len(ports)+8)
	for k, v := range ports {
		out[k] = v
	}
	for _, p := range installerPorts() {
		if out[p.Env] > 0 {
			continue
		}
		if d, ok := p.defaultFor(instance); ok {
			out[p.Env] = d
		}
	}
	return out
}

// defaultInstallDir returns the default launcher install directory
// (~/.local/bin), mirroring scripts/install.sh.
func defaultInstallDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".local/bin"
	}
	return filepath.Join(home, ".local", "bin")
}

// installOrchLauncher symlinks the sibling `orch` binary (next to the
// running executable) into installDir. Idempotent: an existing symlink we
// own is refreshed in place; a pre-existing regular file is warned + skipped
// unless ORCHICON_FORCE_LAUNCHER=1 replaces it; a missing sibling binary is
// a non-fatal warning.
func installOrchLauncher(installDir string) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("warning: could not locate the running executable to find the sibling orch binary: %v\n", err)
		return
	}
	installOrchLauncherFrom(exe, installDir)
}

// installOrchLauncherFrom symlinks the sibling `orch` binary next to the
// given executable path into installDir. Split out so tests can pass a
// controlled executable path (os.Executable() is not redirectable).
func installOrchLauncherFrom(exe, installDir string) {
	sibling := filepath.Join(filepath.Dir(exe), "orch")
	if runtime.GOOS == "windows" {
		sibling += ".exe"
	}
	if _, err := os.Stat(sibling); err != nil {
		fmt.Printf("warning: sibling orch binary not found next to %s — skipping launcher install (build with `make build` to produce bin/orch)\n", exe)
		return
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		fmt.Printf("warning: could not create install dir %s: %v\n", installDir, err)
		return
	}
	link := filepath.Join(installDir, "orch")
	if runtime.GOOS == "windows" {
		link += ".exe"
	}
	// An existing path at the launcher location.
	//
	// A SYMLINK POINTING ELSEWHERE IS NOT OURS TO REPOINT, and that was a real bug
	// caught in the wild: `orchicon install` run from a build directory refreshed
	// the symlink to point at ITS OWN sibling, silently hijacking a launcher that
	// another install — or a development checkout — had put there. The operator
	// then ran `orch` and got a binary they never chose, with the thing they DID
	// choose quietly detached.
	//
	// So only a symlink already pointing at OUR sibling is treated as ours, and is
	// then a no-op. Anything else — a foreign symlink, a regular file — is left
	// alone with a warning, and ORCHICON_FORCE_LAUNCHER=1 is the explicit opt-in to
	// replace it. Installing must never relocate someone else's launcher.
	if st, err := os.Lstat(link); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(link)
			if target == sibling {
				fmt.Printf("orch launcher already installed: %s\n", link)
				return
			}
			if os.Getenv("ORCHICON_FORCE_LAUNCHER") != "1" {
				fmt.Printf("warning: %s is a symlink to %s, not to this install's %s — leaving it alone (set ORCHICON_FORCE_LAUNCHER=1 to replace)\n", link, target, sibling)
				return
			}
			fmt.Printf("replacing existing %s (ORCHICON_FORCE_LAUNCHER=1)\n", link)
			_ = os.Remove(link)
		} else {
			// A regular file (or other non-symlink) occupies the path.
			if os.Getenv("ORCHICON_FORCE_LAUNCHER") != "1" {
				fmt.Printf("warning: %s exists and is not an orch symlink — skipping (set ORCHICON_FORCE_LAUNCHER=1 to replace)\n", link)
				return
			}
			fmt.Printf("replacing existing %s (ORCHICON_FORCE_LAUNCHER=1)\n", link)
			_ = os.Remove(link)
		}
	}
	if err := os.Symlink(sibling, link); err != nil {
		fmt.Printf("warning: could not symlink orch launcher: %v\n", err)
		return
	}
	fmt.Printf("orch launcher installed: %s → %s\n", link, sibling)
}

// adapterCLIPresent reports whether an adapter CLI is installed on the host
// (on PATH or at ~/.<name>/bin/<name>).
//
// IT IS A PRESENCE CHECK, NOT A REQUIREMENT. It was requireAdapterCLI and returned
// an error, which made `orchicon install` refuse to run on a host with no external
// adapter — true when opencode was the only way to run anything, and false since the
// plane gained its own engine. When the binary IS present it is still bind-mounted
// into the containers (see the mount list above), so its absence costs the operator
// only that capability, never the install.
func adapterCLIPresent(name string) bool {
	cands := adapterCandidates(name)
	return len(cands) > 0
}

// adapterCandidates is EVERY place the adapter was found, with provenance — the surface that makes discovery
// visible instead of magic.
//
// IT REPLACES A BARE BOOL, and the reason is a reported experience: an operator who has the CLI installed and
// working in their shell was told it was not found, with no way to see WHERE we looked or WHAT we would run.
// A list answers both questions, and it is the only form that can tell them two installs exist.
//
// Returns nil when nothing was found (an empty list is normal — an adapter is optional).
func adapterCandidates(name string) []adapter.Candidate {
	home, _ := os.UserHomeDir()
	return adapter.Candidates(adapter.HostGOOS(), home, name, os.Getenv)
}

// reportAdapterCLI prints what was found, and is DELIBERATELY CHATTY about ambiguity.
//
// ONE INSTALL names the path. TWO OR MORE is the case that must never be silent: different installs are
// different versions and possibly different credentials, so the operator needs to see both and choose — a
// silent pick is how a session signs in as the wrong account. It does not refuse the install (the adapter is
// optional and a container can still run), it makes the choice visible.
func reportAdapterCLI(name string) {
	cands := adapterCandidates(name)
	switch {
	case len(cands) == 0:
		return // optional, and nothing to say
	case len(cands) == 1:
		c := cands[0]
		fmt.Printf("orchicon: %s found on this host at %s (%s) — optional, and it will be used as a runtime "+
			"when a model ref asks for it\n", name, c.Path, c.Source)
		if !c.ContainerOK {
			fmt.Printf("orchicon:   NOTE: runtime containers cannot use that binary — %s\n", c.Why)
		}
	default:
		fmt.Printf("orchicon: %s is installed %d times; Orchicon will use the first:\n", name, len(cands))
		for i, c := range cands {
			mark := " "
			if i == 0 {
				mark = "*"
			}
			fmt.Printf("orchicon:   %s %s (%s)%s\n", mark, c.Path, c.Source, containerNote(c))
		}
		fmt.Printf("orchicon:   to choose another, set %s to the one you want\n", adapter.BinEnv(name))
	}
}

// containerNote renders the container-usable half for the listing, empty when there is nothing to say.
func containerNote(c adapter.Candidate) string {
	if c.ContainerOK {
		return ""
	}
	return " — NOT usable in runtime containers (Linux containers cannot execute it)"
}

// ensureInstallDaemon starts the runtime daemon if its socket is not
// healthy. The daemon owns the Docker socket and spawns per-workflow
// runtime containers; the supervisor container mounts its socket dir.
func ensureInstallDaemon(socketDir string) error {
	socketPath := filepath.Join(socketDir, "runtime.sock")
	// HEALTHY IS NOT THE SAME AS CURRENT.
	//
	// This used to return early on any live socket, which let a daemon started from an older binary
	// outlive every upgrade. REPORTED as "runtime image fetch failed for orchicon-runtime:local:
	// runtime image pull: runtime daemon: request failed", diagnosed on the reporting host: the
	// daemon had been up 8d19h with /proc/<pid>/exe pointing at a DELETED inode, so it did not have
	// the /v1/images/pull route the current client calls — and it answered 404, which the client then
	// flattened into "request failed". A liveness-only check cannot see any of that.
	//
	// SO THE BUILD IS COMPARED. /v1/health reports the daemon's tag+commit (internal/runtime), and a
	// mismatch means this install wrote a newer binary than the one serving the socket: stop it and
	// start the new one. That is safe here because the daemon spawns per-workflow containers and
	// holds no durable state of its own — the images live in the engine, not in the process.
	if socketHealthy(socketPath) {
		if current, running := daemonBuildMatches(socketPath); current {
			fmt.Println("runtime daemon already running")
			return nil
		} else if running != "" {
			fmt.Printf("runtime daemon is running an older build (%s) — restarting it on this one\n", running)
			if err := stopDaemon(socketPath); err != nil {
				fmt.Printf("  (could not stop the old daemon: %v — it will keep serving until it exits)\n", err)
			}
		} else {
			// A daemon that answers /v1/health with {"status":"ok"} but no build is from before this
			// check existed, i.e. certainly older than the binary being installed.
			fmt.Println("runtime daemon is running an unversioned build — restarting it on this one")
			if err := stopDaemon(socketPath); err != nil {
				fmt.Printf("  (could not stop the old daemon: %v — it will keep serving until it exits)\n", err)
			}
		}
	}
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		return fmt.Errorf("create runtime socket dir: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		self = "orchicon"
	}
	fmt.Println("starting runtime daemon …")
	if err := startDetachedDaemon(self, []string{"runtime-daemon"}, filepath.Join(socketDir, "runtime-daemon.log")); err != nil {
		return fmt.Errorf("start runtime daemon: %w", err)
	}
	// Wait for the daemon to answer /v1/health. Said out loud, because ten silent
	// seconds is indistinguishable from a hang — the same reason every other wait in
	// this command now announces itself.
	fmt.Println("waiting for the runtime daemon to answer /v1/health …")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if socketHealthy(socketPath) {
			fmt.Println("runtime daemon up")
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("runtime daemon did not become ready — see %s/runtime-daemon.log", socketDir)
}

// daemonBuildMatches reports whether the daemon on socketPath was built from the same source as THIS
// binary. current is true when it matches (or when the socket is unanswerable, in which case the
// caller starts a fresh one anyway); running is the daemon's tag+commit, empty when it predates the
// build being reported at all.
func daemonBuildMatches(socketPath string) (current bool, running string) {
	out, err := daemonHealth(socketPath)
	if err != nil {
		return false, ""
	}
	if out["status"] != "ok" {
		return false, ""
	}
	tag, commit := out["tag"], out["commit"]
	if tag == "" && commit == "" {
		return false, "" // pre-versioning daemon: older than anything we ship now
	}
	mine := version.Current()
	if tag != mine.Tag || commit != mine.Commit {
		return false, strings.TrimSpace(tag + " " + commit)
	}
	return true, tag
}

// daemonHealth reads /v1/health off the runtime daemon socket.
func daemonHealth(socketPath string) (map[string]string, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://runtime/v1/health")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// stopDaemon stops the runtime daemon by asking it to exit, then removes the stale socket.
//
// BY HTTP, NOT BY SIGNAL, and not by process name: a signal would have to find the pid (unavailable
// on Windows, where this runs too) and a name match could hit a second instance's daemon. The daemon
// already serves a shutdown route for exactly this; if it does not answer, the socket is unlinked so
// the next start is not blocked by a dead listener.
func stopDaemon(socketPath string) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest(http.MethodPost, "http://runtime/v1/shutdown", nil)
	if err == nil {
		if resp, derr := client.Do(req); derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	// Wait for the socket to stop accepting, so the caller's start does not race the old listener.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !socketHealthy(socketPath) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Still answering: remove the socket file so a new daemon can bind. The old process keeps
	// running on its now-unlinked inode and exits on its own; nothing else references it.
	_ = os.Remove(socketPath)
	return nil
}

func socketHealthy(socketPath string) bool {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://runtime/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode == 200 && out["status"] == "ok"
}

func dirOnPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == dir {
			return true
		}
	}
	return false
}

// imagePresent reports whether a Docker image tag exists locally.
func imagePresent(img string) bool {
	out, err := engineCommand("image", "inspect", img).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// runtimeVariantRef returns the registry ref for a derived runtime image variant
// (:gui or :dev) at the requested tag.
//
// THE SHAPE MIRRORS THE RELEASE WORKFLOW EXACTLY, and the installer previously
// got it wrong. release.yml publishes, for a release:
//
//	<base>:gui-<version>   and the FLOATING   <base>:gui
//	<base>:dev-<version>   and the FLOATING   <base>:dev
//
// so the floating tag DROPS the version rather than becoming "-latest". The
// installer built "<suffix>-<tag>", which for the default tag "latest" produced
// ":gui-latest" — published by nothing, which aborted every fresh install at the
// image pull. TestRuntimeVariantRefMatchesReleaseWorkflow pins this against
// release.yml so the two cannot drift again.
func runtimeVariantRef(suffix, imageTag string) string {
	const base = "ghcr.io/beardedparrott/orchicon-runtime"
	if imageTag == "" || imageTag == "latest" {
		return base + ":" + suffix
	}
	return base + ":" + suffix + "-" + imageTag
}

// ensureImage makes the local copy of ref CURRENT, pulling it.
//
// A LOCAL TAG IS NOT EVIDENCE OF CURRENCY, and assuming it was is what shipped a container that could never
// be upgraded. `pullImageIfAbsent` skipped the pull whenever the tag existed locally, so `:latest` — a
// MOVABLE pointer — stayed at whatever the host first downloaded, for ever. An install then pulled nothing,
// compared the container against that same stale local image, found them equal, and correctly concluded
// there was no drift: the instance kept running 11-hour-old layers across any number of installs, which is
// exactly the report ("the container was restarted but the created date still has 11 hours ago").
//
// SO THE RULE IS: ASK THE REGISTRY. A tag can be repointed at any time; only a DIGEST is immutable, and only
// a digest may be trusted from the local cache. There is no tag-name heuristic here (not "latest is
// floating, vX.Y.Z is pinned") on purpose: a release tag can be re-pushed too, and a rule that guesses
// which tags move is a rule that is wrong silently. A pull of an unchanged image is a manifest check.
//
// It is safe to insist on the pull because nothing else calls this installer with a locally-built image:
// `scripts/container.sh` builds and runs its own containers for dev and never invokes `orchicon install`.
func ensureImage(ref string) error {
	if strings.Contains(ref, "@sha256:") {
		if imagePresent(ref) {
			fmt.Printf("image %s present (pinned by digest, cannot move)\n", ref)
			return nil
		}
		return pullImage(ref)
	}
	fmt.Printf("refreshing %s …\n", ref)
	return pullImage(ref)
}

// pullImage runs `docker pull` for ref, streaming its progress.
func pullImage(img string) error {
	fmt.Printf("pulling %s …\n", img)
	// THE PULL'S OUTPUT IS STREAMED, NOT CAPTURED. This is the longest stretch of a
	// first install — four images, hundreds of megabytes — and it used to be
	// swallowed: CombinedOutput held docker's progress until the command finished, so
	// a working install was indistinguishable from a hung one until it was over.
	// docker's progress now goes to this process's stdout as docker writes it, and a
	// bounded tail is kept so a failure can still say WHY.
	cmd := engineCommand("pull", img)
	tail := &tailWriter{limit: 4096}
	cmd.Stdout = io.MultiWriter(os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pull %s: %v: %s", img, err, strings.TrimSpace(tail.String()))
	}
	fmt.Printf("pulled %s\n", img)
	return nil
}

// tailWriter keeps only the LAST `limit` bytes written to it, so a failed command can
// still report why without holding an unbounded amount of progress output in memory.
type tailWriter struct {
	buf   []byte
	limit int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.limit {
		w.buf = w.buf[len(w.buf)-w.limit:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.buf) }

// containerImageCurrent reports whether the running container's image IS the target image, compared by
// IMAGE ID rather than by tag.
//
// A tag is a movable pointer: `:latest` (and every version tag this project publishes) can be repointed at
// a new build while a container created from the previous one keeps running the old layers. Comparing IDs is
// what makes "is this instance up to date?" a fact rather than a guess.
//
// An unreadable state answers TRUE — "leave it alone" — deliberately: recreating a live instance because a
// docker inspect failed transiently is worse than leaving a stale one for the operator to see, which is the
// same call recordedResidency makes about the SHAPE.
func containerImageCurrent(name, target string) bool {
	cur, err := engineCommand("inspect", "--format", "{{.Image}}", name).Output()
	if err != nil {
		return true
	}
	want, err := engineCommand("image", "inspect", "--format", "{{.Id}}", target).Output()
	if err != nil {
		// The target is not local. It has just been pulled, so this should not happen; if it does, the
		// container cannot be compared and is left alone rather than guessed at.
		return true
	}
	return strings.TrimSpace(string(cur)) == strings.TrimSpace(string(want))
}

func containerRunning(name string) (bool, error) {
	out, err := engineCommand("inspect", "--format", "{{.State.Running}}", name).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func containerExists(name string) (bool, error) {
	out, err := engineCommand("inspect", "--format", "{{.Id}}", name).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir string, ports map[string]int, containersUsed, healthy bool, engine containerengine.Status) {
	// WHERE THE SERVICES RUN follows from the shape and the host: a host-resident plane has its services as
	// host processes when there is no engine to put them in a container, and in the instance container when
	// there is.
	//
	// THE ENGINE DECISION IS THE INSTALL'S OWN, THREADED IN RATHER THAN RE-PROBED. This used to call
	// containerEngineAvailable() again right here, which is a SECOND decision that can disagree with the one
	// the install actually acted on (a PATH that changes mid-run, a probe that answers differently) — so the
	// summary could describe a carrier the install never created. The install decides this ONCE; the summary
	// reports THAT decision.
	hostServices := true
	// Read back the ports this install actually bound: re-deriving them from the
	// instance name would print a URL pointing at a port we never bound.
	controlPort := fmt.Sprintf("%d", ports["ORCHICON_CONTROL_PORT"])
	fmt.Println()
	if healthy {
		fmt.Println("┌─────────────────────────────────────────────────────────────┐")
		fmt.Println("│  Orchicon is installed and running.                         │")
		fmt.Println("└─────────────────────────────────────────────────────────────┘")
	} else {
		fmt.Println("┌─────────────────────────────────────────────────────────────┐")
		fmt.Println("│  Orchicon is installed, but its control plane is not        │")
		fmt.Println("│  answering yet — the URLs below will fail until then.       │")
		fmt.Println("└─────────────────────────────────────────────────────────────┘")
	}
	// State the SHAPE, because it decides where the plane lives and therefore how
	// the operator restarts it — a host plane is a process on this machine, not
	// something the container brings back on `docker start`.
	//
	// AND STATE WHERE THE SERVICES RUN, because the restart RECIPE differs: a container shape's services come
	// back with `docker start <name>`, while host services are processes of this user that a container command
	// cannot touch at all.
	if residency == residencyHost {
		fmt.Printf("  Plane residency: host — the control plane runs on this host,\n")
		if hostServices {
			user := os.Getenv("USER")
			if user == "" {
				user = "this user"
			}
			fmt.Printf("                   the services run as HOST processes of %s (postgres, nats, telemetry).\n", user)
		} else {
			fmt.Printf("                   the services run in %s.\n", name)
		}
		if dataDir, err := hostDataDir(instance); err == nil {
			fmt.Printf("  Host state:      %s (secrets KEK, blobs, plane PID/logs)\n", dataDir)
		}
	} else {
		fmt.Println("  Plane residency: container — the whole stack runs in the container.")
	}
	fmt.Printf("  Control plane:  http://localhost:%s\n", controlPort)
	fmt.Printf("  Health check:   curl %s\n", healthURL)
	fmt.Println()
	// The manage-the-instance block names the commands that actually apply to this shape: a container shape is
	// managed with docker, a host shape with the orchestration verbs (a host plane is not something `docker
	// start` brings back, and there may be no docker at all).
	fmt.Println("  Manage the instance:")
	if hostServices {
		fmt.Println("    Restart: orchicon restart   (stops and restarts the host plane and services)")
		// THE STATE DIR IS NAMED, NOT ASSUMED. `serve --stop` resolves its PID file from
		// ORCHICON_SERVE_STATE_DIR, whose default is a RELATIVE `.dev` — so a bare
		// `orchicon serve --stop` run from any other directory looks in the wrong place and answers
		// "serve is not running" over a plane that is very much up. The install already knows the
		// per-instance state dir, so it prints the command that actually stops THIS instance.
		fmt.Printf("    Stop:    ORCHICON_SERVE_STATE_DIR=%s/serve orchicon serve --stop\n", hostDataDirFor(instance))
		fmt.Printf("    Logs:    %s/serve/logs/orchicon.log and %s/services.log\n", hostDataDirFor(instance), hostDataDirFor(instance))
	} else {
		fmt.Printf("    Stop:   docker stop %s\n", name)
		fmt.Printf("    Start:  docker start %s\n", name)
		fmt.Printf("    Logs:   docker logs -f %s\n", name)
	}
	fmt.Println()
	// PER-WORKFLOW RUNTIME CONTAINERS BELONG TO THE CONTAINER ENGINE, so this block reports the shape the
	// install actually produced. It used to be unconditional, and on a host with no engine that made the
	// summary CONTRADICT the install: the same run printed "runtime containers are unavailable (work runs
	// in-process)" in step 1 and then closed by promising "every execution runs in its own short-lived
	// container" and reporting a runtime daemon that will never launch one. The daemon process does run on
	// such a host, but it has no engine to spawn containers with — describing it as the container carrier is
	// exactly the kind of dead-container claim this item removes from the install path.
	if containersUsed {
		fmt.Printf("  Runtime daemon (per-workflow runtime containers): running\n")
		fmt.Printf("    socket: %s/runtime.sock   runtime image: %s\n", socketDir, runtimeImage)
	} else {
		fmt.Println("  Runtime daemon: running, but there is NO usable container engine on this host —")
		fmt.Println("    per-workflow runtime containers cannot be launched, so work runs in-process.")
	}
	// THE AVAILABILITY LINE IS THE INSTALL'S OWN DECISION (ready / provisioning / unavailable
	// with the reason), threaded in rather than re-probed — the summary must describe the
	// carrier this run produced, never a fresh guess (AC5).
	fmt.Printf("  Container engine: %s\n", describeEngineStatus(engine))
	if hostServices {
		fmt.Printf("  Data: host data dir (see above; the KEK lives in its secrets/)\n")
	} else {
		fmt.Printf("  Data: volume %s (preserved across restarts)\n", dataVolume)
	}
	fmt.Println()
	fmt.Printf("  orch launcher: %s/orch (remote TUI client)\n", installDir)
	if !dirOnPath(installDir) {
		fmt.Printf("  %s is not on your PATH — add it to your shell profile:\n", installDir)
		fmt.Printf("    export PATH=\"$PATH:%s\"\n", installDir)
	}
	fmt.Println()
	if containersUsed {
		fmt.Println("  Per-workflow runtime containers are used automatically when a")
		fmt.Println("  workflow runs. Open the UI, log in with the dev IdP, and create")
		fmt.Println("  a workflow — every execution runs in its own short-lived container.")
	} else {
		fmt.Println("  Open the UI, log in with the dev IdP, and create a workflow —")
		fmt.Println("  every execution runs IN-PROCESS on this host (no runtime container).")
	}
}

// instanceAlreadyProvisioned reports whether an instance has already been set up on this host: it has host state
// (its data dir carries a KEK) AND a recorded port set. Both are needed — a KEK alone could be a half-finished
// first run, and a port record alone could be a stale one from an instance since wiped.
func instanceAlreadyProvisioned(instance string) bool {
	dir, err := hostDataDir(instance)
	if err != nil || dir == "" {
		return false
	}
	if _, err := os.Stat(dir + string(os.PathSeparator) + "secrets" + string(os.PathSeparator) + "kek"); err != nil {
		return false
	}
	ports, perr := readResolvedPorts(instance)
	return perr == nil && len(ports) > 0
}
