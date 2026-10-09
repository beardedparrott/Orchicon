package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	instance := env("ORCHICON_INSTANCE", "dev")
	imageTag := env("ORCHICON_IMAGE_TAG", "latest")
	containerImage := env("ORCHICON_CONTAINER_IMAGE", "ghcr.io/beardedparrott/orchicon:"+imageTag)
	runtimeImage := env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:"+imageTag)

	name := "orchicon-cnt-" + instance
	dataVolume := "orchicon-cnt-" + instance + "-data"
	socketDir := env("ORCHICON_RUNTIME_SOCKET_DIR", filepath.Join(os.TempDir(), "orchicon-runtime"))

	// 1. Docker must be present and running.
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		return fmt.Errorf("docker is required (start Docker first): %v: %s", err, strings.TrimSpace(string(out)))
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
	residency, err := residencyForInstall()
	if err != nil {
		return err
	}
	tty, ttyOwned := promptTerminal()
	if ttyOwned {
		defer tty.Close()
	}
	ports, err := resolveInstallPorts(instance, residency, os.Stdout, tty)
	if err != nil {
		return err
	}

	// 2. Ensure the published images are present (skip the pull when the tag is
	// already local — idempotent re-runs and local dev images).
	//
	// REQUIRED images: the instance image and the workflow runtime base. Without
	// these the install cannot produce a working instance, so a failure is fatal.
	for _, img := range []string{containerImage, runtimeImage} {
		if err := pullImageIfAbsent(img); err != nil {
			return err
		}
	}
	// SUPPLEMENTARY images: the :gui and :dev variants are the stock runtime
	// images a work item can pick, not prerequisites for an install. A missing tag
	// therefore WARNS rather than failing.
	//
	// THIS BEING FATAL IS HOW THE INSTALL BROKE. The refs were built as
	// "<suffix>-<tag>", so the default "latest" produced "...:gui-latest" — a tag
	// release.yml NEVER PUBLISHES (it publishes "<suffix>-<version>" plus a
	// FLOATING ":gui"/":dev"). Every fresh install aborted right here, before
	// creating anything, with:
	//   pull ghcr.io/beardedparrott/orchicon-runtime:gui-latest: not found
	// The ref is built correctly now (runtimeVariantRef), and a supplementary
	// image can no longer fail an install even if the registry drops a tag again.
	for _, img := range []string{runtimeVariantRef("gui", imageTag), runtimeVariantRef("dev", imageTag)} {
		if err := pullImageIfAbsent(img); err != nil {
			fmt.Printf("warning: %v\n", err)
			fmt.Println("  (the stock runtime image variants are supplementary — continuing)")
		}
	}

	// 3. Ensure the host-side runtime daemon is running (the container
	// mounts its socket directory; per-workflow runtime containers spawn
	// from the runtime image above).
	if err := ensureInstallDaemon(socketDir); err != nil {
		return err
	}

	// 4. Create + start the single-container instance (or report the
	// existing one).
	if err := ensureInstallContainer(instance, residency, name, dataVolume, socketDir, containerImage, ports); err != nil {
		return err
	}

	// 4.5. HOST RESIDENCY (the product's default): the container runs the SERVICES
	// only, so the control plane is a process on THIS host. The order is
	// load-bearing and mirrors scripts/container.sh — migrate the data dir FIRST,
	// because the plane loads the secrets KEK from it on boot and a plane started
	// against a fresh empty dir would mint a NEW KEK and orphan every tenant
	// secret; then wait for the backend; then start the plane.
	if residency == residencyHost {
		dataDir, err := hostDataDir(instance)
		if err != nil {
			return err
		}
		if err := migrateHostDataDir(instance, dataVolume, dataDir, os.Stdout); err != nil {
			return err
		}
		waitServicesReady(name, os.Stdout)
		if err := startHostPlane(instance, ports, dataDir, filepath.Join(socketDir, "runtime.sock"), os.Stdout); err != nil {
			return err
		}
	}

	// 5. Wait for the control plane to serve health, ON THE PORT THIS INSTALL
	// PUBLISHED — read back from the resolved set rather than re-derived from
	// the instance name. Re-deriving is how a moved port gets probed at the old
	// one, and a neighbouring instance answering there would report a healthy
	// install that is not this one.
	controlPort := fmt.Sprintf("%d", ports["ORCHICON_CONTROL_PORT"])
	healthURL := "http://localhost:" + controlPort + "/healthz"
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(healthURL); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}

	// 6. Install/refresh the `orch` companion launcher on PATH (the thin
	// remote TUI client). Non-fatal: a missing sibling binary or a clobber
	// guard only warns.
	installDir := env("ORCHICON_INSTALL_DIR", defaultInstallDir())
	installOrchLauncher(installDir)

	printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir, ports)
	return nil
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
	// Refresh an owned symlink in place.
	if st, err := os.Lstat(link); err == nil && st.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(link); err == nil && target == sibling {
			fmt.Printf("orch launcher already installed: %s\n", link)
			return
		}
		_ = os.Remove(link)
	} else if err == nil {
		// A regular file (or other non-symlink) occupies the path.
		if os.Getenv("ORCHICON_FORCE_LAUNCHER") != "1" {
			fmt.Printf("warning: %s exists and is not an orch symlink — skipping (set ORCHICON_FORCE_LAUNCHER=1 to replace)\n", link)
			return
		}
		fmt.Printf("replacing existing %s (ORCHICON_FORCE_LAUNCHER=1)\n", link)
		_ = os.Remove(link)
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
	// Wait for the daemon to answer /v1/health.
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

// ensureInstallContainer creates (or reuses) the single-container
// instance with the same scoped mounts as scripts/container.sh: opencode
// config/data read-only, git identity read-only, and the runtime daemon
// socket directory. Project dirs are added later via the UI (the plane
// writes /var/lib/orchicon/project-mounts; re-running `orchicon install`
// or using scripts/container.sh sync-mounts applies them).
func ensureInstallContainer(instance, residency, name, dataVolume, socketDir, image string, ports map[string]int) error {
	running, _ := containerRunning(name)
	if running {
		// A running instance is only "already done" when its SHAPE matches. The
		// residency is recorded at create time and cannot be changed on a live
		// container, so an instance of the wrong shape has to be RECREATED — the
		// same thing scripts/container.sh does on a residency mismatch. An
		// unreadable shape is treated as matching: recreating a live instance on a
		// transient inspect failure is worse than leaving a wrong-shaped one for
		// the operator to see.
		existing, known := containerResidency(name)
		if !known || existing == residency {
			fmt.Printf("instance %q already running (%s)\n", name, image)
			return nil
		}
		fmt.Printf("instance %q was created for %s residency but this install wants %s — recreating\n", name, existing, residency)
	}
	// A stopped/partial container with the same name blocks recreation.
	if exists, _ := containerExists(name); exists {
		fmt.Printf("removing existing %s …\n", name)
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			return fmt.Errorf("remove existing %s: %v: %s", name, err, strings.TrimSpace(string(out)))
		}
	}

	home, _ := os.UserHomeDir()
	hostUID := os.Getuid()
	hostGID := os.Getgid()
	// The Grafana public URL is the PLANE origin (Grafana is proxied same-origin
	// under /grafana), so it tracks the control port. The publishes themselves are
	// built by publishArgs below, from the resolved set for THIS shape.
	controlPort := fmt.Sprintf("%d", ports["ORCHICON_CONTROL_PORT"])

	args := []string{"run", "-d", "--name", name,
		"--label", "orchicon-instance=" + instance,
		"--log-driver", "json-file", "--log-opt", "max-size=100m", "--log-opt", "max-file=7",
		"-v", dataVolume + ":/var/lib/orchicon",
		"-e", "ORCHICON_GRAFANA_PUBLIC_URL=http://localhost:" + controlPort + "/grafana",
		"-e", fmt.Sprintf("ORCHICON_HOST_UID=%d", hostUID),
		"-e", fmt.Sprintf("ORCHICON_HOST_GID=%d", hostGID),
		"-e", "ORCHICON_HOST_HOME=" + home,
		"-e", "ORCHICON_INSTANCE=" + instance,
	}
	// The residency is CREATE-time state: the supervisor reads this flag to decide
	// whether to boot the plane inside this container or leave it to the host.
	if residency == residencyHost {
		args = append(args, "-e", "ORCHICON_CONTAINER_SERVICES_ONLY=1")
	}
	// The image's HEALTHCHECK probes the plane, which a host-resident container
	// does not run — without this override it reports unhealthy forever. See
	// healthArgs.
	args = append(args, healthArgs(residency)...)
	// Publishes come from the RESOLVED set for THIS shape: an override moves the
	// port, and the shape decides which ports are exposed at all — a host-resident
	// instance publishes the services so its host plane can reach them over
	// loopback, and does NOT publish the plane port it owns itself.
	args = append(args, publishArgs(residency, ports, activeInstallPorts(residency))...)
	// Scoped host-home mounts (not the whole $HOME).
	if st, err := os.Stat(filepath.Join(home, ".config/opencode")); err == nil && st.IsDir() {
		args = append(args, "-v", filepath.Join(home, ".config/opencode")+":"+filepath.Join(home, ".config/opencode")+":ro")
	}
	if st, err := os.Stat(filepath.Join(home, ".local/share/opencode")); err == nil && st.IsDir() {
		args = append(args, "-v", filepath.Join(home, ".local/share/opencode")+":"+filepath.Join(home, ".local/share/opencode")+":ro")
	}
	if st, err := os.Stat(filepath.Join(home, ".opencode", "bin", "opencode")); err == nil && !st.IsDir() {
		args = append(args, "-v", filepath.Join(home, ".opencode")+":"+filepath.Join(home, ".opencode")+":ro")
	}
	for _, f := range []string{".gitconfig", ".git-credentials"} {
		if st, err := os.Stat(filepath.Join(home, f)); err == nil && !st.IsDir() {
			args = append(args, "-v", filepath.Join(home, f)+":"+filepath.Join(home, f)+":ro")
		}
	}
	// GitHub CLI auth + state (read-only) so in-process PR/merge workers
	// and the host opencode serve can run `gh` authenticated.
	if st, err := os.Stat(filepath.Join(home, ".config", "gh")); err == nil && st.IsDir() {
		args = append(args, "-v", filepath.Join(home, ".config", "gh")+":"+filepath.Join(home, ".config", "gh")+":ro")
	}
	if st, err := os.Stat(filepath.Join(home, ".local", "share", "gh")); err == nil && st.IsDir() {
		args = append(args, "-v", filepath.Join(home, ".local", "share", "gh")+":"+filepath.Join(home, ".local", "share", "gh")+":ro")
	}
	// Runtime daemon socket directory (directory mount survives daemon
	// restarts).
	args = append(args, "-v", socketDir+":/var/run/orchicon-runtime", image)

	fmt.Printf("starting %s …\n", name)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("%s started\n", name)
	return nil
}

