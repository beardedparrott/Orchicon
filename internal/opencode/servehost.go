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
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/guard"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/procwin"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

// HostServe is the DEMAND-KEYED opencode serve for the in-process (local) execution population:
// standalone task dispatches, follow-up executions.
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
	// startedAt is when the CURRENT process was spawned, used to notice that the operator's
	// credential store has been written since — see recycleIfCredentialsChanged.
	startedAt time.Time
	// profile is the permission profile this serve is built with.
	profile PermissionProfile

	// startMu serializes LAZY start (EnsureStarted): without it two concurrent first-demands would each
	// spawn a serve.
	startMu sync.Mutex
	// superviseCancel stops the supervision goroutine armed by
	// EnsureStarted (Watch). Nil while the serve was never started lazily.
	superviseCancel context.CancelFunc
	// startErr is the most recent EnsureStarted failure, kept so a caller
	// can surface the SAME loud reason the start produced (nil once up).
	startErr error

	// mcpSet is the RESOLVED MCP set this serve is built for, when it comes from a HostServePool entry.
	mcpSet mcpclient.Resolution
}

// NewHostServe constructs the host-serve manager.
func NewHostServe(log *slog.Logger, dataDir, home string) *HostServe {
	return &HostServe{log: log, dataDir: dataDir, home: home}
}

// NewAskHostServe constructs the Ask Orchicon host serve: the same demand-keyed serve manager, but
// built with the INTERACTIVE permission profile.
func NewAskHostServe(log *slog.Logger, dataDir, home string) *HostServe {
	return &HostServe{log: log, dataDir: dataDir, home: home, profile: ProfileInteractive}
}

// PermissionProfile reports the profile this serve was built with.
func (h *HostServe) PermissionProfile() PermissionProfile {
	return h.profile
}

// SetMCPSet binds the RESOLVED MCP set this serve is built for (used by HostServePool).
func (h *HostServe) SetMCPSet(set mcpclient.Resolution) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mcpSet = set
}

// Enabled reports whether the host serve is configured for this plane.
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

// Start spawns the serve, waits for it to answer /global/health, and builds the session client.
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
	// ARMED HERE, NOT ONLY IN EnsureStarted.
	h.armSupervision()
	h.log.Info("host opencode serve ready",
		"url", h.URL(), "data_dir", h.dataDir, "pid", h.pid())
	return nil
}

// armSupervision starts a watchdog for this serve — and starts NONE if one is already running.
func (h *HostServe) armSupervision() {
	h.mu.Lock()
	already := h.superviseCancel != nil
	h.mu.Unlock()
	if already {
		return
	}
	sc, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	// RE-CHECK UNDER THE SAME LOCK that installs it: two concurrent Starts must not both arm.
	if h.superviseCancel != nil {
		h.mu.Unlock()
		cancel()
		return
	}
	h.superviseCancel = cancel
	h.mu.Unlock()
	go h.Watch(sc)
}

// Watch supervises the serve: it polls /global/health and restarts the process with backoff when it
// dies, so the session host stays up.
var watchLiveHook func(delta int)

