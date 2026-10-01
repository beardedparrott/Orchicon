package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGrafanaProxyEndToEnd simulates the /grafana reverse proxy chain
// through the REAL Mount() wiring: a fake upstream running Grafana in
// sub-path mode (serve_from_sub_path), the auth middleware exactly as
// production wraps it, and a client that fetches /grafana/... with NO
// credentials — the exact shape of the Telemetry page's embedded iframe
// (frontend/src/routes/telemetry.tsx), which cannot set an Authorization
// header. Regression-pins the dispatch split in Mount(): the proxy is
// mounted OUTSIDE ResolveAuth, so the panes must load without a bearer
// token, while every other path stays credential-guarded (pinned in
// api_test.go's TestMountRequiresCredential, re-asserted here because the
// grafanaMounted branch returns a different root mux).
//
// Grafana is reached AT its subpath and generates every asset/API URL
// with the /grafana prefix itself, so the proxy forwards the full
// /grafana/... path UNCHANGED (no StripPrefix — stripping would make
// Grafana see "/" and 301-redirect back to the subpath, a loop).
// The same-origin /grafana path lets the Telemetry iframe share the
// Orchicon shell (docs/10 §11).
func TestGrafanaProxyEndToEnd(t *testing.T) {
	const grafanaIndex = `<!doctype html>
<html lang="en">
	<head>
		<base href="/grafana/" />
		<link rel="shortcut icon" href="/grafana/public/img/fav32.png" />
		<script type="module" crossorigin src="/grafana/public/build/index-XXXXX.js"></script>
	</head>
	<body><div id="root"></div></body>
</html>`

	// Fake Grafana upstream: serves the SPA shell for HTML paths (with
	// its /grafana subpath preserved) and returns real payloads for
	// assets/API. The upstream sees the full /grafana/... path because
	// the proxy does not strip it.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/grafana/public/"):
			w.Header().Set("Content-Type", "application/javascript")
			w.Write([]byte("// fake grafana bundle: " + r.URL.Path))
		case strings.HasPrefix(r.URL.Path, "/grafana/api/health"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok"}`))
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(grafanaIndex))
		}
	}))
	defer upstream.Close()

	// The REAL Mount wiring — testDeps is the api_test.go harness (DB-free
	// auth handler, nil pool; no RPC handler is reached by these tests).
	// GrafanaURL is set, so Mount returns the root mux with the proxy
	// OUTSIDE the auth chain.
	mux := http.NewServeMux()
	deps := testDeps()
	deps.GrafanaURL = upstream.URL
	handler := Mount(mux, &deps)

	// 1. Fetch the proxied SPA shell under /grafana/explore — with NO
	// credentials, exactly as the iframe loads it. No redirect, no 401:
	// the upstream serves the subpath directly.
	resp, err := http.Get(handlerServe(t, handler) + "/grafana/explore")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from credential-less /grafana/explore, got %d", resp.StatusCode)
	}
	html := string(body)
	mustContain(t, html, `<base href="/grafana/" />`)
	mustContain(t, html, `src="/grafana/public/build/index-XXXXX.js"`)

	// 2. Asset requests route through the proxy to upstream's subpath,
	// also credential-less (the iframe's subresource requests look the same).
	jsResp, err := http.Get(handlerServe(t, handler) + "/grafana/public/build/index-XXXXX.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBody, _ := io.ReadAll(jsResp.Body)
	jsResp.Body.Close()
	if jsResp.StatusCode != 200 {
		t.Fatalf("expected 200 from credential-less asset path, got %d", jsResp.StatusCode)
	}
	mustContain(t, string(jsBody), "/grafana/public/build/index-XXXXX.js")

	// 3. Grafana API calls (e.g. health) also route through unauthenticated.
	apiResp, err := http.Get(handlerServe(t, handler) + "/grafana/api/health")
	if err != nil {
		t.Fatal(err)
	}
	apiBody, _ := io.ReadAll(apiResp.Body)
	apiResp.Body.Close()
	mustContain(t, string(apiBody), `"ok"`)

	// 4. The bypass does NOT open the guarded surface: a non-Grafana path
	// with no credentials still dies at ResolveAuth (the grafanaMounted
	// branch of Mount returns a different root mux — re-pin the invariant
	// against it specifically).
	guarded := httptest.NewRequest(http.MethodPost, "/orchicon.api.v1.ProjectService/ListProjects", nil)
	guardedRec := httptest.NewRecorder()
	handler.ServeHTTP(guardedRec, guarded)
	if guardedRec.Code != http.StatusUnauthorized {
		t.Fatalf("credential-less API status = %d, want 401 even with the grafana proxy mounted", guardedRec.Code)
	}

	// 5. When GrafanaURL is unset, no /grafana route exists: the
	// credential-less request falls through to ResolveAuth and dies 401 —
	// the bypass cannot widen a surface the mux does not serve.
	mux2 := http.NewServeMux()
	deps2 := testDeps() // GrafanaURL deliberately empty
	handler2 := Mount(mux2, &deps2)
	noRoute := httptest.NewRequest(http.MethodGet, "/grafana/explore", nil)
	noRouteRec := httptest.NewRecorder()
	handler2.ServeHTTP(noRouteRec, noRoute)
	if noRouteRec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for /grafana with no GrafanaURL configured", noRouteRec.Code)
	}
}

// handlerServe pins the handler into an httptest server so requests go
// through the real HTTP stack (the proxy needs a real request pipeline).
func handlerServe(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q to contain %q", haystack, needle)
	}
}
