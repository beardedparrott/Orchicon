package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/auth/op"
	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
)

// RefreshCookie is the HttpOnly cookie name carrying the refresh token
// (docs/10 §7). HttpOnly + Secure + SameSite=Strict in production; in
// local dev over plain HTTP the Secure flag is relaxed so the cookie
// survives the non-TLS connection.
const RefreshCookie = "orchicon_refresh"

// LocalCredentialVerifier verifies a username + password pair for a local
// account within a tenant, returning the identity id on success. It is the
// credential-boundary function the local-login path (and, in principle, the
// embedded-OP flow) consumes, so credential logic never touches OP protocol
// code and OP protocol code never touches SQL (docs/07 §6.1).
type LocalCredentialVerifier func(ctx context.Context, tenantID, username, password string) (identityID string, ok bool, err error)

// Handler exposes the out-of-band auth HTTP endpoints (docs/07 §6.1):
//
//	POST /auth/local-login    Local-account login (embedded IdP, username+password)
//	POST /auth/signup         Self-service account creation (embedded IdP, username+password)
//	POST /auth/refresh        Exchange a refresh token for a new access token
//	POST /auth/logout         Clear the HttpOnly refresh cookie (end the browser session)
//	GET  /auth/oidc/login     Redirect to the IdP authorize URL
//	GET  /auth/oidc/callback  IdP callback: exchange code, issue tokens
//	GET  /auth/session        Return the current resolved identity
//	GET  /auth/config         Capability flags for the pre-login SPA (public)
//
// The access token is returned in the JSON body (kept in memory by the
// frontend); the refresh token is set as an HttpOnly cookie so JS cannot
// read it (docs/10 §7).
type Handler struct {
	cfg      config.AuthConfig
	mode     config.DeploymentMode
	issuer   *TokenIssuer
	resolver *Resolver
	oidc     *OIDCVerifier
	op       *op.Provider
	pool     *db.Pool
	log      *slog.Logger
	// deploymentTenantID is the single tenant this deployment owns
	// (ORCHICON_DEPLOYMENT_TENANT_ID, default "tnt_dev"). Every auth path
	// resolves logins into it — OIDC callback, the embedded-OP local
	// login, and the local-admin bootstrap — so a non-default install
	// never splits identities across tenants (decision #178).
	deploymentTenantID string
	// verifyCred is the injected LocalCredentialVerifier (the credential
	// boundary). Defaults to the DB-backed lookup + hash check; a test or
	// future OP wiring can replace it.
	verifyCred LocalCredentialVerifier
}

// NewHandler constructs the auth HTTP handler.
func NewHandler(cfg config.Config, pool *db.Pool, log *slog.Logger) *Handler {
	issuer := NewTokenIssuer(cfg.Auth.SigningKey, "orchicon", "orchicon-api",
		cfg.Auth.AccessTTL, cfg.Auth.RefreshTTL)
	h := &Handler{
		cfg:                cfg.Auth,
		mode:               cfg.Mode,
		issuer:             issuer,
		resolver:           NewResolver(pool),
		pool:               pool,
		log:                log,
		deploymentTenantID: cfg.DeploymentTenantID,
	}
	h.verifyCred = h.verifyLocalCredential
	if cfg.Auth.Issuer != "local" && cfg.Auth.Issuer != "" {
		h.oidc = NewOIDCVerifier(cfg.Auth.Issuer, cfg.Auth.ClientID, cfg.Auth.ClientSecret, cfg.Auth.RedirectURL)
	}
	if cfg.Auth.EmbeddedOP {
		if err := h.buildEmbeddedOP(); err != nil {
			h.log.Warn("embedded OpenID Provider disabled (construction failed)", "error", err)
		}
	}
	return h
}

// SetLocalCredentialVerifier overrides the default DB-backed credential
// verifier. Intended for the embedded-OP wiring and tests; never call from
// control-plane business logic.
func (h *Handler) SetLocalCredentialVerifier(v LocalCredentialVerifier) {
	if v != nil {
		h.verifyCred = v
	}
}

