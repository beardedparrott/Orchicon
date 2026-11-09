package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"github.com/beardedparrott/orchicon/internal/instancectl"
	workruntime "github.com/beardedparrott/orchicon/internal/runtime"
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
var installProgress *setupTracker

// setupTracker writes the machine-readable progress record (design of record, D4) beside the instance's
// ports.json, which is the channel a GUI polls while a detached install runs.
type setupTracker struct {
	instance  string
	lastPhase string
	// startedAt is when THIS install began, carried through every write.
	startedAt string
	// pid is this install process, so "is setup actually running?" is answered by the PROCESS TABLE
	// rather than by guesswork about a file's age.
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

// write is the ONE place a record is written, so every field that must persist (started_at, pid)
// persists on every state — which is exactly what the two real-run gaps.
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
func installPhase(msg string) {
	fmt.Printf("== %s\n", msg)
	installProgress.phase(msg)
}

// runInstall implements `orchicon install` — the one-command setup for a fresh machine.
func runInstall(args []string, log *slog.Logger) error {
	// A NON-INTERACTIVE RUN NEVER RELAUNCHES FOR A CONSOLE.
	interactiveAllowed := !hasNonInteractiveFlag(args)
	// THE MSI'S INSTALL WAS DYING WITH THE WIZARD. A LAUNCHER-ONLY RUN MUST NOT OPEN A WINDOW.
	wantConsole := (!hasNonInteractiveFlag(args) || hasConsoleFlag(args)) && !hasLauncherOnlyFlag(args)
	if wantConsole && ensureInteractiveConsole(interactiveAllowed) {
		log.Info("orchicon install: relaunched in its own console")
		return nil
	}
	// THE WIZARD'S ANSWERS COME IN AS FLAGS AND BECOME THE ENVIRONMENT THIS INSTALL ALREADY READS.
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
	// THE ARGUMENTS ARE PARSED, AND THAT IS A FIX RATHER THAN A TIDY-UP.
	if flags.LauncherOnly {
		// THE ESCAPE HATCH FOR THE ONE FAILURE THAT LEAVES A HEALTHY PLANE.
		installDir := env("ORCHICON_INSTALL_DIR", defaultInstallDir())
		return reportLauncherOnly(installOrchLauncher(installDir), installDir)
	}
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}

	// THE PROGRESS RECORD'S LIFETIME IS THIS INSTALL'S (design of record, D4).
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

	// 1.
	containersUsed, err := resolveContainerEngine()
	if err != nil {
		return err
	}

	// 1.5.
	if adapterCLIPresent("opencode") {
		reportAdapterCLI("opencode")
	} else {
		fmt.Println("orchicon: no external adapter CLI found — the built-in engine will run sessions (nothing to install)")
	}

	// 1.6.
	installPhase("resolving the plane residency and host ports")
	residency, err := residencyForInstall()
	if err != nil {
		return err
	}
	// THE PLANE AND ITS SERVICES ALWAYS RUN ON THE HOST.
	if residency == residencyContainer {
		return fmt.Errorf("ORCHICON_PLANE_RESIDENCY=%s is no longer supported: the plane and its services now\n"+
			"  always run on the HOST. Containers are used only for a workflow's runtime container.\n"+
			"  Unset ORCHICON_PLANE_RESIDENCY (or set it to %s) and re-run.",
			residencyContainer, residencyHost)
	}
	residency = residencyHost
	// 1.6a.
	if err := ensureHostPostgresPrivileges(); err != nil {
		return err
	}
	tty, ttyOwned := promptTerminal()
	if ttyOwned {
		defer tty.Close()
	}
	// 1.6a-bis.
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
	promptOut := io.Writer(os.Stdout)
	if ttyOwned {
		promptOut = tty
	}
	// A NON-INTERACTIVE RUN PASSES NO READER, so planInstallPorts takes the record or the defaults and
	// PRINTS which ports it used.
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
	// nothing to read.
	writeResolvedPorts(instance, withUnpublishedPorts(instance, ports))

	// 1.7.
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

	// 2. RUNTIME IMAGES ONLY.
	if containersUsed {
		installPhase("runtime images (optional)")
		// ASKED ONCE PER INSTANCE, then answered from the record.
		forceAsk := strings.TrimSpace(os.Getenv("ORCHICON_RUNTIME_IMAGES_ASK")) != ""
		// THE ANSWER IS READ FROM THE TERMINAL, NOT FROM STDIN.
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
	installPhase("checking the container binary")
	reportContainerBinary()

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

	// 4.
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
		// finds.
		fmt.Printf("note: %v — continuing the install over what is left\n", err)
	}

	installPhase("starting the host services")
	if err := startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout); err != nil {
		return err
	}

	// 4c. The services are up, so wait for them and then start the plane on THIS host. Waiting first keeps the
	// plane's migrations from racing a half-started postgres.
	{
		// This read `waitServicesReady(instance, ports, os.Stdout)` and ignored the bool, so an install
		// whose DATABASE NEVER CAME UP went on to start the plane anyway, found nothing to talk.
		if !waitServicesReady(instance, ports, os.Stdout) {
			return servicesNotReadyError(instance, hostDataDirFor(instance))
		}
		if err := startHostPlane(instance, ports, hostDir, filepath.Join(socketDir, "runtime.sock"), os.Stdout); err != nil {
			return err
		}
		// It is the ONLY shape now, and the record is still the thing `orchicon restart` (and every other
		// plane-lifecycle surface that reuses startHostPlane/stopHostPlane) has to decide which carrier.
		recordResidency(instance, residency)
	}

	// 5.
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
	// A TIMEOUT IS NOT SUCCESS, and reporting one as success is the worst outcome available: the
	// operator is told the install is running, opens a URL that does not answer.
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
		// POINT AT THE LOG THAT EXISTS ON THIS HOST.
		fmt.Println("  the instance itself is up. find out why the plane is not:")
		fmt.Printf("    %s\n", filepath.Join(hostDataDirFor(instance), "serve", "logs", "orchicon.log"))
		fmt.Printf("    %s   (the host services)\n", filepath.Join(hostDataDirFor(instance), "services.log"))
		fmt.Println("  then re-run `orchicon install` — it is idempotent")
	}

	// 5b.
	if down := degradedServices(instance, ports); len(down) > 0 {
		fmt.Printf("note: %s is up, but these telemetry components are not answering: %s\n",
			instance, strings.Join(down, ", "))
		fmt.Println("      the UI and API work; the Logs/Traces/Metrics views for them will report")
		fmt.Println("      their backend as unreachable until they do. Why:")
		fmt.Printf("        %s\n", filepath.Join(hostDataDirFor(instance), "services.log"))
		fmt.Printf("      They are retried automatically while the services run, and `%s status %s` lists them.\n",
			filepath.Base(os.Args[0]), instance)
	}

	// 6.
	installDir := env("ORCHICON_INSTALL_DIR", defaultInstallDir())
	// THE OUTCOME IS CARRIED INTO THE SUMMARY, not discarded.
	launcher := installOrchLauncher(installDir)
	// THE PATH ENTRY IS MADE HERE FOR THE SAME REASON, AND THE SUMMARY RENDERS THE OUTCOME.
	pathOutcome := ensureLoginProfilePATH(installDir)

	printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir, launcher, pathOutcome, ports, containersUsed, healthy, engineStatus)

	// THE HANDOVER, AND IT HAPPENS HERE BECAUSE THE FACT IS KNOWN HERE. ONLY ON A HEALTHY INSTALL.
	if (flags.LaunchDesktop || env(desktopLauncherEnv, "") == "1") && healthy {
		launchDesktop()
	}
	return nil
}

