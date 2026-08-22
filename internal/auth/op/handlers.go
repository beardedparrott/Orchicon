package op

import (
	"net/http"
	"net/url"
)

// SessionReader validates the caller's Orchicon session (the HttpOnly
// orchicon_refresh cookie) and resolves the authenticated identity. ok is
// false when the cookie is absent, invalid, or the identity no longer
// exists (a deleted identity cannot log in through the embedded OP).
type SessionReader func(r *http.Request) (tenantID, identityID string, ok bool)

// RegisterLoginBridge mounts the OP login bridge on the plane mux. The OP
// redirects every authorize request here (client.LoginURL = /auth/op/login
// ?id=<id>); the bridge authenticates the caller's existing Orchicon
// session and completes the auth request, or bounces to the SPA login page
// with a same-origin next so the user returns after authenticating.
func RegisterLoginBridge(mux *http.ServeMux, prov *Provider, read SessionReader) {
	mux.HandleFunc("/auth/op/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing auth request id", http.StatusBadRequest)
			return
		}
		tenantID, identityID, ok := read(r)
		if !ok {
			// No valid session. Send the user to the SPA login with a
			// same-origin next (encoded so the id survives) — the SPA
			// only follows same-origin paths, so this is not an open
			// redirect.
			next := url.QueryEscape("/auth/op/login?id=" + id)
			http.Redirect(w, r, "/login?next="+next, http.StatusFound)
			return
		}
		if err := prov.MarkAuthenticated(r.Context(), id, identityID, tenantID); err != nil {
			http.Error(w, "invalid auth request", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/authorize/callback?id="+id, http.StatusFound)
	})
}
