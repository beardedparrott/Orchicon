package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/guard"
)

// AgentRequest is a single dispatch from the daemon to the in-container
// supervisor. It travels as one JSON document over the supervisor's unix
// socket (written by `orchicon runtime-client`, which the daemon reaches
// via `docker exec`). The only commands left after the one-shot exec
// transport was removed are "ping" (readiness) and "serve" (the container's
// opencode serve handshake).
type AgentRequest struct {
	Cmd        string   `json:"cmd"` // "ping" | "serve"
	Argv       []string `json:"argv,omitempty"`
	Env        []string `json:"env,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	ProjectDir string   `json:"project_dir,omitempty"`
}

// AgentEvent is one JSON-lines record the supervisor or the image-build
// path streams back: the {event:"serve", port, password} handshake answer,
// an {event:"error"} for a failed bring-up, {pong:true} for a ping, or a
// {stream,data} chunk of `docker build` output (Runtime Image Deploy).
type AgentEvent struct {
	Stream   string `json:"stream,omitempty"` // "stdout" | "stderr" (docker build relay)
	Data     string `json:"data,omitempty"`
	Event    string `json:"event,omitempty"` // "error" | "exit" | "serve"
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Pong     bool   `json:"pong,omitempty"`
	Port     int    `json:"port,omitempty"`     // serve cmd: the container-internal serve port
	Password string `json:"password,omitempty"` // serve cmd: the container's serve password
	// PlaneEnabled reports (serve handshake) whether this image boots the
	// sandbox plane (postgres + nats-server present). The daemon uses it
	// to publish the plane's /healthz URL so the run-start gate can verify
	// the sandbox plane before dispatching. Base/gui images answer false.
	PlaneEnabled bool `json:"plane_enabled,omitempty"`
}

// DefaultAgentSocket is the in-container path of the supervisor's unix
// socket. It lives under /tmp (tmpfs) so it is ephemeral like everything
// else in the runtime container.
const DefaultAgentSocket = "/tmp/orchicon-agent.sock"

// Sandbox plane — a disposable, in-container Orchicon control plane that
// the supervisor boots at container start on images that bake the pieces
// (currently :orchicon-dev: PostgreSQL + nats-server + the mounted orchicon
// binary). Postgres -> NATS -> `orchicon serve` on container-local ports
// gives every worker a consistent full environment — curl
// http://localhost:8080/healthz, DB-backed tests against localhost:5432,
// and the `orchicon_*` MCP tools against the sandbox DB — instead of each
// worker booting Postgres/the app itself ad-hoc. The plane dies with the
// container (pool reset recreates pristine), preserving the no-DB-route
// sandbox invariant: it never reaches the host plane's Postgres.
const (
	// SandboxPostgresDSN is the in-container DSN of the sandbox plane's
	// Postgres (trust auth, user orchicon). Exported for the opencode
	// package, which injects it into the sandbox Orchicon MCP sidecar.
	SandboxPostgresDSN = "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"

	// sandboxDataDir is the stable per-container sandbox state root (the
	// rootfs is chowned to the runtime uid, so the non-root user owns it).
	// Stable across supervisor/plane restarts — the watchdog reuses the
	// Postgres cluster instead of re-initdb'ing — and wiped when the
	// container dies (pool reset recreates pristine).
	sandboxDataDir     = "/var/lib/orchicon-sandbox"
	sandboxPgDataDir   = "/var/lib/orchicon-sandbox/postgres"
	sandboxNATSDataDir = "/var/lib/orchicon-sandbox/nats"
	sandboxPgLog       = "/var/lib/orchicon-sandbox/postgres.log"

	// Container-local ports the sandbox plane binds (nothing else in the
	// runtime container uses them).
	sandboxPgPort    = 5432
	sandboxNATSPort  = 4222
	sandboxPlanePort = 8080

	// Reserved exec ids for the sandbox plane children (the opencode serve
	// keeps serveExecID).
	sandboxNATSExecID  = "__orchicon_sandbox_nats__"
	sandboxPlaneExecID = "__orchicon_sandbox_plane__"

	// sandboxWatchInterval is how often the sandbox-plane watchdog polls
	// the plane's /healthz.
	sandboxWatchInterval = 15 * time.Second
)

// IsDevImageTag reports whether a runtime image tag is a dev variant — the
// stock variant that bakes the sandbox-plane pieces (postgres + nats-server),
// so the sandbox plane boots there and the container serve should register
// the Orchicon MCP against the sandbox DB. Mirrors the dev-tag suffix
// resolution in internal/runtimeimage (resolveVariant).
func IsDevImageTag(tag string) bool {
	lower := strings.ToLower(tag)
	colon := strings.LastIndex(lower, ":")
	suffix := lower
	if colon >= 0 {
		suffix = lower[colon+1:]
	}
	return strings.HasSuffix(suffix, "orchicon-dev") ||
		strings.HasSuffix(suffix, "dev") ||
		strings.HasPrefix(suffix, "dev-") ||
		strings.HasPrefix(suffix, "dev_")
}

// runtimeBinAllowlist is the set of binaries the runtime supervisor may
// exec (argv[0] basenames). It mirrors the adapter CLIs Orchicon drives:
// opencode today, with Claude Code (`claude`) and Codex (`codex`) to be
// added here when those adapters land. Orchicon never ships any of these
// in the image — the daemon mounts the operator's host installs into the
// container at runtime (see daemon.go standard mounts).
var runtimeBinAllowlist = map[string]bool{
	"opencode": true,
	"orchicon": true,
	"bash":     true,
	"sh":       true,
}

// RunSupervisor runs the in-container dispatch loop as PID 1. It accepts
// ping/serve requests on socketPath and runs each as a child
// process, streaming stdout/stderr and tracking children by exec_id so a
// later signal request can target one.
func RunSupervisor(socketPath string, log *slog.Logger) error {
	if socketPath == "" {
		socketPath = DefaultAgentSocket
	}
	_ = os.RemoveAll(socketPath)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return fmt.Errorf("supervisor: mkdir: %w", err)
	}
	// Pre-create the worker scratch directory (matching the opencode
	// external_directory carve-out in internal/opencode/config.go) so it
	// exists as the runtime user's own dir before any worker runs. /tmp is
	// a tmpfs here, so this is ephemeral — wiped with the container.
	if err := os.MkdirAll("/tmp/orchicon", 0o755); err != nil {
		return fmt.Errorf("supervisor: mkdir scratch: %w", err)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("supervisor: listen %s: %w", socketPath, err)
	}
	defer l.Close()
	log.Info("runtime supervisor listening", "socket", socketPath)

	// Reap orphaned children on exit so a container kill never leaves a
	// stray opencode running inside.
	handle := newChildRegistry(log)
	defer handle.killAll(syscall.SIGKILL)
	defer handle.stopSandboxPlane()

	// Serve watchdog: poll the container's opencode serve and restart it
	// when it stops answering /global/health (wedged OR exited). A serve
	// that wedges is otherwise invisible — watchExec only fires on process
	// exit — and the daemon's idempotent handshake would keep reporting it
	// as up while every dispatch burned its 30s readiness probe. The
	// watchdog restarts the serve in place (same port + password, stable
	// XDG data dir), so sessions survive and the SSE client re-attaches.
	go handle.watchServe()

	// Sandbox plane: boot the disposable in-container Orchicon control
	// plane (Postgres -> NATS -> `orchicon serve`) on images that bake the
	// pieces, so workers get a consistent full environment without booting
	// Postgres/the app themselves ad-hoc. Self-gating on binary presence —
	// base/gui images (no postgres/nats-server) skip entirely and behave
	// exactly as today. Both run in the background so the socket accept
	// loop (ping / serve handshakes) stays responsive during the ~2s boot.
	go handle.bootSandboxPlane()
	go handle.watchSandboxPlane()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Warn("supervisor: accept", "error", err)
			continue
		}
		go handle.serve(conn)
	}
}

// execSession is one running child. After the one-shot exec transport was
// removed, the only child the supervisor tracks is the container's opencode
// serve (a DETACHED process: no attached clients, no reconnect grace — it
// lives until explicitly killed or the container tears down). It keeps the
// terminal state so a later signal/kill can target it and so the serve
// watchdog can restart a wedged process.
type execSession struct {
	id       string
	cmd      *exec.Cmd
	guardDir string
	detached bool

	mu       sync.Mutex
	exited   bool
	exitCode int
	exitErr  error
	done     chan struct{}
}

func newExecSession(id string) *execSession {
	return &execSession{
		id:   id,
		done: make(chan struct{}),
	}
}

// childRegistry tracks running children by exec_id so signals can target
// a specific execution (the TaskReconciler's wall-clock timeout path
// relies on this).
type childRegistry struct {
	mu  sync.Mutex
	log *slog.Logger
	cmd map[string]*execSession
	// serveMu serializes serve lifecycle operations (bring-up + watchdog
	// restart). A concurrent runServe handshake and watchServe restart must
	// never both spawn a serve on the same port — the second Start fails
	// ("address already in use") and would corrupt the registry.
	serveMu sync.Mutex
	// servePw is the container's opencode serve password, generated once
	// by the supervisor on first serve startup and reused for the
	// container's lifetime so idempotent serve handshakes return a stable
	// credential.
	servePw string
	// serveReq is the AgentRequest the serve was last started with. The
	// watchdog reuses it to restart the serve (same argv/env/cwd) after a
	// wedge. Guarded by mu.
	serveReq AgentRequest
	// serveStarted marks that the serve has been brought up at least once,
	// so the watchdog only acts on a serve the plane actually requested.
	// Guarded by mu.
	serveStarted bool
	// sandboxChecked/sandboxOK cache the sandbox-plane self-gate (binary
	// presence in the image — the image's contents never change while the
	// container runs). Guarded by mu.
	sandboxChecked bool
	sandboxOK      bool
	// sandboxMu serializes sandbox-plane bring-up (boot + watchdog
	// restart), mirroring serveMu for the opencode serve.
	sandboxMu sync.Mutex
}

func newChildRegistry(log *slog.Logger) *childRegistry {
	return &childRegistry{log: log, cmd: make(map[string]*execSession)}
}

func (h *childRegistry) serve(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	var req AgentRequest
	if err := dec.Decode(&req); err != nil {
		h.log.Warn("supervisor: decode request", "error", err)
		return
	}
	enc := json.NewEncoder(conn)
	switch req.Cmd {
	case "ping":
		_ = enc.Encode(AgentEvent{Pong: true})
	case "serve":
		h.runServe(enc, req)
	default:
		_ = enc.Encode(AgentEvent{Event: "error", Error: "unknown cmd: " + req.Cmd})
	}
}

// serveExecID is the reserved exec id for the container's opencode serve.
const serveExecID = "__orchicon_serve__"

// defaultServePort is the container-internal port the serve binds (and
// the daemon publishes to a random host loopback port).
const defaultServePort = 4096

// serveDataDir is the stable per-container XDG_DATA_HOME for the serve.
// It is deliberately NOT a fresh MkdirTemp per serve start (that is what
// isolateOpenCodeData does for one-shot execs): a stable dir means a
// watchdog restart of the serve preserves the container's sessions, and
// the SSE client re-attaches by session id (same contract as the host
// serve, which keeps its data dir across restarts). /tmp is a tmpfs in
// runtime containers, so this is still ephemeral — wiped with the
// container, never reaching the host's opencode data.
const serveDataDir = "/tmp/orchicon-serve-data"

// runServe starts the container's opencode serve as a DETACHED child and
// answers with the port once it is healthy. The serve owns the agent
// loops for every session in this container (one per execution); the
// plane reaches it through the daemon-published loopback port. The serve
// is registered under a reserved exec id so signals and container teardown
// can target it; it has no clients and no reconnect-grace kill.
//
// argv defaults to `opencode serve --hostname 127.0.0.1 --port 4096` (the
// port the daemon publishes); a request may override the argv.
func (h *childRegistry) runServe(enc *json.Encoder, req AgentRequest) {
	// Serialize serve lifecycle against the watchdog: a concurrent
	// runServe handshake and watchServe restart must never both spawn a
	// serve on the same port.
	h.serveMu.Lock()
	defer h.serveMu.Unlock()

	h.mu.Lock()
	if existing, ok := h.cmd[serveExecID]; ok {
		pw := h.servePw
		h.mu.Unlock()
		// Serve already registered. Liveness-gate the idempotent path: a
		// WEDGED serve (process alive but not answering health) must NOT be
		// reported as up — that was the failure mode where every dispatch
		// retry burned its 30s probe against a dead serve and then degraded.
		// If the registered serve no longer answers, kill it, let it be
		// removed from the registry, and start a fresh one below.
		if pw != "" && serveHealthy(defaultServePort, pw) {
			_ = existing
			_ = enc.Encode(AgentEvent{Event: "serve", Port: defaultServePort, Password: pw, PlaneEnabled: h.sandboxAvailable()})
			return
		}
		h.log.Warn("serve registered but not healthy — restarting", "pid", existing.pidOrZero())
		existing.kill()
		// Fall through: h.cmd[serveExecID] is removed by watchExec once the
		// killed process exits, so the fresh-start path below can register a
		// new session without racing the stale entry. To make that immediate
		// (not waiting on the reap), remove it now.
		h.mu.Lock()
		delete(h.cmd, serveExecID)
		h.mu.Unlock()
	} else {
		h.mu.Unlock()
	}

	pw := h.servePw
	if pw == "" {
		pw = randomServePassword()
		h.servePw = pw
	}

	if len(req.Argv) == 0 {
		req.Argv = []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprintf("%d", defaultServePort)}
	}
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "argv[0] not allowlisted: " + base})
		return
	}

	s := newExecSession(serveExecID)
	s.detached = true

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := agentEnv(req)
	env = setEnv(env, "OPENCODE_SERVER_PASSWORD", pw)
	// The serve uses a STABLE XDG data dir (not a fresh temp dir) so a
	// watchdog restart preserves sessions across the restart.
	env = isolateOpenCodeDataInto(env, serveDataDir)
	// The serve runs the agent's tools, so the same safety guard applies:
	// destructive commands are refused even from worker subprocesses.
	guardDir, guardErr := guard.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil {
		h.log.Warn("supervisor: guard not applied to serve", "error", guardErr)
	} else {
		env = prependGuard(env, guardDir)
		s.guardDir = guardDir
	}
	cmd.Env = env
	cmd.Stdout = os.Stderr // serve logs go to the supervisor log, not telemetry
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		if s.guardDir != "" {
			os.RemoveAll(s.guardDir)
		}
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}

	h.mu.Lock()
	h.cmd[serveExecID] = s
	h.serveReq = req
	h.serveStarted = true
	h.mu.Unlock()
	s.cmd = cmd
	go h.watchExec(s)

	// Wait for the serve to answer /global/health with the password, so
	// the plane never races a half-initialized serve.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveHealthy(defaultServePort, pw) {
			h.log.Info("runtime opencode serve ready", "port", defaultServePort, "pid", cmd.Process.Pid)
			_ = enc.Encode(AgentEvent{Event: "serve", Port: defaultServePort, Password: pw, PlaneEnabled: h.sandboxAvailable()})
			return
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-s.done:
			_ = enc.Encode(AgentEvent{Event: "error", Error: "serve exited before becoming ready"})
			return
		}
	}
	_ = cmd.Process.Kill()
	_ = enc.Encode(AgentEvent{Event: "error", Error: "serve did not become ready within 30s"})
}

// pidOrZero returns the child's process id (0 when not started).
func (s *execSession) pidOrZero() int {
	if s != nil && s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// watchServe is the serve health watchdog. It polls the container's
// opencode serve every few seconds and, when it stops answering
// /global/health (wedged — alive but hung — or exited), restarts it in
// place: same port, same password, same stable XDG data dir, so sessions
// survive and the SSE client re-attaches by id (the same contract as the
// host serve's Watch loop). A wedged serve is otherwise invisible to
// watchExec (which only fires on process exit) and would make every
// dispatch burn its 30s readiness probe then fail.
func (h *childRegistry) watchServe() {
	backoff := serveWatchInterval
	for {
		time.Sleep(serveWatchInterval)
		h.mu.Lock()
		s, ok := h.cmd[serveExecID]
		req := h.serveReq
		started := h.serveStarted
		pw := h.servePw
		h.mu.Unlock()
		if !ok || !started || s == nil || s.cmd == nil || s.cmd.Process == nil {
			backoff = serveWatchInterval
			continue
		}
		if serveHealthy(defaultServePort, pw) {
			backoff = serveWatchInterval
			continue
		}
		// Unhealthy. Restart with backoff: kill the wedged process, let
		// watchExec unregister it, then bring the serve back up with the
		// same request. If the serve has genuinely gone away (crashed), a
		// fresh one takes its place.
		h.log.Warn("serve unhealthy — restarting", "pid", s.pidOrZero(), "backoff", backoff.String())
		time.Sleep(backoff)
		h.serveMu.Lock()
		// Re-check under serveMu: a runServe handshake may have already
		// replaced the serve while we slept. Only restart if the registered
		// session is still this (wedged) one and still unhealthy.
		h.mu.Lock()
		cur, ok := h.cmd[serveExecID]
		if !ok || cur != s {
			h.mu.Unlock()
			h.serveMu.Unlock()
			backoff = serveWatchInterval
			continue
		}
		if serveHealthy(defaultServePort, h.servePw) {
			h.mu.Unlock()
			h.serveMu.Unlock()
			backoff = serveWatchInterval
			continue
		}
		s.kill()
		delete(h.cmd, serveExecID)
		req = h.serveReq
		h.mu.Unlock()
		if err := h.startServeAgain(req); err != nil {
			h.serveMu.Unlock()
			h.log.Error("serve restart failed", "error", err)
			if backoff < serveWatchMaxBackoff {
				backoff *= 2
			}
			continue
		}
		h.serveMu.Unlock()
		backoff = serveWatchInterval
		h.log.Info("serve restarted by watchdog", "port", defaultServePort)
	}
}

// startServeAgain restarts the container's opencode serve after a wedge,
// using the stored AgentRequest. It shares the fresh-start path of
// runServe but encodes nothing (the requesting plane's conn is long
// gone) — the daemon's next Create handshake converges to it.
func (h *childRegistry) startServeAgain(req AgentRequest) error {
	if len(req.Argv) == 0 {
		req.Argv = []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprintf("%d", defaultServePort)}
	}
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		return fmt.Errorf("argv[0] not allowlisted: %s", base)
	}

	h.mu.Lock()
	pw := h.servePw
	if pw == "" {
		pw = randomServePassword()
		h.servePw = pw
	}
	h.mu.Unlock()

	s := newExecSession(serveExecID)
	s.detached = true

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := agentEnv(req)
	env = setEnv(env, "OPENCODE_SERVER_PASSWORD", pw)
	env = isolateOpenCodeDataInto(env, serveDataDir)
	guardDir, guardErr := guard.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil {
		h.log.Warn("supervisor: guard not applied to serve restart", "error", guardErr)
	} else {
		env = prependGuard(env, guardDir)
		s.guardDir = guardDir
	}
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		if s.guardDir != "" {
			os.RemoveAll(s.guardDir)
		}
		return err
	}

	h.mu.Lock()
	h.cmd[serveExecID] = s
	h.serveReq = req
	h.mu.Unlock()
	s.cmd = cmd
	go h.watchExec(s)

	// Wait for the restarted serve to answer health so the next dispatch
	// converges to a usable serve rather than racing a half-initialized one.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveHealthy(defaultServePort, pw) {
			return nil
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-s.done:
			return fmt.Errorf("serve restart exited before becoming ready")
		}
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("serve restart did not become ready within 30s")
}

// serveWatchInterval and serveWatchMaxBackoff tune the serve watchdog.
const (
	serveWatchInterval    = 10 * time.Second
	serveWatchMaxBackoff  = 60 * time.Second
)

// randomServePassword returns a hex password for the container's serve.
func randomServePassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "orchicon-serve"
	}
	return hex.EncodeToString(b)
}

// serveHealthy reports whether the in-container serve answers
// /global/health on the given port with basic auth.
func serveHealthy(port int, password string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/global/health", port), nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth("opencode", password)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// sandboxAvailable reports whether this image can boot the sandbox plane
// (a Postgres server bin dir + nats-server on PATH). Base/gui images
// answer false and behave exactly as today — no plane, no extra children.
// The probe is cheap (a few stat calls) and cached once per supervisor
// lifetime: the image's contents never change while the container runs.
func (h *childRegistry) sandboxAvailable() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sandboxChecked {
		_, natsErr := exec.LookPath("nats-server")
		h.sandboxOK = sandboxPgBinDir() != "" && natsErr == nil
		h.sandboxChecked = true
	}
	return h.sandboxOK
}

// sandboxPgBinDir resolves the Postgres server bin dir (initdb/pg_ctl/
// pg_isready/postgres). Postgres 15 on Debian installs these under
// /usr/lib/postgresql/<ver>/bin, not on PATH. Returns "" when absent
// (base/gui images — the sandbox plane is skipped entirely).
func sandboxPgBinDir() string {
	dirs, err := filepath.Glob("/usr/lib/postgresql/*/bin")
	if err != nil {
		return ""
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, dir := range dirs {
		if sandboxPgBinDirComplete(dir) {
			return dir
		}
	}
	return ""
}

// sandboxPgBinDirComplete reports whether a Postgres bin dir contains ALL
// four server binaries the sandbox plane execs (initdb, pg_ctl,
// pg_isready, postgres). A partial dir must NOT satisfy the self-gate:
// bootSandboxPlane execs each of these, so a dir with only some of them
// would pass sandboxAvailable() and then fail at boot.
func sandboxPgBinDirComplete(dir string) bool {
	for _, bin := range []string{"initdb", "pg_ctl", "pg_isready", "postgres"} {
		if _, err := os.Stat(filepath.Join(dir, bin)); err != nil {
			return false
		}
	}
	return true
}

// bootSandboxPlane brings up the sandbox plane in dependency order —
// Postgres (initdb once into the stable data dir, then pg_ctl start) ->
// nats-server -js (tracked child) -> `orchicon serve` (tracked child,
// sandbox env). Idempotent and restart-safe: a watchdog re-runs it after
// a failure, and each component's "already up" probe short-circuits, so a
// restart reuses the stable Postgres cluster instead of re-initializing.
func (h *childRegistry) bootSandboxPlane() {
	h.sandboxMu.Lock()
	defer h.sandboxMu.Unlock()
	if !h.sandboxAvailable() {
		h.log.Info("sandbox plane skipped: postgres/nats-server not present in image")
		return
	}
	if err := os.MkdirAll(sandboxDataDir, 0o755); err != nil {
		h.log.Error("sandbox plane: mkdir data dir", "error", err)
		return
	}
	if err := h.bootSandboxPostgres(); err != nil {
		h.log.Error("sandbox plane: postgres failed", "error", err)
		return
	}
	if err := h.bootSandboxNATS(); err != nil {
		h.log.Error("sandbox plane: nats failed", "error", err)
		return
	}
	if err := h.bootSandboxServe(); err != nil {
		h.log.Error("sandbox plane: serve failed", "error", err)
		return
	}
	h.log.Info("sandbox plane ready", "http", fmt.Sprintf("http://localhost:%d", sandboxPlanePort))
}

// bootSandboxPostgres initializes (once) and starts the sandbox Postgres,
// then ensures the `orchicon` database exists for the plane's migrations.
func (h *childRegistry) bootSandboxPostgres() error {
	pgb := sandboxPgBinDir()
	if pgb == "" {
		return errors.New("postgres bin dir not found")
	}
	if _, err := os.Stat(filepath.Join(sandboxPgDataDir, "PG_VERSION")); os.IsNotExist(err) {
		h.log.Info("sandbox plane: initdb", "dir", sandboxPgDataDir)
		if out, err := exec.Command(filepath.Join(pgb, "initdb"),
			"-D", sandboxPgDataDir, "-U", "orchicon", "--auth=trust").CombinedOutput(); err != nil {
			return fmt.Errorf("initdb: %v: %s", err, out)
		}
	}
	// pg_ctl -w start blocks until the server is up; it errors when the
	// server is already running (a previous boot / watchdog restart) — the
	// readiness probe below decides either way. unix_socket_directories
	// must point at a dir the runtime user owns: /var/run/postgresql (the
	// default) is root-owned in the container (docker's /run tmpfs), and
	// postgres refuses to start without a writable socket dir.
	opts := fmt.Sprintf("-p %d -c listen_addresses=localhost -c unix_socket_directories=%s", sandboxPgPort, sandboxDataDir)
	if out, err := exec.Command(filepath.Join(pgb, "pg_ctl"),
		"-D", sandboxPgDataDir, "-l", sandboxPgLog, "-o", opts, "-w", "start").CombinedOutput(); err != nil {
		h.log.Warn("sandbox plane: pg_ctl start (may already be running)", "output", string(out), "error", err)
	}
	if !h.waitPgReady(pgb, 30*time.Second) {
		return errors.New("postgres did not become ready")
	}
	// initdb creates only postgres/template DBs; the plane's migrations
	// target the `orchicon` database (the DSN's db name).
	cmd := exec.Command(filepath.Join(pgb, "createdb"),
		"-h", "localhost", "-p", fmt.Sprintf("%d", sandboxPgPort), "-U", "orchicon", "orchicon")
	if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("createdb orchicon: %v: %s", err, out)
	}
	h.log.Info("sandbox plane: postgres ready", "port", sandboxPgPort)
	return nil
}

// waitPgReady polls pg_isready until the sandbox Postgres accepts
// connections or the window expires.
func (h *childRegistry) waitPgReady(pgb string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command(filepath.Join(pgb, "pg_isready"),
			"-h", "localhost", "-p", fmt.Sprintf("%d", sandboxPgPort), "-U", "orchicon").CombinedOutput()
		if err == nil && strings.Contains(string(out), "accepting connections") {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// bootSandboxNATS starts the sandbox nats-server (JetStream) as a tracked
// child and waits for it to accept connections.
func (h *childRegistry) bootSandboxNATS() error {
	if natsReady() {
		return nil
	}
	h.killAndClear(sandboxNATSExecID)
	if err := os.MkdirAll(sandboxNATSDataDir, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("nats-server", "-js", "-p", fmt.Sprintf("%d", sandboxNATSPort), "-sd", sandboxNATSDataDir)
	cmd.Stdout = os.Stderr // sandbox logs go to the supervisor log
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("nats-server start: %w", err)
	}
	s := newExecSession(sandboxNATSExecID)
	s.cmd = cmd
	h.mu.Lock()
	h.cmd[sandboxNATSExecID] = s
	h.mu.Unlock()
	go h.watchExec(s)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if natsReady() {
			h.log.Info("sandbox plane: nats ready", "port", sandboxNATSPort)
			return nil
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-s.done:
			return errors.New("nats-server exited before becoming ready")
		}
	}
	return errors.New("nats-server did not become ready within 15s")
}

// natsReady reports whether the sandbox nats-server accepts connections.
func natsReady() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", sandboxNATSPort), 1*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// bootSandboxServe starts the sandbox `orchicon serve` (the sandbox
// control plane) as a tracked child and waits for its /healthz. The plane
// runs migrations + tenant/worker seeding on boot against the sandbox DB.
// Sessions are OFF (ORCHICON_OPCODE_SESSION_TRANSPORT=0): the sandbox is
// an API/DB/MCP surface, not a second execution plane, so it never spawns
// a nested opencode serve (whose eager MCP connects would hang) and never
// competes with the supervisor's opencode serve.
func (h *childRegistry) bootSandboxServe() error {
	if sandboxPlaneHealthy() {
		return nil
	}
	h.killAndClear(sandboxPlaneExecID)
	cmd := exec.Command("orchicon", "serve")
	cmd.Stdout = os.Stderr // sandbox plane logs go to the supervisor log
	cmd.Stderr = os.Stderr
	env := os.Environ()
	env = setEnv(env, "ORCHICON_POSTGRES_DSN", SandboxPostgresDSN)
	env = setEnv(env, "ORCHICON_NATS_URL", fmt.Sprintf("nats://localhost:%d", sandboxNATSPort))
	env = setEnv(env, "ORCHICON_HTTP_ADDR", fmt.Sprintf(":%d", sandboxPlanePort))
	env = setEnv(env, "ORCHICON_GRPC_ADDR", ":9090")
	env = setEnv(env, "ORCHICON_TELEMETRY", "none")
	env = setEnv(env, "ORCHICON_OPCODE_SESSION_TRANSPORT", "0")
	env = setEnv(env, "ORCHICON_SANDBOX_PLANE", "1")
	env = setEnv(env, "ORCHICON_INSTANCE", "sandbox")
	env = setEnv(env, "ORCHICON_BLOB_DIR", filepath.Join(sandboxDataDir, "blobs"))
	env = setEnv(env, "ORCHICON_INDEX_CHECK_INTERVAL", "0")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sandbox serve start: %w", err)
	}
	s := newExecSession(sandboxPlaneExecID)
	s.cmd = cmd
	h.mu.Lock()
	h.cmd[sandboxPlaneExecID] = s
	h.mu.Unlock()
	go h.watchExec(s)
	// The plane runs migrations + seeds on boot against a fresh DB; give
	// it the full window (the run-start gate's own 120s window bounds the
	// total wait either way).
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if sandboxPlaneHealthy() {
			h.log.Info("sandbox plane: serve ready", "port", sandboxPlanePort)
			return nil
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-s.done:
			return fmt.Errorf("sandbox serve exited before becoming ready")
		}
	}
	return errors.New("sandbox serve did not become ready within 90s")
}

// sandboxPlaneHealthy reports whether the sandbox control plane answers
// /healthz on the container-local port.
func sandboxPlaneHealthy() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/healthz", sandboxPlanePort))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// watchSandboxPlane is the sandbox-plane health watchdog. It polls the
// plane's /healthz and, when it stops answering (wedged or exited —
// invisible to watchExec's process-exit-only watch), re-boots the stack
// via the idempotent bootSandboxPlane: Postgres is restarted in place
// (pg_ctl reuses the stable cluster — no re-initdb), NATS re-leased, the
// serve re-spawned. A dead plane would otherwise fail the run-start gate
// with no recovery path.
func (h *childRegistry) watchSandboxPlane() {
	interval := sandboxWatchInterval
	for {
		time.Sleep(interval)
		if !h.sandboxAvailable() {
			return // base/gui image — nothing to watch
		}
		if sandboxPlaneHealthy() {
			interval = sandboxWatchInterval
			continue
		}
		h.log.Warn("sandbox plane unhealthy — restarting", "backoff", interval.String())
		time.Sleep(interval)
		h.bootSandboxPlane()
		interval = sandboxWatchInterval
	}
}

// killAndClear terminates a tracked child (if any) and removes it from
// the registry, waiting briefly for the reap so a restart doesn't race
// "address already in use" on the next Start.
func (h *childRegistry) killAndClear(id string) {
	h.mu.Lock()
	s, ok := h.cmd[id]
	h.mu.Unlock()
	if ok && s != nil {
		s.kill()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
	}
	h.mu.Lock()
	delete(h.cmd, id)
	h.mu.Unlock()
}

// stopSandboxPlane is a best-effort teardown of the sandbox plane on
// supervisor exit. The tracked children (nats + serve) are covered by
// killAll; this stops the pg_ctl-detached Postgres. Container teardown is
// the real guarantee (pool resets `docker rm -f`) — this just avoids a
// stray postgres lingering if the supervisor exits while the container
// lives.
func (h *childRegistry) stopSandboxPlane() {
	if !h.sandboxAvailable() {
		return
	}
	pgb := sandboxPgBinDir()
	if pgb == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, filepath.Join(pgb, "pg_ctl"),
		"-D", sandboxPgDataDir, "-m", "fast", "stop").Run()
}

// watchExec waits for the child to exit, records the terminal state, and
// closes the session. The serve has no attached clients (it is detached),
// so there is nothing to broadcast to — the terminal state is what a
// later liveness-gated runServe / the watchdog uses to decide a restart.
func (h *childRegistry) watchExec(s *execSession) {
	waitErr := s.cmd.Wait()
	code := 1
	if s.cmd.ProcessState != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	s.mu.Lock()
	s.exited = true
	s.exitCode = code
	if waitErr != nil && !errors.Is(waitErr, io.EOF) {
		s.exitErr = waitErr
	}
	s.mu.Unlock()
	close(s.done)
	// Only unregister if this session is STILL the registered one. A wedged
	// serve that was killed and replaced by the watchdog / a liveness-gated
	// runServe has a new session under the same serveExecID — its stale
	// watchExec must not remove the replacement from the registry.
	h.mu.Lock()
	if cur, ok := h.cmd[s.id]; ok && cur == s {
		delete(h.cmd, s.id)
	}
	h.mu.Unlock()
	h.cleanupSession(s)
}

func (h *childRegistry) cleanupSession(s *execSession) {
	if s.guardDir != "" {
		os.RemoveAll(s.guardDir)
	}
}

// kill terminates the child process (SIGKILL) without waiting. Used to
// tear down a wedged serve so a fresh one can take its place.
func (s *execSession) kill() {
	if s != nil && s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (h *childRegistry) killAll(sig syscall.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.cmd {
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(sig)
		}
	}
}

// agentEnv builds the child environment: the supervisor's environment
// plus the daemon-provided overrides (HOME, OPENCODE_CONFIG_CONTENT,
// OPENCODE_EXECUTION_ID, etc.).
func agentEnv(req AgentRequest) []string {
	out := os.Environ()
	for _, kv := range req.Env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		// Replace any inherited value of the same key.
		kept := out[:0]
		for _, cur := range out {
			ck, _, _ := strings.Cut(cur, "=")
			if ck != key {
				kept = append(kept, cur)
			}
		}
		out = append(kept, kv)
	}
	// Ensure the mounted adapter CLI bin (~/.opencode/bin) is on the
	// child's PATH — belt-and-suspenders to the daemon's container-level
	// PATH so workers and their subprocesses can also resolve opencode.
	if home := os.Getenv("HOME"); home != "" {
		bin := filepath.Join(home, ".opencode", "bin")
		if st, err := os.Stat(bin); err == nil && st.IsDir() {
			out = setEnv(out, "PATH", bin+string(os.PathListSeparator)+envPath(out))
		}
	}
	return out
}

func envPath(env []string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			return strings.TrimPrefix(kv, "PATH=")
		}
	}
	return ""
}

// isolateOpenCodeDataInto redirects opencode state into a caller-chosen
// directory (seeded with the model auth) and returns the env with
// XDG_DATA_HOME set. The serve uses a STABLE dir (serveDataDir) so a
// watchdog restart preserves sessions.
func isolateOpenCodeDataInto(env []string, xdg string) []string {
	if xdg == "" {
		return env
	}
	home := os.Getenv("HOME")
	src := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if b, err := os.ReadFile(src); err == nil {
		dir := filepath.Join(xdg, "opencode")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if werr := os.WriteFile(filepath.Join(dir, "auth.json"), b, 0o600); werr != nil {
				return env
			}
		}
	}
	return setEnv(env, "XDG_DATA_HOME", xdg)
}

// setEnv replaces an existing key=value in env (or appends it).
func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		ck, _, _ := strings.Cut(kv, "=")
		if ck == key {
			out = append(out, key+"="+value)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return out
}

func prependGuard(env []string, guardDir string) []string {	out := make([]string, 0, len(env)+1)
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = append(out, "PATH="+guardDir+string(os.PathListSeparator)+strings.TrimPrefix(kv, "PATH="))
			found = true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+guardDir)
	}
	return out
}

// RunClient connects to the supervisor socket, forwards one request (read
// from stdin) and relays the streamed events to stdout as JSON-lines. It
// exits with the child's exit code so the daemon can propagate it. It is
// invoked by the daemon via `docker exec`.
func RunClient(socketPath string, in io.Reader, out io.Writer) (int, error) {
	if socketPath == "" {
		socketPath = DefaultAgentSocket
	}
	var req AgentRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return 2, fmt.Errorf("runtime-client: read request: %w", err)
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return 2, fmt.Errorf("runtime-client: dial %s: %w", socketPath, err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return 2, fmt.Errorf("runtime-client: send request: %w", err)
	}
	// Signal the request boundary; events then stream back.
	outEnc := json.NewEncoder(out)
	dec := json.NewDecoder(conn)
	for {
		var ev AgentEvent
		if err := dec.Decode(&ev); err != nil {
			return 2, fmt.Errorf("runtime-client: read event: %w", err)
		}
		_ = outEnc.Encode(ev)
		if ev.Pong {
			return 0, nil
		}
		if ev.Event == "serve" {
			// The supervisor answered the serve request with the port;
			// relay the event and exit cleanly.
			return 0, nil
		}
		if ev.Event == "exit" {
			if ev.Error != "" && ev.ExitCode == 0 {
				ev.ExitCode = 1
			}
			return ev.ExitCode, nil
		}
		if ev.Event == "error" {
			return 1, nil
		}
	}
}
