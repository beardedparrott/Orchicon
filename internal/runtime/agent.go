package runtime

import (
	"bufio"
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/guard"
)

// AgentRequest is a single dispatch from the daemon to the in-container
// supervisor. It travels as one JSON document over the supervisor's unix
// socket (written by `orchicon runtime-client`, which the daemon reaches
// via `docker exec`).
type AgentRequest struct {
	Cmd        string   `json:"cmd"` // "exec" | "signal" | "ping"
	ExecID     string   `json:"exec_id,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	Env        []string `json:"env,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	ProjectDir string   `json:"project_dir,omitempty"`
	Signal     string   `json:"signal,omitempty"` // e.g. "SIGTERM"
	// ReconnectGraceSeconds is how long a disconnected exec session's child
	// is kept running (waiting for a re-attach) before being killed.
	ReconnectGraceSeconds int64 `json:"reconnect_grace_seconds,omitempty"`
}

// AgentEvent is one JSON-lines record streamed back for an exec: either a
// {stream,data} chunk of child output, or the final {event:"exit"} marker.
type AgentEvent struct {
	Stream   string `json:"stream,omitempty"` // "stdout" | "stderr"
	Data     string `json:"data,omitempty"`
	Event    string `json:"event,omitempty"` // "exit" | "error" | "status" | "serve"
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Alive    bool   `json:"alive,omitempty"`
	Pong     bool   `json:"pong,omitempty"`
	Port     int    `json:"port,omitempty"`     // serve cmd: the container-internal serve port
	Password string `json:"password,omitempty"` // serve cmd: the container's serve password
}

// DefaultAgentSocket is the in-container path of the supervisor's unix
// socket. It lives under /tmp (tmpfs) so it is ephemeral like everything
// else in the runtime container.
const DefaultAgentSocket = "/tmp/orchicon-agent.sock"

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

// defaultReconnectGrace is how long a disconnected exec session's child is
// kept running (waiting for the client to re-attach) before being killed.
// Overridable per-exec via AgentRequest.ReconnectGraceSeconds.
const defaultReconnectGrace = 60 * time.Second

// RunSupervisor runs the in-container dispatch loop as PID 1. It accepts
// exec/signal/ping requests on socketPath and runs each exec as a child
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

// execSession is one running child plus its attached clients. A session
// survives a client disconnect: the child keeps running for a reconnect
// grace, so a transient transport blip can re-attach instead of killing
// the execution. On grace expiry (no client re-attached — the plane is
// really gone) the child is killed. A DETACHED session (the opencode
// serve for the container) has no clients and no grace: it lives until
// explicitly signalled or the container tears down.
type execSession struct {
	id       string
	cmd      *exec.Cmd
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	guardDir string
	detached bool

	mu       sync.Mutex
	clients  map[net.Conn]*json.Encoder
	exited   bool
	exitCode int
	exitErr  error
	done     chan struct{}
	grace    *time.Timer
	graceDur time.Duration
	// writeMu serializes writes to the attached clients (two broadcast
	// goroutines + the exit broadcast) so a conn is never written
	// concurrently.
	writeMu sync.Mutex
}

func newExecSession(id string, graceDur time.Duration) *execSession {
	return &execSession{
		id:       id,
		clients:  make(map[net.Conn]*json.Encoder),
		done:     make(chan struct{}),
		graceDur: graceDur,
	}
}

// childRegistry tracks running children by exec_id so signals can target
// a specific execution (the TaskReconciler's wall-clock timeout path
// relies on this).
type childRegistry struct {
	mu  sync.Mutex
	log *slog.Logger
	cmd map[string]*execSession
	// servePw is the container's opencode serve password, generated once
	// by the supervisor on first serve startup and reused for the
	// container's lifetime so idempotent serve handshakes return a stable
	// credential.
	servePw string
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
	case "exec":
		h.runExec(conn, enc, req)
	case "serve":
		h.runServe(enc, req)
	case "signal":
		h.signal(enc, req)
	case "status":
		h.status(enc, req)
	default:
		_ = enc.Encode(AgentEvent{Event: "error", Error: "unknown cmd: " + req.Cmd})
	}
}

