package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/logging"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/opencode"
	"github.com/beardedparrott/orchicon/internal/permpolicy"
	"github.com/beardedparrott/orchicon/internal/server"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/version"
)

// killOrphans kills the leftover opencode and orchicon mcp processes found by
// every plane boot's sweep. These accumulate when the server is killed before
// the ChatStream subprocess exits (e.g. during a forced binary replacement).
//
// WHAT COUNTS AS AN ORPHAN: a process that matches pgrep AND carries the
// PLANE SPAWN MARKER (carriesPlaneMarker) AND has been reparented away from the
// process that started it (reparentedAway). BOTH halves are required.
//
// The marker is the half that keeps the sweep honest. The point of this guard
// has always been to leave the operator's OWN opencode alone, and parentage
// alone cannot express that: on a host, an `opencode` started from a terminal
// that has since closed and a plane-spawned `opencode` left behind by a crash
// are reparented to the SAME place. Only the marker says which is which, so
// only a marked process is ever a candidate.
//
// The parentage half is what makes it an ORPHAN: a marked process whose parent
// is still alive is one we are still supervising (the `orchicon mcp` sidecars
// are children of a live opencode), and killing it would be a self-inflicted
// outage.
//
// The reap point is NOT always PID 1. Reparenting climbs to the nearest
// ancestor that set PR_SET_CHILD_SUBREAPER and stops there, falling back to
// PID 1 only when there is none. In a container nothing sets it, so orphans
// land on PID 1; in a HOST user session the session manager sets it, so a
// crashed plane's leftover opencode reparents to the session manager instead.
// A PID-1-only test would therefore reap in a container and silently collect
// nothing on a host — a sweep that reports success and leaves the processes
// running. See reapPoints.
//
// WHY THE PARENTAGE GUARD EXISTS: inside a container the pgrep could only ever
// see the plane's own children, so an unconditional SIGTERM sweep was harmless
// there. On a HOST-resident plane the same pgrep sees the WHOLE USER SESSION,
// and the harm is asymmetric — a sibling instance's plane is restarted by its
// serve watchdog, but the operator's own `opencode` (a legitimate, unrelated
// process) simply dies. The sweep therefore bounds itself to genuine orphans.
//
// Skipped in the runtime-container sandbox plane (ORCHICON_SANDBOX_PLANE=1):
// there the opencode serve is a LIVE child of the runtime supervisor, not
// an orphan of a crashed plane — pgrep would match and SIGTERM it (plus any
// live `orchicon mcp` sidecars serving worker sessions). The supervisor's
// own serve watchdog owns that process's lifecycle.
func killOrphans() {
	if os.Getenv("ORCHICON_SANDBOX_PLANE") != "" {
		return
	}
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		return
	}
	// Resolve the reap set ONCE per sweep: it is an environment property, and
	// every candidate must be judged against the same answer.
	reap := reapPoints()
	for _, name := range []string{"opencode", "orchicon mcp"} {
		sweepOrphans(orphanCandidates(pgrep, name), reap)
	}
}

// orphanCandidates returns the PIDs pgrep matches for name. `-x` matches the
// process NAME exactly; a multi-word pattern such as "orchicon mcp" cannot be
// a process name, so it is matched against the whole command line (`-f`) —
// both patterns get the identical parentage treatment. A pgrep that finds
// nothing (or is unusable) yields no candidates.
func orphanCandidates(pgrep, name string) []int {
	args := []string{"-x"}
	if strings.Contains(name, " ") {
		args = []string{"-f"} // use -f for multi-word patterns
	}
	out, err := exec.Command(pgrep, append(args, name)...).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// sweepOrphans SIGTERMs the candidates that are genuine orphans
// (reparentedAway) and returns how many it signalled. reap is this session's
// reap set, resolved once by the caller so every candidate is judged against
// the same answer.
func sweepOrphans(pids []int, reap map[int]bool) int {
	signalled := 0
	for _, pid := range pids {
		if !carriesPlaneMarker(pid) || !reparentedAway(pid, reap) {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(syscall.SIGTERM)
			signalled++
		}
	}
	return signalled
}

// planeSpawnMarker is the variable the plane stamps on every process it starts
// (opencode.PlaneSpawnEnv). It is read from the package that SETS it, so the
// sweeper and the spawner cannot drift apart.
const planeSpawnMarker = opencode.PlaneSpawnEnv

// carriesPlaneMarker reports whether pid's environment shows the plane spawn
// marker — i.e. whether an Orchicon plane started it, directly or through the
// opencode that inherited the marker and passed it on.
//
// /proc/<pid>/environ is readable only for processes of the same user, which is
// exactly the set this sweep may ever touch. An unreadable environ reports
// false, and false means the process is LEFT ALONE: a missed orphan is a leak,
// while a wrong SIGTERM kills the operator's own opencode. The guard fails in
// the direction that keeps processes alive, as it always has.
func carriesPlaneMarker(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	for _, kv := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(kv, planeSpawnMarker+"=") {
			return true
		}
	}
	return false
}

// reapProbeTimeout bounds the orphan probe below. Reparenting is immediate
// once the spawning shell exits; this only covers a pathologically slow
// scheduler.
const reapProbeTimeout = 2 * time.Second

