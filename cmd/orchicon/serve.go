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

// runServe loads configuration from the environment, applies pending
// migrations, constructs the control plane server, wraps it with the
// embedded frontend SPA, and runs until SIGTERM or SIGINT.
// It is the production-like server mode — no Compose management, no
// process forking. Used headless (`orchicon serve --detach`) and as the
// control-plane child of the single-container supervisor (`orchicon
// container`, which spawns `orchicon serve`).
//
// Migrations are run here (the embedded runner writes the
// _orchicon_migrations tracking table) so every boot path — headless,
// detached, or container — stays consistent.
// serveEnvDetached marks a serve subprocess that was forked by `serve
// --detach`; it tells the child to run the server directly instead of
// forking again.
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
			// THERE IS NO `serve --start`. Without this arm an unrecognised flag falls through to
			// serveForeground() below and silently runs a BLOCKING foreground plane — the opposite of
			// starting an instance, and the caller appears to hang. The verb that starts an instance is
			// `orchicon start` (services then plane) or `orchicon plane start` (the plane alone).
			// Exit 2 is a usage error, distinct from 1 = "not running", so no script's rc reading moves.
			bin := filepath.Base(os.Args[0])
			fmt.Fprintf(os.Stderr, "✗ `serve --start` does not exist — start an instance with:\n"+
				"    %s start [instance]        # services, then the plane\n"+
				"    %s plane start [instance]  # the plane only\n", bin, bin)
			return 2
		}
	}
	return serveForeground()
}

