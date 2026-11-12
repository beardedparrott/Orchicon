package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/logging"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/planecontrol"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
)

// runServe loads configuration from the environment, applies pending migrations, constructs the
// control plane server, wraps it with the embedded frontend SPA, and runs until SIGTERM or SIGINT.
const serveEnvDetached = "ORCHICON_SERVE_DETACHED"

// serveEnvLogFile tells the serve child where to write its rotating log
// file (set by serveDetach). When unset, logs go to stdout/stderr.
const serveEnvLogFile = "ORCHICON_SERVE_LOG_FILE"

// runServe dispatches `serve` subcommands:
//
//	orchicon serve              run the server in the foreground (blocks)
//	orchicon serve --detach     fork the server into the background, write
//	                            the PID file, wait for /healthz, and return
//	orchicon serve --stop       stop a detached serve via the PID file
//
// --detach exists so scripts and AI agents can start the control plane
// without a command that never returns (a foreground server keeps the
// caller's stdout/stderr pipe open and hangs the session).
func runServe(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--detach", "-d":
			if os.Getenv(serveEnvDetached) == "" {
				return serveDetach()
			}
			// fall through: this is the forked child — run the server.
		case "--stop":
			return serveStop()
		case "--status":
			return serveStatus()
		case "--start", "-start":
			// THERE IS NO `serve --start`.
			bin := filepath.Base(os.Args[0])
			fmt.Fprintf(os.Stderr, "✗ `serve --start` does not exist — start an instance with:\n"+
				"    %s start [instance]        # services, then the plane\n"+
				"    %s plane start [instance]  # the plane only\n", bin, bin)
			return 2
		}
	}
	return serveForeground()
}

// serveForeground runs the server until SIGTERM/SIGINT.
func serveForeground() int {
	ctx, cancel := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer cancel()

	var logOut io.Writer = os.Stdout
	var rotator *logging.RotatingWriter
	if logPath := os.Getenv(serveEnvLogFile); logPath != "" {
		cfg := config.Default()
		rw, err := logging.New(loggingFromEnv(cfg, logPath))
		if err != nil {
			// Detached: stdout/stderr are /dev/null, so a silent fallback would lose every log line.
			if ferr, e2 := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); e2 == nil {
				fmt.Fprintf(ferr, "failed to open rotating log file: %v\n", err)
				ferr.Close()
			}
			fmt.Fprintf(os.Stderr, "✗ failed to open rotating log file %s: %v\n", logPath, err)
			return 1
		}
		rotator = rw
		logOut = rw
		logging.RedirectStdStreams(rw.Current())
		rw.SetOnRotate(func() { logging.RedirectStdStreams(rw.Current()) })
		defer rw.Close()
	}

	log := slog.New(telemetry.MultiHandler(
		slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}),
		telemetry.NewOtelSlogHandler(),
	))
	killOrphans()
	log.Info("orchicon serve starting", "version", version.Current().String())

	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "error", err)
		return 1
	}

	// THE PERSISTENT PERMISSION POLICY, CHECKED BEFORE ANYTHING SERVES.
	if err := permpolicy.Boot(cfg.PermissionPolicyPath); err != nil {
		// TWO DIFFERENT FAILURES REACH HERE, and conflating them sends the operator to the wrong place.
		if errors.Is(err, fs.ErrPermission) {
			log.Error("the data directory is not writable, so the permission policy cannot be stored",
				"data_dir", cfg.DataDir,
				"path", cfg.PermissionPolicyPath,
				"error", err,
				"fix", "set ORCHICON_DATA_DIR to a writable directory, or start the plane the way the "+
					"installer does (`orchicon install` sets it per instance). Running `serve` by hand "+
					"on the host falls back to a root-only data directory.")
			return 1
		}
		log.Error("invalid permission policy", "path", cfg.PermissionPolicyPath, "error", err)
		return 1
	}
	log.Info("permission policy loaded", "path", cfg.PermissionPolicyPath)

	// Run embedded migrations before the server starts, using the same tracking table
	// (_orchicon_migrations) that devStartParent uses.
	if !migrateOnBoot(ctx, cfg, log) {
		return 1
	}

	srv, err := server.New(cfg, log, rotator)
	if err != nil {
		log.Error("failed to construct server", "error", err)
		return 1
	}

	handler := withFrontend(srv.Handler(), log)
	srv.SetHandler(handler)

	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", "error", err)
		return 1
	}
	log.Info("orchicon serve stopped")
	return 0
}

