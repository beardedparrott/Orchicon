// Package client builds the Connect client set orch uses to talk to a
// remote Orchicon plane. It is a pure consumer of the buf-generated
// clients (api/gen/go/orchicon/api/v1/apiv1connect) — the same API source
// of truth the server and GUI build from — plus a /versionz probe.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// VersionzResponse is the JSON body of GET /versionz (public endpoint,
// internal/api/api.go: `{"version":"<tag>"}`).
type VersionzResponse struct {
	Version string `json:"version"`
}

// LoginRequest / LoginResponse mirror POST /auth/local-login
// (internal/auth/handlers.go tokenResponse: snake_case JSON). The refresh
// token is an HttpOnly browser cookie — a non-browser client only gets the
// access token, which is why password mode is secondary to API keys.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	IdentityID  string `json:"identity_id"`
	TenantID    string `json:"tenant_id"`
	IsAdmin     bool   `json:"is_admin"`
}

// bearerInterceptor injects `Authorization: Bearer <cred>` on every
// request (unary + streams). Server-side auth is resolved at the HTTP
// layer (internal/middleware/auth.go ResolveAuth), so covering the
// transport covers every RPC including server-streams.
type bearerInterceptor struct {
	cred string
}

func (b *bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.cred)
		return next(ctx, req)
	}
}

func (b *bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.cred)
		return conn
	}
}

func (b *bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client-side only; server half is a passthrough
}

// unaryDeadlineInterceptor bounds unary RPCs to d when the caller's
// context carries no deadline of its own (call-site deadlines win).
// Streaming RPCs pass through untouched — a whole-request deadline is
// what killed every TUI live stream at exactly 30s (http.Client.Timeout
// also caps response-body reads, which is a stream's entire lifetime).
type unaryDeadlineInterceptor struct{ d time.Duration }

func (u *unaryDeadlineInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, u.d)
			defer cancel()
		}
		return next(ctx, req)
	}
}

func (u *unaryDeadlineInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // streams are never capped
}

func (u *unaryDeadlineInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client-side only
}

// Options carries the resolved connection parameters.
//
// Timeout is the UNARY RPC deadline (0 → 30s). It must never cap
// server-streams: a live stream (StreamExecutionEvents, ChatStream,
// WatchTurnStream) outlives any sane whole-request deadline — heartbeats
// keep it alive and contexts/Close bound its lifetime. The deadline is
// enforced by unaryDeadlineInterceptor so call-site context deadlines
// win; http.Client.Timeout is left at zero (it kills streaming bodies).
type Options struct {
	BaseURL            string
	Token              string
	InsecureSkipVerify bool
	Timeout            time.Duration // unary RPC deadline (0 → 30s); streams never capped
}

// Clients is the v1 client set orch consumes (read-only v1 surface; §6 of
// the plan). Streams are requested through the same clients.
type Clients struct {
	Ask        apiv1connect.AskOrchiconServiceClient
	Projects   apiv1connect.ProjectServiceClient
	WorkItems  apiv1connect.WorkItemServiceClient
	Executions apiv1connect.ExecutionServiceClient
	Workflows  apiv1connect.WorkflowServiceClient
	Policies   apiv1connect.PolicyServiceClient
	Approvals  apiv1connect.ApprovalServiceClient
	Recovery   apiv1connect.RecoveryServiceClient
	Workers    apiv1connect.WorkerServiceClient
	Images     apiv1connect.RuntimeImageServiceClient
	Secrets    apiv1connect.SecretsServiceClient
	MCP        apiv1connect.MCPServiceClient
	Settings   apiv1connect.SettingsServiceClient
	Auth       apiv1connect.AuthServiceClient
	Telemetry  apiv1connect.TelemetryServiceClient
	FileEdits  apiv1connect.FileEditServiceClient

	HTTP *http.Client // underlying client (tests can stub transports)
}

// New builds the client set for the given options. baseURL is normalized
// (trailing slash trimmed). The HTTP client deliberately carries NO
// whole-request Timeout — it would kill server-streams mid-flight at the
// deadline (http.Client.Timeout caps body reads too). Unary RPCs are
// bounded by the unary deadline interceptor; dial/handshake attempts are
// bounded by the transport so a black-holed server cannot hang a dial
// forever.
func New(opts Options) *Clients {
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify},
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
	return NewWithHTTPClient(opts, httpClient)
}

