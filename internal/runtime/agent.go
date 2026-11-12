package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/guard"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// AgentRequest is a single dispatch from the daemon to the in-container supervisor.
type AgentRequest struct {
	Cmd  string   `json:"cmd"` // "ping" | "serve" | "exec" | "stdio" | "signal"
	Argv []string `json:"argv,omitempty"`
	// ExecID names the tracked child for the "stdio"/"signal" cmds (the long-lived streaming stdio
	// transport).
	ExecID string `json:"exec_id,omitempty"`
	// Data is one stdin frame payload ("stdio" connection, cmd "stdin").
	Data string `json:"data,omitempty"`
	// Signal is the signal name for cmd "signal" ("INT"|"TERM"|"KILL").
	Signal string `json:"signal,omitempty"`
	// AdapterKind is the adapter kind whose serve this request brings up (the "serve" cmd only).
	AdapterKind string   `json:"adapter_kind,omitempty"`
	Env         []string `json:"env,omitempty"`
	// Cwd and ProjectDir are HOST paths until mapHostPathFields converts them (hostpaths.go). The tag is the
	// registry: a path field without it is the shape of the reported chdir failure.
	Cwd        string `json:"cwd,omitempty" orchicon:"hostpath"`
	ProjectDir string `json:"project_dir,omitempty" orchicon:"hostpath"`
}

// AgentEvent is one JSON-lines record the supervisor or the image-build path streams back: the
// {event:"serve", port, password} handshake answer, an {event:"error"} for a failed bring-up.
type AgentEvent struct {
	Stream   string `json:"stream,omitempty"` // "stdout" | "stderr" (docker build relay)
	Data     string `json:"data,omitempty"`
	Event    string `json:"event,omitempty"` // "error" | "exit" | "serve"
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Pong     bool   `json:"pong,omitempty"`
	PID      int    `json:"pid,omitempty"`      // stdio cmd: the streaming child's pid
	Port     int    `json:"port,omitempty"`     // serve cmd: the container-internal serve port
	Password string `json:"password,omitempty"` // serve cmd: the container's serve password
	// PlaneEnabled reports (serve handshake) whether this image boots the sandbox plane (postgres +
	// nats-server present).
	PlaneEnabled bool `json:"plane_enabled,omitempty"`
	// Skipped reports (pull) that the requested image was ALREADY present locally, so no download ran.
	Skipped bool `json:"skipped,omitempty"`
}

// envGitdirMaps carries the host->guest project prefix pairs this container was mounted with.
const envGitdirMaps = "ORCHICON_GITDIR_MAPS"

// DefaultAgentSocket is the in-container path of the supervisor's unix socket.
const DefaultAgentSocket = "/tmp/orchicon-agent.sock"