// serveDetach forks the server into the background (own process group, logs to the rotating file at
// serveLogFile), writes the PID file, and returns immediately — the caller must NOT block.
func serveDetach() int {
	if pid, running := procRunning(servePIDFile()); running {
		fmt.Fprintf(os.Stderr, "✗ serve is already running (PID %s)\n", pid)
		fmt.Fprintf(os.Stderr, "  Stop it with: %s serve --stop\n", filepath.Base(os.Args[0]))
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(servePIDFile()), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to create PID directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(serveLogFile()), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to create log directory: %v\n", err)
		return 1
	}

	cmd := exec.Command(os.Args[0], "serve")
	// THE INSTANCE'S OWN STATE DIR IS PINNED, NOT INHERITED, and both log destinations are passed
	// explicitly.
	cmd.Env = append(envWithout(os.Environ(), serveStateDirEnv, serveEnvLogFile, serveEnvDetached,
		"ORCHICON_INSTANCE"),
		"ORCHICON_INSTANCE="+instanceForServe(),
		serveStateDirEnv+"="+serveStateDir(),
		serveEnvLogFile+"="+serveLogFile(),
		serveEnvDetached+"=1")
	cmd.Stdin = nil // /dev/null — the child must not inherit the caller's stdin
	cmd.Stdout = nil
	cmd.Stderr = nil
	setProcAttrBackground(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to start serve: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(servePIDFile(), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  ! failed to write PID file: %v\n", err)
	}
	// Release the child from our care so nothing waits on it. Start it in
	// its own process group (setProcAttrBackground) and detach.
	_ = cmd.Process.Release()

	fmt.Printf("✓ serve detached (PID %d)\n", pid)
	fmt.Printf("  Logs: %s\n", serveLogFile())
	fmt.Printf("  Check: %s serve --status\n", filepath.Base(os.Args[0]))
	fmt.Printf("  Stop: %s serve --stop\n", filepath.Base(os.Args[0]))
	return 0
}

// serveStop sends SIGTERM to a detached serve and clears the PID file.
var serveStopGrace = 10 * time.Second

// SO: the PID file is removed ONLY when the process is confirmed gone, and a survivor is reported
// as a FAILURE (non-zero) so the caller — `scripts/container.sh plane_stop`, `make rebuild-*`.
func serveStop() int {
	pidFile := servePIDFile()
	pid, running := procRunning(pidFile)
	if !running {
		// A PID FILE NAMING NOTHING IS STALE STATE, not merely an absent plane: it is what makes `--status`
		// keep claiming a plane exists.
		if _, err := os.Stat(pidFile); err == nil {
			_ = os.Remove(pidFile)
		}
		// BUT AN ABSENT PID FILE IS NOT EVIDENCE THAT NOTHING IS SERVING, and assuming it was made `--stop`
		// LIE. A plane can outlive its PID file (the file was lost, or a rebuild replaced the binary and the
		// record went with the old serve dir). This function then printed "serve is not running" while the
		// port stayed bound — and every caller that cross-checks `--status` (which DOES discover an
		// unrecorded plane, via planecontrol.StatusOf) saw the contradiction and refused to proceed:
		//
		//	✗ orchicon prod: the plane is STILL running after stop — refusing to report success
		//	make: *** [Makefile:908: instance-down] Error 1
		//
		// which failed every `make instance-rebuild` over a serving instance.
		//
		// So the SAME discovery `--status` uses is applied here. The identification is strict — the listener
		// on this instance's recorded control port must be an orchicon plane binary carrying THIS instance's
		// tag — so a sibling instance is never signalled.
		// THE FALLBACK MAY ONLY RUN WHEN WE ARE READING THE INSTANCE'S CANONICAL STATE DIR.
		//
		// Without this guard the discovery is DANGEROUS, and it caused a real outage. serveStateDir()
		// prefers ORCHICON_INSTANCE over ORCHICON_SERVE_STATE_DIR, so a caller that redirected the state
		// dir (a test, or any operator pinning a temp dir) is NOT looking at the instance's record at all —
		// the "absence" of the PID file is manufactured, not evidence that a plane outlived its record.
		//
		// Measured consequence, with ORCHICON_INSTANCE=prod exported: a test that redirected the state dir
		// to a temp dir still resolved servePIDFile() to the REAL prod record, wrote a bogus PID into it,
		// cleared it, and then this fallback discovered the live prod plane on :8091 and STOPPED it. The
		// symptom was a plane that "kept crashing" and a session that disconnected mid-turn.
		//
		// When the paths disagree the honest answer is what it always was: nothing of OURS to stop here.
		if !serveStopMayDiscover() {
			fmt.Println("serve is not running")
			return 1
		}
		if discovered, ok := planecontrol.UnrecordedPlanePID(instanceForServe()); ok {
			fmt.Printf("serve has no PID file but is serving this instance's port (PID %d) — stopping it\n", discovered)
			pid = strconv.Itoa(discovered)
			running = true
		} else {
			fmt.Println("serve is not running")
			return 1
		}
	}
	pidNum, err := strconv.Atoi(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ invalid PID file (%s): %v\n", pidFile, err)
		return 1
	}
	staleState := func() {
		// The PID file names a process that is gone or that we cannot see: it is stale, and leaving it
		// would make `--status` claim a plane exists.
		_ = os.Remove(pidFile)
	}
	proc, err := os.FindProcess(pidNum)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ no such process %d (%v) — removing the stale PID file\n", pidNum, err)
		staleState()
		return 1
	}
	// THE SIGNAL NAMES ARE PLATFORM-DEPENDENT and live in signals_unix.go / signals_windows.go.
	graceful := sendGracefulStop(proc)
	if errors.Is(graceful, os.ErrProcessDone) {
		fmt.Printf("✓ serve was already gone (PID %d)\n", pidNum)
		staleState()
		return 0
	}
	if graceful != nil {
		fmt.Fprintf(os.Stderr, "%s\n", gracefulStopUnavailable(pidNum, graceful))
		if gracefulStopFailureIsFatal {
			return 1
		}
	} else if waitForExit(pidNum, serveStopGrace) {
		staleState()
		fmt.Printf("✓ serve stopped (PID %d)\n", pidNum)
		return 0
	}
	// IT DID NOT GO QUIETLY. Escalate, exactly as the operator was doing by hand — and say so, because a
	// force-stopped server may have skipped its shutdown path.
	if graceful == nil {
		fmt.Fprintf(os.Stderr, "%s\n", forceStopNotice(pidNum, serveStopGrace.String()))
	}
	if err := sendForceStop(proc); err != nil && !errors.Is(err, os.ErrProcessDone) {
		fmt.Fprintf(os.Stderr, "✗ force-stopping PID %d failed: %v — the PID file is KEPT so --status still "+
			"reports the process\n", pidNum, err)
		return 1
	}
	if waitForExit(pidNum, serveStopGrace) {
		staleState()
		fmt.Println(forceStopResult(pidNum))
		return 0
	}
	// Neither signal took. KEEP the PID file: a state file that names a live process is the only remaining
	// handle on it, and reporting success here is what made the stale plane invisible.
	fmt.Fprintf(os.Stderr, "✗ PID %d is STILL RUNNING after the force stop — it may be unkillable (a stuck "+
		"syscall or a zombie). The PID file is kept at %s; investigate before rebuilding, or the next start "+
		"will lose the port race and keep serving this process.\n", pidNum, pidFile)
	return 1
}