func (h *childRegistry) runExec(conn net.Conn, enc *json.Encoder, req AgentRequest) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "empty argv"})
		return
	}
	// Safety: the supervisor only ever runs a closed set of entry points
	// (adapter CLI runs and in-container tooling). The daemon already
	// enforces this, but belt-and-suspenders here too. The adapter CLIs
	// (opencode; claude/codex when their adapters land) are mounted in at
	// runtime by the daemon, never baked into the image.
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "argv[0] not allowlisted: " + base})
		return
	}

	// Re-attach: if the exec is already running (the client reconnected
	// after a transport blip), do NOT start a second child — attach this
	// connection to the existing session.
	h.mu.Lock()
	existing, ok := h.cmd[req.ExecID]
	h.mu.Unlock()
	if ok {
		existing.attach(conn, enc)
		return
	}

	graceDur := defaultReconnectGrace
	if req.ReconnectGraceSeconds > 0 {
		graceDur = time.Duration(req.ReconnectGraceSeconds) * time.Second
	}
	s := newExecSession(req.ExecID, graceDur)

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = agentEnv(req)
	cmd.Stdin = nil // non-interactive; opencode runs with --auto

	// OpenCode data isolation: give the worker an ephemeral XDG data dir
	// seeded with the model auth read from the READ-ONLY host mount. The
	// worker's opencode sessions/keys/telemetry then land in the
	// ephemeral filesystem (wiped at container teardown) instead of the
	// host's real ~/.local/share/opencode.
	cmd.Env = isolateOpenCodeData(cmd.Env)

	// Build the execution guard shim inside the container so every child
	// the worker spawns resolves rm/sudo/dd/etc. through the shim.
	guardDir, guardErr := guard.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil {
		h.log.Warn("supervisor: guard not applied", "error", guardErr, "exec", req.ExecID)
	} else {
		cmd.Env = prependGuard(cmd.Env, guardDir)
		s.guardDir = guardDir
		h.log.Debug("supervisor: guard applied", "dir", guardDir, "path", envPath(cmd.Env), "exec", req.ExecID)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	s.cmd, s.stdout, s.stderr = cmd, stdout, stderr

	if err := cmd.Start(); err != nil {
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}

	h.mu.Lock()
	// A concurrent re-attach may have registered the same exec_id first
	// (client retried while this create was in flight). Kill the duplicate
	// child and attach to the registered session.
	if dup, registered := h.cmd[req.ExecID]; registered {
		h.mu.Unlock()
		_ = cmd.Process.Kill()
		h.cleanupSession(s)
		dup.attach(conn, enc)
		return
	}
	h.cmd[req.ExecID] = s
	h.mu.Unlock()

	go h.watchExec(s)
	go s.broadcastStream("stdout", s.stdout)
	go s.broadcastStream("stderr", s.stderr)
	s.attach(conn, enc)
}

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
	const serveExecID = "__orchicon_serve__"
	h.mu.Lock()
	if existing, ok := h.cmd[serveExecID]; ok {
		// Serve already up: return the stable port + password. Idempotent
		// — the plane re-Creates the runtime on every dispatch, and the
		// container (created earlier by the WorkflowReconciler without a
		// serve) must converge to the same serve on the first dispatch.
		pw := h.servePw
		h.mu.Unlock()
		_ = existing
		_ = enc.Encode(AgentEvent{Event: "serve", Port: defaultServePort, Password: pw})
		return
	}
	pw := h.servePw
	if pw == "" {
		pw = randomServePassword()
		h.servePw = pw
	}
	h.mu.Unlock()

	if len(req.Argv) == 0 {
		req.Argv = []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprintf("%d", defaultServePort)}
	}
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "argv[0] not allowlisted: " + base})
		return
	}

	s := newExecSession(serveExecID, 0)
	s.detached = true

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := agentEnv(req)
	env = setEnv(env, "OPENCODE_SERVER_PASSWORD", pw)
	env = isolateOpenCodeData(env)
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
	h.mu.Unlock()
	s.cmd = cmd
	go h.watchExec(s)

	// Wait for the serve to answer /global/health with the password, so
	// the plane never races a half-initialized serve.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveHealthy(defaultServePort, pw) {
			h.log.Info("runtime opencode serve ready", "port", defaultServePort, "pid", cmd.Process.Pid)
			_ = enc.Encode(AgentEvent{Event: "serve", Port: defaultServePort, Password: pw})
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