// Issuer returns the token issuer (for the middleware to verify tokens).
func (h *Handler) Issuer() *TokenIssuer { return h.issuer }

// Resolver returns the identity resolver (for the middleware).
func (h *Handler) Resolver() *Resolver { return h.resolver }

// Register mounts the auth HTTP endpoints on the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/auth/local-login", h.localLogin)
	mux.HandleFunc("/auth/signup", h.signup)
	mux.HandleFunc("/auth/refresh", h.refresh)
	mux.HandleFunc("/auth/logout", h.logout)
	mux.HandleFunc("/auth/oidc/login", h.oidcLogin)
	mux.HandleFunc("/auth/oidc/callback", h.oidcCallback)
	mux.HandleFunc("/auth/session", h.session)
	mux.HandleFunc("/auth/config", h.authConfig)
	h.registerEmbeddedOP(mux)
}

// authConfigResponse is the capability-flags payload the pre-login SPA
// login page reads (docs/07 §6.1). Public and secret-free: it mirrors the
// plane's auth configuration (which IdPs are reachable) so the UI renders
// exactly the sign-in affordances the running plane supports. It never
// carries the issuer URL, client id, or any signing material — those stay
// server-side (AGENTS.md: the frontend reflects server state, never makes
// policy).
type authConfigResponse struct {
	Mode         string `json:"mode"`
	EmbeddedOP   bool   `json:"embedded_op"`
	ExternalOIDC bool   `json:"external_oidc"`
	// Signup advertises self-service account creation over the embedded IdP
	// (true exactly when the embedded OP is enabled). The SPA shows the
	// "Create an account" affordance only when the plane advertises it.
	Signup bool `json:"signup"`
}

// authConfig returns the plane's auth capability flags.
func (h *Handler) authConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, authConfigResponse{
		Mode:         string(h.mode),
		EmbeddedOP:   h.cfg.EmbeddedOP,
		ExternalOIDC: h.oidc != nil,
		Signup:       h.cfg.EmbeddedOP,
	})
}

// tokenResponse is the JSON body returned on login/refresh.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	IdentityID  string `json:"identity_id"`
	TenantID    string `json:"tenant_id"`
	IsAdmin     bool   `json:"is_admin"`
	// Next is the same-origin path the SPA should full-page-load after a
	// local login (set only when a pending embedded-OP authorize request
	// was completed). Always server-constructed — never echoes the client.
	Next string `json:"next,omitempty"`
	// ForcePasswordChange is true when the signed-in credential is flagged
	// for a forced password change (the bootstrap admin seeded with the
	// built-in default). The SPA renders a full-screen change-password gate
	// in place of the app content while it is true; the token is still
	// issued so the session can call the change RPC (ADR-3).
	ForcePasswordChange bool `json:"force_password_change,omitempty"`
}

// localLoginRequest is the body for POST /auth/local-login.
type localLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Next is the OP login-bridge path the browser was redirected from
	// (/auth/op/login?id=<id>); its id completes the pending authorize
	// request. Only same-origin OP bridge paths are honored.
	Next string `json:"next"`
}

// localLogin authenticates a local account (embedded IdP) by username +
// password. Flow: look up the local_credentials row, verify the stored
// argon2id/bcrypt hash, mint the Orchicon token pair, set the HttpOnly
// refresh cookie, and — when `next` carries an embedded-OP auth-request id —
// complete the pending authorize request so the browser returns to the
// relying party with a code. Any failure returns the same generic 401 (no
// user-enumeration hint); no identity is auto-provisioned by a login.
// auditAuthEvent writes an audit row for an auth HTTP endpoint in a short
// tenant-scoped tx. These endpoints are public paths (no identity in the
// request context) so the actor is resolved after credential validation
// and passed in explicitly. A failed write logs and returns nil: the
// auth action itself is not a DB mutation and must not be blocked by an
// audit-side effect (login/logout/refresh have no state to roll back).
func (h *Handler) auditAuthEvent(ctx context.Context, tenantID, identityID, action, authMethod string) {
	if tenantID == "" {
		return
	}
	ttx, err := h.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		h.log.Error("audit: begin tx", "error", err, "action", action)
		return
	}
	defer ttx.Rollback(ctx)
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: identityID,
		ActorType:       "user",
		AuthMethod:      authMethod,
		Action:          action,
		TargetType:      "identity",
		TargetID:        identityID,
	}); err != nil {
		h.log.Error("audit: record", "error", err, "action", action)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		h.log.Error("audit: commit", "error", err, "action", action)
	}
}

