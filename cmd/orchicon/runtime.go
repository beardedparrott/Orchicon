package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/beardedparrott/orchicon/internal/containerengine"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// runRuntimeDaemon runs the host-side runtime orchestrator (`orchicon
// runtime-daemon`). It owns the container engine (podman by default) and serves
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

	// Resolve the container engine BEFORE the daemon exists: a missing engine is
	// an operator-facing INSTALL problem, so it must fail here with the exact
	// install command (runtime.ResolveEngine's error) rather than surfacing later
	// as a bare `exec: "podman": executable file not found in $PATH` on the first
	// container create — or, worse, as a container that never becomes ready.
	//
	// This supersedes the earlier "resolve, don't assume `docker`" fix from the
	// Podman-provisioning branch: the engine is still resolved rather than
	// assumed, but the resolution now lives in internal/runtime (the seam the
	// daemon and the scheduler share), so a missing engine is reported as the
	// operator-facing install problem it is instead of a silent fallback.
	eng, err := runtime.ResolveEngineFromEnv()
	if err != nil {
		return err
	}
	log.Info("runtime engine resolved", "engine", eng.Name, "bin", eng.Bin)

	// THE MACHINE IS THE DAEMON'S BUSINESS TOO, not only the installer's.
	//
	// On Windows and macOS a container engine of this kind runs containers inside a VM, and that VM must be
	// created and started before a single container can be. The installer does this (EnsureMachine), which is
	// why an install ends with a working engine — but a daemon started LATER has no such guarantee: after a
	// reboot, after `podman machine stop`, or when the operator started the daemon by hand. The failure then is
	// not "no engine"; it is every container create failing for a reason the daemon never mentions.
	//
	// Best-effort and NON-FATAL, deliberately: a machine that will not start is reported and the daemon still
	// serves, exactly as an unavailable engine does. `podman machine init --now` and `machine start` need no
	// elevation, so this repairs the common cases itself rather than asking.
	if eng.Name == "podman" {
		// The two Engine types are different packages' views of the same fact; pass the resolved binary across.
		ce := containerengine.Engine{Name: containerengine.EnginePodman, Bin: eng.Bin}
		if ms, merr := containerengine.EnsureMachine(context.Background(), containerengine.DefaultProbe(), ce, os.Stdout); merr != nil {
			log.Warn("runtime daemon: the podman machine is not usable — containers will fail until it is",
				"error", merr, "state", ms)
		} else {
			log.Info("runtime daemon: podman machine ready", "state", ms)
		}
	}

	baseImage := env("ORCHICON_RUNTIME_IMAGE", "ghcr.io/beardedparrott/orchicon-runtime:latest")
	d := &runtime.Daemon{
		SocketPath:    socketPath,
		Engine:        eng,
		Image:         baseImage,
		Images:        runtimeImagesAllowlist(env("ORCHICON_RUNTIME_IMAGES", "")),
		UserID:        hostUID,
		GroupID:       hostGID,
		HostHome:      hostHome,
		AllowedRoots:  allowedRoots,
		ExePath:       exePath,
		CPUs:          env("ORCHICON_RUNTIME_CPUS", "4"),
		Memory:        env("ORCHICON_RUNTIME_MEMORY", "4g"),
		TmpfsSize:     env("ORCHICON_RUNTIME_TMPFS", "2g"),
		HomeTmpfsSize: env("ORCHICON_RUNTIME_HOME_TMPFS", "2g"),
		// PATH MAPPING (internal/runtime/pathmap.go). On Windows the engine runs in a Linux VM whose shared
		// filesystem is /mnt/<drive>/..., so a project at C:\projects\app must reach the engine as
		// /mnt/c/projects/app or the container cannot see the tree it is meant to work on. Identity on Linux and
		// macOS, so this is a no-op everywhere it already worked.
		Paths:         runtime.NewPathMapperFromEnv(runtime.GOOS()),
		MaxAge:        envDur("ORCHICON_RUNTIME_MAX_AGE", 24*time.Hour),
		SweepInterval: envDur("ORCHICON_RUNTIME_SWEEP_INTERVAL", 5*time.Minute),
		Log:           log,
	}

	// PUBLISH THE BASE TAG THE RESOLVE CHAIN SHOULD FALL BACK TO.
	//
	// The scheduler's resolve chain (explicit item image -> project default -> base) ended at a
	// placeholder, "orchicon-runtime:base", that matched no installed tag on any host: a release
	// tags the base ghcr.io/beardedparrott/orchicon-runtime:latest and a dev host builds it as
	// orchicon-runtime:local. An imageless work item therefore fell back to an image this daemon
	// refuses at create time ("image not allowed: orchicon-runtime:base").
	//
	// THIS PROCESS IS THE ONLY ONE THAT KNOWS d.Image, so it is the only one that can repair the
	// fallback. The direction matters when the daemon and the plane are separate processes: the
	// scheduler applies the barrier from the environment (scheduler.UseDaemonBaseImage), so both
	// halves agree in a split deployment as well as in-process. Setting it here too keeps a
	// co-resident plane correct even if its env differs from the daemon's.
	db.SetBaseRuntimeImage(baseImage)
	log.Info("runtime base image published to the resolve chain", "base", db.RuntimeBaseImage())

	// Platform-dependent stop signals: SIGINT+SIGTERM on Unix, os.Interrupt on Windows (signals_*.go).
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
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
	// CURRENT MEANS IDENTICAL BY CONTENT, NOT BY TIMESTAMP.
	//
	// This compared ModTime and returned early when src was not NEWER than dst. That is wrong in a way
	// that hides for days: bin/orchicon is rebuilt by every `make build`, but its mtime can be OLDER
	// than the copy — a `git checkout` or `cp -p` preserves timestamps, a clock can move backwards
	// between a build and a start, and a restored file carries its original time. When that happens
	// the copy is SKIPPED, the daemon keeps bind-mounting the previous binary into every runtime
	// container, and RESTARTING DOES NOT HELP: the restart calls this again and skips again.
	//
	// Measured on a real host: the stable copy was 13 bytes larger than bin/orchicon and older-copy
	// mtime won, so `make runtime-daemon` reported success while the daemon served a binary that no
	// longer existed on disk. The daemon was eight days stale and every restart had "succeeded".
	//
	// Comparing content costs one read of a ~100 MB file at daemon start — once per daemon, not per
	// container — and it is the only check that answers the question being asked.
	if sameFile(src, dst) {
		return nil
	}
	_ = os.Remove(dst)
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

