// Command orchicon host-services supervises ONE instance's infrastructure
// processes AS HOST PROCESSES: Postgres, NATS and the telemetry plane
// (Tempo, Loki, VictoriaMetrics, OTel collector — optional via
// ORCHICON_TELEMETRY).
//
// IT IS NOT A CONTAINER, and there is no container path left in it. The plane
// and its services are host processes, always: `orchicon install` /
// `orchicon start` re-exec this binary (install_host.go startHostServices) and
// record its PID in the instance's host-services PID file, which
// stopHostServices signals. A container exists for exactly one purpose — a
// workflow's runtime container, opt-in per project.
//
// It is a minimal process supervisor, not an init system:
//   - spawns children in dependency order, gating on readiness probes
//     (postgres -> nats -> telemetry)
//   - prefixes each child's stdout/stderr with its component name
//   - restarts crashed children with exponential backoff
//   - forwards SIGTERM/SIGINT to all children and waits for graceful exit
//   - reaps children (Go's os/exec does this on Wait)
//
// THERE IS NO CONTROL-PLANE CHILD: the plane is its own host process
// (`orchicon serve`), started by the same installer.
//
// Data lives under ORCHICON_DATA_DIR (default ~/.local/share/orchicon). Config
// files are written there from the embedded ServiceConfigsFS, with @DATA_DIR@
// and the resolved @..._PORT@ placeholders substituted, so
// Tempo/Loki/collector find their config without any repo mounts.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/version"
)

const (
	dataDirEnv      = "ORCHICON_DATA_DIR"
	telemetryEnv    = "ORCHICON_TELEMETRY" // none | embedded | remote (default embedded)
	configDirName   = "config"
	shutdownTimeout = 20 * time.Second
)

// runHostServices is the `orchicon host-services` entrypoint. It runs until
// SIGTERM/SIGINT.
func runHostServices(args []string) int {
	// THE STOP SIGNALS ARE PLATFORM-DEPENDENT: SIGTERM+SIGINT on Unix, os.Interrupt on Windows, where
	// SIGTERM compiles and is never delivered. See signals_unix.go / signals_windows.go.
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("orchicon host-services starting", "version", version.Current().String())

	dataDir := env(dataDirEnv, defaultDataDirFor())
	telemetryMode := env(telemetryEnv, "embedded")
	log.Info("service shape resolved", "residency", "host", "data_dir", dataDir)

	sup := &supervisor{
		log:     log,
		dataDir: dataDir,
		ports:   servicePortsFromEnv(),
		// Set BEFORE prepare(): prepare() is what acquires the binaries, and it
		// must know whether the four telemetry backends are part of this boot —
		// a plane with ORCHICON_TELEMETRY=none must not download ~300 MB of
		// backends it will never start.
		telemetryEnabled: telemetryMode == "embedded",
		children:         make(map[string]*procState),
	}
	if err := sup.prepare(ctx); err != nil {
		log.Error("host-services prepare failed", "error", err)
		return 1
	}

	// Critical chain: postgres -> nats. Block on each so neither is raced.
	if err := sup.startAndWait(ctx, sup.postgresProc(), 120*time.Second); err != nil {
		log.Error("postgres did not become ready", "error", err)
	}
	if err := sup.startAndWait(ctx, sup.natsProc(), 60*time.Second); err != nil {
		log.Error("nats did not become ready", "error", err)
	}

	// Telemetry plane (optional). Spawned in parallel; nothing gates on it
	// (the plane's OTel dial is non-blocking and degrades).
	switch telemetryMode {
	case "embedded":
		for _, p := range []*managedProc{sup.tempoProc(), sup.lokiProc(), sup.vmProc(), sup.collectorProc()} {
			if err := sup.start(ctx, p); err != nil {
				log.Error("failed to start", "component", p.name, "error", err)
			}
		}
	case "none", "remote":
		log.Info("telemetry disabled", "mode", telemetryMode)
	default:
		log.Warn("unknown ORCHICON_TELEMETRY value (use none|embedded|remote)", "value", telemetryMode)
	}

	// A supervisor running without a plane is the SUPPORTED shape here (the plane
	// is its own host process), and nothing else in the log would say so.
	log.Info("postgres, nats and telemetry run as processes of the invoking user; no control-plane child")

	// Monitor children; restart crashed ones; return on ctx cancel.
	if err := sup.monitor(ctx); err != nil {
		log.Error("host-services monitor failed", "error", err)
		return 1
	}
	log.Info("orchicon host-services stopped")
	return 0
}