func (h *Handler) localLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The embedded IdP is the local-account surface; without it there is no
	// OP to complete and the endpoint is dead weight.
	if !h.cfg.EmbeddedOP {
		http.Error(w, "local login is disabled", http.StatusNotFound)
		return
	}
	var req localLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	if len(req.Username) > 255 {
		http.Error(w, "username too long", http.StatusBadRequest)
		return
	}
	if len(req.Password) > MaxPasswordLen {
		http.Error(w, "password too long", http.StatusBadRequest)
		return
	}
	// The embedded OP is single-tenant for the auth flow; local accounts
	// resolve within the deployment tenant (ORCHICON_DEPLOYMENT_TENANT_ID).
	tenantID := h.deploymentTenantID
	identityID, ok, err := h.verifyCred(r.Context(), tenantID, req.Username, req.Password)
	if err != nil || !ok {
		if err != nil {
			h.log.Error("local-login: verify credential", "error", err)
		}
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	ents, isAdmin, err := h.resolver.ResolveIdentity(r.Context(), tenantID, identityID)
	if err != nil {
		// The credential exists but the identity was deleted or disabled:
		// same generic rejection, no enumeration hint.
		h.log.Warn("local-login: resolve identity", "error", err)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	pair, err := h.issuer.IssuePair(identityID, tenantID, ents, isAdmin)
	if err != nil {
		http.Error(w, "failed to issue tokens", http.StatusInternalServerError)
		return
	}
	h.setRefreshCookie(w, pair.RefreshToken)
	// Server-sourced forced-change flag (ADR-4): the SPA renders the
	// change-password gate for flagged credentials; the token is still
	// issued so the gate can call the change RPC.
	cred := h.localCredential(r.Context(), tenantID, identityID)
	resp := tokenResponse{
		AccessToken: pair.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   pair.ExpiresIn,
		IdentityID:  identityID,
		TenantID:    tenantID,
		IsAdmin:     isAdmin,
		ForcePasswordChange: cred != nil && cred.ForcePasswordChange,
	}
	// Complete a pending embedded-OP authorize request (the login bridge
	// redirects unauthenticated browsers here with next=/auth/op/login?id=…).
	if id, ok := opAuthRequestID(req.Next); ok && h.op != nil {
		if err := h.op.MarkAuthenticated(r.Context(), id, identityID, tenantID); err != nil {
			h.log.Warn("local-login: complete op auth request", "error", err)
		} else {
			resp.Next = "/authorize/callback?id=" + url.QueryEscape(id)
		}
	}
	h.auditAuthEvent(r.Context(), tenantID, identityID, "auth.login", "local")
	writeJSON(w, http.StatusOK, resp)
}

// signupRequest is the body for POST /auth/signup.
type signupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Next is the OP login-bridge path the browser was redirected from
	// (/auth/op/login?id=<id>); its id completes the pending authorize
	// request. Only same-origin OP bridge paths are honored (identical to
	// localLoginRequest).
	Next string `json:"next"`
}