// Sandbox plane — a disposable, in-container Orchicon control plane that the supervisor boots at
// container start on images that bake the pieces (currently :orchicon-dev.
const (
	// SandboxPostgresDSN is the in-container DSN of the sandbox plane's Postgres (trust auth, user
	// orchicon).
	SandboxPostgresDSN = "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable"

	// sandboxDataDir is the stable per-container sandbox state root (the rootfs is chowned to the
	// runtime uid, so the non-root user owns it).
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

// IsDevImageTag reports whether a runtime image tag is a dev variant — the stock variant that bakes
// the sandbox-plane pieces (postgres + nats-server).
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

// runtimeBinAllowlist is the set of binaries the runtime supervisor may exec (argv[0] basenames).
var runtimeBinAllowlist = map[string]bool{
	"opencode": true,
	"claude":   true,
	"orchicon": true,
	"bash":     true,
	"sh":       true,
}

// guestArgv0Refusal is the single explanation for a refused argv[0], and it names the HOST-PATH
// case explicitly because that case does not look like one from this side.
func guestArgv0Refusal(prefix, argv0, base string) string {
	reason := prefix + " not allowlisted: " + base
	if strings.ContainsRune(argv0, '\\') || strings.HasSuffix(strings.ToLower(argv0), ".exe") {
		reason += " — argv[0] is a HOST path, not a name this container can exec. A container dispatch " +
			"must send the binary the IMAGE provides (e.g. \"claude\"), never the operator's install path: " +
			"the host path does not exist in here, and a non-POSIX one has no basename to allowlist."
	}
	return reason
}

// RunSupervisor runs the in-container dispatch loop as PID 1.
func RunSupervisor(socketPath string, log *slog.Logger) error {
	if socketPath == "" {
		socketPath = DefaultAgentSocket
	}
	_ = os.RemoveAll(socketPath)
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return fmt.Errorf("supervisor: mkdir: %w", err)
	}
	// Pre-create the worker scratch directory (matching the opencode external_directory carve-out in
	// internal/opencode/config.go) so it exists as the runtime user's own dir before any worker runs.
	if err := os.MkdirAll("/tmp/orchicon", 0o755); err != nil {
		return fmt.Errorf("supervisor: mkdir scratch: %w", err)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("supervisor: listen %s: %w", socketPath, err)
	}
	defer l.Close()
	log.Info("runtime supervisor listening", "socket", socketPath)

	// GIT IS TOLD HOW TO READ A WORKTREE; THE WORKTREE IS NOT ALTERED.

	// Reap orphaned children on exit so a container kill never leaves a
	// stray opencode running inside.
	handle := newChildRegistry(log)
	defer handle.killAll(syscall.SIGKILL)
	defer handle.stopSandboxPlane()

	// Serve watchdog: poll the container's opencode serve and restart it when it stops answering
	// /global/health (wedged OR exited).
	go handle.watchServe()

	// Sandbox plane: boot the disposable in-container Orchicon control plane (Postgres -> NATS ->
	// `orchicon serve`) on images that bake the pieces.
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

// execSession is one running child.
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

	// pumpsDone is closed once the stdio pumps have drained their pipes.
	pumpsDone chan struct{}
}

func newExecSession(id string) *execSession {
	return &execSession{
		id:   id,
		done: make(chan struct{}),
	}
}

// childRegistry tracks running children by exec_id so signals can target a specific execution (the
// TaskReconciler's wall-clock timeout path relies on this).
type childRegistry struct {
	mu  sync.Mutex
	log *slog.Logger
	cmd map[string]*execSession
	// serveMu serializes serve lifecycle operations (bring-up + watchdog restart).
	serveMu sync.Mutex
	// serves holds the per-ADAPTER serve state, keyed by adapter kind.
	serves map[string]*serveState
	// sandboxChecked/sandboxOK cache the sandbox-plane self-gate (binary presence in the image — the
	// image's contents never change while the container runs).
	sandboxChecked bool
	sandboxOK      bool
	// sandboxMu serializes sandbox-plane bring-up (boot + watchdog
	// restart), mirroring serveMu for the opencode serve.
	sandboxMu sync.Mutex
}

func newChildRegistry(log *slog.Logger) *childRegistry {
	return &childRegistry{
		log:    log,
		cmd:    make(map[string]*execSession),
		serves: make(map[string]*serveState),
	}
}

// serveState is one adapter kind's in-container serve.
type serveState struct {
	// execID is the reserved child-registry id this serve is tracked under.
	execID string
	// pw is the serve's basic-auth password, generated once by the supervisor on first startup and
	// reused for the container's lifetime so idempotent handshakes return a stable credential.
	pw string
	// req is the AgentRequest the serve was last started with. The watchdog
	// reuses it to restart the serve (same argv/env/cwd) after a wedge.
	req AgentRequest
	// started marks that this kind's serve has been brought up at least once, so the watchdog only acts
	// on a serve the plane actually asked for.
	started bool
}

// serveStateLocked returns the serve state for kind, creating it on first
// use. The caller must hold h.mu.
func (h *childRegistry) serveStateLocked(kind string) *serveState {
	kind = normalizedKind(kind)
	if h.serves == nil {
		h.serves = make(map[string]*serveState)
	}
	st := h.serves[kind]
	if st == nil {
		st = &serveState{execID: serveExecIDFor(kind)}
		h.serves[kind] = st
	}
	return st
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
	case "exec":
		h.runExec(enc, req)
	case "stdio":
		h.runStdio(dec, enc, req)
	case "signal":
		h.runSignal(enc, req)
	default:
		_ = enc.Encode(AgentEvent{Event: "error", Error: "unknown cmd: " + req.Cmd})
	}
}

// serveExecID is the reserved exec id for the container's opencode serve.
const serveExecID = "__orchicon_serve__"

