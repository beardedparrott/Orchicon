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

// HostServe is the DEMAND-KEYED opencode serve for the in-process (local)
// execution population: standalone task dispatches, follow-up executions,
// and any execution that isn't bound to a workflow-run runtime container.
// One serve per plane hosts a persistent session per execution; the
// control plane supervises it (spawn on FIRST DEMAND, health watchdog,
// restart with backoff) so the session host is never down once it exists.
//
// It is deliberately NOT started at plane boot: a plane whose adapter demand
// set contains no opencode (the default for a fresh tenant — native-only
// worker refs) is "opencode-free", and staying in that state must cost
// nothing. EnsureStarted is the single demand-keyed entry point every
// opencode consumer goes through.
//
// Isolation: the serve runs against a DEDICATED data dir
// (~/.local/share/orchicon/opencode) seeded with the operator's model
// auth, so it never shares an opencode.db with the operator's own
// opencode instances. Sessions persist there across serve restarts, so a
// restart is transparent to sessions (the client re-attaches by id).
//
// CROSS-PROJECT UNION IS FORBIDDEN ON A SHARED SERVE. This serve's config is
// built ONCE (serveConfig) and lives for the plane, so unioning several
// projects' MCP sets onto it would make one project's servers visible in
// another project's session — a leak with no consent boundary. A session
// whose set differs must run on a serve built for that set (HostServePool,
// servepool.go), each with its OWN data dir; the pool is why this is safe to
// state absolutely, and it is the strategy the runtime-container bake uses
// too (one serve per run, config applied once).
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
	// profile is the permission profile this serve is built with. The zero
	// value ("") normalizes to ProfileWorker, so a serve built by
	// NewHostServe carries the worker sandbox; NewAskHostServe opts into
	// ProfileInteractive.
	profile PermissionProfile

	// startMu serializes LAZY start (EnsureStarted): without it two
	// concurrent first-demands would each spawn a serve. It is held only
	// across start + supervision arming, never by the serving paths
	// themselves.
	startMu sync.Mutex
	// superviseCancel stops the supervision goroutine armed by
	// EnsureStarted (Watch). Nil while the serve was never started lazily.
	superviseCancel context.CancelFunc
	// startErr is the most recent EnsureStarted failure, kept so a caller
	// can surface the SAME loud reason the start produced (nil once up).
	startErr error

	// mcpSet is the RESOLVED MCP set this serve is built for, when it comes
	// from a HostServePool entry. It is baked into the serve's config ONCE
	// (serveConfig), so it must be the exact set the sessions landing on this
	// serve are entitled to and nothing else — a cross-project UNION here
	// would leak one project's servers into another project's session. The
	// empty set (the default serve) means today's config, unchanged.
	mcpSet mcpclient.Resolution
}

// NewHostServe constructs the host-serve manager. dataDir is the
// dedicated opencode data directory (created + seeded with model auth on
// Start). home overrides the operator home used to locate the real
// opencode data dir whose auth.json is seeded (empty = os.UserHomeDir).
func NewHostServe(log *slog.Logger, dataDir, home string) *HostServe {
	return &HostServe{log: log, dataDir: dataDir, home: home}
}

// NewAskHostServe constructs the Ask Orchicon host serve: the same demand-keyed
// serve manager, but built with the INTERACTIVE permission profile.
//
// It is a SEPARATE process because opencode's permission config is per-process
// (it rides OPENCODE_CONFIG_CONTENT on the serve's environment), so one serve
// cannot carry the worker sandbox for executions and the interactive profile
// for Ask at the same time. Callers must give it its OWN data dir: sharing a
// sqlite data dir between two serve processes is unsafe, and Ask keeps no
// session continuity across the split anyway (a stale session id takes the
// existing ErrSessionNotFound recreate-and-re-seed path).
func NewAskHostServe(log *slog.Logger, dataDir, home string) *HostServe {
	return &HostServe{log: log, dataDir: dataDir, home: home, profile: ProfileInteractive}
}