// errAccountExists is the internal sentinel for the duplicate-username /
// existing-identity rejection; the HTTP layer maps it to 409. Reusing one
// error keeps the two indistinguishable client-facing (no enumeration
// beyond the inherent duplicate-username signal).
var errAccountExists = errors.New("auth: account already exists")

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). Used to map the concurrent signup races — the
// identity insert and the credential upsert — to already-exists.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// signup creates a self-service local account over the embedded IdP: it
// atomically provisions a fresh identity + argon2id-hashed local credential
// in the deployment tenant, then runs the local-login tail verbatim — mint
// the token pair, set the HttpOnly refresh cookie, complete a pending
// embedded-OP authorize request when `next` carries one. No role binding is
// created: a self-signed-up account is a plain user identity with zero
// entitlements (least privilege for an open endpoint — the bootstrap admin
// stays the sole initial admin).
func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The embedded IdP is the local-account surface; without it there is no
	// OP to complete and sign-up is dead weight (same gate as local-login).
	if !h.cfg.EmbeddedOP {
		http.Error(w, "sign up is disabled", http.StatusNotFound)
		return
	}
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	if len(req.Username) > 255 {
		http.Error(w, "username too long", http.StatusBadRequest)
		return
	}
	// The exact charset SetLocalCredential enforces, so email-style handles
	// work and sign-up cannot mint a username the admin path would reject.
	if !localUsernameRE.MatchString(req.Username) {
		http.Error(w, "username must match ^[a-z0-9][a-z0-9._@+-]*$", http.StatusBadRequest)
		return
	}
	if len(req.Password) > MaxPasswordLen {
		http.Error(w, "password too long", http.StatusBadRequest)
		return
	}
	// Hash at the boundary (argon2id PHC); plaintext never leaves this
	// function for storage or logging.
	hash, err := HashPassword(req.Password)
	if err != nil {
		h.log.Error("sign-up: hash password", "error", err)
		http.Error(w, "failed to create account", http.StatusInternalServerError)
		return
	}
	tenantID := h.deploymentTenantID
	ident, err := h.createLocalAccount(r.Context(), tenantID, req.Username, hash)
	if err != nil {
		if errors.Is(err, errAccountExists) {
			http.Error(w, "an account with this username already exists", http.StatusConflict)
			return
		}
		h.log.Error("sign-up: create account", "error", err)
		http.Error(w, "failed to create account", http.StatusInternalServerError)
		return
	}
	h.log.Info("local account created", "identity", ident.ID, "tenant", tenantID)
	// Local-login tail: resolve entitlements, mint the token pair, set the
	// refresh cookie, and complete a pending OP authorize request.
	ents, isAdmin, err := h.resolver.ResolveIdentity(r.Context(), tenantID, ident.ID)
	if err != nil {
		h.log.Error("sign-up: resolve identity", "error", err)
		http.Error(w, "failed to create account", http.StatusInternalServerError)
		return
	}
	pair, err := h.issuer.IssuePair(ident.ID, tenantID, ents, isAdmin)
	if err != nil {
		http.Error(w, "failed to issue tokens", http.StatusInternalServerError)
		return
	}
	h.setRefreshCookie(w, pair.RefreshToken)
	resp := tokenResponse{
		AccessToken: pair.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   pair.ExpiresIn,
		IdentityID:  ident.ID,
		TenantID:    tenantID,
		IsAdmin:     isAdmin,
	}
	if id, ok := opAuthRequestID(req.Next); ok && h.op != nil {
		if err := h.op.MarkAuthenticated(r.Context(), id, ident.ID, tenantID); err != nil {
			h.log.Warn("sign-up: complete op auth request", "error", err)
		} else {
			resp.Next = "/authorize/callback?id=" + url.QueryEscape(id)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// createLocalAccount atomically provisions a fresh identity + local
// credential within a tenant. It rejects when either the username is
// already bound to a credential (the standard duplicate-username signup
// signal) or an identity with the same subject already exists (the
// identity-squatting guard: a BYO-IdP provisioned identity must never get a
// local password bound to it by a stranger signing up with the same
// handle). Both insert sites race under concurrency; their unique
// violations map to errAccountExists so exactly one concurrent signup for a
// username succeeds.
func (h *Handler) createLocalAccount(ctx context.Context, tenantID, username, hash string) (db.IdentityRow, error) {
	ttx, err := h.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.IdentityRow{}, err
	}
	defer ttx.Rollback(ctx)
	// Reject when the username already has a credential.
	if _, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, username); err == nil {
		return db.IdentityRow{}, errAccountExists
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.IdentityRow{}, err
	}
	// Create a fresh identity for the username (subject = handle). An
	// existing identity under the same subject is the squatting case.
	ident, created, err := db.GetOrCreateIdentity(ctx, ttx.Tx, tenantID, username, username, "user")
	if err != nil {
		if isUniqueViolation(err) {
			return db.IdentityRow{}, errAccountExists
		}
		return db.IdentityRow{}, err
	}
	if !created {
		return db.IdentityRow{}, errAccountExists
	}
	// Bind the credential to the fresh identity. A concurrent signup that
	// won the identity insert can lose here on the username index; the
	// unique violation maps to already-exists so only one succeeds.
	if _, err := db.UpsertLocalCredential(ctx, ttx.Tx, tenantID, ident.ID, username, hash, false); err != nil {
		if isUniqueViolation(err) {
			return db.IdentityRow{}, errAccountExists
		}
		return db.IdentityRow{}, err
	}
	// Self-service account creation: actor == target (the fresh identity).
	// The audit row commits atomically with the identity + credential
	// insert. action = auth.signup (the signup act); identity.created is
	// implied by the row's existence.
	if err := audit.Record(ctx, ttx.Tx, audit.Entry{
		TenantID:        tenantID,
		ActorIdentityID: ident.ID,
		ActorType:       "user",
		AuthMethod:      "signup",
		Action:          "auth.signup",
		TargetType:      "identity",
		TargetID:        ident.ID,
		After:           audit.Snapshot(identityAuditSnapshot(ident)),
	}); err != nil {
		h.log.Error("audit: signup", "error", err)
		return db.IdentityRow{}, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return db.IdentityRow{}, err
	}
	return ident, nil
}