func (h *HostServe) Watch(ctx context.Context) {
	if watchLiveHook != nil {
		watchLiveHook(1)
		defer watchLiveHook(-1)
	}
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
		// LIVENESS, NOT DISPATCH-READINESS.
		if serveAnswering(ctx, client) {
			backoff = 5 * time.Second
			continue
		}
		h.log.Warn("host opencode serve unhealthy — restarting", "backoff", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		// SERIALIZED AGAINST A CONCURRENT EnsureStarted.
		h.startMu.Lock()
		if h.ready() {
			// Somebody else got there first. A DEAD serve no longer reports ready (its exit clears the
			// state), so this can only be a live one.
			h.startMu.Unlock()
			backoff = 5 * time.Second
			continue
		}
		h.kill()
		startErr := h.startOnce(ctx)
		h.startMu.Unlock()
		if startErr != nil {
			h.log.Error("host opencode serve restart failed", "error", startErr)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
		h.log.Info("host opencode serve restarted", "url", h.URL())
	}
}

// recycleIfCredentialsChanged recycles a RUNNING serve when the operator has signed in to a
// provider since it was spawned. THE CALLER MUST HOLD startMu.
func (h *HostServe) recycleIfCredentialsChanged(ctx context.Context) {
	if h.log == nil || h.home == "" {
		return
	}
	h.mu.Lock()
	started, ready := h.startedAt, h.started
	h.mu.Unlock()
	if !ready || started.IsZero() {
		return
	}
	at, ok := credentialStoreWrittenAt(h.home)
	if !ok || !credentialStoreIsNewer(at, started) {
		return
	}
	h.log.Info("host opencode serve: the operator's credential store changed since this serve started — "+
		"recycling it so the new credentials are delivered",
		"store_written", at.UTC().Format(time.RFC3339), "serve_started", started.UTC().Format(time.RFC3339))
	h.kill()
}

// credentialStoreIsNewer reports whether a store written at `written` postdates a serve started at
// `started`, with a small allowance so a sign-in landing in the same second-ish as the spawn does.
func credentialStoreIsNewer(written, started time.Time) bool {
	return written.After(started.Add(2 * time.Second))
}

// credentialStoreWrittenAt is when the operator's opencode credential store was last written.
func credentialStoreWrittenAt(home string) (time.Time, bool) {
	dir := runtime.OpenCodeStoreDir(home)
	if dir == "" {
		return time.Time{}, false
	}
	var newest time.Time
	found := false
	for _, name := range []string{"opencode.db", "auth.json"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			if at := st.ModTime(); !found || at.After(newest) {
				newest, found = at, true
			}
		}
	}
	return newest, found
}

// EnsureStarted starts the host serve ON FIRST DEMAND and arms its supervision, or returns
// immediately when it is already up.
func (h *HostServe) EnsureStarted(ctx context.Context) error {
	h.startMu.Lock()
	defer h.startMu.Unlock()
	h.recycleIfCredentialsChanged(ctx)
	if h.ready() {
		return nil
	}
	if !h.Enabled() {
		err := fmt.Errorf("host opencode serve disabled (ORCHICON_OPCODE_SESSION_TRANSPORT=0)")
		h.mu.Lock()
		h.startErr = err
		h.mu.Unlock()
		return err
	}
	if err := h.Start(ctx); err != nil {
		h.markStartFailed(err)
		h.log.Warn("host opencode serve lazy start failed", "error", err)
		return err
	}
	// NO WATCH ARMING HERE — Start armed it (armSupervision).
	h.mu.Lock()
	h.startErr = nil
	h.mu.Unlock()
	h.log.Info("host opencode serve started on demand", "url", h.URL())
	return nil
}

// StartError returns the most recent lazy-start failure (nil once the
// serve is up), so a caller can surface the same loud reason later.
func (h *HostServe) StartError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.startErr
}

// markStartFailed records a failed lazy start AND clears any PARTIAL start state.
func (h *HostServe) markStartFailed(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.guard != nil {
		h.guard.Close()
		h.guard = nil
	}
	h.started = false
	h.client = nil
	h.startErr = err
}

// ready reports whether a live serve + client are in place (h.mu-guarded).
func (h *HostServe) ready() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.started && h.client != nil
}

// reapAsync waits for a serve process and records its exit.
func (h *HostServe) reapAsync(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		h.markExited(cmd, cmd.Wait())
	}()
}

// markExited records that a serve process has EXITED, and is the only place that happens for a
// serve that died on its own. IT CLEARS STATE ONLY WHEN cmd IS STILL THE CURRENT ONE.
func (h *HostServe) markExited(cmd *exec.Cmd, err error) {
	h.mu.Lock()
	current := h.cmd == cmd
	if current {
		h.cmd = nil
		h.client = nil
		h.started = false
		h.port = 0
	}
	h.mu.Unlock()
	if current {
		h.log.Warn("host opencode serve exited — supervision will restart it", "error", err)
	}
}

// serveAnswering reports whether the serve is currently answering, which is the ONLY question the
// watchdog asks.
func serveAnswering(ctx context.Context, c *SessionClient) bool {
	return c != nil && c.Healthy(ctx)
}

// Stop kills the serve process, stops its supervision, and releases the
// guard.
func (h *HostServe) Stop() {
	h.kill()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.superviseCancel != nil {
		h.superviseCancel()
		h.superviseCancel = nil
	}
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
		// NOT cmd.Wait() — the goroutine armed by startOnce owns the reap.
	}
}

