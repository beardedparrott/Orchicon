package acp

// client.go — THE HANDSHAKE AND THE CLIENT LIFECYCLE.
//
// Start spawns agy and starts the reader; it does NOT initialize. That split matters because the
// caller may want the process (and its stderr tail) before deciding what to authenticate as, and
// because `initialize` and `session/new` succeed WITHOUT auth — agy defers the credential check to
// the first model call (docs 3.12), so "session created" must never be read as "signed in".

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// Config configures Start.
type Config struct {
	// Factory spawns the child. Required — there is deliberately no implicit spawner, so a
	// caller can never accidentally start a real agy in a test.
	Factory ProcFactory
	// Launch selects the agy binary and its flags.
	Launch LaunchOptions
	// Spec is the spawn request (cwd, env, project dir, workflow run id). Its Argv is derived
	// from Launch when left empty.
	Spec ProcSpec

	// Handler answers permission requests. A nil Handler REJECTS everything — the fail-closed
	// default, not an "allow all" convenience.
	Handler Handler

	// AuthMethod is the authenticate methodId (child 4's setting). Empty means SKIP authenticate
	// and let agy use whatever credential is already on the host, which is the documented
	// behaviour for an interactive-OAuth profile.
	AuthMethod string

	ClientName    string
	ClientVersion string

	Logger *slog.Logger
}

// Client is one live `agy acp` process.
type Client struct {
	conn   *conn
	logger *slog.Logger
	name   string
	ver    string

	mu       sync.Mutex
	caps     AgentCapabilities
	auths    []AuthMethod
	initDone bool

	closeOnce sync.Once
	closeErr  error
}

// Start spawns agy and starts its reader. Initialize must be called before any session method.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Factory == nil {
		return nil, errors.New("acp: Config.Factory is required — there is no default spawner")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	spec := cfg.Spec
	if len(spec.Argv) == 0 {
		spec.Argv = cfg.Launch.Argv()
	}
	proc, err := cfg.Factory(ctx, spec)
	if err != nil {
		return nil, err
	}

	name := cfg.ClientName
	if name == "" {
		name = defaultClientName
	}
	c := &Client{
		conn:   newConn(proc, cfg.Handler, logger, startStderrDrain(proc.Stderr())),
		logger: logger,
		name:   name,
		ver:    cfg.ClientVersion,
	}
	go c.conn.read()
	return c, nil
}

// Initialize performs the `initialize` handshake and records agy's capabilities and auth
// methods. Calling it twice returns the recorded handshake rather than re-asking, because a
// second initialize is a protocol error on agy's side.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	c.mu.Lock()
	if c.initDone {
		res := &InitializeResult{AgentCapabilities: c.caps, AuthMethods: append([]AuthMethod(nil), c.auths...)}
		c.mu.Unlock()
		return res, nil
	}
	c.mu.Unlock()

	params := InitializeParams{
		ProtocolVersion: protocolVersion,
		ClientCapabilities: ClientCapabilities{
			// DECISION (see ClientCapabilities): agy never calls fs/* or terminal/*.
			FS:       FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
		ClientInfo: ClientInfo{Name: c.name, Version: c.ver},
	}
	var res InitializeResult
	if err := c.conn.call(ctx, methodInitialize, params, &res); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.caps = res.AgentCapabilities
	c.auths = res.AuthMethods
	c.initDone = true
	c.mu.Unlock()
	return &res, nil
}

// Capabilities reports what agy said it can do (the zero value before Initialize).
func (c *Client) Capabilities() AgentCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// AuthMethods reports the methods agy offered (empty before Initialize).
func (c *Client) AuthMethods() []AuthMethod {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AuthMethod(nil), c.auths...)
}

// Authenticate signs in with one of agy's offered method ids.
//
// EVERY failure is an *AuthError, not only the ones the shared signal list recognises: agy reports
// a missing API key with text matching nothing in adapter.AntigravityAuthFailure, and letting that
// through as a generic RPC error would lose the one actionable instruction the operator has.
func (c *Client) Authenticate(ctx context.Context, methodID string) error {
	if methodID == "" {
		return &AuthError{Message: adapter.AntigravityAuthRequiredMessage, Detail: "no auth method configured"}
	}
	var res struct {
		Meta json.RawMessage `json:"_meta"`
	}
	if err := c.conn.call(ctx, methodAuthenticate, AuthenticateParams{MethodID: methodID}, &res); err != nil {
		return authErrorFrom(err)
	}
	return nil
}

// Stderr returns the retained tail of the child's stderr (most recent last).
func (c *Client) Stderr() []string { return c.conn.stderr.Tail() }

// Exited reports whether the child process has gone.
func (c *Client) Exited() bool {
	select {
	case <-c.conn.done:
		return true
	default:
		return false
	}
}

// ExitErr describes why the child is gone, or nil while it is alive. It carries the exit code and
// the stderr tail, because a transport failure without the child's own diagnostic is
// unactionable.
func (c *Client) ExitErr() error {
	if !c.Exited() {
		return nil
	}
	return c.conn.closedErr()
}

// Close shuts the child down: in-flight handlers fail closed, stdin closes, then the process is
// killed if it has not exited within the grace period. Idempotent.
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		// Fail every blocked permission handler closed BEFORE the process goes, so the answer is
		// written while there is still a wire to write it on.
		c.conn.serveCancel(errTeardown)
		if err := c.conn.proc.Close(); err != nil {
			c.closeErr = err
		}
		// The reader ends when stdout closes; bound the wait so a wedged child cannot hang a
		// shutdown forever.
		wait := DefaultShutdownGrace + time.Second
		select {
		case <-c.conn.readerDone:
		case <-ctx.Done():
		case <-time.After(wait):
		}
		c.conn.stderr.stop()
	})
	return c.closeErr
}