// ----- supervisor -----

type supervisor struct {
	log       *slog.Logger
	dataDir   string
	configDir string

	// ports is the single decision point for each service's bind port and its
	// readiness probe; see servicePortsFromEnv in residency.go.
	ports servicePorts

	// telemetryEnabled is whether this boot runs the four telemetry backends
	// (ORCHICON_TELEMETRY=embedded). It gates ACQUISITION as well as the proc
	// fleet: the backends are ~300 MB of downloads, and a `none` boot must not
	// fetch them.
	telemetryEnabled bool

	// infra is the RESOLVED infrastructure binaries (postgres, initdb, pg_ctl,
	// pg_isready, createdb, nats-server), set once by prepare() and read by every
	// service proc below.
	//
	// EVERY proc execs these absolute paths rather than a bare name on PATH: a
	// downloaded binary lives in this instance's data dir, which is not on PATH, so
	// a bare name would silently run a DIFFERENT postgres from the host (or none).
	infra *infraBinaries

	mu       sync.Mutex
	children map[string]*procState
}

type procState struct {
	proc     *managedProc
	cmd      *exec.Cmd
	exitCh   chan error
	restarts int
	// recovered records that this generation already had its state quarantined, so
	// recovery runs once rather than on every subsequent crash.
	recovered bool
}

// recoveryThreshold is how many consecutive crashes prove that a restart cannot
// fix the component. See managedProc.recoverState.
const recoveryThreshold = 3

// managedProc describes a supervised child process.
type managedProc struct {
	name    string
	command string
	args    []string
	env     []string // extra env (merged over the inherited environment)
	ready   func(ctx context.Context) bool
	// postReady runs once after the readiness gate passes (e.g. create the
	// orchicon database once postgres is up).
	postReady func(ctx context.Context) error
	restart   bool
	// recoverState runs when a component has crashed enough times to prove the
	// restart CANNOT fix it, and it is the difference between a service that heals
	// and one that crash-loops for ever.
	//
	// WHY IT EXISTS, measured. Loki refuses to start when its WAL has a
	// non-sequential segment — the state an unclean kill leaves behind (a 0-byte
	// segment written but never filled, then a jump in the numbering):
	//
	//	get segment range: segments are not sequential
	//	error initialising module: ingester
	//
	// That is PERMANENT. The supervisor's restart-with-backoff is exactly right for
	// a transient crash and exactly wrong for this one: observed in the field as
	// `restarts=71 backoff=32s` on loki, retrying every 32 seconds for hours while
	// the Logs view reported "loki is unreachable" — a service that could never
	// recover, burning a process slot and filling the log with the same failure.
	//
	// The hook QUARANTINES the unstartable state (moving it aside, never deleting)
	// so the next start begins from a clean directory. Moving rather than removing
	// is deliberate: the bytes are the operator's telemetry, and a recovery that
	// silently destroys data is worse than the crash-loop it fixes. Nil = this
	// component has no recoverable state, which is most of them.
	recoverState func() error
}

