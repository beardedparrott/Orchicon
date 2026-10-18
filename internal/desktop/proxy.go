package desktop

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// proxyPrefixes are the paths the shell forwards to the plane. Everything else
// is served from the embedded SPA.
//
// THIS LIST IS THE ONE PLACE THAT CAN DRIFT, and it is kept honest by tests
// rather than by care:
//   - TestProxyPrefixesCoverPlanePassthrough reads the plane's own
//     withFrontend passthrough list (cmd/orchicon/serve_state.go) and asserts
//     every one of its paths is covered here;
//   - TestProxyCoversEveryPlaneRoute reads the routes internal/api actually
//     registers (mux.Handle* literals + the generated Connect procedure paths)
//     and asserts the same. Adding a plane route without widening this list is
//     therefore a failing test, not a blank page in the app.
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
//
// THREE DETAILS ARE LOAD-BEARING:
//   - FlushInterval: -1 flushes every write immediately. The SPA's execution
//     view reads a Connect server-stream for the life of an execution
//     (frontend/src/api/useStream.ts); a buffered proxy turns that into "the
//     view silently stops updating", which is the specific failure this shell
//     must not have.
//   - The auth headers are NOT touched. This shell has no auth logic at all:
//     Authorization, Cookie and Set-Cookie pass through verbatim, so the
//     HttpOnly refresh cookie reaches the webview's cookie jar and the 401
//     retry in frontend/src/api/clients.ts can work.
//   - Rewrite (not Director) so X-Forwarded-* are not guess-populated; the
//     plane sees its own host, exactly as it would from a browser on that host.
func (s *Shell) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := *s.planeURL
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