// sameFile reports whether two files have identical content. A size mismatch short-circuits; equal
// sizes are compared byte for byte.
//
// It is the replacement for the mtime comparison above, and it is deliberately content-based: the
// question is "is the daemon running the binary that is on disk NOW", and only the bytes answer it.
func sameFile(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr != nil || berr != nil {
		return false
	}
	if ai.Size() != bi.Size() {
		return false
	}
	af, aerr := os.Open(a)
	if aerr != nil {
		return false
	}
	defer af.Close()
	bf, berr := os.Open(b)
	if berr != nil {
		return false
	}
	defer bf.Close()
	const chunk = 1 << 20
	abuf := make([]byte, chunk)
	bbuf := make([]byte, chunk)
	for {
		an, aerr := io.ReadFull(af, abuf)
		bn, berr := io.ReadFull(bf, bbuf)
		if an != bn || !bytes.Equal(abuf[:an], bbuf[:bn]) {
			return false
		}
		if aerr != nil || berr != nil {
			// EOF on both is the success case; anything else is a short/errored read.
			return (aerr == io.EOF || aerr == io.ErrUnexpectedEOF) &&
				(berr == io.EOF || berr == io.ErrUnexpectedEOF)
		}
	}
}

// runRuntimeSupervisor runs the in-container PID-1 dispatch loop.
func runRuntimeSupervisor(args []string, log *slog.Logger) error {
	socketPath := ""
	if len(args) > 0 {
		socketPath = args[0]
	}
	ensureHomeDirs(log)
	return runtime.RunSupervisor(socketPath, log)
}

// ensureHomeDirs creates the standard $HOME subdirectories the runtime and the tooling it runs write
// into: `.local/share`, `.local/state`, `.config`, `.cache`, `.npm` and `projects`.
//
// IT LIVES HERE, IN THE CONTAINER AND AT START, RATHER THAN IN THE IMAGE AT BUILD TIME — which is
// where it used to be, and why it only ever worked for one person. The image baked in
// `mkdir -p /home/<the author's username>/...` because the container's HOME is the HOST user's home
// (the daemon passes `HOME=$HostHome` so the host-path mounts line up). Baked at build time, that
// path is fixed while HOME is not, so on every other machine the directories were created where
// nothing looks for them and the operator's real home was left without any — the runtime uid could
// then fail to write `$HOME/.config` and the rest, the exact opposite of the intent.
//
// Reading $HOME here is correct by construction: it is whatever the daemon set, so the directories
// land where the mounts and the tooling expect them, for every user.
//
// BEST-EFFORT ON PURPOSE. A mount may cover one of these read-only, or the home may not be writable:
// the runtime must still start, so a failure is reported and skipped rather than fatal.
func ensureHomeDirs(log *slog.Logger) {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return
	}
	for _, sub := range []string{".local/share", ".local/state", ".config", ".cache", ".npm", "projects"} {
		p := filepath.Join(home, sub)
		if err := os.MkdirAll(p, 0o755); err != nil {
			log.Warn("runtime supervisor: could not create a home directory — the runtime will start "+
				"without it, and tooling writing there may fail", "path", p, "error", err)
		}
	}
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
