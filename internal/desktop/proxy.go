package desktop

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxyPrefixes are the paths the shell forwards to the plane.
var proxyPrefixes = []string{
	"/orchicon.api.v1", // every generated Connect-ES service
	"/auth/",           // login / refresh / session — the cookie path auth needs
	"/grafana",         // the telemetry iframe's dashboards
	"/healthz",
	"/versionz",
	"/enginez", // container-engine availability (internal/api/api.go mux.Handle)
	// Embedded OpenID Provider paths. They are NOT /auth/-prefixed, so without
	// these the SPA fallback would answer an OIDC redirect with index.html.
	"/.well-known/openid-configuration",
	"/authorize", // covers /authorize/callback
	"/token",
	"/userinfo",
	"/jwks",
}

// isPlanePath reports whether a request path belongs to the plane rather than
// the embedded SPA.
func isPlanePath(p string) bool {
	for _, prefix := range proxyPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// proxy builds the reverse proxy to the configured plane.
func (s *Shell) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// READ THE TARGET PER REQUEST, UNDER THE LOCK.
			s.mu.Lock()
			target := *s.planeURL
			s.mu.Unlock()
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			// Preserve the request path as-is (no path rewriting: the plane's
			// routes are rooted, and the prefix table is the whole mapping).
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logMessage("desktop: proxy %s %s: %v", r.Method, r.URL.Path, err)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("orchicon desktop: the plane at " + s.planeURL.String() + " is unreachable — is it running?\n"))
		},
	}
}

// planeURLFromString parses and validates the configured plane URL.
func planeURLFromString(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errNoPlaneURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, &url.Error{Op: "parse", URL: raw, Err: errBadPlaneScheme}
	}
	if u.Host == "" {
		return nil, &url.Error{Op: "parse", URL: raw, Err: errNoPlaneHost}
	}
	// Trailing slash normalised away so path joining stays exact.
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}
