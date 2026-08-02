package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/beardedparrott/orchicon/internal/opencode"
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
}

// AgentEvent is one JSON-lines record streamed back for an exec: either a
// {stream,data} chunk of child output, or the final {event:"exit"} marker.
type AgentEvent struct {
	Stream   string `json:"stream,omitempty"` // "stdout" | "stderr"
	Data     string `json:"data,omitempty"`
	Event    string `json:"event,omitempty"` // "exit" | "error"
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Pong     bool   `json:"pong,omitempty"`
}

// DefaultAgentSocket is the in-container path of the supervisor's unix
// socket. It lives under /tmp (tmpfs) so it is ephemeral like everything
// else in the runtime container.
const DefaultAgentSocket = "/tmp/orchicon-agent.sock"

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

// childRegistry tracks running children by exec_id so signals can target
// a specific execution (the TaskReconciler's wall-clock timeout path
// relies on this).
type childRegistry struct {
	mu  sync.Mutex
	log *slog.Logger
	cmd map[string]*exec.Cmd
}

func newChildRegistry(log *slog.Logger) *childRegistry {
	return &childRegistry{log: log, cmd: make(map[string]*exec.Cmd)}
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
		h.runExec(enc, req)
	case "signal":
		h.signal(enc, req)
	default:
		_ = enc.Encode(AgentEvent{Event: "error", Error: "unknown cmd: " + req.Cmd})
	}
}

func (h *childRegistry) runExec(enc *json.Encoder, req AgentRequest) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "empty argv"})
		return
	}
	// Safety: the supervisor only ever runs a closed set of entry points
	// (opencode runs and in-container tooling). The daemon already
	// enforces this, but belt-and-suspenders here too.
	base := filepath.Base(req.Argv[0])
	if base != "opencode" && base != "orchicon" && base != "bash" && base != "sh" {
		_ = enc.Encode(AgentEvent{Event: "error", Error: "argv[0] not allowlisted: " + base})
		return
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = agentEnv(req)
	cmd.Stdin = nil // non-interactive; opencode runs with --auto

	// Build the execution guard shim inside the container so every child
	// the worker spawns resolves rm/sudo/dd/etc. through the shim.
	guardDir, guardErr := opencode.MakeGuard("/tmp", req.ProjectDir)
	if guardErr != nil {
		h.log.Warn("supervisor: guard not applied", "error", guardErr, "exec", req.ExecID)
	} else {
		cmd.Env = prependGuard(cmd.Env, guardDir)
		h.log.Debug("supervisor: guard applied", "dir", guardDir, "path", envPath(cmd.Env), "exec", req.ExecID)
		defer os.RemoveAll(guardDir)
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

	h.mu.Lock()
	h.cmd[req.ExecID] = cmd
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.cmd, req.ExecID)
		h.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		_ = enc.Encode(AgentEvent{Event: "error", Error: err.Error()})
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(enc, "stdout", stdout, &wg)
	go streamLines(enc, "stderr", stderr, &wg)
	waitErr := cmd.Wait()
	wg.Wait()

	code := 1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	ev := AgentEvent{Event: "exit", ExitCode: code}
	if waitErr != nil && !errors.Is(waitErr, io.EOF) {
		ev.Error = waitErr.Error()
	}
	_ = enc.Encode(ev)
}

func (h *childRegistry) signal(enc *json.Encoder, req AgentRequest) {
	h.mu.Lock()
	cmd, ok := h.cmd[req.ExecID]
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
	_ = cmd.Process.Signal(sig)
	_ = enc.Encode(AgentEvent{Event: "exit", ExitCode: 0})
}

func (h *childRegistry) killAll(sig syscall.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cmd := range h.cmd {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(sig)
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

// streamLines reads a pipe line by line and encodes each as a
// {stream,data} event. Buffer size is capped at 64k per line.
func streamLines(enc *json.Encoder, stream string, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for sc.Scan() {
		_ = enc.Encode(AgentEvent{Stream: stream, Data: sc.Text()})
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