// prepare creates the data/config dirs, writes the embedded config files, then
// initializes the Postgres data directory. Every dir is created as the invoking
// user, which owns it already — there is nothing to hand over.
func (s *supervisor) prepare(ctx context.Context) error {
	s.configDir = filepath.Join(s.dataDir, configDirName)
	for _, d := range []string{s.dataDir, s.configDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	if err := s.writeConfigs(); err != nil {
		return err
	}

	// Resolve the infrastructure binaries ONCE, before anything execs them, and
	// hold the result on the supervisor. This is the authoritative acquisition
	// point: this process is what actually runs postgres/nats, and it is what owns
	// the instance data dir the downloaded build is unpacked into.
	//
	// The resolved set is also written to <dataDir>/infra-binaries.json, so a
	// failure here can be diagnosed from the record and so a later process reads
	// the same answer instead of probing again.
	//
	// THE OPTIONS COME FROM THE SHARED CONSTRUCTOR, not assembled here: that is what
	// makes ORCHICON_SKIP_DOWNLOAD and the URL/checksum overrides mean the same thing
	// to this boot as they do to `orchicon install`. Two callers assembling their own
	// options is how one of them silently stops honoring an operator's setting.
	// A supervisor that already carries a resolved set (a test fixture, or a
	// caller that resolved earlier) KEEPS it: resolution happens once, and
	// re-deriving it here would be the second resolution path this record exists
	// to prevent.
	if s.infra == nil {
		// THE ARTIFACT SUBSET IS THIS BOOT'S DECISION, not the resolver's: an
		// embedded-telemetry boot needs all six artifacts, a `none`/`remote` boot
		// needs postgres and nats only. Passing the subset keeps a disabled boot
		// from downloading backends it will never exec (AC7).
		infra, err := acquireInfraBinaries(ctx, defaultAcquireOptions(s.dataDir, s.acquiredArtifacts()...))
		if err != nil {
			return fmt.Errorf("acquire infrastructure binaries: %w", err)
		}
		s.infra = infra
		s.log.Info("infrastructure binaries resolved",
			"source", infra.Source,
			"postgres", infra.Postgres,
			"postgres_version", infra.PostgresVersion,
			"nats", infra.NatsServer,
			"telemetry_enabled", s.telemetryEnabled,
			"tempo", infra.Tempo, "loki", infra.Loki,
			"victoria_metrics", infra.VictoriaMetrics, "otel_collector", infra.OTelCollector)
	}
	// THE PRIVILEGE GUARD RUNS BEFORE initdb, NOT AFTER IT. Postgres on Windows refuses
	// to run under an Administrators token, and the refusal would otherwise arrive from
	// inside initdb with no cause named — the "install that appears to work and then
	// cannot start the database" failure. This process is what execs initdb and
	// postgres, so this is the earliest moment that actually matters. It is a no-op on
	// every platform except an elevated Windows shell (see privilege.go).
	if err := ensureHostPostgresPrivileges(); err != nil {
		return err
	}
	if err := s.initPostgres(); err != nil {
		return err
	}
	return nil
}

// acquiredArtifacts is the artifact subset THIS boot needs: every artifact when
// telemetry is embedded, postgres + nats alone otherwise. One method rather than
// an inline slice so the acquisition gate and the proc fleet read the same
// condition.
func (s *supervisor) acquiredArtifacts() []string {
	if s.telemetryEnabled {
		return nil // every artifact
	}
	return []string{"postgres", "nats-server"}
}

// configTokens is the substitution table writeConfigs applies to every embedded
// config: the data dir plus EVERY port the child binds. ONE TABLE, read by both
// writeConfigs and the tests, so a token added to a config without a value here
// fails immediately instead of shipping a literal "@…@" to a child that then
// refuses to parse its config.
//
// THE PORTS ARE HERE AND NOT IN ARGV because that is where tempo/loki/the
// collector keep them: their listen ports live in the config file. Templating
// the config from the same servicePorts the readiness probe reads is what makes
// them movable on a host without breaking the gate — the earlier code could only
// probe the config's hardcoded port.
func (s *supervisor) configTokens() map[string]string {
	p := s.ports
	return map[string]string{
		"@DATA_DIR@":        s.dataDir,
		"@TEMPO_HTTP_PORT@": strconv.Itoa(p.tempo),
		// Resolved, not copied: an INTERNAL listener still has to be bindable.
		// See internalListenerPort.
		"@TEMPO_GRPC_PORT@":          strconv.Itoa(internalListenerPort(p.tempoGRPC)),
		"@TEMPO_OTLP_GRPC_PORT@":     strconv.Itoa(p.tempoOTLPGRPC),
		"@TEMPO_OTLP_HTTP_PORT@":     strconv.Itoa(p.tempoOTLPHTTP),
		"@LOKI_HTTP_PORT@":           strconv.Itoa(p.loki),
		"@VM_HTTP_PORT@":             strconv.Itoa(p.vm),
		"@COLLECTOR_OTLP_GRPC_PORT@": strconv.Itoa(p.collectorOTLPGRPC),
		"@COLLECTOR_OTLP_HTTP_PORT@": strconv.Itoa(p.collectorOTLPHTTP),
		"@COLLECTOR_HEALTH_PORT@":    strconv.Itoa(p.collectorHealth),
		// The collector's self-metrics exporter is an INTERNAL listener whose
		// DEFAULT is localhost:8888, so like Tempo's gRPC port it has to be
		// walked off whatever the previous instance already owns. Nothing in
		// this repo scrapes it; it is kept (rather than disabled) so an operator
		// still has the collector's own stats when they want them.
		"@COLLECTOR_SELF_METRICS_PORT@": strconv.Itoa(internalListenerPort(p.collectorSelfMetrics)),
	}
}

// internalListenerPort returns a port the child can actually BIND, starting at
// preferred and walking up until something is free.
//
// WHY THIS EXISTS. Every other port in the table is a host port the OPERATOR
// reaches (the plane, the databases, the telemetry UIs), so the installer
// assigns each instance a distinct one and a collision would be an install-time
// error the operator sees. The children's INTERNAL listeners are different:
// nothing outside the process dials them, so nothing ever assigned them, and
// two instances on one host both tried to own the same number. Measured on this
// host: prod running normally on :9095, dev's Tempo crash-looping for ever on
// "listen tcp :9095: bind: address already in use" — retrying every 32s, with
// the operator's Traces view reporting the backend unreachable because of it.
//
// THE PROBE IS BEST-EFFORT, AND THAT IS DELIBERATE. Freeing the port and letting
// the child take it is not atomic, so a racing process can still win. The
// alternative — a permanently, deterministically broken bind for the second
// instance — is strictly worse, and the failure mode here is the same crash a
// fixed port produces, not a new one.
func internalListenerPort(preferred int) int {
	const span = 32
	for port := preferred; port < preferred+span; port++ {
		if port == 0 || port > 65535 {
			break
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			continue // in use (or unavailable) — try the next one
		}
		_ = ln.Close()
		return port
	}
	return preferred
}

// applyConfigTokens substitutes every configTokens key in body. Tokens are
// substituted LONGEST FIRST so a name that is a prefix of another
// (@TEMPO_OTLP_GRPC_PORT@ vs @TEMPO_OTLP_HTTP_PORT@ is fine, but the general
// rule matters) can never be partially rewritten by a shorter match.
func applyConfigTokens(keys []string, body string, tokens map[string]string) string {
	for _, k := range keys {
		body = strings.ReplaceAll(body, k, tokens[k])
	}
	return body
}

// writeConfigs lays out the embedded ServiceConfigsFS into the config dir,
// substituting the data dir and the resolved port table. The embedded paths
// start with "deploy/configs/".
func (s *supervisor) writeConfigs() error {
	const prefix = "deploy/configs/"
	tokens := s.configTokens()
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return fs.WalkDir(assets.ServiceConfigsFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasPrefix(path, prefix) {
			return nil
		}
		rel := strings.TrimPrefix(path, prefix)
		target := filepath.Join(s.configDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := assets.ServiceConfigsFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		content := applyConfigTokens(keys, string(data), tokens)
		return os.WriteFile(target, []byte(content), 0o644)
	})
}

// initPostgres runs initdb on the data dir if it is not yet initialized.
// Postgres refuses to run as root, so it runs as the INVOKING USER, who owns the
// data dir.
//
// IDEMPOTENT ON EVERY PLATFORM, by the PG_VERSION check below rather than by
// anything platform-specific: initdb is destructive against an initialized dir, so
// the check is the whole guard and it reads the same marker on Linux, macOS and
// Windows.
//
// `--auth=trust` IS DELIBERATE: initdb writes LOOPBACK-ONLY HBA rules (pinned by a
// test), postgresListenAddr keeps postgres on localhost, and the data dir is 0700
// owned by the invoking user. The code that ADDED wider rules for a published
// container port (pgHBATrustRules/ensurePostgresHBATrust) was deleted with the
// single-container deployment, so the HBA stays initdb's default.
func (s *supervisor) initPostgres() error {
	dataDir := filepath.Join(s.dataDir, "postgres")
	initialized := false
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err == nil {
		initialized = true
	}
	// THE RETIRED-CONTAINER CLUSTER GUARD.
	//
	// The deleted single-container deployment kept postgres in a container volume,
	// not in <dataDir>/postgres, and nothing migrated between them. So on a boot
	// here: with NO cluster, initdb would create an empty database beside the real
	// one; with a STALE cluster, postgres would start on old data. Both look
	// healthy from outside, which is why this runs BEFORE initdb and refuses
	// rather than warns. See postgres_legacy_volume.go.
	//
	// It is a no-op for an instance with no container-era volume and for a host
	// with no engine.
	if err := guardContainerEraPostgresCluster(s.dataDir, initialized, os.Stderr); err != nil {
		return err
	}
	if initialized {
		return nil // already initialized
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	// The services run as the invoking user, so the dir is already theirs —
	// there is nothing to hand over (and chown is impossible without root).
	if err := hostDataDirWritable(dataDir); err != nil {
		// CRITERION 3, THE HOST HALF: "a data dir owned by another user produces
		// an ACTIONABLE error rather than a corruption risk."
		//
		// On a host we cannot chown (no root), so an uninitialized dir owned by
		// someone else is exactly the shape that used to surface as initdb's own
		// "could not change permissions of directory ...: Operation not permitted"
		// — or, worse, as a HALF-WRITTEN data dir if initdb got far enough to write
		// before failing. The probe below touches the dir the same way initdb will,
		// BEFORE initdb runs, so the failure names the cause and the remedy while
		// nothing has been created. It is a file create, not a chmod: on Windows a
		// foreign-owned dir under a different ACL fails the same way and this is the
		// portable way to observe it.
		return err
	}
	cmd := exec.Command(s.resolved().Initdb,
		"-D", dataDir, "-U", "orchicon", "--auth=trust", "-E", "UTF8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("initdb: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// postgresListenAddr is the single decision point for postgres'
// listen_addresses: LOOPBACK, always.
//
// Nothing forwards to a host process, and the plane reaches postgres over
// loopback, which initdb --auth=trust already trusts. Widening the address would
// only expose the database.
func (s *supervisor) postgresListenAddr() string {
	return "localhost"
}

// postgresUIDGID returns the uid/gid the postgres child should run as: the
// INVOKING USER, because the services ARE that user's processes.
//
// THE VALUES COME FROM A PLATFORM SEAM, not os.Getuid()/os.Getgid(), because
// those return -1 on Windows (os/syscall_windows.go). Windows reports 0 instead,
// which means "there is no uid to drop to". Nothing chowns with these values — a
// host cannot chown.
func (s *supervisor) postgresUIDGID() (int, int) {
	return invokingUID(), invokingGID()
}

// postgresSocketDir is the directory postgres keeps its Unix-domain socket and
// its .s.PGSQL.<port>.lock in, or "" to leave postgres on its own default.
//
// POSTGRES MUST NOT KEEP THE DEFAULT (/var/run/postgresql): it runs as the
// INVOKING USER, who does not own /var/run/postgresql — the postgres package owns it as postgres:postgres,
// group-writable only to that group — so postgres refuses to start
// ("could not create lock file .../.s.PGSQL.<port>.lock: Permission denied")
// and the readiness gate can never pass. The data dir belongs to the invoking
// user, so the socket lives there. Same fix, same reason, as the container
// sandbox plane (internal/runtime/agent.go).
func (s *supervisor) postgresSocketDir() string {
	// WINDOWS HAS NO UNIX-DOMAIN SOCKETS. The Unix socket transport does not exist
	// there, so the flag must not be passed to postgres.exe at all — at best it is
	// ignored, at worst it is a startup error, and either way claiming a socket
	// directory on a platform that has none is a lie the next reader has to disprove.
	// unixSocketsSupported is the platform seam (true on Linux/macOS, false on
	// Windows), so this is a compile-time fact rather than a runtime probe.
	if unixSocketsSupported {
		return s.dataDir
	}
	return ""
}

// start spawns a child and begins supervising it (log pipe + exit monitor).
func (s *supervisor) start(ctx context.Context, p *managedProc) error {
	cmd := exec.CommandContext(ctx, p.command, p.args...)
	cmd.Env = append(os.Environ(), p.env...)
	cmd.Stdout = s.logPipe(p.name)
	cmd.Stderr = s.logPipe(p.name)

	s.mu.Lock()
	if _, exists := s.children[p.name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("already running: %s", p.name)
	}
	st := &procState{proc: p, cmd: cmd, exitCh: make(chan error, 1)}
	s.children[p.name] = st
	s.mu.Unlock()

	if err := cmd.Start(); err != nil {
		s.mu.Lock()
		delete(s.children, p.name)
		s.mu.Unlock()
		return fmt.Errorf("start %s: %w", p.name, err)
	}
	s.log.Info("started", "component", p.name, "pid", cmd.Process.Pid)

	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		st := s.children[p.name]
		s.mu.Unlock()
		if st != nil {
			st.exitCh <- err
		}
	}()
	return nil
}

// startAndWait spawns a child and blocks until its readiness gate passes
// (or the timeout elapses, which only logs a warning — the process keeps
// running and the monitor restarts it if it crashes). It returns early if
// the child exits during the wait so the caller can proceed to the next
// dependency and the monitor can restart the dead child.
func (s *supervisor) startAndWait(ctx context.Context, p *managedProc, timeout time.Duration) error {
	if err := s.start(ctx, p); err != nil {
		return err
	}
	s.mu.Lock()
	st := s.children[p.name]
	s.mu.Unlock()
	if st == nil {
		return fmt.Errorf("no proc state for %s", p.name)
	}
	deadline := time.Now().Add(timeout)
	for {
		if p.ready != nil && p.ready(ctx) {
			if p.postReady != nil {
				if err := p.postReady(ctx); err != nil {
					s.log.Warn("postReady hook failed", "component", p.name, "error", err)
				}
			}
			s.log.Info("ready", "component", p.name)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-st.exitCh:
			return fmt.Errorf("%s exited during startup: %w", p.name, err)
		case <-time.After(500 * time.Millisecond):
			if time.Now().After(deadline) {
				s.log.Warn("readiness timeout, continuing", "component", p.name)
				return nil
			}
		}
	}
}

// monitor waits for children to exit, restarting crashed ones with
// exponential backoff, until ctx is cancelled (shutdown).
func (s *supervisor) monitor(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			s.shutdown(ctx)
			return nil
		case <-time.After(1 * time.Second):
			s.reapRestart(ctx)
		}
	}
}

// restartBackoff is the delay before the Nth consecutive restart of a crashed
// child: 2s, then 4, 8, 16, and 32s — capped there so a permanently broken child
// is retried every 32s instead of hot-looping.
func restartBackoff(restarts int) time.Duration {
	return time.Duration(1<<min(restarts, 5)) * time.Second
}

// reapRestart checks all children for exits and restarts crashed ones.
func (s *supervisor) reapRestart(ctx context.Context) {
	s.mu.Lock()
	states := make([]*procState, 0, len(s.children))
	for _, st := range s.children {
		states = append(states, st)
	}
	s.mu.Unlock()

	for _, st := range states {
		select {
		case err := <-st.exitCh:
			s.mu.Lock()
			delete(s.children, st.proc.name)
			s.mu.Unlock()
			if !st.proc.restart {
				s.log.Warn("component exited (no restart)", "component", st.proc.name, "error", err)
				continue
			}
			st.restarts++
			backoff := restartBackoff(st.restarts)
			s.log.Warn("component exited; restarting", "component", st.proc.name,
				"restarts", st.restarts, "backoff", backoff.String(), "error", err)
			// PAST THE POINT WHERE A RESTART CAN HELP, TRY TO REPAIR THE STATE.
			//
			// Three crashes in a row is the threshold: one crash is a transient
			// fault and two can be a race, but three consecutive failures of the SAME
			// child with the SAME backoff means the restart is not converging — the
			// process is being handed the same unstartable state every time. That is
			// what a corrupt WAL looks like from here, and no amount of retrying
			// reaches a different outcome.
			//
			// It runs ONCE per recovery generation (the counter resets when the
			// process comes up, see below), so a component that genuinely cannot run
			// does not have its state moved aside on every tick — the log records
			// that recovery was attempted, and the next crashes take the ordinary
			// backoff path.
			if st.proc.recoverState != nil && st.restarts == recoveryThreshold && !st.recovered {
				st.recovered = true
				s.log.Warn("component has failed repeatedly — quarantining its state and retrying",
					"component", st.proc.name, "restarts", st.restarts)
				if rerr := st.proc.recoverState(); rerr != nil {
					s.log.Error("state recovery failed; the component will keep failing",
						"component", st.proc.name, "error", rerr)
				}
			}
			time.Sleep(backoff)
			if err := s.start(ctx, st.proc); err != nil {
				s.log.Error("restart failed", "component", st.proc.name, "error", err)
			} else {
				// start() installs a FRESH procState (restarts=0). Carry the
				// consecutive-crash count forward, or the backoff never escalates:
				// every restart would recompute from 1 and sleep 2s forever.
				s.mu.Lock()
				if ns := s.children[st.proc.name]; ns != nil {
					ns.restarts = st.restarts
				}
				s.mu.Unlock()
			}
		default:
		}
	}
}

// shutdown sends SIGTERM to every child, waits up to shutdownTimeout, then
// SIGKILLs stragglers.
func (s *supervisor) shutdown(ctx context.Context) {
	s.mu.Lock()
	states := make([]*procState, 0, len(s.children))
	for _, st := range s.children {
		states = append(states, st)
	}
	s.mu.Unlock()

	s.log.Info("shutting down", "children", len(states))
	for _, st := range states {
		if st.cmd.Process != nil {
			_ = sendGracefulStop(st.cmd.Process)
		}
	}

	deadline := time.Now().Add(shutdownTimeout)
	for _, st := range states {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case <-st.exitCh:
		case <-time.After(remaining):
		}
	}
	// Force-kill anything still alive after the grace period.
	for _, st := range states {
		if st.cmd.Process != nil {
			_ = st.cmd.Process.Kill()
		}
	}
}

