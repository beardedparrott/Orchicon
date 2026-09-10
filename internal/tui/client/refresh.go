package client

// refresh.go — the TUI's refresh-token auto-refresh (Phase 2b): password
// mode's access token expires (server settings carry
// SessionAccessTokenTtlSeconds=900) while the login also issues a 24h
// refresh token (SessionRefreshTokenTtlSeconds=86400). The server hands
// the refresh token ONLY as the HttpOnly `orchicon_refresh` Set-Cookie —
// the JSON body carries the access token alone — so a non-browser client
// stores the cookie value and replays it on POST /auth/refresh
// (refreshRequest body: {"refresh_token": …}; readRefreshToken accepts
// cookie OR body).
//
// refreshingInterceptor (installed FIRST, so it wraps the bearer
// interceptor) classifies a UNAUTHENTICATED (401) response, mints a new
// access token via /auth/refresh, updates the token in place, and RETRIES
// the original request once with the fresh credential. Only when refresh
// itself fails does the auth-expired error surface (driving the /connect
// overlay). Unary + streaming paths both covered.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
)

// SessionClient carries refreshable password-mode state. The bearer
// interceptor reads its current token on every request, so an in-place
// refresh updates the credential for every subsequent RPC without
// rebuilding the client set (live streams included).
type SessionClient struct {
	// Token returns the CURRENT credential (api-key mode: a fixed key that
	// never refreshes; password mode: the live access token).
	Token func() string
	// SetToken installs a freshly refreshed access token (password mode).
	// Called only from the refresh interceptor; nil for api-key mode.
	SetToken func(string)
	// Refresh performs POST /auth/refresh with the stored refresh token and
	// returns the new access token (nil = no refresh token stored: api-key
	// mode → never refresh, the 401 surfaces as-is).
	Refresh func(ctx context.Context) (string, error)
	// OnRefreshed fires after a successful refresh (nil-safe). The shell
	// uses it to redial live streams with the fresh credential — a stream
	// opened before the refresh keeps the old bearer until it re-dials.
	OnRefreshed func()
	// reDial re-opens a stream spec with the current credential. The
	// refreshingStreamConn uses it to re-dial a stream whose OPEN surfaced
	// UNAUTHENTICATED; nil (api-key mode) → streams surface 401 as-is.
	reDial func(ctx context.Context, spec connect.Spec) (connect.StreamingClientConn, error)
	// cell is the live token cell (tests read the refreshed token through
	// Holder()); Token/SetToken close over it.
	cell *tokenHolder
}

// Holder returns the session's live token cell (the same pointer SetToken
// mutates) so tests can assert the refresh landed. Test-only seam.
func (sc *SessionClient) Holder() *tokenHolder { return sc.cell }

// tokenHolder is the in-memory mutable credential cell the client set
// reads through SessionClient.Token. Refreshes mutate it in place; the
// caller keeps the same value inside its profile for persistence.
type tokenHolder struct {
	mu    sync.RWMutex
	token string
}

func (h *tokenHolder) get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.token
}

func (h *tokenHolder) set(t string) {
	h.mu.Lock()
	h.token = t
	h.mu.Unlock()
}

// NewSessionClient builds the SessionClient plumbing for a profile. For
// API-key mode (empty refresh token) both hooks reduce to the static key
// and the refresh interceptor is inert.
func NewSessionClient(profileToken, refreshToken, baseURL string, insecure bool) (*SessionClient, *tokenHolder) {
	h := &tokenHolder{token: profileToken}
	sc := &SessionClient{
		Token:    h.get,
		SetToken: h.set,
		cell:     h,
	}
	if refreshToken != "" {
		sc.Refresh = func(ctx context.Context) (string, error) {
			lr, err := RefreshToken(ctx, baseURL, insecure, refreshToken)
			if err != nil {
				return "", err
			}
			return lr.AccessToken, nil
		}
	}
	return sc, h
}

// RefreshResponse mirrors the JSON body of POST /auth/refresh
// (internal/auth/handlers.go tokenResponse — same shape as the login
// body; the refresh token itself is never rotated server-side).
type RefreshResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	IdentityID  string `json:"identity_id"`
	TenantID    string `json:"tenant_id"`
	IsAdmin     bool   `json:"is_admin"`
}

func tlsConfig(insecure bool) *tls.Config {
	return &tls.Config{InsecureSkipVerify: insecure}
}

// RefreshToken performs POST /auth/refresh with the stored refresh token
// in the JSON body (the server's readRefreshToken accepts cookie OR body;
// a non-browser client has no cookie jar, so the body carries it).
func RefreshToken(ctx context.Context, baseURL string, insecure bool, refreshToken string) (*RefreshResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{TLSClientConfig: tlsConfig(insecure)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/auth/refresh", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed: HTTP %d", resp.StatusCode)
	}
	var rr RefreshResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		return nil, fmt.Errorf("refresh decode: %w", err)
	}
	if rr.AccessToken == "" {
		return nil, fmt.Errorf("refresh failed: no access token in response")
	}
	return &rr, nil
}