// IT IS NAMED "ports.json" AND LIVES IN THE INSTANCE STATE DIR, which matters to uninstall: a state
// dir containing ONLY this file is not an instance.
func resolvedPortsFile(instance string) (string, error) {
	dir, err := hostDataDir(instance)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ports.json"), nil
}

// writeResolvedPorts records the ports this install bound.
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

// orchLauncher is WHAT HAPPENED when the companion launcher was installed.
//
// IT EXISTS BECAUSE THE SUMMARY USED TO LIE. installOrchLauncherFrom only printed; the summary then
// announced "<installDir>/orch (remote TUI client)" unconditionally. On Windows os.Symlink needs
// SeCreateSymbolicLinkPrivilege — which a non-elevated account without Developer Mode does NOT have — so on a
// stock Windows box the launcher was NEVER created and the summary claimed it was. Reported from a real
// machine, where the operator had no `orch` and a summary telling them they did:
//
//	warning: could not symlink orch launcher: A required privilege is not held by the client.
//	  orch launcher: C:\Users\operator\.local\bin/orch (remote TUI client)
//
// An installer that reports something it did not do is worse than one that fails, because the failure is the
// thing the operator needed. So the outcome is a VALUE the summary renders, not a print the summary ignores.
type orchLauncher struct {
	// Path is where the launcher was placed (or would have been). Empty only when the install dir could not be
	// resolved, in which case there is nothing to report.
	Path string
	// Installed is true when a launcher that will actually run is at Path.
	Installed bool
	// Method is "symlink", "copy", or "" — how it got there, so the summary can say "copied" on Windows rather
	// than implying a symlink exists.
	Method string
	// Problem is why it is not installed, in one line, or "" when it is.
	Problem string
}

