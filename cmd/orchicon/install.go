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

	// 1.5. The runtime adapter CLI (opencode) must be installed on the
	// HOST — Orchicon never ships adapter CLIs in its images (licensing;
	// Claude Code, for example, prohibits bundling). It is bind-mounted
	// into the containers at runtime, so fail loudly here rather than
	// letting every worker execution fail later with "binary not found".
	if err := requireAdapterCLI("opencode"); err != nil {
		return err
	}

	// 2. Ensure the published images are present (skip the pull when the
	// tag is already local — idempotent re-runs and local dev images).
	// The :gui and :dev runtime variants ship with the product (work-item
	// dropdown stock images), so pull them alongside the base.
	for _, img := range []string{containerImage, runtimeImage,
		"ghcr.io/beardedparrott/orchicon-runtime:gui-" + imageTag,
		"ghcr.io/beardedparrott/orchicon-runtime:dev-" + imageTag,
	} {
		if imagePresent(img) {
			fmt.Printf("image %s present\n", img)
			continue
		}
		fmt.Printf("pulling %s …\n", img)
		if out, err := exec.Command("docker", "pull", img).CombinedOutput(); err != nil {
			return fmt.Errorf("pull %s: %v: %s", img, err, strings.TrimSpace(string(out)))
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
	if err := ensureInstallContainer(instance, name, dataVolume, socketDir, containerImage); err != nil {
		return err
	}

	// 5. Wait for the control plane to serve health.
	controlPort := "8080"
	if instance == "prod" {
		controlPort = "8091"
	}
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

	printInstallInfo(instance, name, dataVolume, socketDir, healthURL, runtimeImage)
	return nil
}

// requireAdapterCLI verifies an adapter CLI is installed on the host
// (on PATH or at ~/.<name>/bin/<name>). Orchicon never ships adapter CLIs
// in its images — the operator installs them and they are bind-mounted
// into the containers at runtime.
func requireAdapterCLI(name string) error {
	if _, err := exec.LookPath(name); err == nil {
		return nil
	}
	if home, herr := os.UserHomeDir(); herr == nil {
		cand := filepath.Join(home, "."+name, "bin", name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return nil
		}
	}
	return fmt.Errorf("%s is required but not installed on this host — Orchicon does not ship adapter CLIs in its images (install it first, e.g. for opencode: curl -fsSL https://opencode.ai/install | bash)", name)
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
func ensureInstallContainer(instance, name, dataVolume, socketDir, image string) error {
	running, _ := containerRunning(name)
	if running {
		fmt.Printf("instance %q already running (%s)\n", name, image)
		return nil
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
	grafanaPort := "3002"
	controlPort := "8080"
	if instance == "prod" {
		grafanaPort = "3003"
		controlPort = "8091"
	}

	args := []string{"run", "-d", "--name", name,
		"--label", "orchicon-instance=" + instance,
		"--log-driver", "json-file", "--log-opt", "max-size=100m", "--log-opt", "max-file=7",
		"-p", controlPort + ":8080", "-p", grafanaPort + ":3000",
		"-v", dataVolume + ":/var/lib/orchicon",
		"-e", "ORCHICON_GRAFANA_PUBLIC_URL=http://localhost:" + controlPort + "/grafana",
		"-e", fmt.Sprintf("ORCHICON_HOST_UID=%d", hostUID),
		"-e", fmt.Sprintf("ORCHICON_HOST_GID=%d", hostGID),
		"-e", "ORCHICON_HOST_HOME=" + home,
		"-e", "ORCHICON_INSTANCE=" + instance,
	}
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

// imagePresent reports whether a Docker image tag exists locally.
func imagePresent(img string) bool {
	out, err := exec.Command("docker", "image", "inspect", img).CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) != ""
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

func printInstallInfo(instance, name, dataVolume, socketDir, healthURL, runtimeImage string) {
	controlPort := "8080"
	grafanaPort := "3002"
	if instance == "prod" {
		controlPort = "8091"
		grafanaPort = "3003"
	}
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("│  Orchicon is installed and running.                        │")
	fmt.Println("└─────────────────────────────────────────────────────────────┘")
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
	fmt.Println("  Per-workflow runtime containers are used automatically when a")
	fmt.Println("  workflow runs. Open the UI, log in with the dev IdP, and create")
	fmt.Println("  a workflow — every execution runs in its own short-lived container.")
}