// defaultServePort is the container-internal port the serve binds (and
// the daemon publishes to a random host loopback port).
const defaultServePort = 4096

// serveExecIDFor returns the reserved exec id for adapter kind's in-container serve.
func serveExecIDFor(kind string) string {
	k := normalizedKind(kind)
	if k == adapter.DefaultAdapterKind {
		return serveExecID
	}
	return "__orchicon_serve_" + k + "__"
}

// servePortFor returns the container-internal port adapter kind's serve binds.
func servePortFor(kind string) int {
	if normalizedKind(kind) == adapter.DefaultAdapterKind {
		return defaultServePort
	}
	return 0
}

// serveDataDir is the stable per-container XDG_DATA_HOME for the serve.
const serveDataDir = "/tmp/orchicon-serve-data"

// runServe starts the container's opencode serve as a DETACHED child and answers with the port once
// it is healthy.
func (h *childRegistry) runServe(enc *json.Encoder, req AgentRequest) {
	// Serialize serve lifecycle against the watchdog: a concurrent runServe handshake and watchServe
	// restart must never both spawn a serve on the same port.
	h.serveMu.Lock()
	defer h.serveMu.Unlock()

	kind := normalizedKind(req.AdapterKind)
	execID := serveExecIDFor(kind)
	port := servePortFor(kind)
	if port == 0 {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "no in-container serve for adapter kind: " + kind})
		return
	}
	// argv defaults to `opencode serve --hostname 0.0.0.0 --port <port>`
	// (the port the daemon publishes); a request may override the argv.
	if len(req.Argv) == 0 {
		req.Argv = []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprintf("%d", port)}
	}
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "argv[0] not allowlisted: " + base})
		return
	}

	// Snapshot the registry under mu, then RELEASE it before the health probe: serveHealthy is an HTTP
	// call with a 2s timeout.
	h.mu.Lock()
	st := h.serveStateLocked(kind)
	pw := st.pw
	existing, registered := h.cmd[execID]
	if registered {
		cachedPw := pw
		h.mu.Unlock()
		// This kind's serve is already registered.
		if cachedPw != "" && serveHealthy(port, cachedPw) {
			_ = enc.Encode(AgentEvent{Event: "serve", Port: port, Password: cachedPw, PlaneEnabled: h.sandboxAvailable()})
			return
		}
		h.log.Warn("serve registered but not healthy — restarting", "kind", kind, "pid", existing.pidOrZero())
		existing.kill()
		// Re-take mu for the registry mutation.
		h.mu.Lock()
		delete(h.cmd, execID)
	}
	if pw == "" {
		pw = randomServePassword()
		st.pw = pw
	}
	h.mu.Unlock()

	s := newExecSession(execID)
	s.detached = true

	cmd := procwin.Command(req.Argv[0], req.Argv[1:]...)
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
	if guardErr != nil || guardDir == "" {
		// guardDir == "" is the UNSUPPORTED-PLATFORM case (Windows), not a failure: MakeGuard reports it
		// with a nil error because no guard can exist there.
		h.log.Warn("supervisor: guard not applied to serve", "error", guardErr, "supported", guardDir != "")
	} else {
		env = prependGuard(env, guardDir)
		s.guardDir = guardDir
	}
	// DIAGNOSTIC: record WHAT this serve was given, by NAME.
	//
	// A 2.x execution failed with a model-provider rejection that nothing in the platform
	// could explain, and the inputs that differed from a WORKING path were not recoverable
	// afterwards: the config rides the serve PROCESS's environment rather than the
	// container's, so it is invisible to `docker inspect` and dies with the process. This
	// line makes the next such failure self-describing.
	//
	// NAMES AND DIGESTS ONLY. The environment carries provider credentials, and this line is
	// permanent, so no value is ever rendered — the config is reduced to a digest and a
	// length, which is enough to tell two different configs apart without logging either.
	h.log.Info("supervisor: starting in-container serve",
		"exec", execID, "kind", kind, "argv", strings.Join(req.Argv, " "),
		"cwd", cmd.Dir, "env", serveEnvSummary(env))
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
	h.cmd[execID] = s
	st.req = req
	st.started = true
	h.mu.Unlock()
	s.cmd = cmd
	go h.watchExec(s)

	// Wait for the serve to answer /global/health with the password, so
	// the plane never races a half-initialized serve.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveHealthy(port, pw) {
			h.log.Info("runtime adapter serve ready", "kind", kind, "port", port, "pid", cmd.Process.Pid)
			_ = enc.Encode(AgentEvent{Event: "serve", Port: port, Password: pw, PlaneEnabled: h.sandboxAvailable()})
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