// randomServePassword returns a hex password for the container's serve.
func randomServePassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "orchicon-serve"
	}
	return hex.EncodeToString(b)
}

// defaultServePort is the container-internal port the serve binds (and
// the daemon publishes to a random host loopback port).
const defaultServePort = 4096

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

// watchExec waits for the child to exit, records the terminal state, and
// broadcasts the exit event to every attached client.
func (h *childRegistry) watchExec(s *execSession) {
	waitErr := s.cmd.Wait()
	code := 1
	if s.cmd.ProcessState != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	ev := AgentEvent{Event: "exit", ExitCode: code}
	if waitErr != nil && !errors.Is(waitErr, io.EOF) {
		ev.Error = waitErr.Error()
	}
	s.mu.Lock()
	s.exited = true
	s.exitCode = code
	if waitErr != nil && !errors.Is(waitErr, io.EOF) {
		s.exitErr = waitErr
	}
	s.cancelGrace()
	s.writeMu.Lock()
	for _, enc := range s.clients {
		_ = enc.Encode(ev)
	}
	s.clients = map[net.Conn]*json.Encoder{}
	s.writeMu.Unlock()
	s.mu.Unlock()
	close(s.done)
	h.mu.Lock()
	delete(h.cmd, s.id)
	h.mu.Unlock()
	h.cleanupSession(s)
}

func (h *childRegistry) cleanupSession(s *execSession) {
	if s.guardDir != "" {
		os.RemoveAll(s.guardDir)
	}
}

// attach registers a client with the session. The session's broadcast
// goroutines fan output out to every attached client; this call just adds
// the client, watches for its disconnect (starting the reconnect grace if
// it was the last), and blocks until the exec completes.
func (s *execSession) attach(conn net.Conn, enc *json.Encoder) {
	s.mu.Lock()
	if s.exited {
		// The exec already finished — report its final state immediately.
		ev := AgentEvent{Event: "exit", ExitCode: s.exitCode}
		if s.exitErr != nil {
			ev.Error = s.exitErr.Error()
		}
		s.mu.Unlock()
		_ = enc.Encode(ev)
		return
	}
	s.clients[conn] = enc
	s.cancelGrace()
	s.mu.Unlock()

	go s.watchClient(conn)
	<-s.done
}

// watchClient reads a client connection until it closes (disconnect). A
// client is dropped the moment it goes away so the reconnect grace starts
// immediately — independent of whether the child produces output.
func (s *execSession) watchClient(conn net.Conn) {
	_, _ = io.Copy(io.Discard, conn)
	s.mu.Lock()
	if _, ok := s.clients[conn]; ok {
		delete(s.clients, conn)
		if len(s.clients) == 0 && !s.exited {
			s.startGrace()
		}
	}
	s.mu.Unlock()
}

// broadcastStream reads a child pipe line by line and fans each event out
// to every attached client. Clients whose write fails are dropped (they
// disconnected). Line cap 1MB — matching the control-plane adapter.
func (s *execSession) broadcastStream(stream string, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		ev := AgentEvent{Stream: stream, Data: line}
		s.writeMu.Lock()
		s.mu.Lock()
		for conn, enc := range s.clients {
			if err := enc.Encode(ev); err != nil {
				delete(s.clients, conn)
				if len(s.clients) == 0 && !s.exited {
					s.startGrace()
				}
			}
		}
		s.mu.Unlock()
		s.writeMu.Unlock()
	}
}