// refreshingInterceptor retries once with a fresh access token on 401.
// Single-flight: concurrent 401s park behind the in-flight refresh instead
// of stampeding /auth/refresh; losers re-read the (fresh) token and retry.
type refreshingInterceptor struct {
	sc *SessionClient

	mu    sync.Mutex
	inFly bool
	done  chan struct{}
	err   error
}

// refreshOnce performs (or parks behind) the single-flight refresh.
func (r *refreshingInterceptor) refreshOnce(ctx context.Context) error {
	r.mu.Lock()
	if r.inFly {
		ch := r.done
		r.mu.Unlock()
		select {
		case <-ch:
			return r.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.inFly = true
	r.done = make(chan struct{})
	r.mu.Unlock()

	var err error
	defer func() {
		r.mu.Lock()
		r.err = err
		r.inFly = false
		close(r.done)
		r.mu.Unlock()
	}()

	if r.sc == nil || r.sc.Refresh == nil {
		err = fmt.Errorf("no refresh token stored (api-key mode)")
		return err
	}
	tok, rerr := r.sc.Refresh(ctx)
	if rerr != nil {
		err = fmt.Errorf("session refresh failed: %w", rerr)
		return err
	}
	if tok != "" && r.sc.SetToken != nil {
		r.sc.SetToken(tok)
	}
	if r.sc.OnRefreshed != nil {
		r.sc.OnRefreshed()
	}
	return nil
}

// retryable reports whether err is a 401 the interceptor should absorb by
// refreshing. API-key mode never refreshes (Refresh nil) — its 401s are
// genuine authorization failures and surface as-is.
func (r *refreshingInterceptor) retryable(err error) bool {
	if err == nil || r.sc == nil || r.sc.Refresh == nil {
		return false
	}
	return connect.CodeOf(err) == connect.CodeUnauthenticated
}

func (r *refreshingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		if !r.retryable(err) {
			return resp, err
		}
		if rerr := r.refreshOnce(ctx); rerr != nil {
			return resp, err // the original 401 is the meaningful failure
		}
		req.Header().Set("Authorization", "Bearer "+r.sc.Token())
		return next(ctx, req)
	}
}

func (r *refreshingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", r.sc.Token())
		return &refreshingStreamConn{conn: conn, interceptor: r, ctx: ctx}
	}
}

func (r *refreshingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client-side only
}

// refreshingStreamConn re-dials the stream once with a refreshed credential
// when the initial request surfaces UNAUTHENTICATED. Connect surfaces a
// rejected stream OPEN as an error from the first Receive; the wrapper
// swaps in the re-dialed connection transparently (the receive contract —
// false + stream.Err() — is preserved on real failures).
type refreshingStreamConn struct {
	conn        connect.StreamingClientConn
	interceptor *refreshingInterceptor
	ctx         context.Context
	retried     bool
}

func (s *refreshingStreamConn) RequestHeader() http.Header { return s.conn.RequestHeader() }

func (s *refreshingStreamConn) Send(msg any) error           { return s.conn.Send(msg) }
func (s *refreshingStreamConn) Peer() connect.Peer           { return s.conn.Peer() }
func (s *refreshingStreamConn) Spec() connect.Spec           { return s.conn.Spec() }
func (s *refreshingStreamConn) CloseRequest() error          { return s.conn.CloseRequest() }
func (s *refreshingStreamConn) CloseResponse() error         { return s.conn.CloseResponse() }
func (s *refreshingStreamConn) ResponseHeader() http.Header  { return s.conn.ResponseHeader() }
func (s *refreshingStreamConn) ResponseTrailer() http.Header { return s.conn.ResponseTrailer() }

func (s *refreshingStreamConn) Receive(msg any) error {
	err := s.conn.Receive(msg)
	if err == nil || err == io.EOF || connect.CodeOf(err) != connect.CodeUnauthenticated {
		return err
	}
	if s.retried || s.interceptor == nil || s.interceptor.sc == nil {
		return err
	}
	if rerr := s.interceptor.refreshOnce(s.ctx); rerr != nil {
		return err // the original 401 is the meaningful failure
	}
	if s.interceptor.sc.reDial == nil {
		return err
	}
	redialed, err2 := s.interceptor.sc.reDial(s.ctx, s.conn.Spec())
	if err2 != nil || redialed == nil {
		return err
	}
	s.retried = true
	_ = s.conn.CloseResponse()
	_ = s.conn.CloseRequest()
	s.conn = redialed
	return s.conn.Receive(msg)
}
