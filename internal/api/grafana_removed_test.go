package api

// grafana_removed_test.go — the INVERSE of the /grafana proxy test that used to live here
// (grafana_proxy_test.go, TestGrafanaProxyEndToEnd). The proxy is gone and its subject with it, so this file
// asserts the property that matters now: a request to /grafana reaches NOTHING unauthenticated.
//
// THE REASONING THAT MUST SURVIVE THE DELETION (do not lose this when refactoring):
//
//	The /grafana reverse proxy was mounted OUTSIDE ResolveAuth because the Telemetry page embedded Grafana in
//	an <iframe>, and AN IFRAME REQUEST CANNOT CARRY AN AUTHORIZATION HEADER — so under ResolveAuth every pane
//	401'd with {"error":"missing credentials"}, while the tests passed because they exercised the proxy
//	without the wrapper. Mounting a second, unguarded handler chain was the WRONG remedy for a REAL
//	constraint.
//
// If a future UI surface cannot attach a credential (an iframe, an <img>, a download link, an OAuth
// callback), the fix is to fetch through the authenticated Connect client and render the DATA — never to
// mount anything outside ResolveAuth. There is no outer mux in this package any more, and
// TestAPIHasNoOuterMux (lifecycle_surface_test.go) is the tripwire that keeps it that way.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGrafanaRouteFallsThroughToAuth pins AC4: the removed bypass leaves nothing unauthenticated behind.
// These are the paths the old outer mux served (the SPA shell, its assets, and the Grafana API); every one of
// them must now die at ResolveAuth with 401 — without a single one being registered on the guarded mux.
func TestGrafanaRouteFallsThroughToAuth(t *testing.T) {
	mux := http.NewServeMux()
	deps := testDeps()
	handler := Mount(mux, &deps)

	for _, path := range []string{
		"/grafana",
		"/grafana/",
		"/grafana/explore",
		"/grafana/api/health",
		"/grafana/public/build/index.js",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("credential-less GET %s = %d, want 401 — a route is reachable outside ResolveAuth "+
					"(no UI surface may be mounted outside the auth wrapper; fetch through the authenticated "+
					"client instead)", path, rec.Code)
			}
		})
	}
}

// TestRemovedGrafanaPathsAreNotMounted is the companion positive check: none of the old /grafana paths are
// registered on the guarded mux either. A route that exists on the mux would answer 401 here too, so the
// served-surface assertion has to be structural — mux.Handler reports the "/" catch-all (or no handler) for
// a path that was never registered. Keeping it explicit means a reintroduced /grafana registration shows up
// as a failure of THIS test rather than as a silently still-guarded-but-present route.
func TestRemovedGrafanaPathsAreNotMounted(t *testing.T) {
	mux := http.NewServeMux()
	deps := testDeps()
	Mount(mux, &deps)

	for _, path := range []string{"/grafana", "/grafana/", "/grafana/explore"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "/grafana" || pattern == "/grafana/" {
			t.Fatalf("path %q is still registered on the api mux as %q — the Grafana surface must be gone", path, pattern)
		}
	}
}