// verifyLocalCredential is the default LocalCredentialVerifier: it looks up
// the tenant-scoped local_credentials row by username and verifies the
// stored hash against the plaintext in constant time. It is the only path
// that reads credential rows from internal/auth.
func (h *Handler) verifyLocalCredential(ctx context.Context, tenantID, username, password string) (identityID string, ok bool, err error) {
	ttx, err := h.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", false, err
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetLocalCredentialByUsername(ctx, ttx.Tx, tenantID, username)
	if err != nil {
		// Unknown username is a normal login failure, not an error: return it
		// as a plain not-ok so the caller produces the identical generic
		// rejection (and no log line) it does for a wrong password. Only a
		// real DB failure surfaces as an error.
		if errors.Is(err, db.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if row.Status != "active" {
		return "", false, nil
	}
	valid, err := VerifyPassword(password, row.PasswordHash)
	if err != nil || !valid {
		return "", false, nil
	}
	return row.IdentityID, true, nil
}

// opAuthRequestID extracts a same-origin embedded-OP auth-request id from a
// `next` value. Only the two OP bridge paths are accepted, only when the
// value is a same-origin path, and only when `id` is the sole query
// parameter — so a malicious `next` can never redirect the browser to an
// external host or smuggle extra parameters into the completed request.
func opAuthRequestID(next string) (string, bool) {
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(u.Path, "//") || strings.HasPrefix(u.Path, "/\\") {
		return "", false
	}
	switch u.Path {
	case "/auth/op/login", "/authorize/callback":
		q := u.Query()
		if len(q) != 1 {
			return "", false
		}
		if id := q.Get("id"); id != "" {
			return id, true
		}
	}
	return "", false
}

// refresh exchanges a refresh token (cookie or body) for a new access token.
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := readRefreshToken(r)
	if raw == "" {
		http.Error(w, "missing refresh token", http.StatusBadRequest)
		return
	}
	claims, err := h.issuer.VerifyRefresh(raw)
	if err != nil {
		http.Error(w, "invalid or expired refresh token", http.StatusUnauthorized)
		return
	}
	ents, isAdmin, err := h.resolver.ResolveIdentity(r.Context(), claims.TenantID, claims.Subject)
	if err != nil {
		http.Error(w, "identity not found", http.StatusUnauthorized)
		return
	}
	access, err := h.issuer.IssueAccess(claims.Subject, claims.TenantID, ents, isAdmin)
	if err != nil {
		http.Error(w, "failed to issue access token", http.StatusInternalServerError)
		return
	}
	h.auditAuthEvent(r.Context(), claims.TenantID, claims.Subject, "auth.refresh", "refresh")
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresIn:   int64(h.cfg.AccessTTL / time.Second),
		IdentityID:  claims.Subject,
		TenantID:    claims.TenantID,
		IsAdmin:     isAdmin,
	})
}

