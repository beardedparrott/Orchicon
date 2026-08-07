package opencode

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/guard"
)

// HostServe is the always-on opencode serve for the in-process (local)
// execution population: standalone task dispatches, follow-up executions,
// and any execution that isn't bound to a workflow-run runtime container.
// One serve per plane hosts a persistent session per execution; the
// control plane supervises it (spawn on boot, health watchdog, restart
// with backoff) so the session host is never down.
//
// Isolation: the serve runs against a DEDICATED data dir
// (~/.local/share/orchicon/opencode) seeded with the operator's model
// auth, so it never shares an opencode.db with the operator's own
// opencode instances. Sessions persist there across serve restarts, so a
// restart is transparent to sessions (the client re-attaches by id).
//
// Safety: the serve process runs the agent's tools for every local
// execution, so the OS-level execution guard is applied to its PATH in
// the no-project mode (all absolute targets blocked; relative paths stay
// within each session's own directory, scoped by opencode per session).
type HostServe struct {
	log *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	port     int
	password string
	client   *SessionClient
	guard    *guard.Guard
	dataDir  string
	home     string
	started  bool
}

// NewHostServe constructs the host-serve manager. dataDir is the
// dedicated opencode data directory (created + seeded with model auth on
// Start). home overrides the operator home used to locate the real
// opencode data dir whose auth.json is seeded (empty = os.UserHomeDir).
func NewHostServe(log *slog.Logger, dataDir, home string) *HostServe {
	return &HostServe{log: log, dataDir: dataDir, home: home}
}

// Enabled reports whether the host serve is configured for this plane.
// It is disabled when the operator set ORCHICON_OPCODE_SESSION_TRANSPORT=0
// (global kill-switch) — in that case every execution uses the legacy
// one-shot subprocess path.
func (h *HostServe) Enabled() bool {
	return os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT") != "0"
}

// URL returns the serve base URL (valid after Start).
func (h *HostServe) URL() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d", h.port)
}

// Password returns the serve's basic-auth password.
func (h *HostServe) Password() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.password
}

// Client returns a session client for the host serve (valid after Start).
func (h *HostServe) Client() *SessionClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.client
}

// Start spawns the serve, waits for it to answer /global/health, and
// builds the session client. If the `opencode` binary is absent or the
// serve cannot come up, it returns an error — the caller logs it and
// keeps running on the one-shot subprocess path (degradation).
func (h *HostServe) Start(ctx context.Context) error {
	if !h.Enabled() {
		return fmt.Errorf("host opencode serve disabled (ORCHICON_OPCODE_SESSION_TRANSPORT=0)")
	}
	password, err := randomPassword()
	if err != nil {
		return fmt.Errorf("host serve password: %w", err)
	}
	h.mu.Lock()
	h.password = password
	h.mu.Unlock()

	if err := h.seedAuth(); err != nil {
		h.log.Warn("host serve: seed model auth", "error", err)
	}

	if err := h.startOnce(ctx); err != nil {
		return err
	}
	h.log.Info("host opencode serve ready",
		"url", h.URL(), "data_dir", h.dataDir, "pid", h.pid())
	return nil
}