// NewWithHTTPClient builds the client set over a caller-supplied HTTP
// client (tests inject one backed by httptest).
func NewWithHTTPClient(opts Options, httpClient *http.Client) *Clients {
	base := opts.BaseURL
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	interceptors := []connect.Interceptor{}
	if opts.Token != "" {
		interceptors = append(interceptors, &bearerInterceptor{cred: opts.Token})
	}
	interceptors = append(interceptors, &unaryDeadlineInterceptor{d: timeout})
	opts2 := []connect.ClientOption{connect.WithInterceptors(interceptors...)}
	c := &Clients{HTTP: httpClient}
	c.Ask = newClient(apiv1connect.NewAskOrchiconServiceClient, httpClient, base, opts2)
	c.Projects = newClient(apiv1connect.NewProjectServiceClient, httpClient, base, opts2)
	c.WorkItems = newClient(apiv1connect.NewWorkItemServiceClient, httpClient, base, opts2)
	c.Executions = newClient(apiv1connect.NewExecutionServiceClient, httpClient, base, opts2)
	c.Workflows = newClient(apiv1connect.NewWorkflowServiceClient, httpClient, base, opts2)
	c.Policies = newClient(apiv1connect.NewPolicyServiceClient, httpClient, base, opts2)
	c.Approvals = newClient(apiv1connect.NewApprovalServiceClient, httpClient, base, opts2)
	c.Recovery = newClient(apiv1connect.NewRecoveryServiceClient, httpClient, base, opts2)
	c.Workers = newClient(apiv1connect.NewWorkerServiceClient, httpClient, base, opts2)
	c.Images = newClient(apiv1connect.NewRuntimeImageServiceClient, httpClient, base, opts2)
	c.Secrets = newClient(apiv1connect.NewSecretsServiceClient, httpClient, base, opts2)
	c.MCP = newClient(apiv1connect.NewMCPServiceClient, httpClient, base, opts2)
	c.Settings = newClient(apiv1connect.NewSettingsServiceClient, httpClient, base, opts2)
	c.Auth = newClient(apiv1connect.NewAuthServiceClient, httpClient, base, opts2)
	c.Telemetry = newClient(apiv1connect.NewTelemetryServiceClient, httpClient, base, opts2)
	c.FileEdits = newClient(apiv1connect.NewFileEditServiceClient, httpClient, base, opts2)
	return c
}

// newClient is a generic shim over the generated constructor shape
// (httpClient, baseURL, opts...).
func newClient[T any](fn func(connect.HTTPClient, string, ...connect.ClientOption) T, httpClient connect.HTTPClient, baseURL string, opts []connect.ClientOption) T {
	return fn(httpClient, baseURL, opts...)
}

// Ping probes GET <base>/versionz. It is a plain HTTP endpoint, not a
// Connect RPC. serverVersion == "" means unreachable/malformed.
func Ping(ctx context.Context, baseURL string, insecure bool) (*VersionzResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/versionz", nil)
	if err != nil {
		return nil, fmt.Errorf("versionz request: %w", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("versionz: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("versionz: unexpected status %d", resp.StatusCode)
	}
	var vr VersionzResponse
	if err := json.NewDecoder(resp.Body).Decode(&vr); err != nil {
		return nil, fmt.Errorf("versionz decode: %w", err)
	}
	return &vr, nil
}

// ConnectRecv adapts a Connect server-stream into the recv-func shape the
// tui/stream engine consumes: each call returns the next message (as a
// pointer — proto messages must not be copied by value), or an error when
// the stream ends (io.EOF = graceful server close).
func ConnectRecv[Resp any](stream *connect.ServerStreamForClient[Resp]) func() (*Resp, error) {
	return func() (*Resp, error) {
		if !stream.Receive() {
			err := stream.Err()
			if err == nil {
				err = io.EOF
			}
			return nil, err
		}
		return stream.Msg(), nil
	}
}

// Login performs POST /auth/local-login and returns the access token
// response. Password mode only — API keys skip this entirely.
func Login(ctx context.Context, baseURL string, insecure bool, username, password string) (*LoginResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, err := json.Marshal(LoginRequest{Username: username, Password: password})
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/auth/local-login", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("login failed: HTTP %d", resp.StatusCode)
	}
	var lr LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, fmt.Errorf("login decode: %w", err)
	}
	if lr.AccessToken == "" {
		return nil, fmt.Errorf("login failed: no access token in response")
	}
	return &lr, nil
}