// installOrchLauncher places the sibling `orch` binary (next to the running executable) into installDir.
func installOrchLauncher(installDir string) orchLauncher {
	exe, err := os.Executable()
	if err != nil {
		return orchLauncher{Problem: fmt.Sprintf("could not locate the running executable to find the sibling orch binary: %v", err)}
	}
	return installOrchLauncherFrom(exe, installDir)
}

// installOrchLauncherFrom places the sibling `orch` binary next to the given executable path into
// installDir.
func installOrchLauncherFrom(exe, installDir string) orchLauncher {
	sibling := filepath.Join(filepath.Dir(exe), "orch")
	if runtime.GOOS == "windows" {
		sibling += ".exe"
	}
	link := filepath.Join(installDir, "orch")
	if runtime.GOOS == "windows" {
		link += ".exe"
	}
	out := orchLauncher{Path: link}
	if _, err := os.Stat(sibling); err != nil {
		out.Problem = fmt.Sprintf("the sibling orch binary is not next to %s", exe)
		fmt.Printf("warning: sibling orch binary not found next to %s — skipping launcher install (build with `make build` to produce bin/orch)\n", exe)
		return out
	}
	// THE LAUNCHER *IS* THE BINARY WHEN BOTH LIVE IN THE SAME DIRECTORY.
	if filepath.Clean(link) == filepath.Clean(sibling) {
		out.Installed, out.Method = true, "in place"
		fmt.Printf("orch launcher already installed: %s\n", link)
		return out
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		out.Problem = fmt.Sprintf("could not create install dir %s: %v", installDir, err)
		fmt.Printf("warning: %s\n", out.Problem)
		return out
	}
	// An existing path at the launcher location.
	if st, err := os.Lstat(link); err == nil {
		force := os.Getenv("ORCHICON_FORCE_LAUNCHER") == "1"
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(link)
			if resolvesTo(target, sibling) {
				out.Installed, out.Method = true, "symlink"
				fmt.Printf("orch launcher already installed: %s\n", link)
				return out
			}
			if !force {
				out.Problem = fmt.Sprintf("%s is a symlink to %s, not to this install's %s", link, target, sibling)
				fmt.Printf("warning: %s — leaving it alone (set ORCHICON_FORCE_LAUNCHER=1 to replace)\n", out.Problem)
				return out
			}
			fmt.Printf("replacing existing %s (ORCHICON_FORCE_LAUNCHER=1)\n", link)
		case isOurCopy(link, sibling):
			// A COPY WE — OR THE SHELL INSTALLER — MADE. THE GATE HERE WAS WINDOWS-ONLY, WHICH MADE THE GUARD
			// ACCUSE OUR OWN FILE ON macOS.
			fmt.Printf("refreshing the copied orch launcher at %s\n", link)
		default:
			if !force {
				out.Problem = fmt.Sprintf("%s exists and was not placed there by this install", link)
				fmt.Printf("warning: %s — skipping (set ORCHICON_FORCE_LAUNCHER=1 to replace)\n", out.Problem)
				return out
			}
			fmt.Printf("replacing existing %s (ORCHICON_FORCE_LAUNCHER=1)\n", link)
		}
		_ = os.Remove(link)
	}

	// THE SYMLINK FIRST, BECAUSE IT IS THE ONE THAT REFRESHES ITSELF.
	if err := os.Symlink(sibling, link); err == nil {
		out.Installed, out.Method = true, "symlink"
		fmt.Printf("orch launcher installed: %s → %s\n", link, sibling)
		return out
	} else if runtime.GOOS != "windows" {
		// On POSIX a failed symlink is a real failure (permissions, read-only dir) and a copy would be a
		// different promise than the one the docs make. Report it.
		out.Problem = fmt.Sprintf("could not symlink orch launcher: %v", err)
		fmt.Printf("warning: %s\n", out.Problem)
		return out
	} else {
		// WINDOWS: the expected path for an ordinary account (no SeCreateSymbolicLinkPrivilege). Fall back to a
		// copy, which is what scripts/install.ps1 does, and say so — the mechanism changed, the promise did not.
		if cerr := copyLauncher(sibling, link); cerr != nil {
			out.Problem = fmt.Sprintf("could not place the orch launcher at %s: %v", link, cerr)
			fmt.Printf("warning: %s\n", out.Problem)
			return out
		}
		out.Installed, out.Method = true, "copy"
		fmt.Printf("orch launcher installed: %s (copied — Windows symlinks need an elevated or Developer Mode shell)\n", link)
		return out
	}
}