// waitForExit polls until the process is gone, or the grace expires. Signal 0 is the probe: it succeeds
// while the PID exists, so "gone" means the kernel no longer has it — not merely that it stopped listening.
func waitForExit(pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if _, alive := procRunningPID(pid); !alive {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// procRunningPID reports whether a PID is a running process, independent of any PID FILE — the
// check waitForExit needs, since the file it started from may already be gone.
func procRunningPID(pid int) (string, bool) {
	return strconv.Itoa(pid), pidAlive(pid)
}

// THE EXIT CODE IS A CONTRACT AND MUST NOT CHANGE.
func serveStatus() int {
	st, err := planecontrol.StatusOf(planecontrol.Defaults(instanceForServe(), os.Stdout))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(st.String())
	if st.PID > 0 {
		return 0
	}
	return 1
}

// loggingFromEnv layers the log rotation config for a given log file path.
func loggingFromEnv(cfg config.Config, logPath string) logging.Config {
	c := logging.DefaultConfig()
	if d := os.Getenv("ORCHICON_LOG_DIR"); d != "" {
		c.Dir = d
	}
	if v := os.Getenv("ORCHICON_LOG_MAX_SIZE_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MaxSizeBytes = n << 20
		}
	}
	if v := os.Getenv("ORCHICON_LOG_ROLL_INTERVAL_HOURS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.RollInterval = time.Duration(n) * time.Hour
		}
	}
	if v := os.Getenv("ORCHICON_LOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.RetentionDays = int(n)
		}
	}
	if v := os.Getenv("ORCHICON_LOG_MAX_FILES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			c.MaxFiles = int(n)
		}
	}
	dir, base := filepath.Dir(logPath), filepath.Base(logPath)
	if dir != "" {
		c.Dir = dir
	}
	if base != "" {
		c.BaseName = base
	}
	return c
}

// instanceForServe names the instance this plane belongs to, for the child's environment. IT IS
// TRUE ONLY WHEN THE PID FILE WE JUST READ IS THE INSTANCE'S CANONICAL ONE.
func serveStopMayDiscover() bool {
	inst := instanceForServe()
	return servePIDFile() == planecontrol.ServePIDFile(inst)
}

func instanceForServe() string {
	if inst := strings.TrimSpace(os.Getenv("ORCHICON_INSTANCE")); inst != "" {
		return inst
	}
	return "dev"
}

// migrateOnBoot applies the embedded migrations before the server starts, and reports whether it is
// safe to continue.
func migrateOnBoot(ctx context.Context, cfg config.Config, log *slog.Logger) bool {
	if !cfg.MigrateOnBoot {
		return true
	}
	pool, err := db.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		log.Error("failed to connect for migrations", "error", err)
		return false
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		log.Error("migrations failed", "error", err)
		return false
	}
	log.Info("migrations applied")
	return true
}
