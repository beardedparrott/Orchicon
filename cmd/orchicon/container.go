// Command orchicon container runs the entire Orchicon stack inside a single
// container as PID 1: Postgres, NATS, the Grafana telemetry plane (Tempo,
// Loki, VictoriaMetrics, OTel collector, Grafana — optional via
// ORCHICON_TELEMETRY), and the Orchicon control plane itself.
//
// It is a minimal process supervisor, not an init system:
//   - spawns children in dependency order, gating on readiness probes
//     (postgres → nats → telemetry → control plane)
//   - prefixes each child's stdout/stderr with its component name
//   - restarts crashed children with exponential backoff
//   - forwards SIGTERM/SIGINT to all children and waits for graceful exit
//   - reaps children (Go's os/exec does this on Wait)
//
// The control plane's /healthz is the container's aggregate readiness
// signal; published ports are the control plane (:8080) and Grafana
// (:3000 → host :3002 by default in docker run).
//
// Data lives under ORCHICON_DATA_DIR (default /var/lib/orchicon). Config
// files are written there from the embedded ContainerFS, with @DATA_DIR@
// substituted for the data dir, so Tempo/Loki/Grafana/collector find their
// config without any repo mounts.
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
	"strings"
	"sync"
	"syscall"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/version"
)

const (
	containerModeEnv = "ORCHICON_CONTAINER_MODE"
	dataDirEnv       = "ORCHICON_DATA_DIR"
	telemetryEnv     = "ORCHICON_TELEMETRY" // none | embedded | remote (default embedded)
	configDirName    = "config"
	shutdownTimeout  = 20 * time.Second
)

// runContainer is the `orchicon container` entrypoint. It runs until
// SIGTERM/SIGINT.
func runContainer(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("orchicon container starting", "version", version.Current().String())

	dataDir := env(dataDirEnv, "/var/lib/orchicon")
	telemetryMode := env(telemetryEnv, "embedded")

	sup := &supervisor{log: log, dataDir: dataDir, children: make(map[string]*procState)}
	if err := sup.prepare(ctx); err != nil {
		log.Error("container prepare failed", "error", err)
		return 1
	}

	// Critical chain: postgres → nats → control plane. Block on each so the
	// plane never boots without its backends.
	if err := sup.startAndWait(ctx, sup.postgresProc(), 120*time.Second); err != nil {
		log.Error("postgres did not become ready", "error", err)
	}
	if err := sup.startAndWait(ctx, sup.natsProc(), 60*time.Second); err != nil {
		log.Error("nats did not become ready", "error", err)
	}

	// Telemetry plane (optional). Spawned in parallel; nothing gates on it
	// (the control plane's OTel dial is non-blocking and degrades).
	switch telemetryMode {
	case "embedded":
		for _, p := range []*managedProc{sup.tempoProc(), sup.lokiProc(), sup.vmProc(), sup.collectorProc(), sup.grafanaProc()} {
			if err := sup.start(ctx, p); err != nil {
				log.Error("failed to start", "component", p.name, "error", err)
			}
		}
	case "none", "remote":
		log.Info("telemetry disabled", "mode", telemetryMode)
	default:
		log.Warn("unknown ORCHICON_TELEMETRY value (use none|embedded|remote)", "value", telemetryMode)
	}

	if err := sup.startAndWait(ctx, sup.planeProc(), 120*time.Second); err != nil {
		log.Error("control plane did not become ready", "error", err)
	}

	// Monitor children; restart crashed ones; return on ctx cancel.
	if err := sup.monitor(ctx); err != nil {
		log.Error("container monitor failed", "error", err)
		return 1
	}
	log.Info("orchicon container stopped")
	return 0
}

// ----- supervisor -----

type supervisor struct {
	log      *slog.Logger
	dataDir  string
	configDir string

	mu       sync.Mutex
	children map[string]*procState
}

type procState struct {
	proc     *managedProc
	cmd      *exec.Cmd
	exitCh   chan error
	restarts int
}

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
}

// prepare creates the data/config dirs and writes the embedded config
// files, then initializes the Postgres data directory.
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
	if err := s.initPostgres(); err != nil {
		return err
	}
	return nil
}

// writeConfigs lays out the embedded ContainerFS into the config dir,
// substituting @DATA_DIR@ with the real data dir. The embedded paths start
// with "deploy/container/configs/".
func (s *supervisor) writeConfigs() error {
	const prefix = "deploy/container/configs/"
	return fs.WalkDir(assets.ContainerFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
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
		data, err := assets.ContainerFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		content := strings.ReplaceAll(string(data), "@DATA_DIR@", s.dataDir)
		return os.WriteFile(target, []byte(content), 0o644)
	})
}

