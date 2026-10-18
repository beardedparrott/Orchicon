package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// installPhase prints a phase banner for `orchicon install`.
//
// It exists because the one-command install is minutes long and largely unattended:
// with no spine through its output, the operator's only evidence that it is working
// is that nothing has failed yet, and the only way to describe a failure is "during
// install". Naming each phase as it starts makes progress visible, and puts a failure
// under the step that caused it.
func installPhase(msg string) {
	fmt.Printf("== %s\n", msg)
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
	// THE ARGUMENTS ARE PARSED, AND THAT IS A FIX RATHER THAN A TIDY-UP. `install` used to
	// accept `args` and never read a byte of them, so `orchicon install --instance prod`
	// installed (or REPAIRED) `dev` while the operator believed they had chosen prod. The
	// installer's own `--instance` flag worked, which made the binary's silence worse: the
	// flag existed one layer up and did nothing here.
	instance, err := parseInstanceTarget(args, env("ORCHICON_INSTANCE", "dev"))
	if err != nil {
		return err
	}
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
		fmt.Println("orchicon: opencode found on this host — optional, and it will be used as a runtime when a model ref asks for it")
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
	if err := ensureHostPostgresPrivileges(residency); err != nil {
		return err
	}
	tty, ttyOwned := promptTerminal()
	if ttyOwned {
		defer tty.Close()
	}
	// THE PROMPT GOES TO THE TERMINAL, NOT TO STDOUT, whenever there is a terminal.
	// In every normal run those are the same place. They are NOT the same place when
	// something wraps this installer and captures its stdout — which is exactly what
	// the Windows one-liner does — and written to a captured stdout the question is
	// invisible while resolveInstallPorts blocks reading the terminal: an install that
	// hangs, for ever, with nothing on screen. That is the report this answers.
	// (The resolved ports are also listed in the summary, which is the record a
	// captured log keeps.)
	promptOut := io.Writer(os.Stdout)
	if tty != nil {
		promptOut = tty
	}
	ports, err := resolveInstallPorts(instance, residency, promptOut, tty)
	if err != nil {
		return err
	}
	// WRITE THE PORTS DOWN, because the operator can CHANGE them and a wrapper that re-prints them has
	// nothing to read. This is the bug it fixes, seen on Windows: the operator answered 3001 for Grafana,
	// this installer printed 3001, and then scripts/install.ps1 — which re-prints the URLs after the setup
	// step — printed a hardcoded 3002, because the resolved values existed only as output on a stream it
	// had already consumed. It is the same class of defect the port table itself was introduced to fix
	// ("a URL pointing at a port this install never bound"): every port the operator is PROMPTED for is a
	// port a wrapper cannot guess.
	//
	// The instance's own state dir is the right home for it — it is the record of what THIS instance
	// bound. Failure is a warning rather than fatal: an unwritable home directory must not fail an install
	// that has already resolved everything, and the wrapper's fallback is the documented default.
	writeResolvedPorts(instance, ports)

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
	installPhase("starting the runtime daemon")
	if err := ensureInstallDaemon(socketDir); err != nil {
		return err
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
	installPhase("starting the host services")
	if err := startHostServices(instance, ports, hostDataDirFor(instance), os.Stdout); err != nil {
		return err
	}

	// 4c. The services are up, so wait for them and then start the plane on THIS host. Waiting first keeps the
	// plane's migrations from racing a half-started postgres.
	{
		waitServicesReady(instance, ports, os.Stdout)
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

	// 6. Install/refresh the `orch` companion launcher on PATH (the thin
	// remote TUI client). Non-fatal: a missing sibling binary or a clobber
	// guard only warns.
	installDir := env("ORCHICON_INSTALL_DIR", defaultInstallDir())
	installOrchLauncher(installDir)

	printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir, ports, containersUsed, healthy, engineStatus)
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
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(home, "."+name, "bin", name))
	return err == nil && !st.IsDir()
}

// ensureInstallDaemon starts the runtime daemon if its socket is not
// healthy. The daemon owns the Docker socket and spawns per-workflow
// runtime containers; the supervisor container mounts its socket dir.
func ensureInstallDaemon(socketDir string) error {
	socketPath := filepath.Join(socketDir, "runtime.sock")
	if socketHealthy(socketPath) {
		fmt.Println("runtime daemon already running")
		return nil
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
	grafanaPort := fmt.Sprintf("%d", ports["ORCHICON_GRAFANA_PORT"])
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
	fmt.Printf("  Grafana:        http://localhost:%s\n", grafanaPort)
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