// parentPID reads a process's parent straight from /proc. ok is false when the
// entry is unreadable — another user's process, a pid that has already exited,
// or a non-Linux host.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// discoverReapPoint finds where THIS environment hands orphans, by making one:
// a short-lived shell spawns a sleeper and exits, and we observe where the
// sleeper lands. Returns 0 when it cannot be established.
//
// WHY A PROBE RATHER THAN REASONING ABOUT THE PROCESS TREE. The reap point is
// the nearest ancestor carrying PR_SET_CHILD_SUBREAPER, and that attribute is
// not readable from outside the process that holds it. An earlier version of
// this walked our own ancestry and took the outermost non-init ancestor, on the
// theory that a login session's manager sits there. That theory is WRONG
// whenever anything sits ABOVE the real reaper — a container entrypoint, a
// nested subreaper — and it fails SILENTLY, because the walk still returns a
// plausible-looking pid; the sweep then matches nothing and reports success.
// (Demonstrated by planting a subreaper beneath an outer ancestor: the orphan
// landed on the planted reaper while the walk returned the outer process.)
// Making one real orphan and observing it is exact in any topology, and costs
// one subprocess per sweep.
func discoverReapPoint() int {
	out, err := exec.Command("/bin/sh", "-c", "/bin/sleep 30 >/dev/null 2>&1 & echo $!; echo $$").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return 0
	}
	spawner, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	// The probe's own sleeper is ours to clean up, whatever we learn.
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	deadline := time.Now().Add(reapProbeTimeout)
	for time.Now().Before(deadline) {
		if pp, ok := parentPID(pid); ok && pp != spawner {
			return pp
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0
}

// reapPoints returns the parent PIDs that mean "whatever spawned this process is
// gone, and it has been handed to the reaper": PID 1, plus whatever
// discoverReapPoint observed.
//
// WHY NOT JUST PID 1. Reparenting does not necessarily end at PID 1 — it stops
// at the nearest ancestor carrying PR_SET_CHILD_SUBREAPER. A container has no
// such ancestor, so orphans land on PID 1. A host user session manager sets the
// attribute on itself, so on a HOST-resident plane every orphan of a crashed
// plane lands THERE. Testing only for PID 1 is therefore a container-shaped
// assumption: the sweep would look like it ran and quietly collect nothing on
// the host, which is where users are.
//
// This process is deliberately NEVER in the set: its own children are LIVE
// children, and reaping them would be the worst possible failure of this sweep.
func reapPoints() map[int]bool {
	pts := map[int]bool{1: true}
	if rp := discoverReapPoint(); rp > 1 && rp != os.Getpid() {
		pts[rp] = true
	}
	return pts
}

// reparentedAway reports whether pid's parent is one of the session's reap
// points (reap) — the signature of a process a crashed plane left behind,
// because the parent that spawned it is gone.
//
// WHY /proc RATHER THAN `pgrep -P <reaper>`: the parentage test is the whole
// safety property of this sweep, so it lives in Go where it is explicit and
// covered by a test that needs no reparented process (and where it applies
// identically to both patterns, whatever pgrep is installed). An unreadable
// /proc entry (another user's process, a non-Linux host) reports false: a
// missed orphan is a leak, a wrong SIGTERM is the operator's opencode dying —
// so the guard fails in the direction that keeps processes alive.
//
// A LIVE child of OURS is never an orphan, whatever the reap set says: it is
// still supervised by this very process. This is checked here, structurally,
// rather than relied on from the call site.
func reparentedAway(pid int, reap map[int]bool) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	pp, ok := parentPID(pid)
	if !ok || pp == os.Getpid() {
		return false
	}
	return reap[pp]
}

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
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
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
	if pid, running := procRunning(servePIDFile); running {
		fmt.Fprintf(os.Stderr, "✗ serve is already running (PID %s)\n", pid)
		fmt.Fprintf(os.Stderr, "  Stop it with: %s serve --stop\n", filepath.Base(os.Args[0]))
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(servePIDFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to create PID directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(serveLogFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to create log directory: %v\n", err)
		return 1
	}

	cmd := exec.Command(os.Args[0], "serve")
	cmd.Env = append(os.Environ(), serveEnvDetached+"=1", serveEnvLogFile+"="+serveLogFile)
	cmd.Stdin = nil // /dev/null — the child must not inherit the caller's stdin
	cmd.Stdout = nil
	cmd.Stderr = nil
	setProcAttrBackground(cmd)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ failed to start serve: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(servePIDFile, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  ! failed to write PID file: %v\n", err)
	}
	// Release the child from our care so nothing waits on it. Start it in
	// its own process group (setProcAttrBackground) and detach.
	_ = cmd.Process.Release()

	fmt.Printf("✓ serve detached (PID %d)\n", pid)
	fmt.Printf("  Logs: %s\n", serveLogFile)
	fmt.Printf("  Check: %s serve --status\n", filepath.Base(os.Args[0]))
	fmt.Printf("  Stop: %s serve --stop\n", filepath.Base(os.Args[0]))
	return 0
}

// serveStop sends SIGTERM to a detached serve and clears the PID file.
func serveStop() int {
	pid, running := procRunning(servePIDFile)
	if !running {
		fmt.Println("serve is not running")
		return 1
	}
	pidNum, err := strconv.Atoi(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ invalid PID file (%s): %v\n", servePIDFile, err)
		return 1
	}
	if proc, err := os.FindProcess(pidNum); err == nil {
		if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintf(os.Stderr, "✗ failed to signal PID %d: %v\n", pidNum, err)
			return 1
		}
	}
	_ = os.Remove(servePIDFile)
	fmt.Printf("✓ serve stopped (PID %d)\n", pidNum)
	return 0
}

// serveStatus prints whether a detached serve is running.
func serveStatus() int {
	if pid, running := procRunning(servePIDFile); running {
		fmt.Printf("serve is running (PID %s)\n", pid)
		return 0
	}
	fmt.Println("serve is not running")
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