// resolvesTo reports whether a launcher's recorded symlink TARGET names the same file as sibling.
func resolvesTo(target, sibling string) bool {
	if filepath.Clean(target) == filepath.Clean(sibling) {
		return true
	}
	ti, terr := os.Stat(target)
	si, serr := os.Stat(sibling)
	if terr != nil || serr != nil {
		return false
	}
	return os.SameFile(ti, si)
}

// isOurCopy reports whether path is a copy of sibling that THIS installer placed there. IT CANNOT
// BE A PERFECT TEST, AND PRETENDING OTHERWISE WOULD BE WORSE THAN THE LIMIT.
func isOurCopy(path, sibling string) bool {
	return filepath.Base(path) == filepath.Base(sibling) && sameFile(path, sibling)
}

// copyLauncher copies src to dst, replacing dst, and marks the result executable.
func copyLauncher(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

// adapterCLIPresent reports whether an adapter CLI is installed on the host (on PATH or at
// ~/.<name>/bin/<name>).
func adapterCLIPresent(name string) bool {
	cands := adapterCandidates(name)
	return len(cands) > 0
}

// adapterCandidates is EVERY place the adapter was found, with provenance — the surface that makes
// discovery visible instead of magic.
func adapterCandidates(name string) []adapter.Candidate {
	home, _ := os.UserHomeDir()
	return adapter.Candidates(adapter.HostGOOS(), home, name, os.Getenv)
}

// reportAdapterCLI prints what was found, and is DELIBERATELY CHATTY about ambiguity.
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

// ensureInstallDaemon starts the runtime daemon if its socket is not healthy.
func reportContainerBinary() {
	if runtime.GOOS == "linux" {
		fmt.Println("container execution: ready (this host's own binary is the container's)")
		return
	}
	self, _ := os.Executable()
	resolved, err := workruntime.ResolveContainerExe(runtime.GOOS, self, os.Getenv(workruntime.ContainerExeEnv),
		config.Default().DataDir)
	if err != nil {
		fmt.Println("container execution: NOT READY — a Linux container needs a linux/amd64 orchicon binary")
		fmt.Printf("  %v\n", err)
		return
	}
	fmt.Printf("container execution: ready (%s)\n", resolved)
}

func ensureInstallDaemon(socketDir string) error {
	socketPath := filepath.Join(socketDir, "runtime.sock")
	// HEALTHY IS NOT THE SAME AS CURRENT.
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
	// Wait for the daemon to answer /v1/health.
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

// daemonBuildMatches reports whether the daemon on socketPath was built from the same source as
// THIS binary.
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

// runtimeVariantRef returns the registry ref for a derived runtime image variant (:gui or :dev) at the requested
// tag. It is THE ONE implementation, shared by the installer's image prompt (install_runtime_images.go) and the
// runtime daemon's allowlist (runtime.go).
//
// RELEASE TAGS DO NOT GET A "-latest", and this is the third place the same bug has had to be fixed. The reason
// it kept returning is that the rule lived in three implementations; the two that were fixed are
// internal/runtimeimage/seed.go (whose catalog documents the identical failure) and here. The one that was MISSED
// was the daemon's allowlist — which is the producer that actually decides the tag a seeded catalogue row carries,
// so it is the one that mattered.
//
// release.yml publishes, for a release:
//
//	<repo>:gui-<version>   and the FLOATING   <repo>:gui
//	<repo>:dev-<version>   and the FLOATING   <repo>:dev
//
// so the floating variant tags DROP the version rather than becoming "-latest". Building "<variant>-<tag>"
// instead produced ":gui-latest" and ":dev-latest", which nothing publishes, and a fetch failed with exactly
// what an operator reported:
//
//	Error: unable to copy from source docker://ghcr.io/beardedparrott/orchicon-runtime:dev-latest:
//	reading manifest dev-latest in ghcr.io/beardedparrott/orchicon-runtime: manifest unknown
//
// An explicitly-named, non-release tag keeps the "<variant>-<tag>" spelling, which is what a locally-built tag
// uses. TestRuntimeVariantRefMatchesReleaseWorkflow pins this against release.yml so the two cannot drift again.
func runtimeVariantRef(variant, imageTag string) string {
	return runtimeVariantRefIn(runtimeImageRepo, variant, imageTag)
}

// runtimeImageRepo is the DEFAULT published repository.
const runtimeImageRepo = "ghcr.io/beardedparrott/orchicon-runtime"

// runtimeVariantRefIn is THE rule, for a given repository: a release tag has no variant prefix beyond the
// variant's own name, and an explicitly-named tag keeps "<variant>-<tag>".
func runtimeVariantRefIn(repo, variant, imageTag string) string {
	if imageTag == "" || imageTag == "latest" {
		return repo + ":" + variant
	}
	return repo + ":" + variant + "-" + imageTag
}

// ensureImage makes the local copy of ref CURRENT, pulling it.
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
	// THE PULL'S OUTPUT IS STREAMED, NOT CAPTURED.
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

// containerImageCurrent reports whether the running container's image IS the target image, compared
// by IMAGE ID rather than by tag.
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

func printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir string, launcher orchLauncher, path pathHint, ports map[string]int, containersUsed, healthy bool, engine containerengine.Status) {
	// WHERE THE SERVICES RUN follows from the shape and the host: a host-resident plane has its
	// services as host processes when there is no engine to put them in a container.
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
	// State the SHAPE, because it decides where the plane lives and therefore how the operator restarts
	// it — a host plane is a process on this machine.
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
	// The manage-the-instance block names the commands that actually apply to this shape: a container
	// shape is managed with docker.
	fmt.Println("  Manage the instance:")
	if hostServices {
		fmt.Println("    Restart: orchicon restart   (stops and restarts the host plane and services)")
		// THE STATE DIR IS NAMED, NOT ASSUMED.
		fmt.Printf("    Stop:    ORCHICON_SERVE_STATE_DIR=%s/serve orchicon serve --stop\n", hostDataDirFor(instance))
		fmt.Printf("    Logs:    %s/serve/logs/orchicon.log and %s/services.log\n", hostDataDirFor(instance), hostDataDirFor(instance))
	} else {
		fmt.Printf("    Stop:   docker stop %s\n", name)
		fmt.Printf("    Start:  docker start %s\n", name)
		fmt.Printf("    Logs:   docker logs -f %s\n", name)
	}
	fmt.Println()
	// PER-WORKFLOW RUNTIME CONTAINERS BELONG TO THE CONTAINER ENGINE, so this block reports the shape
	// the install actually produced.
	if containersUsed {
		fmt.Printf("  Runtime daemon (per-workflow runtime containers): running\n")
		fmt.Printf("    socket: %s/runtime.sock   runtime image: %s\n", socketDir, runtimeImage)
	} else {
		fmt.Println("  Runtime daemon: running, but there is NO usable container engine on this host —")
		fmt.Println("    per-workflow runtime containers cannot be launched, so work runs in-process.")
	}
	// THE AVAILABILITY LINE IS THE INSTALL'S OWN DECISION (ready / provisioning / unavailable with the
	// reason), threaded in rather than re-probed.
	fmt.Printf("  Container engine: %s\n", describeEngineStatus(engine))
	if hostServices {
		fmt.Printf("  Data: host data dir (see above; the KEK lives in its secrets/)\n")
	} else {
		fmt.Printf("  Data: volume %s (preserved across restarts)\n", dataVolume)
	}
	fmt.Println()
	// THE LAUNCHER LINE REPORTS WHAT HAPPENED, NOT WHAT WAS ATTEMPTED.
	if launcher.Installed {
		fmt.Printf("  orch launcher: %s (remote TUI client, %s)\n", launcher.Path, launcher.Method)
	} else if launcher.Path != "" {
		fmt.Printf("  orch launcher: NOT installed — %s\n", launcher.Problem)
		fmt.Printf("    The plane itself is up and complete; only the `orch` TUI client is missing.\n")
		// THE REMEDY NAMES THE THING THAT ACTUALLY STANDS IN THE WAY, per platform.
		if runtime.GOOS == "windows" {
			fmt.Printf("    Retry just the launcher with `orchicon install --install-launcher` from a NON-elevated\n")
			fmt.Printf("    shell (do not use \"Run as administrator\" — Postgres will not run elevated), or copy\n")
			fmt.Printf("    orch%s from beside orchicon%s into %s yourself.\n", exeSuffix(), exeSuffix(), installDir)
		} else {
			fmt.Printf("    Retry just the launcher with `orchicon install --install-launcher`, or copy\n")
			fmt.Printf("    orch%s from beside orchicon%s into %s yourself.\n", exeSuffix(), exeSuffix(), installDir)
		}
	}
	printPathHint(path, installDir)
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

// instanceAlreadyProvisioned reports whether an instance has already been set up on this host: it
// has host state (its data dir carries a KEK) AND a recorded port set.
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