// PermissionProfile reports the profile this serve was built with.
func (h *HostServe) PermissionProfile() PermissionProfile {
	return h.profile
}

// SetMCPSet binds the RESOLVED MCP set this serve is built for (used by
// HostServePool). It must be called before the serve starts: the set is baked
// into OPENCODE_CONFIG_CONTENT ONCE. The empty set is the default (today's
// config).
func (h *HostServe) SetMCPSet(set mcpclient.Resolution) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mcpSet = set
}

// Enabled reports whether the host serve is configured for this plane.
// It is disabled when the operator set ORCHICON_OPCODE_SESSION_TRANSPORT=0
// (global kill-switch) — with the one-shot path removed, a disabled
// transport means local executions FAIL fast rather than degrading.
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
// serve cannot come up, it returns an error — with the one-shot path
// removed, executions that need this serve fail fast instead.
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
	// ARMED HERE, NOT ONLY IN EnsureStarted. Supervision used to be armed by ONE caller, so a serve
	// started any other way — a boot-time Start, a future direct call — ran completely unsupervised,
	// and nothing said so. Arming inside Start makes "a serve exists" and "a serve is watched" the same
	// fact, which is the only version of this that cannot drift.
	h.armSupervision()
	h.log.Info("host opencode serve ready",
		"url", h.URL(), "data_dir", h.dataDir, "pid", h.pid())
	return nil
}

// armSupervision starts a watchdog for this serve — and starts NONE if one is already running.
//
// IDEMPOTENT BY REFUSAL, NOT BY REPLACEMENT, and that distinction is load-bearing. Cancelling a previous
// watchdog to install a new one looks tidier and is a trap: Watch's own shutdown path calls h.Stop(), so a
// SUPERSEDED watchdog waking on its cancelled context would tear down the replacement's serve on its way
// out. Refusing to arm a second watchdog means that situation cannot arise — one watchdog survives across
// any number of serve restarts, which is exactly what supervision is for.
//
// h.superviseCancel is cleared by Stop() (and only there), so a watchdog that has legitimately been shut
// down can be replaced on the next Start.
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

// Watch supervises the serve: it polls /global/health and restarts the
// process with backoff when it dies, so the session host stays up. A
// restart preserves the data dir, so sessions survive (the client
// re-attaches by session id). Blocks until ctx is cancelled.
// watchLiveHook is a TEST SEAM reporting watchdog entry (+1) and exit (-1), so "exactly one watchdog is
// running" can be asserted rather than inferred from the source. Nil in production.
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
		// LIVENESS, NOT DISPATCH-READINESS. This asked MCPHealthy — which CREATES A REAL SESSION every
		// 15 seconds — and that conflated two different questions with a measurable cost:
		//
		//   * creating a session on opencode 2.x can have to BOOT the managed service for the location
		//     (the same eviction/re-boot race that makes `opencode models` intermittently empty), so a
		//     probe that is merely SLOW reads as DEAD;
		//   * the watchdog then restarts a serve that was healthy, which KILLS IN-FLIGHT TURNS and
		//     leaves a refused-connection window until the replacement listens — reported as a turn
		//     that spun on "thinking" without ever returning, followed by "connection refused" on the
		//     retry;
		//   * and it leaked a session into the store every 15 seconds (aborted, never deleted).
		//
		// Measured on the operator's prod plane: "host opencode serve ready" followed by "unhealthy —
		// restarting" three times in one evening, on a serve that was answering the whole time.
		//
		// The watchdog's only question is "is the process still answering?" — Healthy answers that,
		// cheaply and definitively, and a genuinely wedged or dead serve fails it. WHETHER a serve can
		// accept work is asked where it matters, at dispatch, by ProbeUsable (the run-start gate).
		// A NIL CLIENT IS NOT A HEALTHY SERVE. This read `client == nil || client.Healthy(ctx)` and
		// CONTINUED on both, so a Watch that outlived a Stop() — which nils the client — spun forever
		// reporting nothing while no serve existed at all. Nil means there is nothing to answer with,
		// which is precisely when a restart is due.
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
		// SERIALIZED AGAINST A CONCURRENT EnsureStarted. Both paths can decide to (re)start, and two
		// startOnce calls racing would leave two serve processes chasing one port — the second wins the
		// bind and the first is leaked. startMu is the same lock EnsureStarted holds, so the loser waits
		// and then finds the serve already up instead of stacking another.
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