// serveForeground runs the server until SIGTERM/SIGINT. Also the body of
// the forked child started by serveDetach (the ORCHICON_SERVE_DETACHED
// env var short-circuits the dispatch above so the child never re-forks).
//
// When ORCHICON_SERVE_LOG_FILE is set (detached mode), the JSON slog
// output goes through a rotating file writer (size + time ceiling,
// retention pruning — internal/logging) instead of stdout, and the log
// file is dup2'd onto fds 1/2 so panics and stray prints land in the
// current log. The log rotation config is layered from env/config at
// boot; the server live-applies Settings → Defaults changes afterwards.
func serveForeground() int {
	ctx, cancel := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer cancel()

	var logOut io.Writer = os.Stdout
	var rotator *logging.RotatingWriter
	if logPath := os.Getenv(serveEnvLogFile); logPath != "" {
		cfg := config.Default()
		rw, err := logging.New(loggingFromEnv(cfg, logPath))
		if err != nil {
			// Detached: stdout/stderr are /dev/null, so a silent fallback
			// would lose every log line. Surface the failure at the log
			// path the operator is already watching, then bail.
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
	//
	// Boot installs the shipped preset when that is the rule (no explicit
	// ORCHICON_PERMISSION_POLICY and no file yet) and then strict-loads the
	// file. A MALFORMED policy stops the boot here, naming the path and the
	// parse error: a policy file that silently parses to nothing is worse
	// than no file at all, because the operator believes the exclusions are
	// in force. Failing loud is the only safe reading.
	if err := permpolicy.Boot(cfg.PermissionPolicyPath); err != nil {
		// TWO DIFFERENT FAILURES REACH HERE, and conflating them sends the operator to the wrong
		// place. A MALFORMED file is a policy problem — "invalid permission policy" plus the path
		// and the parse error, which is the loud failure the comment above is about, and which a
		// malformed-policy boot test pins. An UNWRITABLE DATA DIRECTORY is an environment problem:
		// the policy is not at fault and may not even exist yet, so reporting it this way sent an
		// operator to inspect a file that was fine.
		//
		// THE REPORTED CASE: `orchicon serve` run by hand in WSL. DataDir then defaults to
		// /var/lib/orchicon, which is right INSIDE the container image and needs root on the host —
		// where the installer supplies a per-instance directory instead. The message below names
		// the directory that is actually wrong, because that is the thing to change.
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

	// Run embedded migrations before the server starts, using the same
	// tracking table (_orchicon_migrations) that devStartParent uses.
	// This ensures consistency regardless of whether the user calls
	// orchicon-prod start (parent path) or orchicon-prod serve directly.
	if cfg.MigrateOnBoot {
		pool, err := db.Open(ctx, cfg.PostgresDSN)
		if err != nil {
			log.Error("failed to connect for migrations", "error", err)
			return 1
		}
		if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
			log.Error("migrations failed", "error", err)
			pool.Close()
			return 1
		}
		pool.Close()
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

// serveDetach forks the server into the background (own process group,
// logs to the rotating file at serveLogFile), writes the PID file, and
// returns immediately — the caller must NOT block, because a tool or
// script waiting on this command would hang until the server exits. The
// caller polls /healthz (see serveStatus / docs).
//
// The child (not the parent) owns the log file: serveForeground opens a
// rotating writer on ORCHICON_SERVE_LOG_FILE, redirects its own fd 1/2
// to it, and live-applies Settings → Defaults log management. The parent
// only passes the path and creates the directory.
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
	//
	// OBSERVED: a dev plane (ORCHICON_INSTANCE=dev, serving :8080) had fd 1 and fd 2 open on
	// ~/.local/share/orchicon-prod/serve/logs/orchicon.log. Its own log had not been written to for hours,
	// so the failure being debugged was invisible while a SIBLING instance's log filled up — two instances'
	// diagnostics colliding, which is exactly the confusion the per-instance state dir exists to prevent.
	//
	// Inheriting the ambient environment means whichever shell started the plane decides where its output
	// lands. Pinning both variables to the values THIS process resolved removes the ambient variable from
	// the decision entirely.
	// ALL THREE ARE PER-PROCESS AND NONE MAY BE INHERITED. `envWithout` clears them, then this process
	// states them explicitly — including ORCHICON_SERVE_DETACHED, which must be 1 for the child (it is the
	// marker that stops the child re-forking) but must come from US rather than from whatever the caller's
	// shell happened to carry.
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
// serveStopGrace is how long a plane is given to exit on SIGTERM before it is killed.
//
// A server that closes listeners, drains connections and closes its pools needs more than an instant; one
// that is refusing to die needs to be escalated to. 10s is the same order the container's own stop
// grace takes, and it is a variable only so a test does not have to wait it out.
var serveStopGrace = 10 * time.Second

// serveStop signals the detached plane to stop, WAITS for it to actually go, ESCALATES if it does not, and
// reports only what it verified.
//
// WHY WAITING AND ESCALATING ARE THE WHOLE POINT. This used to send SIGTERM, delete the PID file
// unconditionally, and print "✓ serve stopped" — never checking. A plane that ignored SIGTERM (a goroutine
// wedged in a blocked write, a stuck shutdown hook, a process parked on a socket) therefore SURVIVED while
// the command reported success AND THREW AWAY THE PID FILE. After that there was no way back with this
// tool: `--stop` and `--status` both resolve from the PID file, so both then answered "serve is not
// running" while the old process still owned the port and kept serving its old binary.
//
// OBSERVED, REPEATEDLY, BY THE OPERATOR: "I did that and the serve was still running and I had to do a
// kill -9 on the PIDs... I have run into this problem a few times now where I thought I was on the newest
// code but was still running a stale binary." The stale binary was this bug: a rebuild would stop nothing,
// start a second binary, lose the race for the port, and leave the OLD plane serving the OLD UI while every
// command said the restart had succeeded.
//
// SO: the PID file is removed ONLY when the process is confirmed gone, and a survivor is reported as a
// FAILURE (non-zero) so the caller — `scripts/container.sh plane_stop`, `make rebuild-*` — cannot continue
// as though the plane were down. A stop that cannot stop is not a success.
func serveStop() int {
	pidFile := servePIDFile()
	pid, running := procRunning(pidFile)
	if !running {
		// A PID FILE NAMING NOTHING IS STALE STATE, not merely an absent plane: it is what makes `--status`
		// keep claiming a plane exists. Clearing it here is safe precisely because we have just proved the
		// process is not running — and it cannot mask a survivor, since a survivor is `running`.
		if _, err := os.Stat(pidFile); err == nil {
			_ = os.Remove(pidFile)
		}
		fmt.Println("serve is not running")
		return 1
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
	// THE SIGNAL NAMES ARE PLATFORM-DEPENDENT and live in signals_unix.go / signals_windows.go. Unix sends
	// SIGTERM, waits, then SIGKILL. Windows has no SIGTERM: the graceful attempt is a Ctrl-Break to the
	// plane's process group, which usually cannot be delivered to a DETACHED plane (its own console), so the
	// ladder escalates to a force stop and SAYS SO rather than claiming SIGTERM was ignored.
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

// procRunningPID reports whether a PID is a running process, independent of any PID FILE — the check
// waitForExit needs, since the file it started from may already be gone. It shares pidAlive's definition of
// "running" (a zombie is not), so the stop path and --status cannot disagree.
func procRunningPID(pid int) (string, bool) {
	return strconv.Itoa(pid), pidAlive(pid)
}

// serveStatus reports the instance's plane through the SHARED status surface — the same call
// `orchicon plane status` makes — so the two spellings cannot disagree about the instance, the PID,
// the serving binary, drift or health. A second implementation is exactly the defect being removed
// here: two status surfaces that agree only by discipline.
//
// THE EXIT CODE IS A CONTRACT AND MUST NOT CHANGE. `scripts/orchicon.sh` (plane_status_out, and the
// post-stop verification in stop_host) machine-reads it: 0 means the PID file names a LIVE process.
// StatusOf never fails for want of a port record or a PID file — those are states, not errors — so it
// returns Status{PID: 0} when nothing runs, which is why PID > 0 is the whole of the success
// condition. A richer exit scheme would break the script's own "is it running?" reading.
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

// loggingFromEnv layers the log rotation config for a given log file
// path. Precedence: ORCHICON_LOG_* env vars, then built-in defaults. The
// DB-backed Settings → Defaults values are applied live by the server
// after boot (server.New + Run). logPath wins as the directory/base even
// when ORCHICON_LOG_DIR is set, because it is the path serveDetach
// actually opened for the PID/log contract.
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

// instanceForServe names the instance this plane belongs to, for the child's environment. The state dir is
// derived from it, so it must be explicit rather than inherited.
func instanceForServe() string {
	if inst := strings.TrimSpace(os.Getenv("ORCHICON_INSTANCE")); inst != "" {
		return inst
	}
	return "dev"
}
