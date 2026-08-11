package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/runtime"
)

// runRuntimeDaemon runs the host-side runtime orchestrator (`orchicon
// runtime-daemon`). It owns the Docker socket and serves the narrow
// workflow-runtime API over a unix socket. Run it on the host, not in a
// container.
func runRuntimeDaemon(args []string, log *slog.Logger) error {
	hostUID := envInt("ORCHICON_HOST_UID", 1000)
	hostGID := envInt("ORCHICON_HOST_GID", 1000)
	hostHome := os.Getenv("ORCHICON_HOST_HOME")
	if hostHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			hostHome = h
		}
	}
	allowedRoots := strings.Split(env("ORCHICON_RUNTIME_ALLOWED_ROOTS", filepath.Join(hostHome, "projects")), ",")
	for i := range allowedRoots {
		allowedRoots[i] = strings.TrimSpace(allowedRoots[i])
	}

	socketPath := env("ORCHICON_RUNTIME_SOCKET", filepath.Join(os.TempDir(), "orchicon-runtime", "runtime.sock"))
	if len(args) > 0 {
		socketPath = args[0]
	}

	// Resolve the daemon's own executable once, at process start. It is
	// bind-mounted into every runtime container (see Daemon.ExePath) so the
	// container runs the SAME binary as the daemon, even after a rebuild —
	// no image rebuild required.
	exePath := ""
	if p, err := os.Executable(); err == nil {
		exePath = p
		if abs, aerr := filepath.Abs(p); aerr == nil {
			exePath = abs
		}
	}

	// Self-copy the daemon binary to a STABLE path next to the socket. The
	// running daemon's original file can be deleted by dev hygiene (`make
	// clean` removes bin/orchicon) while the daemon keeps running — a stale
	// ExePath then fails every container create with "executable missing"
	// and every workflow-run execution with "no opencode serve available".
	// Copying once at startup means the mounted binary is never the file a
	// cleanup target; a rebuilt + restarted daemon refreshes the copy.
	if exePath != "" {
		stableExe := filepath.Join(filepath.Dir(socketPath), "orchicon")
		if err := os.MkdirAll(filepath.Dir(stableExe), 0o755); err == nil {
			if cerr := copySelf(exePath, stableExe); cerr != nil {
				log.Warn("daemon self-copy failed — runtime containers may break after make clean", "error", cerr)
			} else {
				exePath = stableExe
			}
		} else {
			log.Warn("daemon self-copy dir", "error", err)
		}
	}

	d := &runtime.Daemon{
		SocketPath:   socketPath,
		DockerBin:    "docker",
		Image:        env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:latest"),
		Images:       runtimeImagesAllowlist(env("ORCHICON_RUNTIME_IMAGES", "")),
		UserID:       hostUID,
		GroupID:      hostGID,
		HostHome:     hostHome,
		AllowedRoots:  allowedRoots,
		ExePath:      exePath,
		CPUs:          env("ORCHICON_RUNTIME_CPUS", "4"),
		Memory:        env("ORCHICON_RUNTIME_MEMORY", "4g"),
		TmpfsSize:     env("ORCHICON_RUNTIME_TMPFS", "2g"),
		MaxAge:        envDur("ORCHICON_RUNTIME_MAX_AGE", 24*time.Hour),
		SweepInterval: envDur("ORCHICON_RUNTIME_SWEEP_INTERVAL", 5*time.Minute),
		Log:           log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.ListenAndServe(ctx); err != nil {
		return err
	}
	return nil
}

// copySelf copies src to dst, refreshing the copy only when src is newer
// (a daemon rebuild) so a stable copy isn't rewritten on every start. The
// copy keeps the daemon's binary mountable even after the original file is
// deleted by `make clean`. Best-effort on a running Linux process: the
// source is readable (its inode is alive), the copy is made before any
// cleanup can run.
func copySelf(src, dst string) error {
	if st, err := os.Stat(dst); err == nil {
		if si, serr := os.Stat(src); serr == nil && !si.ModTime().After(st.ModTime()) {
			return nil // copy is current
		}
		_ = os.Remove(dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// runRuntimeSupervisor runs the in-container PID-1 dispatch loop.
func runRuntimeSupervisor(args []string, log *slog.Logger) error {
	socketPath := ""
	if len(args) > 0 {
		socketPath = args[0]
	}
	return runtime.RunSupervisor(socketPath, log)
}

// runRuntimeClient forwards one request (from stdin) to the supervisor
// socket and relays events to stdout, exiting with the child's code.
func runRuntimeClient(args []string, log *slog.Logger) error {
	socketPath := ""
	if len(args) > 0 {
		socketPath = args[0]
	}
	code, err := runtime.RunClient(socketPath, os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
	return nil
}

// envDur parses a duration env var with a fallback (0 on invalid).
func envDur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// splitCSV splits a comma-separated env var into a trimmed slice,
// dropping empties.
func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runtimeImagesAllowlist resolves the daemon's stock image allowlist.
// The published :gui and :dev variants ship with the product and are
// included by default (based on the daemon's default image tag); an
// explicit ORCHICON_RUNTIME_IMAGES overrides the defaults entirely.
func runtimeImagesAllowlist(csv string) []string {
	if csv != "" {
		return splitCSV(csv)
	}
	// Default base tag is "<repo>:<tag>"; variants tag as
	// "<repo>:<variant>-<tag>" (e.g. "orchicon-runtime:gui-latest").
	img := env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:latest")
	colon := strings.LastIndex(img, ":")
	if colon <= 0 {
		return nil
	}
	repo, tag := img[:colon], img[colon+1:]
	return []string{repo + ":gui-" + tag, repo + ":dev-" + tag}
}