// runExec runs one synchronous shell command inside the container — the always-container native
// transport.
const (
	// stdioDrainKillTimeout bounds the wait for the pumps after stdin closes. A child
	// that will not exit is killed, and killing it is what closes the write ends.
	stdioDrainKillTimeout = 15 * time.Second

	// stdioPumpDrainGrace bounds the wait for the pumps BEFORE reaping.
	stdioPumpDrainGrace = 20 * time.Second
)

func (h *childRegistry) runExec(enc *json.Encoder, req AgentRequest) {
	base := ""
	if len(req.Argv) > 0 {
		base = filepath.Base(req.Argv[0])
	}
	if base != "bash" && base != "sh" {
		_ = enc.Encode(AgentEvent{Event: "error", Error: guestArgv0Refusal("exec argv[0]", req.Argv[0], base)})
		return
	}
	cmd := procwin.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := agentEnv(req)
	guardDir, guardErr := guard.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil || guardDir == "" {
		h.log.Warn("supervisor: guard not applied to exec", "error", guardErr, "supported", guardDir != "")
	} else {
		env = prependGuard(env, guardDir)
		defer os.RemoveAll(guardDir)
	}
	cmd.Env = env
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
	if err := cmd.Start(); err != nil {
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	// json.Encoder is not safe for concurrent use: serialize the two
	// stream pumps through a local mutex.
	var wmu sync.Mutex
	write := func(ev AgentEvent) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = enc.Encode(ev)
	}
	var wg sync.WaitGroup
	pump := func(stream string, r io.Reader) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				write(AgentEvent{Stream: stream, Data: string(buf[:n])})
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump("stdout", stdout)
	go pump("stderr", stderr)
	// DRAIN BEFORE REAPING — the same hazard as watchExec, and the same measurement: reaping closes the
	// read ends, so a fast child's output is gone before a pump that is only scheduled can read it.
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(stdioPumpDrainGrace):
	}
	werr := cmd.Wait()
	<-drained
	code := 0
	emsg := ""
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else {
			emsg = werr.Error()
			code = 1
		}
	}
	write(AgentEvent{Event: "exit", ExitCode: code, Error: emsg})
}

// runStdio runs one long-lived duplex streaming child — the transport the claude adapter drives
// across many turns on a SINGLE subprocess.
func (h *childRegistry) runStdio(dec *json.Decoder, enc *json.Encoder, req AgentRequest) {
	if req.ExecID == "" {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "stdio requires exec_id"})
		return
	}
	if len(req.Argv) == 0 {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "stdio requires argv"})
		return
	}
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		_ = enc.Encode(AgentEvent{Event: "error", Error: guestArgv0Refusal("stdio argv[0]", req.Argv[0], base)})
		return
	}
	// ONE active child per exec id: a second start is refused loudly (never
	// two writers to the same session's pipe/transcript).
	h.mu.Lock()
	if ex, ok := h.cmd[req.ExecID]; ok && ex != nil && !ex.hasExited() {
		h.mu.Unlock()
		_ = enc.Encode(AgentEvent{Event: "error", Error: "stdio child already live for exec id " + req.ExecID})
		return
	}
	h.mu.Unlock()

	cmd := procwin.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := agentEnv(req)
	guardDir, guardErr := guard.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil || guardDir == "" {
		h.log.Warn("supervisor: guard not applied to stdio", "error", guardErr, "supported", guardDir != "")
	} else {
		env = prependGuard(env, guardDir)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		if guardDir != "" {
			os.RemoveAll(guardDir)
		}
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if guardDir != "" {
			os.RemoveAll(guardDir)
		}
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		if guardDir != "" {
			os.RemoveAll(guardDir)
		}
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		if guardDir != "" {
			os.RemoveAll(guardDir)
		}
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}
	s := newExecSession(req.ExecID)
	s.cmd = cmd
	s.guardDir = guardDir
	// The reaper is NOT started here.
	pumpsDone := make(chan struct{})
	s.pumpsDone = pumpsDone
	h.mu.Lock()
	h.cmd[req.ExecID] = s
	h.mu.Unlock()

	// json.Encoder is not safe for concurrent use: serialize the pumps.
	var wmu sync.Mutex
	write := func(ev AgentEvent) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = enc.Encode(ev)
	}
	write(AgentEvent{Event: "started", PID: cmd.Process.Pid})

	// Line-framed pumps: each output line is one event, so the plane can
	// reassemble JSONL without a length prefix.
	var wg sync.WaitGroup
	pump := func(stream string, r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			write(AgentEvent{Stream: stream, Data: string(sc.Bytes()) + "\n"})
		}
	}
	wg.Add(2)
	go pump("stdout", stdout)
	go pump("stderr", stderr)
	// NOW the reaper may start, and it waits for these pumps before reaping.
	go func() { wg.Wait(); close(pumpsDone) }()
	go h.watchExec(s)

	// Control loop: read follow-up frames off the SAME connection until the
	// plane closes its request body (dec.Err on EOF) or sends "close".
	for {
		var f AgentRequest
		if err := dec.Decode(&f); err != nil {
			break
		}
		switch f.Cmd {
		case "stdin":
			if _, err := stdin.Write([]byte(f.Data + "\n")); err != nil {
				goto drain
			}
		case "signal":
			if cmd.Process != nil {
				_ = agentSignal(cmd.Process, f.Signal)
			}
		case "close":
			goto drain
		}
	}