// dirOnPath reports whether dir is on the current PATH (mirrors the
// install.sh PATH hint check).
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
	out, err := exec.Command("docker", "image", "inspect", img).CombinedOutput()
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

// pullImageIfAbsent pulls img unless its tag is already local, so an idempotent
// re-run and a locally-built image cost nothing.
func pullImageIfAbsent(img string) error {
	if imagePresent(img) {
		fmt.Printf("image %s present\n", img)
		return nil
	}
	fmt.Printf("pulling %s …\n", img)
	if out, err := exec.Command("docker", "pull", img).CombinedOutput(); err != nil {
		return fmt.Errorf("pull %s: %v: %s", img, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func containerRunning(name string) (bool, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func containerExists(name string) (bool, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", name).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func printInstallInfo(instance, residency, name, dataVolume, socketDir, healthURL, runtimeImage, installDir string, ports map[string]int) {
	// Read back the ports this install actually bound: re-deriving them from the
	// instance name would print a URL pointing at a port we never bound.
	controlPort := fmt.Sprintf("%d", ports["ORCHICON_CONTROL_PORT"])
	grafanaPort := fmt.Sprintf("%d", ports["ORCHICON_GRAFANA_PORT"])
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("│  Orchicon is installed and running.                        │")
	fmt.Println("└─────────────────────────────────────────────────────────────┘")
	// State the SHAPE, because it decides where the plane lives and therefore how
	// the operator restarts it — a host plane is a process on this machine, not
	// something the container brings back on `docker start`.
	if residency == residencyHost {
		fmt.Printf("  Plane residency: host — the services run in %s,\n", name)
		fmt.Println("                   the control plane runs on this host.")
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
	fmt.Println("  Manage the instance:")
	fmt.Printf("    Stop:   docker stop %s\n", name)
	fmt.Printf("    Start:  docker start %s\n", name)
	fmt.Printf("    Logs:   docker logs -f %s\n", name)
	fmt.Println()
	fmt.Printf("  Runtime daemon (per-workflow runtime containers): running\n")
	fmt.Printf("    socket: %s/runtime.sock   runtime image: %s\n", socketDir, runtimeImage)
	fmt.Printf("  Data: volume %s (preserved across restarts)\n", dataVolume)
	fmt.Println()
	fmt.Printf("  orch launcher: %s/orch (remote TUI client)\n", installDir)
	if !dirOnPath(installDir) {
		fmt.Printf("  %s is not on your PATH — add it to your shell profile:\n", installDir)
		fmt.Printf("    export PATH=\"$PATH:%s\"\n", installDir)
	}
	fmt.Println()
	fmt.Println("  Per-workflow runtime containers are used automatically when a")
	fmt.Println("  workflow runs. Open the UI, log in with the dev IdP, and create")
	fmt.Println("  a workflow — every execution runs in its own short-lived container.")
}