// EnsureStarted starts the host serve ON FIRST DEMAND and arms its
// supervision, or returns immediately when it is already up. This is the
// demand-keyed entry point that makes the plane lazy (AC 2): a plane whose
// adapter demand set contains no opencode never calls it, so no serve is
// spawned and no opencode binary is probed (AC 1).
//
// It is the operator kill-switch's fail-fast point too (AC 4): with
// ORCHICON_OPCODE_SESSION_TRANSPORT=0 it returns the disabled error rather
// than silently degrading, and the caller fails the dispatch with that
// reason.
//
// Supervision is unchanged from the boot-spawned topology: Watch polls
// /global/health and restarts the process with 5s→60s backoff, and the
// dedicated data dir means sessions survive a restart (the client
// re-attaches by session id).
//
// No idle shutdown ships (DC3): once started, the serve lives for the plane
// lifetime under Watch. A demand-counted idle reap can be added later behind
// this same seam without changing any caller.
func (h *HostServe) EnsureStarted(ctx context.Context) error {
	h.startMu.Lock()
	defer h.startMu.Unlock()
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
	// NO WATCH ARMING HERE — Start armed it (armSupervision). Arming in both places spawned a SECOND
	// watchdog on every lazy start and then overwrote the handle to the first, leaking it; two watchdogs
	// thereafter both raced to restart one serve.
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

// markStartFailed records a failed lazy start AND clears any PARTIAL start
// state. A failed Start can leave startOnce having already armed `started`
// and `client` for a process it then killed (a readiness timeout, or a
// first-demand ctx cancelled during the readiness wait); if that survived,
// ready() would report a DEAD serve as up, so the next demand would skip the
// start entirely and no supervision would ever be armed. Resetting forces the
// next demand to retry, and releases the execution guard the failed attempt
// installed.
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

// reapAsync waits for a serve process and records its exit. THE WAIT IS THE POINT: it is what stops an
// exited serve from lingering as a defunct process, and it is the only thing that notices the exit at all
// for a serve that died on its own.
func (h *HostServe) reapAsync(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		h.markExited(cmd, cmd.Wait())
	}()
}

// markExited records that a serve process has EXITED, and is the only place that happens for a serve that
// died on its own.
//
// IT CLEARS STATE ONLY WHEN cmd IS STILL THE CURRENT ONE. A restart kills the old process and starts a
// replacement; the old process's reaper then fires with a stale cmd, and clearing here would null out the
// fields belonging to the serve that is now running — after which ready() would report a live serve as
// absent and EnsureStarted would stack a second process on the same port.
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
//
// A NIL CLIENT IS NOT ANSWERING. This is the whole of the previous inversion: nil means there is no
// client to ask, i.e. nothing is serving, which is precisely when a restart is due — not the healthy
// state the old `client == nil || ...` condition treated it as.
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
		// NOT cmd.Wait() — the goroutine armed by startOnce owns the reap. Waiting here would race it
		// ("Wait was already called") and would restore the very window this file now closes: a serve
		// that died on its own would again be reaped only by whichever path happened to run next.
	}
}