// logout ends the browser session: it clears the HttpOnly refresh cookie
// (empty value + MaxAge -1, matching the attributes setRefreshCookie uses).
// The refresh token is a stateless JWT — there is no server-side session to
// revoke, so clearing the cookie is the session's end; a subsequent
// /auth/refresh has nothing to exchange and the app falls back to /login.
// Idempotent and credential-free (it can only end the caller's own session,
// never another browser's).
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Attribute the logout to the session owner when the refresh cookie is
	// still present and valid; a missing/expired cookie writes no row
	// (nothing can be attributed).
	if raw := readRefreshToken(r); raw != "" {
		if claims, err := h.issuer.VerifyRefresh(raw); err == nil {
			h.auditAuthEvent(r.Context(), claims.TenantID, claims.Subject, "auth.logout", "refresh")
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.mode == config.ModeProduction,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
	w.WriteHeader(http.StatusNoContent)
}

// oidcLogin redirects the browser to the IdP authorize URL.
func (h *Handler) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		http.Error(w, "OIDC not configured (local mode)", http.StatusNotFound)
		return
	}
	// state is a short random nonce; for v0.1 we use a random id and
	// accept it back unchanged (single-use state store is a v0.2 hardening).
	state := randID(12)
	authURL, err := h.oidc.AuthCodeURL(r.Context(), state)
	if err != nil {
		http.Error(w, "oidc provider unavailable", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// oidcCallback handles the IdP redirect: exchange code, verify ID token,
// upsert identity, issue Orchicon tokens, redirect into the SPA.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		http.Error(w, "OIDC not configured (local mode)", http.StatusNotFound)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	// state round-trips the PKCE verifier on the PKCE path; a no-op on the
	// byte-for-byte-unchanged no-PKCE path.
	state := r.URL.Query().Get("state")
	out, err := h.oidc.Exchange(r.Context(), state, code)
	if err != nil {
		h.log.Error("oidc callback: exchange", "error", err)
		http.Error(w, "oidc exchange failed", http.StatusBadGateway)
		return
	}
	tenantID := h.deploymentTenantID
	display := out.DisplayName
	if display == "" {
		display = out.Email
	}
	ident, created, err := h.resolver.EnsureIdentityForSubject(r.Context(), tenantID, out.Subject, display, "user")
	if err != nil {
		http.Error(w, "failed to provision identity", http.StatusInternalServerError)
		return
	}
	if created {
		// First-login admin grant (decision #179, idempotent): provision the
		// tenant admin role + binding for the fresh identity. Routed through
		// the bootstrap helpers (ensureAdminRole + bindAdminRole) so there is
		// exactly one admin-provisioning path; errors are logged, never fatal
		// to the callback (an admin can grant roles later via Admin → Roles).
		ttx, err := h.pool.BeginTenantTx(r.Context(), tenantID)
		if err != nil {
			h.log.Warn("oidc callback: begin admin-grant tx", "error", err)
		} else {
			adminRoleID, err := ensureAdminRole(r.Context(), ttx.Tx, tenantID)
			if err == nil {
				err = bindAdminRole(r.Context(), ttx.Tx, tenantID, ident.ID, adminRoleID)
			}
			if err == nil {
				err = ttx.Commit(r.Context())
			}
			if err != nil {
				h.log.Warn("oidc callback: grant first-login admin role", "error", err)
			}
		}
	}
	h.auditAuthEvent(r.Context(), tenantID, ident.ID, "auth.login", "oidc")
	ents, isAdmin, err := h.resolver.ResolveIdentity(r.Context(), tenantID, ident.ID)
	if err != nil {
		ents = nil
	}
	pair, err := h.issuer.IssuePair(ident.ID, tenantID, ents, isAdmin)
	if err != nil {
		http.Error(w, "failed to issue tokens", http.StatusInternalServerError)
		return
	}
	h.setRefreshCookie(w, pair.RefreshToken)
	// Redirect into the SPA with the access token in the URL fragment
	// (fragments are not sent to servers, so the token does not leak into
	// server logs or referrers). The SPA reads it on load.
	frag := url.Values{}
	frag.Set("access_token", pair.AccessToken)
	frag.Set("expires_in", fmt.Sprint(pair.ExpiresIn))
	http.Redirect(w, r, "/#/auth/callback?"+frag.Encode(), http.StatusFound)
}

// session returns the current resolved identity for the SPA.
func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	access := readBearer(r)
	if access == "" {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	claims, err := h.issuer.VerifyAccess(access)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	resp := map[string]any{
		"authenticated": true,
		"identity_id":   claims.Subject,
		"tenant_id":     claims.TenantID,
		"is_admin":      claims.IsAdmin,
		"expires_at":    claims.ExpiresAt,
		// Server-sourced forced-change flag (ADR-4): re-resolved on every
		// full page load so the SPA gate survives reloads; false for
		// non-local credentials (no local_credentials row).
		"force_password_change": false,
	}
	if cred := h.localCredential(r.Context(), claims.TenantID, claims.Subject); cred != nil {
		resp["force_password_change"] = cred.ForcePasswordChange
		// The credential's username: the SPA change-password gate passes it
		// to SetLocalCredential, which requires the username to upsert.
		resp["username"] = cred.Username
	}
	writeJSON(w, http.StatusOK, resp)
}

// localCredential returns the identity's local credential row, or nil when
// there is none (production/OIDC sessions, non-local identities, or a DB
// hiccup). The forced-change flag is a local-credential property, so a
// missing row is simply unflagged — the safe default.
func (h *Handler) localCredential(ctx context.Context, tenantID, identityID string) *db.LocalCredentialRow {
	ttx, err := h.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil
	}
	defer ttx.Rollback(ctx)
	row, err := db.GetLocalCredentialByIdentity(ctx, ttx.Tx, tenantID, identityID)
	if err != nil {
		return nil
	}
	return &row
}

// setRefreshCookie sets the refresh token as an HttpOnly cookie. In
// local mode (plain HTTP) the Secure flag is relaxed so the cookie
// survives the non-TLS connection; production always sets Secure.
func (h *Handler) setRefreshCookie(w http.ResponseWriter, token string) {
	secure := h.mode == config.ModeProduction
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.cfg.RefreshTTL / time.Second),
	})
}

// readRefreshToken returns the refresh token from the cookie or body.
func readRefreshToken(r *http.Request) string {
	if c, err := r.Cookie(RefreshCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if r.Method == http.MethodPost {
		var body refreshRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		return body.RefreshToken
	}
	return ""
}

// readBearer extracts the bearer token from the Authorization header.
func readBearer(r *http.Request) string {
	_, cred, err := ParseBearer(r.Header.Get("Authorization"))
	if err != nil {
		return ""
	}
	return cred
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ErrUnauthenticated is returned when no valid credential is present.
var ErrUnauthenticated = errors.New("auth: unauthenticated")