func (s *execSession) startGrace() {
	if s.grace != nil {
		return
	}
	s.grace = time.AfterFunc(s.graceDur, func() {
		s.mu.Lock()
		if s.exited {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
}

func (s *execSession) cancelGrace() {
	if s.grace != nil {
		s.grace.Stop()
		s.grace = nil
	}
}

// status reports whether the exec is still running. Used by the control
// plane's execution-liveness reaper to detect executions orphaned by a
// plane restart or a lost runtime container.
func (h *childRegistry) status(enc *json.Encoder, req AgentRequest) {
	h.mu.Lock()
	s, ok := h.cmd[req.ExecID]
	h.mu.Unlock()
	alive := false
	if ok {
		s.mu.Lock()
		alive = !s.exited
		s.mu.Unlock()
	}
	_ = enc.Encode(AgentEvent{Event: "status", Alive: alive})
}

func (h *childRegistry) signal(enc *json.Encoder, req AgentRequest) {
	h.mu.Lock()
	s, ok := h.cmd[req.ExecID]
	h.mu.Unlock()
	if !ok {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "no such exec: " + req.ExecID})
		return
	}
	sig := signalByName(req.Signal)
	if sig == syscall.Signal(0) {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "unknown signal: " + req.Signal})
		return
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(sig)
	}
	_ = enc.Encode(AgentEvent{Event: "exit", ExitCode: 0})
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

func signalByName(name string) syscall.Signal {
	switch strings.ToUpper(strings.TrimPrefix(name, "SIG")) {
	case "TERM":
		return syscall.SIGTERM
	case "KILL":
		return syscall.SIGKILL
	case "INT":
		return syscall.SIGINT
	case "HUP":
		return syscall.SIGHUP
	case "":
		return syscall.SIGTERM
	}
	return syscall.Signal(0)
}

// streamTo reads a pipe line by line and encodes each as a {stream,data}
// event, stopping on the first encode error (the client disconnected). The
// early stop matters: a re-attach must be able to resume from the same
// pipe, so a client's stream goroutine must not keep consuming it after
// the client is gone. Line cap 1MB — matching the control-plane adapter's
// local-path scanner (internal/opencode/adapter.go) because opencode
// `--format json` delivers an entire model response as ONE stdout line and
// a review/analysis can legitimately exceed 64KB. A smaller cap silently
// drops the line AND every subsequent event.
func streamTo(enc *json.Encoder, stream string, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if err := enc.Encode(AgentEvent{Stream: stream, Data: sc.Text()}); err != nil {
			// Client disconnected — stop reading so a re-attach can resume.
			return
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

// isolateOpenCodeData redirects the worker's opencode state (sessions,
// keys, telemetry) into an ephemeral directory under /tmp, seeded with
// the model auth from the read-only host mount (~/.local/share/opencode
// is mounted ro by the daemon). The worker can authenticate to the
// model providers but can never write to the host's opencode data.
func isolateOpenCodeData(env []string) []string {
	xdg, err := os.MkdirTemp("/tmp", "opencode-data-*")
	if err != nil {
		return env
	}
	home := os.Getenv("HOME")
	src := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if b, err := os.ReadFile(src); err == nil {
		dir := filepath.Join(xdg, "opencode")
		_ = os.MkdirAll(dir, 0o755)
		if werr := os.WriteFile(filepath.Join(dir, "auth.json"), b, 0o600); werr != nil {
			os.RemoveAll(xdg)
			return env
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
		if ev.Event == "status" {
			if ev.Alive {
				return 0, nil
			}
			return 1, nil
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
