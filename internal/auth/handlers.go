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

	"github.com/beardedparrott/orchicon/internal/config"
	"github.com/beardedparrott/orchicon/internal/db"
)

// RefreshCookie is the HttpOnly cookie name carrying the refresh token
// (docs/10 §7). HttpOnly + Secure + SameSite=Strict in production; in
// local dev over plain HTTP the Secure flag is relaxed so the cookie
// survives the non-TLS connection.
const RefreshCookie = "orchicon_refresh"

// Handler exposes the out-of-band auth HTTP endpoints (docs/07 §6.1):
//
//	POST /auth/dev-login    Local-mode synthetic login (subject → tokens)
//	POST /auth/refresh      Exchange a refresh token for a new access token
//	GET  /auth/oidc/login   Redirect to the IdP authorize URL
//	GET  /auth/oidc/callback  IdP callback: exchange code, issue tokens
//	GET  /auth/session      Return the current resolved identity
//
// The access token is returned in the JSON body (kept in memory by the
// frontend); the refresh token is set as an HttpOnly cookie so JS cannot
// read it (docs/10 §7).
type Handler struct {
	cfg       config.AuthConfig
	mode      config.DeploymentMode
	issuer    *TokenIssuer
	resolver  *Resolver
	oidc      *OIDCVerifier
	pool      *db.Pool
	log       *slog.Logger
}

// NewHandler constructs the auth HTTP handler.
func NewHandler(cfg config.Config, pool *db.Pool, log *slog.Logger) *Handler {
	issuer := NewTokenIssuer(cfg.Auth.SigningKey, "orchicon", "orchicon-api",
		cfg.Auth.AccessTTL, cfg.Auth.RefreshTTL)
	h := &Handler{
		cfg:      cfg.Auth,
		mode:     cfg.Mode,
		issuer:   issuer,
		resolver: NewResolver(pool),
		pool:     pool,
		log:      log,
	}
	if cfg.Auth.Issuer != "local" && cfg.Auth.Issuer != "" {
		h.oidc = NewOIDCVerifier(cfg.Auth.Issuer, cfg.Auth.ClientID, cfg.Auth.ClientSecret, cfg.Auth.RedirectURL)
	}
	return h
}

// Issuer returns the token issuer (for the middleware to verify tokens).
func (h *Handler) Issuer() *TokenIssuer { return h.issuer }

// Resolver returns the identity resolver (for the middleware).
func (h *Handler) Resolver() *Resolver { return h.resolver }

// Register mounts the auth HTTP endpoints on the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/auth/dev-login", h.devLogin)
	mux.HandleFunc("/auth/refresh", h.refresh)
	mux.HandleFunc("/auth/oidc/login", h.oidcLogin)
	mux.HandleFunc("/auth/oidc/callback", h.oidcCallback)
	mux.HandleFunc("/auth/session", h.session)
}

// devLoginRequest is the body for POST /auth/dev-login.
type devLoginRequest struct {
	Subject  string `json:"subject"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// tokenResponse is the JSON body returned on login/refresh.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	IdentityID   string `json:"identity_id"`
	TenantID     string `json:"tenant_id"`
	IsAdmin      bool   `json:"is_admin"`
}

// devLogin mints tokens for a synthetic subject. Local dev only — gated
// by ORCHICON_DEV_LOGIN. Production rejects this path.
func (h *Handler) devLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.mode != config.ModeLocal || !h.cfg.DevLoginAllowed {
		http.Error(w, "dev login is disabled", http.StatusForbidden)
		return
	}
	var req devLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.Subject = strings.TrimSpace(req.Subject)
	if req.Subject == "" {
		http.Error(w, "subject must not be empty", http.StatusBadRequest)
		return
	}
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = "tnt_dev"
	}
	name := req.Name
	if name == "" {
		name = req.Subject
	}
	ident, _, err := h.resolver.EnsureIdentityForSubject(r.Context(), tenantID, req.Subject, name, "user")
	if err != nil {
		h.log.Error("dev-login: ensure identity", "error", err)
		http.Error(w, "failed to provision identity", http.StatusInternalServerError)
		return
	}
	// Seed the admin role + binding so the dev user has full access.
	isAdmin, err := h.ensureDevAdminBinding(r.Context(), tenantID, ident.ID)
	if err != nil {
		h.log.Error("dev-login: ensure admin binding", "error", err)
		http.Error(w, "failed to provision role", http.StatusInternalServerError)
		return
	}
	ents, _, err := h.resolver.ResolveIdentity(r.Context(), tenantID, ident.ID)
	if err != nil {
		h.log.Error("dev-login: resolve entitlements", "error", err)
		http.Error(w, "failed to resolve entitlements", http.StatusInternalServerError)
		return
	}
	pair, err := h.issuer.IssuePair(ident.ID, tenantID, ents, isAdmin)
	if err != nil {
		http.Error(w, "failed to issue tokens", http.StatusInternalServerError)
		return
	}
	h.setRefreshCookie(w, pair.RefreshToken)
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: pair.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   pair.ExpiresIn,
		IdentityID:   ident.ID,
		TenantID:     tenantID,
		IsAdmin:      isAdmin,
	})
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
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access,
		TokenType:   "Bearer",
		ExpiresIn:   int64(h.cfg.AccessTTL / time.Second),
		IdentityID:   claims.Subject,
		TenantID:     claims.TenantID,
		IsAdmin:      isAdmin,
	})
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
	out, err := h.oidc.Exchange(r.Context(), code)
	if err != nil {
		h.log.Error("oidc callback: exchange", "error", err)
		http.Error(w, "oidc exchange failed", http.StatusBadGateway)
		return
	}
	tenantID := "tnt_dev"
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
		_, _ = h.ensureDevAdminBinding(r.Context(), tenantID, ident.ID)
	}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"identity_id":  claims.Subject,
		"tenant_id":    claims.TenantID,
		"is_admin":     claims.IsAdmin,
		"expires_at":   claims.ExpiresAt,
	})
}

// ensureDevAdminBinding creates the tenant admin role (if absent) and
// binds it to the identity, returning whether the identity is now an
// admin. Idempotent. The admin role bypasses per-call RBAC checks.
func (h *Handler) ensureDevAdminBinding(ctx context.Context, tenantID, identityID string) (bool, error) {
	ttx, err := h.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return false, err
	}
	defer ttx.Rollback(ctx)
	// Find or create the admin role.
	roles, err := db.ListRoles(ctx, ttx.Tx, tenantID, 1000, "")
	if err != nil {
		return false, err
	}
	var adminRoleID string
	for _, rl := range roles {
		if rl.Name == "admin" {
			adminRoleID = rl.ID
			break
		}
	}
	if adminRoleID == "" {
		role, err := db.CreateRole(ctx, ttx.Tx, db.RoleRow{
			TenantID:     tenantID,
			Name:         "admin",
			Scope:        "tenant",
			Entitlements: []string{"*"},
		})
		if err != nil {
			return false, err
		}
		adminRoleID = role.ID
	}
	// Bind the identity to the admin role (idempotent: check first).
	bindings, err := db.ListRoleBindings(ctx, ttx.Tx, tenantID, identityID, 1000, "")
	if err != nil {
		return false, err
	}
	for _, b := range bindings {
		if b.RoleID == adminRoleID {
			if err := ttx.Commit(ctx); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if _, err := db.CreateRoleBinding(ctx, ttx.Tx, db.RoleBindingRow{
		TenantID:   tenantID,
		IdentityID: identityID,
		RoleID:     adminRoleID,
		Scope:      "tenant",
	}); err != nil {
		return false, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
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
