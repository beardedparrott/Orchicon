package auth

import (
	"context"
	"net/http"
	"strings"

	authop "github.com/beardedparrott/orchicon/internal/auth/op"
	"github.com/beardedparrott/orchicon/internal/config"
)

// buildEmbeddedOP constructs the in-process OpenID Provider (zitadel/oidc
// v3 pkg/op) and wires it + its login bridge into the auth handler. It is
// called from NewHandler when ORCHICON_OP_ENABLED. The provider derives
// its ES256 signing key deterministically from the auth signing key (so
// the JWKS never leaks the HMAC secret and stays stable across restarts)
// and resolves identity claims live from Postgres via the tenant-scoped
// resolver.
func (h *Handler) buildEmbeddedOP() error {
	prov, err := authop.NewProvider(authop.Config{
		SigningKey:    h.cfg.SigningKey,
		ClientID:      h.cfg.ClientID,
		RedirectURIs:  opRedirectURIs(h.cfg),
		OPIssuer:      h.cfg.OPIssuer,
		TenantID:      h.deploymentTenantID,
		AllowInsecure: h.mode == config.ModeLocal,
		Pool:          h.pool,
		Log:           h.log,
	})
	if err != nil {
		return err
	}
	h.op = prov
	return nil
}

// opRedirectURIs computes the built-in client's registered redirect URIs.
// An explicit ORCHICON_OP_REDIRECT_URIS wins; otherwise the default is the
// auth RedirectURL (the SPA dev callback) plus, when the OP issuer is
// pinned, the plane-origin /auth/callback if it differs. A dynamic issuer
// (no OPIssuer) has no static plane origin, so only the RedirectURL is
// registered.
func opRedirectURIs(cfg config.AuthConfig) []string {
	if cfg.OPRedirectURIs != "" {
		parts := strings.Split(cfg.OPRedirectURIs, ",")
		uris := make([]string, 0, len(parts))
		for _, p := range parts {
			if u := strings.TrimSpace(p); u != "" {
				uris = append(uris, u)
			}
		}
		return uris
	}
	uris := []string{cfg.RedirectURL}
	// THE PLANE'S OWN ORIGIN, ALWAYS. It used to be added only when OPIssuer was pinned, so a local
	// install (dynamic issuer) registered the Vite dev-server callback and NOTHING ELSE — and the
	// browser's login was rejected with "The requested redirect_uri is missing in the client
	// configuration" before any credential was checked. The plane serves the SPA at its own port, so
	// that origin is a first-class callback, not a consequence of pinning the issuer.
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		for _, have := range uris {
			if have == u {
				return
			}
		}
		uris = append(uris, u)
	}
	if cfg.PlaneURL != "" {
		base := strings.TrimSuffix(cfg.PlaneURL, "/")
		add(base + "/auth/callback")
		// localhost and 127.0.0.1 are DIFFERENT origins to a browser, and an operator may reach the
		// plane by either. Register both spellings of the same port.
		if strings.Contains(base, "://localhost:") {
			add(strings.Replace(base, "://localhost:", "://127.0.0.1:", 1) + "/auth/callback")
		} else if strings.Contains(base, "://127.0.0.1:") {
			add(strings.Replace(base, "://127.0.0.1:", "://localhost:", 1) + "/auth/callback")
		}
	}
	if cfg.OPIssuer != "" {
		add(strings.TrimSuffix(cfg.OPIssuer, "/") + "/auth/callback")
	}
	return uris
}

// registerEmbeddedOP mounts the six OP endpoints + the login bridge on the
// plane mux. Only called when the provider is built.
func (h *Handler) registerEmbeddedOP(mux *http.ServeMux) {
	if h.op == nil {
		return
	}
	h.op.Register(mux)
	authop.RegisterLoginBridge(mux, h.op, func(r *http.Request) (string, string, bool) {
		raw := readRefreshToken(r)
		if raw == "" {
			return "", "", false
		}
		claims, err := h.issuer.VerifyRefresh(raw)
		if err != nil {
			return "", "", false
		}
		// Re-resolve the identity so a deleted identity cannot log in.
		if _, _, err := h.resolver.ResolveIdentity(context.Background(), claims.TenantID, claims.Subject); err != nil {
			return "", "", false
		}
		return claims.TenantID, claims.Subject, true
	})
}

// CloseEmbeddedOP stops the OP's storage sweeper (shutdown hook).
func (h *Handler) CloseEmbeddedOP() {
	if h.op != nil {
		h.op.Close()
	}
}