drain:
	_ = stdin.Close()
	// The child should now see EOF and exit.
	select {
	case <-pumpsDone:
	case <-time.After(stdioDrainKillTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-pumpsDone
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	write(AgentEvent{Event: "exit", ExitCode: s.exitCodeValue()})
}

// runSignal delivers a signal to a live stdio child by exec id.
func (h *childRegistry) runSignal(enc *json.Encoder, req AgentRequest) {
	h.mu.Lock()
	s := h.cmd[req.ExecID]
	h.mu.Unlock()
	if s == nil || s.hasExited() || s.cmd == nil || s.cmd.Process == nil {
		_ = enc.Encode(AgentEvent{Pong: true})
		return
	}
	if err := agentSignal(s.cmd.Process, req.Signal); err != nil {
		h.log.Warn("supervisor: stdio signal failed", "exec", req.ExecID, "signal", req.Signal, "error", err)
	}
	_ = enc.Encode(AgentEvent{Pong: true})
}

// agentSignal maps a signal name onto an OS signal. Unknown names are a
// no-op.
func agentSignal(proc *os.Process, name string) error {
	var sig syscall.Signal
	switch name {
	case "INT":
		sig = syscall.SIGINT
	case "TERM":
		sig = syscall.SIGTERM
	case "KILL":
		sig = syscall.SIGKILL
	default:
		return nil
	}
	return proc.Signal(sig)
}

// hasExited reports whether the tracked child has finished. Guarded by the
// session's own mutex (never the registry's).
func (s *execSession) hasExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

// exitCodeValue returns the recorded exit code (0 before the child exits).
func (s *execSession) exitCodeValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

// pidOrZero returns the child's process id (0 when not started).
func (s *execSession) pidOrZero() int {
	if s != nil && s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// watchServe is the serve health watchdog.
func (h *childRegistry) watchServe() {
	backoff := serveWatchInterval
	for {
		time.Sleep(serveWatchInterval)
		// Snapshot every STARTED serve (and its registry session) under mu;
		// health probes and restarts happen outside the lock, per kind.
		type serveSlot struct {
			kind string
			s    *execSession
			req  AgentRequest
			pw   string
			port int
		}
		var slots []serveSlot
		h.mu.Lock()
		for kind, st := range h.serves {
			if st == nil || !st.started {
				continue
			}
			if port := servePortFor(kind); port != 0 {
				if s, ok := h.cmd[st.execID]; ok && s != nil && s.cmd != nil && s.cmd.Process != nil {
					slots = append(slots, serveSlot{kind: kind, s: s, req: st.req, pw: st.pw, port: port})
				}
			}
		}
		h.mu.Unlock()
		if len(slots) == 0 {
			backoff = serveWatchInterval
			continue
		}
		for _, sl := range slots {
			if serveHealthy(sl.port, sl.pw) {
				backoff = serveWatchInterval
				continue
			}
			// Unhealthy.
			h.log.Warn("serve unhealthy — restarting", "kind", sl.kind, "pid", sl.s.pidOrZero(), "backoff", backoff.String())
			time.Sleep(backoff)
			h.serveMu.Lock()
			// Re-check under serveMu: a runServe handshake may have already replaced the serve while we slept.
			h.mu.Lock()
			st := h.serves[sl.kind]
			cur, ok := h.cmd[serveExecIDFor(sl.kind)]
			if st == nil || !ok || cur != sl.s {
				h.mu.Unlock()
				h.serveMu.Unlock()
				backoff = serveWatchInterval
				continue
			}
			if serveHealthy(sl.port, st.pw) {
				h.mu.Unlock()
				h.serveMu.Unlock()
				backoff = serveWatchInterval
				continue
			}
			sl.s.kill()
			delete(h.cmd, st.execID)
			req := st.req
			h.mu.Unlock()
			if err := h.startServeAgain(sl.kind, req); err != nil {
				h.serveMu.Unlock()
				h.log.Error("serve restart failed", "kind", sl.kind, "error", err)
				if backoff < serveWatchMaxBackoff {
					backoff *= 2
				}
				continue
			}
			h.serveMu.Unlock()
			backoff = serveWatchInterval
			h.log.Info("serve restarted by watchdog", "kind", sl.kind, "port", sl.port)
		}
	}
}

// startServeAgain restarts the container's opencode serve after a wedge, using the stored
// AgentRequest.
func (h *childRegistry) startServeAgain(kind string, req AgentRequest) error {
	kind = normalizedKind(kind)
	execID := serveExecIDFor(kind)
	port := servePortFor(kind)
	if port == 0 {
		return fmt.Errorf("no in-container serve for adapter kind: %s", kind)
	}
	if len(req.Argv) == 0 {
		req.Argv = []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprintf("%d", port)}
	}
	base := filepath.Base(req.Argv[0])
	if !runtimeBinAllowlist[base] {
		return fmt.Errorf("%s", guestArgv0Refusal("argv[0]", req.Argv[0], base))
	}

	h.mu.Lock()
	st := h.serveStateLocked(kind)
	pw := st.pw
	if pw == "" {
		pw = randomServePassword()
		st.pw = pw
	}
	h.mu.Unlock()

	s := newExecSession(execID)
	s.detached = true

	cmd := procwin.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	env := agentEnv(req)
	env = setEnv(env, "OPENCODE_SERVER_PASSWORD", pw)
	env = isolateOpenCodeDataInto(env, serveDataDir)
	guardDir, guardErr := guard.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil || guardDir == "" {
		h.log.Warn("supervisor: guard not applied to serve restart", "error", guardErr, "supported", guardDir != "")
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
	h.cmd[execID] = s
	h.serveStateLocked(kind).req = req
	h.mu.Unlock()
	s.cmd = cmd
	go h.watchExec(s)

	// Wait for the restarted serve to answer health so the next dispatch
	// converges to a usable serve rather than racing a half-initialized one.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if serveHealthy(port, pw) {
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
	serveWatchInterval   = 10 * time.Second
	serveWatchMaxBackoff = 60 * time.Second
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

// sandboxAvailable reports whether this image can boot the sandbox plane (a Postgres server bin dir
// + nats-server on PATH).
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

// sandboxPgBinDir resolves the Postgres server bin dir (initdb/pg_ctl/ pg_isready/postgres).
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

// sandboxPgBinDirComplete reports whether a Postgres bin dir contains ALL four server binaries the
// sandbox plane execs (initdb, pg_ctl, pg_isready, postgres).
func sandboxPgBinDirComplete(dir string) bool {
	for _, bin := range []string{"initdb", "pg_ctl", "pg_isready", "postgres"} {
		if _, err := os.Stat(filepath.Join(dir, bin)); err != nil {
			return false
		}
	}
	return true
}

// bootSandboxPlane brings up the sandbox plane in dependency order — Postgres (initdb once into the
// stable data dir, then pg_ctl start) -> nats-server -js (tracked child) -> `orchicon serve`.
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
		if out, err := procwin.Command(filepath.Join(pgb, "initdb"),
			"-D", sandboxPgDataDir, "-U", "orchicon", "--auth=trust").CombinedOutput(); err != nil {
			return fmt.Errorf("initdb: %v: %s", err, out)
		}
	}
	// pg_ctl -w start blocks until the server is up; it errors when the server is already running (a
	// previous boot / watchdog restart) — the readiness probe below decides either way.
	opts := fmt.Sprintf("-p %d -c listen_addresses=localhost -c unix_socket_directories=%s", sandboxPgPort, sandboxDataDir)
	if out, err := procwin.Command(filepath.Join(pgb, "pg_ctl"),
		"-D", sandboxPgDataDir, "-l", sandboxPgLog, "-o", opts, "-w", "start").CombinedOutput(); err != nil {
		h.log.Warn("sandbox plane: pg_ctl start (may already be running)", "output", string(out), "error", err)
	}
	if !h.waitPgReady(pgb, 30*time.Second) {
		return errors.New("postgres did not become ready")
	}
	// initdb creates only postgres/template DBs; the plane's migrations
	// target the `orchicon` database (the DSN's db name).
	cmd := procwin.Command(filepath.Join(pgb, "createdb"),
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
		out, err := procwin.Command(filepath.Join(pgb, "pg_isready"),
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
	cmd := procwin.Command("nats-server", "-js", "-p", fmt.Sprintf("%d", sandboxNATSPort), "-sd", sandboxNATSDataDir)
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

// bootSandboxServe starts the sandbox `orchicon serve` (the sandbox control plane) as a tracked
// child and waits for its /healthz.
func (h *childRegistry) bootSandboxServe() error {
	if sandboxPlaneHealthy() {
		return nil
	}
	h.killAndClear(sandboxPlaneExecID)
	cmd := procwin.Command("orchicon", "serve")
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
	// The plane runs migrations + seeds on boot against a fresh DB; give it the full window (the
	// run-start gate's own 120s window bounds the total wait either way).
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

// watchSandboxPlane is the sandbox-plane health watchdog.
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

// killAndClear terminates a tracked child (if any) and removes it from the registry, waiting
// briefly for the reap so a restart doesn't race "address already in use" on the next Start.
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

// stopSandboxPlane is a best-effort teardown of the sandbox plane on supervisor exit.
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
	_ = procwin.CommandContext(ctx, filepath.Join(pgb, "pg_ctl"),
		"-D", sandboxPgDataDir, "-m", "fast", "stop").Run()
}

// watchExec waits for the child to exit, records the terminal state, and closes the session.
func (h *childRegistry) watchExec(s *execSession) {
	// DRAIN BEFORE REAPING, and this ordering is load-bearing rather than tidy.
	if s.pumpsDone != nil {
		select {
		case <-s.pumpsDone:
		case <-time.After(stdioPumpDrainGrace):
		}
	}
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
	// Only unregister if this session is STILL the registered one.
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

// gitEnvFor returns GIT_DIR and GIT_WORK_TREE for a child whose working directory is cwd, or nil
// when cwd is not a worktree of a mounted project.
func gitEnvFor(cwd string) []string {
	workDir := strings.TrimSpace(cwd)
	if workDir == "" {
		return nil
	}
	if st, err := os.Stat(filepath.Join(workDir, ".git")); err != nil || st.IsDir() {
		return nil // not a worktree: no pointer file, or a real .git directory
	}
	raw, err := os.ReadFile(filepath.Join(workDir, ".git"))
	if err != nil {
		return nil
	}
	pointer := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir: "))
	if pointer == "" {
		return nil
	}
	if !strings.Contains(strings.ReplaceAll(pointer, "\\", "/"), ".git/worktrees") {
		return nil // not a worktree pointer
	}
	gitDir := pointer
	// A host-form pointer must be transposed into this namespace. One that already resolves needs no help and
	// is left exactly as written.
	if _, serr := os.Stat(gitDir); serr != nil {
		mapped, ok := mapHostPointerToGuest(pointer)
		if !ok {
			return nil // cannot transpose confidently — git's own error beats a wrong GIT_DIR
		}
		gitDir = mapped
	}
	return []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + workDir}
}

// mapHostPointerToGuest transposes a host-form gitdir into this container's path space, using the same
// host|guest pairs the daemon emitted for its mounts.
func mapHostPointerToGuest(pointer string) (string, bool) {
	folded := strings.ReplaceAll(pointer, "\\", "/")
	for _, m := range parsePathMappings(os.Getenv(envGitdirMaps)) {
		hostFolded := strings.ReplaceAll(strings.TrimRight(m.Host, "\\/"), "\\", "/")
		if hostFolded == "" || !strings.HasPrefix(folded, hostFolded) {
			continue
		}
		return strings.TrimRight(m.Guest, "/") + strings.TrimPrefix(folded, hostFolded), true
	}
	return "", false
}

// parsePathMappings reads the "<host>|<guest>" pairs the daemon emitted. An entry that does not parse is
// SKIPPED, not fatal: a malformed mapping must not stop a container from starting.
func parsePathMappings(raw string) []pathMapping {
	var out []pathMapping
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, guest, ok := strings.Cut(entry, "|")
		host, guest = strings.TrimSpace(host), strings.TrimSpace(guest)
		if !ok || host == "" || guest == "" || host == guest {
			continue
		}
		out = append(out, pathMapping{Host: host, Guest: guest})
	}
	return out
}

// pathMapping is one host->guest prefix pair.
type pathMapping struct {
	Host  string
	Guest string
}

// agentEnv builds the child environment: the supervisor's environment plus the daemon-provided
// overrides (HOME, OPENCODE_CONFIG_CONTENT, OPENCODE_EXECUTION_ID, etc.).
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
	// Ensure every MOUNTED adapter CLI bin is on the child's PATH — belt-and-suspenders to the daemon's
	// container-level PATH.
	if gitEnv := gitEnvFor(req.Cwd); len(gitEnv) > 0 {
		for _, kv := range gitEnv {
			out = setEnv(out, kv[:strings.Index(kv, "=")], kv[strings.Index(kv, "=")+1:])
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		kinds := splitKinds(os.Getenv("ORCHICON_ADAPTER_KINDS"))
		if dirs := adapterPathPrefix(home, kinds...); len(dirs) > 0 {
			out = setEnv(out, "PATH", strings.Join(dirs, string(os.PathListSeparator))+string(os.PathListSeparator)+envPath(out))
		}
	}
	return out
}

// splitKinds parses a comma-separated kind list, dropping empty entries so
// strings.Split("") ([""]) is treated as no kinds.
func splitKinds(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
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

// isolateOpenCodeDataInto redirects opencode state into a caller-chosen directory (seeded with the
// model auth) and returns the env with XDG_DATA_HOME set.
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

// After the initial request this goroutine is the ONLY writer on the supervisor connection.
func forwardFrames(inDec *json.Decoder, enc *json.Encoder, execID string) {
	for {
		var f AgentRequest
		if err := inDec.Decode(&f); err != nil {
			_ = enc.Encode(AgentRequest{Cmd: "close", ExecID: execID})
			return
		}
		if err := enc.Encode(f); err != nil {
			return
		}
	}
}

func prependGuard(env []string, guardDir string) []string {
	out := make([]string, 0, len(env)+1)
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

// RunClient connects to the supervisor socket, forwards one request (read from stdin) and relays
// the streamed events to stdout as JSON-lines.
func RunClient(socketPath string, in io.Reader, out io.Writer) (int, error) {
	if socketPath == "" {
		socketPath = DefaultAgentSocket
	}
	inDec := json.NewDecoder(in)
	var req AgentRequest
	if err := inDec.Decode(&req); err != nil {
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
	if req.Cmd == "stdio" {
		go forwardFrames(inDec, enc, req.ExecID)
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

// VALUES ARE NEVER RENDERED, and that is a hard rule rather than a caution: this environment
// carries provider credentials (the daemon injects OLLAMA_API_KEY and friends).
func serveEnvSummary(env []string) string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "" {
			continue
		}
		if k == "OPENCODE_CONFIG_CONTENT" {
			sum := sha256.Sum256([]byte(v))
			out = append(out, fmt.Sprintf("%s(sha:%s,len:%d)", k, hex.EncodeToString(sum[:])[:12], len(v)))
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