// env returns the value of the environment variable or a fallback.
func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envInt returns the integer value of an environment variable or a
// fallback (0 when the var is unset/invalid).
func envInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// logPipe returns a writer that prefixes each line with the component name.
func (s *supervisor) logPipe(name string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			s.log.Info(sc.Text(), "component", name)
		}
		_ = pr.Close()
	}()
	return pw
}

// ----- probes -----

func tcpReady(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func httpReady(url string) func(context.Context) bool {
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 500
	}
}

// resolved returns the infrastructure-binary record, and PANICS when it is
// missing.
//
// THE PANIC IS THE POINT. Every consumer of this record execs a binary, and the
// only way it can be missing is a supervisor built without prepare() — a
// programming error. Falling back to the bare name would be a SECOND resolution
// path (it would find whatever `postgres` happens to be on PATH, which for a
// downloaded install is the wrong server or none at all), and the whole reason this
// record exists is that there is exactly one answer.
func (s *supervisor) resolved() *infraBinaries {
	if s.infra == nil {
		panic("supervisor.infrastructure binaries were not resolved — prepare() must run before any service proc")
	}
	return s.infra
}

// ----- process definitions -----

func (s *supervisor) postgresProc() *managedProc {
	dataDir := filepath.Join(s.dataDir, "postgres")
	port := strconv.Itoa(s.ports.postgres)
	pgArgs := []string{"-D", dataDir, "-p", port, "-c", "listen_addresses=" + s.postgresListenAddr()}
	if socketDir := s.postgresSocketDir(); socketDir != "" {
		pgArgs = append(pgArgs, "-c", "unix_socket_directories="+socketDir)
	}
	return &managedProc{
		name:    "postgres",
		command: s.resolved().Postgres,
		args:    pgArgs,
		ready:   func(ctx context.Context) bool { return tcpReady(ctx, "localhost:"+port) },
		postReady: func(ctx context.Context) error {
			// Create the orchicon database on first boot (idempotent).
			// createdb exits 0 when created and errors on duplicate;
			// "already exists" is fine.
			cmd := exec.CommandContext(ctx, s.resolved().Createdb, "-U", "orchicon", "-h", "localhost", "-p", port, "orchicon")
			out, err := cmd.CombinedOutput()
			if err != nil && !strings.Contains(string(out), "already exists") {
				return fmt.Errorf("create orchicon db: %w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		restart: true,
	}
}

func (s *supervisor) natsProc() *managedProc {
	monitor := strconv.Itoa(s.ports.natsMonitor)
	return &managedProc{
		name:    "nats",
		command: s.resolved().NatsServer,
		args: []string{"-js", "-sd", filepath.Join(s.dataDir, "nats"),
			"-p", strconv.Itoa(s.ports.nats), "-m", monitor},
		ready:   httpReady("http://localhost:" + monitor + "/healthz"),
		restart: true,
	}
}

func (s *supervisor) tempoProc() *managedProc {
	return &managedProc{
		name:    "tempo",
		command: s.resolved().Tempo,
		args:    []string{"-config.file=" + filepath.Join(s.configDir, "tempo.yaml")},
		ready:   httpReady("http://localhost:" + strconv.Itoa(s.ports.tempo) + "/ready"),
		restart: true,
	}
}

func (s *supervisor) lokiProc() *managedProc {
	return &managedProc{
		name:    "loki",
		command: s.resolved().Loki,
		args:    []string{"-config.file=" + filepath.Join(s.configDir, "loki.yaml")},
		ready:   httpReady("http://localhost:" + strconv.Itoa(s.ports.loki) + "/ready"),
		restart: true,
		// LOKI IS THE ONE COMPONENT WITH STATE A RESTART CANNOT FIX.
		//
		// It refuses to start on a non-sequential WAL, which is what an unclean kill
		// leaves behind: a segment written but never filled, then a jump in the
		// numbering. Measured in the field — loki at `restarts=71 backoff=32s`,
		// retrying for hours while the Logs view reported it unreachable.
		//
		// The WAL is quarantined by RENAMING the directory, never deleting it: the
		// segments are the operator's own telemetry, and a recovery that destroys
		// data to fix a crash-loop is a worse failure than the loop. Loki starts
		// clean and writes a fresh WAL; the old one is left beside it for an operator
		// who wants to recover the bytes.
		//
		// Only the WAL is moved. Loki's chunks and its index are readable state that
		// survives a restart, and moving them would discard logs that are perfectly
		// good — the corrupt thing is the write-ahead log, so that is all that goes.
		recoverState: func() error {
			wal := filepath.Join(s.dataDir, "loki", "wal")
			if _, err := os.Stat(wal); err != nil {
				return nil // nothing to quarantine
			}
			quarantine := fmt.Sprintf("%s.corrupt-%s", wal, time.Now().UTC().Format("20060102T150405Z"))
			if err := os.Rename(wal, quarantine); err != nil {
				return fmt.Errorf("quarantine loki WAL %s: %w", wal, err)
			}
			s.log.Warn("loki WAL quarantined — a non-sequential WAL is permanent, so retrying could never "+
				"recover it; loki will start with a fresh one and the old segments are kept",
				"moved", wal, "to", quarantine)
			return nil
		},
	}
}

func (s *supervisor) vmProc() *managedProc {
	return &managedProc{
		name:    "victoriametrics",
		command: s.resolved().VictoriaMetrics,
		args: []string{"-storageDataPath=" + filepath.Join(s.dataDir, "victoriametrics"),
			"-retentionPeriod=720h", "-httpListenAddr=:" + strconv.Itoa(s.ports.vm)},
		ready:   httpReady("http://localhost:" + strconv.Itoa(s.ports.vm) + "/health"),
		restart: true,
	}
}

func (s *supervisor) collectorProc() *managedProc {
	return &managedProc{
		name:    "otel-collector",
		command: s.resolved().OTelCollector,
		args:    []string{"--config=" + filepath.Join(s.configDir, "otel-collector.yaml")},
		ready: func(ctx context.Context) bool {
			return tcpReady(ctx, "localhost:"+strconv.Itoa(s.ports.collectorHealth))
		},
		restart: true,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
