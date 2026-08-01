package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// TestGrafanaProxyEndToEnd simulates the /grafana reverse proxy chain:
// a fake upstream running Grafana in sub-path mode (serve_from_sub_path),
// the reverse proxy from Mount(), and a client that fetches /grafana/...
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

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	// The proxy under test. Mirrors the wiring in Mount(): plain
	// SingleHostReverseProxy with the full /grafana path forwarded.
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	proxy.ErrorLog = nil

	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/grafana") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer controlPlane.Close()

	// 1. Fetch the proxied SPA shell under /grafana/explore. No redirect:
	// the upstream serves the subpath directly.
	resp, err := http.Get(controlPlane.URL + "/grafana/explore")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from /grafana/explore, got %d", resp.StatusCode)
	}
	html := string(body)
	mustContain(t, html, `<base href="/grafana/" />`)
	mustContain(t, html, `src="/grafana/public/build/index-XXXXX.js"`)

	// 2. Asset requests route through the proxy to upstream's subpath.
	jsResp, err := http.Get(controlPlane.URL + "/grafana/public/build/index-XXXXX.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBody, _ := io.ReadAll(jsResp.Body)
	jsResp.Body.Close()
	if jsResp.StatusCode != 200 {
		t.Fatalf("expected 200 from asset path, got %d", jsResp.StatusCode)
	}
	mustContain(t, string(jsBody), "/grafana/public/build/index-XXXXX.js")

	// 3. Grafana API calls (e.g. health) also route through.
	apiResp, err := http.Get(controlPlane.URL + "/grafana/api/health")
	if err != nil {
		t.Fatal(err)
	}
	apiBody, _ := io.ReadAll(apiResp.Body)
	apiResp.Body.Close()
	mustContain(t, string(apiBody), `"ok"`)
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q to contain %q", haystack, needle)
	}
}