// serveConfig returns the OPENCODE_CONFIG_CONTENT for the shared serve: the operator's MCP servers,
// the hard permission deny rules, and the Orchicon MCP (tenant-scoped).
func (h *HostServe) serveConfig() string {
	h.mu.Lock()
	set := h.mcpSet
	h.mu.Unlock()
	cfg := BuildConfigContent(ConfigOptions{
		AgentName:         workerAgent,
		AgentPrompt:       sessionToolShell,
		DefaultAgent:      workerAgent,
		ModelRef:          "",
		TenantID:          serveTenantID(),
		OrchiconMCP:       true,
		PermissionProfile: h.profile,
		RunMCP:            set.Servers,
		RunSkills:         set.Skills,
		// Shape is deliberately NOT set: BuildConfigContent resolves it from the same version probe the
		// session client uses (setAPIShape, in startOnce).
	})
	return cfg
}

// startOnce spawns the serve and waits for readiness.
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

	// A GUARD PROBLEM MUST NOT DISABLE CHAT.
	g, err := guard.NewExecutionGuard("")
	if err != nil {
		h.log.Warn("host opencode serve: OS execution guard NOT applied — bash commands run without "+
			"the PATH shim (opencode's own permission rules still apply)", "error", err)
	}
	if g != nil && !g.Enabled() {
		h.log.Warn("host opencode serve: OS execution guard unavailable on this platform — commands "+
			"run without the PATH shim (opencode's own permission rules still apply)",
			"reason", g.DisabledReason())
	}

	// The SAME resolver the adapter spawns through, so the host serve and the in-container spawn can
	// never disagree about which opencode is 'the' one.
	binary, err := resolveOpenCodeBinary()
	if err != nil {
		g.Close()
		return fmt.Errorf("host serve disabled; executions fail fast — no one-shot fallback: %w", err)
	}

	// THE OPERATOR'S PROVIDER CREDENTIALS, DELIVERED BY ENVIRONMENT.
	env := append(os.Environ(),
		ServePasswordEnv+"="+password,
		"OPENCODE_CONFIG_CONTENT="+serveConfig,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
	// BEST-EFFORT AND NEVER FATAL: a host with no opencode credentials is the native-only case, and a
	// serve that starts without them behaves exactly as it did before this.
	if pairs, unusable := h.credentialEnv(); len(pairs) > 0 {
		env = append(env, pairs...)
		h.log.Info("host opencode serve: provider credentials delivered by environment",
			"providers", len(pairs), "undeliverable", strings.Join(unusable, ","))
	} else if len(unusable) > 0 {
		h.log.Warn("host opencode serve: signed-in providers cannot be delivered through the environment — "+
			"turns on them will fail to authenticate", "undeliverable", strings.Join(unusable, ","))
	} else if credentialGapIsReportable(len(pairs), len(unusable), h.operatorHasCredentialStore()) {
		// NOTHING DELIVERED, BUT THE OPERATOR HAS A STORE — that combination used to be completely silent,
		// and the silence is what made the reported incident take weeks to find.
		h.log.Warn("host opencode serve: the operator HAS an opencode credential store but NOTHING was resolved "+
			"from it — the serve will authenticate with whatever its own fresh data dir contains, or with "+
			"nothing at all. A path bug in the resolver looks exactly like this.",
			"store", h.authSource())
	}
	env = g.Apply(env)
	env = setEnvKV(env, "XDG_DATA_HOME", h.dataDir)
	// Stamp the plane spawn marker: it is how the boot sweep tells a process
	// this plane started (and left behind) from the operator's own opencode.
	env = setEnvKV(env, PlaneSpawnEnv, "1")

	cmd := procwin.CommandContext(ctx, binary,
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
	// WHICH API THIS SERVE SPEAKS, asked of the binary that IS the serve.
	h.client.setAPIShape(shapeForBinary(ctx, h.log, binary))
	h.started = true
	// WHEN THIS PROCESS WAS SPAWNED. The environment (and so the provider credentials) is fixed at
	// spawn, so this is the clock a later sign-in has to beat to be noticed.
	h.startedAt = time.Now()
	h.mu.Unlock()

	// It clears them ONLY while `cmd` is still the current one, so a deliberate kill during a restart
	// cannot null out the replacement's state.
	h.reapAsync(cmd)

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

// credentialResolver resolves the operator's provider credentials for this serve. A package var so a test can
// assert the DELIVERY without a real opencode install, and so the production wiring is one readable line.
var credentialResolver = runtime.AdapterCredentialEnv

// credentialEnv returns this serve's provider credentials as KEY=VALUE pairs, plus the provider ids that could
// not be delivered that way.
func (h *HostServe) credentialEnv() ([]string, []string) {
	if h.dataDir == "" {
		return nil, nil
	}
	// THE OPERATOR'S HOME DIRECTORY, NOT THEIR OPENCODE DATA DIRECTORY.
	//
	// REPORTED: "When using the opencode adapter and then using opencode-go as the provider, I get … This
	// API key has expired … That same API key works just fine using the orchicon adapter. Something is not
	// picking up the API key still."
	//
	// It was picking up nothing at all. The resolver takes HOME and appends `.local/share/opencode/…`
	// itself (openCodeDBPath/openCodeAuthPath), and this call passed authSource() — which ALREADY IS
	// `~/.local/share/opencode`. Every lookup therefore composed a path that cannot exist:
	//
	//	passed   ~/.local/share/opencode
	//	looked   ~/.local/share/opencode/.local/share/opencode/opencode.db   ← never there
	//
	// Both stores missed, so no variables were emitted, no `unusable` ids were recorded either (nothing was
	// read to judge), and the whole delivery was SILENT — which is why there is no "provider credentials
	// delivered" line in the plane log and why this looked like the key was simply not being read.
	//
	// The serve then started with no credential in its environment and fell back to opencode's own store,
	// where the operator's `opencode` integration still holds the key they expired weeks ago. Measured on
	// that host: the `opencode` integration answers 401 api_key_expired while `opencode-go` — the one they
	// rotated, and the one the most-recently-updated rule selects — answers 200 on both zen endpoints.
	//
	// So one wrong directory produced the entire reported symptom: the right key was in the store the whole
	// time, and the resolver that knows how to choose it was never pointed at it.
	return credentialResolver(h.operatorHome(), adapter.DefaultAdapterKind)
}

// credentialGapIsReportable answers whether "resolved nothing" must be REPORTED rather than passed
// over.
func credentialGapIsReportable(pairs, unusable int, storePresent bool) bool {
	return pairs == 0 && unusable == 0 && storePresent
}

// operatorHasCredentialStore reports whether the operator's opencode keeps credentials ANYWHERE we
// know how to read — the 2.x SQLite store or the 1.x auth.json.
func (h *HostServe) operatorHasCredentialStore() bool {
	if operatorUsesSQLiteStore(h.authSource()) {
		return true
	}
	_, err := os.Stat(filepath.Join(h.authSource(), "auth.json"))
	return err == nil
}

// operatorHome is the operator's home directory — the base the credential resolver appends
// `.local/share/opencode` to. Split out so credentialEnv and authSource cannot disagree about it again.
func (h *HostServe) operatorHome() string {
	if h.home != "" {
		return h.home
	}
	home, _ := os.UserHomeDir()
	return home
}

// seedAuth gives the serve a credential store it can read, on the versions that keep credentials in
// a FILE. IT IS STILL REQUIRED ON 1.x, AND THAT IS WHY THIS IS A GATE RATHER THAN A DELETION.
func (h *HostServe) seedAuth() error {
	if h.dataDir == "" {
		return nil
	}
	if operatorUsesSQLiteStore(h.authSource()) {
		h.log.Info("host serve: operator's opencode keeps credentials in SQLite — not copying auth.json "+
			"(2.x would import a stale copy once and prefer it); provider credentials are resolved from the "+
			"live stores and delivered by environment instead", "data_dir", h.dataDir)
		return nil
	}
	if h.authSource() == "" {
		// NO KNOWN STORE: nothing to copy, and filepath.Join("", "auth.json") would be a
		// RELATIVE path resolved against the plane's working directory.
		return nil
	}
	src := filepath.Join(h.authSource(), "auth.json")
	b, err := os.ReadFile(src)
	if err != nil {
		// ABSENT IS NOT A FAILURE.
		if os.IsNotExist(err) {
			return nil
		}
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
	return runtime.OpenCodeStoreDir(h.operatorHome())
}

// operatorUsesSQLiteStore reports whether the operator's opencode keeps credentials in the 2.x
// SQLite store rather than in auth.json.
func operatorUsesSQLiteStore(opencodeDataDir string) bool {
	_, err := os.Stat(filepath.Join(opencodeDataDir, "opencode.db"))
	return err == nil
}

// serveTenantID returns the plane's tenant for the built-in Orchicon MCP registered on the shared
// serve.
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