// initPostgres runs initdb on the data dir if it is not yet initialized.
// Postgres refuses to run as root, so it runs as the owner of the data dir
// (default uid 70 — the uid the compose-stack alpine postgres volumes are
// created with — via setpriv).
func (s *supervisor) initPostgres() error {
	dataDir := filepath.Join(s.dataDir, "postgres")
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err == nil {
		return nil // already initialized
	}
	uid, gid := s.postgresUIDGID()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chown(dataDir, uid, gid); err != nil {
		return fmt.Errorf("chown postgres data dir: %w", err)
	}
	cmd := exec.Command("setpriv", "--reuid=70", "--regid=70", "--clear-groups",
		"initdb", "-D", dataDir, "-U", "orchicon", "--auth=trust", "-E", "UTF8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("initdb: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// postgresUIDGID returns the uid/gid the postgres child should run as.
// If the data dir already exists (a preserved compose-stack volume), it
// matches the existing owner so the data is readable; otherwise it uses
// uid 70, matching the postgres:16-alpine compose volumes.
func (s *supervisor) postgresUIDGID() (int, int) {
	dataDir := filepath.Join(s.dataDir, "postgres")
	if fi, err := os.Stat(dataDir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return int(st.Uid), int(st.Gid)
		}
	}
	return 70, 70
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
			backoff := time.Duration(1<<min(st.restarts, 5)) * time.Second // 2,4,8,16,32s cap
			s.log.Warn("component exited; restarting", "component", st.proc.name,
				"restarts", st.restarts, "backoff", backoff.String(), "error", err)
			time.Sleep(backoff)
			if err := s.start(ctx, st.proc); err != nil {
				s.log.Error("restart failed", "component", st.proc.name, "error", err)
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
			_ = st.cmd.Process.Signal(syscall.SIGTERM)
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

// ----- process definitions -----

func (s *supervisor) postgresProc() *managedProc {
	dataDir := filepath.Join(s.dataDir, "postgres")
	uid, gid := s.postgresUIDGID()
	return &managedProc{
		name:    "postgres",
		command: "setpriv",
		args: []string{fmt.Sprintf("--reuid=%d", uid), fmt.Sprintf("--regid=%d", gid), "--clear-groups",
			"postgres", "-D", dataDir, "-p", "5432", "-c", "listen_addresses=localhost"},
		ready: func(ctx context.Context) bool { return tcpReady(ctx, "localhost:5432") },
		postReady: func(ctx context.Context) error {
			// Create the orchicon database on first boot (idempotent).
			// createdb exits 0 when created and errors on duplicate;
			// "already exists" is fine.
			cmd := exec.CommandContext(ctx, "createdb", "-U", "orchicon", "-h", "localhost", "-p", "5432", "orchicon")
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
	return &managedProc{
		name:    "nats",
		command: "nats-server",
		args:    []string{"-js", "-sd", filepath.Join(s.dataDir, "nats"), "-m", "8222"},
		ready:   httpReady("http://localhost:8222/healthz"),
		restart: true,
	}
}

func (s *supervisor) tempoProc() *managedProc {
	return &managedProc{
		name:    "tempo",
		command: "tempo",
		args:    []string{"-config.file=" + filepath.Join(s.configDir, "tempo.yaml")},
		ready:   httpReady("http://localhost:3200/ready"),
		restart: true,
	}
}

func (s *supervisor) lokiProc() *managedProc {
	return &managedProc{
		name:    "loki",
		command: "loki",
		args:    []string{"-config.file=" + filepath.Join(s.configDir, "loki.yaml")},
		ready:   httpReady("http://localhost:3100/ready"),
		restart: true,
	}
}

func (s *supervisor) vmProc() *managedProc {
	return &managedProc{
		name:    "victoriametrics",
		command: "victoria-metrics-prod",
		args: []string{"-storageDataPath=" + filepath.Join(s.dataDir, "victoriametrics"),
			"-retentionPeriod=720h", "-httpListenAddr=:8428"},
		ready:   httpReady("http://localhost:8428/health"),
		restart: true,
	}
}

func (s *supervisor) collectorProc() *managedProc {
	return &managedProc{
		name:    "otel-collector",
		command: "otelcol-contrib",
		args:    []string{"--config=" + filepath.Join(s.configDir, "otel-collector.yaml")},
		ready:   func(ctx context.Context) bool { return tcpReady(ctx, "localhost:13133") },
		restart: true,
	}
}

func (s *supervisor) grafanaProc() *managedProc {
	rootURL := env("ORCHICON_GRAFANA_PUBLIC_URL", "http://localhost:8080/grafana")
	return &managedProc{
		name:    "grafana",
		command: "/usr/share/grafana/bin/grafana",
		args:    []string{"server", "--homepath=/usr/share/grafana", "--config=" + filepath.Join(s.configDir, "grafana.ini")},
		env: []string{
			"GF_SERVER_ROOT_URL=" + rootURL,
			"GF_SERVER_SERVE_FROM_SUB_PATH=true",
			"GF_SECURITY_ALLOW_EMBEDDING=true",
			"GF_AUTH_ANONYMOUS_ENABLED=true",
			"GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer",
			"GF_AUTH_BASIC_ENABLED=false",
			"GF_AUTH_DISABLE_LOGIN_FORM=true",
		},
		ready:   httpReady("http://localhost:3000/api/health"),
		restart: true,
	}
}

// planeProc re-executes this binary as `orchicon serve`, with the
// container-local defaults applied for any env var the user did not set.
func (s *supervisor) planeProc() *managedProc {
	self, err := os.Executable()
	if err != nil {
		self = "orchicon"
	}
	return &managedProc{
		name:    "control-plane",
		command: self,
		args:    []string{"serve"},
		env:     containerChildEnv(),
		ready:   httpReady("http://localhost:8080/healthz"),
		restart: true,
	}
}

// containerChildEnv returns the container-local defaults for the control
// plane child, skipping any variable the user already set explicitly.
func containerChildEnv() []string {
	defaults := map[string]string{
		containerModeEnv:       "1",
		"ORCHICON_HTTP_ADDR":   ":8080",
		"ORCHICON_POSTGRES_DSN": "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable",
		"ORCHICON_NATS_URL":    "nats://localhost:4222",
		"ORCHICON_OTEL_ENDPOINT": "localhost:4317",
		"ORCHICON_GRAFANA_URL": "http://localhost:3000",
		"ORCHICON_TEMPO_URL":   "http://localhost:3200",
		"ORCHICON_LOKI_URL":    "http://localhost:3100",
		"ORCHICON_VM_URL":      "http://localhost:8428",
	}
	var out []string
	for k, v := range defaults {
		if _, ok := os.LookupEnv(k); !ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