// serveConfig returns the OPENCODE_CONFIG_CONTENT for the shared serve:
// the operator's MCP servers, the hard permission deny rules, and the
// Orchicon MCP (tenant-scoped). Agent prompts are NOT baked — the worker
// system prompt is delivered per message via the prompt_async `system`
// field (opencode applies it per turn).
//
// CROSS-PROJECT UNION IS FORBIDDEN ON A SHARED SERVE. This config is built
// ONCE and lives for the plane, so unioning several projects' MCP sets onto
// it would make one project's servers visible in another project's session —
// a leak with no consent boundary. A session whose set differs must run on a
// serve built for that set (HostServePool, servepool.go); the pool is why
// this is safe to state absolutely. h.mcpSet is exactly ONE resolved set (the
// empty set = today's config), never a union across projects.
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
		// Shape is deliberately NOT set: BuildConfigContent resolves it from the
		// same version probe the session client uses (setAPIShape, in startOnce),
		// so the routes and the injected config cannot disagree about which
		// opencode this is.
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

	// The SAME resolver the adapter spawns through, so the host serve and the
	// in-container spawn can never disagree about which opencode is 'the' one.
	// This used to be a second copy of the probe that ignored ORCHICON_OPENCODE_BIN.
	binary, err := resolveOpenCodeBinary()
	if err != nil {
		g.Close()
		return fmt.Errorf("host serve disabled; executions fail fast — no one-shot fallback: %w", err)
	}

	// THE OPERATOR'S PROVIDER CREDENTIALS, DELIVERED BY ENVIRONMENT.
	//
	// REPORTED: every Ask turn through the opencode adapter failed with "This API key has expired … Signing in
	// again will not renew this key", while the operator's own `opencode` worked fine. The cause was here: this
	// serve only ever received a COPY of the operator's auth.json (seedAuth), which opencode 2.x wrote last in
	// a 1.x-era install and never updates — so the copy held a key the operator had long since rotated. 2.x
	// keeps its real credentials in its SQLite store, and re-seeding auth.json cannot help because the store
	// imports that file only ONCE, at migration.
	//
	// THE FIX IS THE CHANNEL THE CONTAINER PATH ALREADY USES. internal/runtime resolves the credential from BOTH
	// stores (database wins) and hands it to a runtime container as the environment; the host serve was simply
	// never given the same treatment, so the two shapes disagreed about which credential a serve runs with.
	//
	// MEASURED ON 2.x BEFORE CHOOSING THIS: against the SAME stale store, prompting opencode-go fails with the
	// reported error, and with the resolved key in OPENCODE_API_KEY the same prompt SUCCEEDS. The environment
	// outranks the stored credential, which is why this needs no store surgery, no re-import and nothing
	// deleted — the serve only has to be told.
	env := append(os.Environ(),
		ServePasswordEnv+"="+password,
		"OPENCODE_CONFIG_CONTENT="+serveConfig,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
	// BEST-EFFORT AND NEVER FATAL: a host with no opencode credentials is the native-only case, and a serve
	// that starts without them behaves exactly as it did before this. The unresolved ids are logged so a silent
	// gap has a trace, instead of surfacing later as an authentication failure with no cause.
	if pairs, unusable := h.credentialEnv(); len(pairs) > 0 {
		env = append(env, pairs...)
		h.log.Info("host opencode serve: provider credentials delivered by environment",
			"providers", len(pairs), "undeliverable", strings.Join(unusable, ","))
	} else if len(unusable) > 0 {
		h.log.Warn("host opencode serve: signed-in providers cannot be delivered through the environment — "+
			"turns on them will fail to authenticate", "undeliverable", strings.Join(unusable, ","))
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
	// WHICH API THIS SERVE SPEAKS, asked of the binary that IS the serve. opencode 2.x moved every
	// session endpoint under /api (apisurface.go); a client that assumed v1 against a 2.x serve
	// would fail every turn on a 404 and name no endpoint.
	h.client.setAPIShape(shapeForBinary(ctx, h.log, binary))
	h.started = true
	h.mu.Unlock()

	// REAP THE CHILD IN A GOROUTINE, AND LET ITS EXIT BE THE DEATH SIGNAL.
	//
	// TWO DEFECTS DIE HERE. cmd.Wait() used to be called ONLY from kill(), so a serve that exited on
	// its own stayed DEFUNCT until the watchdog's next 15-second poll happened to reap it — the zombie
	// processes the operator saw in ps. And nothing RECORDED the death: `started` and `client` were
	// cleared only by Stop() and markStartFailed(), so ready() kept answering TRUE for a serve that was
	// already gone. EnsureStarted therefore short-circuited ("already up") and the dispatch proceeded to
	// fail with "connection refused" instead of restarting.
	//
	// A goroutine that WAITS is both halves at once: the child is reaped the instant it exits, and the
	// exit clears the exact fields ready() reads. It clears them ONLY while `cmd` is still the current
	// one, so a deliberate kill during a restart cannot null out the replacement's state.
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
	return credentialResolver(h.authSource(), adapter.DefaultAdapterKind)
}

// seedAuth gives the serve a credential store it can read, on the versions that keep credentials in a FILE.
//
// IT IS A NO-OP ON 2.x, and that is the fix rather than an omission. On 2.x auth.json is a FOSSIL: the store
// imports it ONCE, at migration, and never rewrites it — so copying it into a fresh data dir is what makes the
// CLI import a DEAD credential at first boot. Measured on the reporting host: the file was last written
// 2026-09-07 while the operator had rotated the key on 2026-10-24, and every Ask turn through the opencode
// adapter failed with "This API key has expired … Signing in again will not renew this key". Re-seeding cannot
// repair it either, because the import happens exactly once.
//
// IT IS STILL REQUIRED ON 1.x, AND THAT IS WHY THIS IS A GATE RATHER THAN A DELETION. On 1.x auth.json IS the
// credential store: the derive-a-private-data-dir design means the serve starts with NO credentials at all
// unless one is copied in, and 1.x has no SQLite store to fall back on. Removing the copy outright would take
// opencode away from every 1.x operator.
//
// (That 1.x reads it is the codebase's documented understanding — see AdapterCredentialEnv's note that "On 1.x
// auth.json IS the credential store" — corroborated by 2.x migrating FROM that file, which only makes sense if
// 1.x kept its credentials there. No 1.x binary was available on the dev host to observe directly, which is why
// this is gated on a FILE-SHAPE test rather than on a version parse.)
//
// THE SIGNAL IS THE OPERATOR'S OWN opencode.db: 2.x creates it on first run, 1.x never does, and both families
// leave it in place across upgrades. One stat, no subprocess, and it cannot be wrong about which store the
// operator's own CLI is using — which is the fact that decides whether a copied auth.json will be imported or
// read live.
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
	src := filepath.Join(h.authSource(), "auth.json")
	b, err := os.ReadFile(src)
	if err != nil {
		// ABSENT IS NOT A FAILURE. A host with no opencode credentials is the native-only case, and this used to
		// return the raw ENOENT so every such plane logged "host serve: seed model auth" on each start. Anything
		// else (a permission error, a directory where a file should be) is still reported: those are the cases
		// where a copy was intended and did not happen.
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
	if h.home != "" {
		return filepath.Join(h.home, ".local", "share", "opencode")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "opencode")
}

// operatorUsesSQLiteStore reports whether the operator's opencode keeps credentials in the 2.x SQLite store
// rather than in auth.json.
//
// THE FILE'S PRESENCE IS THE SIGNAL, deliberately: 2.x creates it on first run and 1.x never does, so it answers
// "which store does the operator's own CLI use?" with one stat and no version parsing. That is the fact which
// decides whether a copied auth.json will be imported ONCE and preferred (2.x) or read live as the store (1.x).
func operatorUsesSQLiteStore(opencodeDataDir string) bool {
	_, err := os.Stat(filepath.Join(opencodeDataDir, "opencode.db"))
	return err == nil
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