// Watch supervises the serve: it polls /global/health and restarts the
// process with backoff when it dies, so the session host stays up. A
// restart preserves the data dir, so sessions survive (the client
// re-attaches by session id). Blocks until ctx is cancelled.
func (h *HostServe) Watch(ctx context.Context) {
	backoff := 5 * time.Second
	const maxBackoff = 60 * time.Second
	for {
		select {
		case <-ctx.Done():
			h.Stop()
			return
		case <-time.After(15 * time.Second):
		}
		client := h.Client()
		if client == nil || client.Healthy(ctx) {
			backoff = 5 * time.Second
			continue
		}
		h.log.Warn("host opencode serve unhealthy — restarting", "backoff", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		h.kill()
		if err := h.startOnce(ctx); err != nil {
			h.log.Error("host opencode serve restart failed", "error", err)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
		h.log.Info("host opencode serve restarted", "url", h.URL())
	}
}

// Stop kills the serve process and releases the guard.
func (h *HostServe) Stop() {
	h.kill()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.guard != nil {
		h.guard.Close()
		h.guard = nil
	}
	h.client = nil
	h.started = false
}

func (h *HostServe) pid() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd != nil && h.cmd.Process != nil {
		return h.cmd.Process.Pid
	}
	return 0
}

func (h *HostServe) kill() {
	h.mu.Lock()
	cmd := h.cmd
	h.cmd = nil
	h.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// serveConfig returns the OPENCODE_CONFIG_CONTENT for the shared serve:
// the operator's MCP servers, the hard permission deny rules, and the
// Orchicon MCP (tenant-scoped). Agent prompts are NOT baked — the worker
// system prompt is delivered per message via the prompt_async `system`
// field (opencode applies it per turn).
func (h *HostServe) serveConfig() string {
	cfg := BuildConfigContent(ConfigOptions{
		AgentName:   workerAgent,
		AgentPrompt: "",
		ModelRef:    "",
		TenantID:    serveTenantID(),
		OrchiconMCP: true,
	})
	return cfg
}

// startOnce spawns the serve and waits for readiness. Caller holds no
// lock; mutates h.cmd under the lock. The port is stable across restarts
// (first spawn picks it, watchdog restarts reuse it) so the session
// client URL never changes.
func (h *HostServe) startOnce(ctx context.Context) error {
	h.mu.Lock()
	port := h.port
	h.mu.Unlock()
	if port == 0 {
		var err error
		port, err = freePort()
		if err != nil {
			return fmt.Errorf("host serve port: %w", err)
		}
	}
	serveConfig := h.serveConfig()
	password := h.Password()

	// No-project execution guard on PATH: destructive commands are refused
	// unconditionally and every absolute-path target is blocked; relative
	// paths stay inside each session's directory.
	g, err := guard.NewExecutionGuard("")
	if err != nil {
		return fmt.Errorf("host serve guard: %w", err)
	}

	binary, err := exec.LookPath("opencode")
	if err != nil {
		g.Close()
		if home, herr := os.UserHomeDir(); herr == nil {
			cand := filepath.Join(home, ".opencode", "bin", "opencode")
			if st, serr := os.Stat(cand); serr == nil && !st.IsDir() {
				binary = cand
			}
		}
	}
	if binary == "" {
		g.Close()
		return fmt.Errorf("opencode binary not found (host serve disabled; falling back to one-shot runs)")
	}

	env := append(os.Environ(),
		ServePasswordEnv+"="+password,
		"OPENCODE_CONFIG_CONTENT="+serveConfig,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
	env = g.Apply(env)
	env = setEnvKV(env, "XDG_DATA_HOME", h.dataDir)

	cmd := exec.CommandContext(ctx, binary,
		"serve", "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port))
	cmd.Env = env
	cmd.Stdout = os.Stderr // serve logs go to the plane's stderr/log file
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		g.Close()
		return fmt.Errorf("host serve start: %w", err)
	}

	h.mu.Lock()
	h.cmd = cmd
	h.port = port
	h.guard = g
	h.client = NewSessionClient(fmt.Sprintf("http://127.0.0.1:%d", port), password, "")
	h.started = true
	h.mu.Unlock()

	// Wait for readiness (cold start loads providers/models/MCP).
	client := h.Client()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if client.Healthy(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			h.kill()
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	h.kill()
	return fmt.Errorf("host serve did not become ready within 90s")
}

// seedAuth copies the operator's opencode auth.json into the dedicated
// data dir so the serve can authenticate to the model providers without
// sharing the real opencode data store.
func (h *HostServe) seedAuth() error {
	if h.dataDir == "" {
		return nil
	}
	src := filepath.Join(h.authSource(), "auth.json")
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dir := filepath.Join(h.dataDir, "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "auth.json"), b, 0o600)
}

// authSource is the operator's real opencode data dir.
func (h *HostServe) authSource() string {
	if h.home != "" {
		return filepath.Join(h.home, ".local", "share", "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode")
}

// serveTenantID returns the plane's tenant for the built-in Orchicon MCP
// registered on the shared serve. Matches the plane's hardcoded default
// tenant ("tnt_dev", see internal/server/server.go) unless overridden.
func serveTenantID() string {
	if t := os.Getenv("ORCHICON_MCP_TENANT_ID"); t != "" {
		return t
	}
	return "tnt_dev"
}

// freePort binds 127.0.0.1:0, reads the assigned port, and releases it.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// randomPassword returns a random URL-safe string for serve basic auth.
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return url.QueryEscape(string(b)), nil
}

// setEnvKV appends or replaces a KEY=VALUE pair in an env slice.
func setEnvKV(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		k, _ := cutEnv(kv)
		if k == key {
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

func cutEnv(kv string) (string, string) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:]
		}
	}
	return kv, ""
}

var _ = serveTenantID
