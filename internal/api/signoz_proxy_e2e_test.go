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

// TestSigNozProxyEndToEnd simulates the full proxy chain: a fake
// upstream that returns SigNoz v0.132's index.html, the reverse proxy
// with our rewrite, and a client that fetches /signoz/logs. The
// resulting HTML must be a usable SPA shell — base href is rewritten,
// asset paths are relative, and the JS/CSS asset requests route
// correctly through the proxy to the upstream.
func TestSigNozProxyEndToEnd(t *testing.T) {
	const signozHTML = `<!doctype html>
<html lang="en">
	<head>
		<base href="/" />
		<link data-react-helmet="true" rel="shortcut icon" href="favicon.ico" />
		<script type="module" crossorigin src="./assets/index-XXXXX.js"></script>
		<link rel="stylesheet" href="css/uPlot.min.css" />
	</head>
	<body><div id="root"></div></body>
</html>`

	// Fake upstream serving the SigNoz SPA + its static assets. This
	// models the behaviour of the real signoz query-service, which
	// returns the same index.html for every path the SPA might request
	// (it always returns the SPA shell; client-side routing handles
	// the actual route).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/assets/"):
			w.Header().Set("Content-Type", "application/javascript")
			w.Write([]byte("// fake js bundle: " + r.URL.Path))
		case strings.HasPrefix(r.URL.Path, "/css/"):
			w.Header().Set("Content-Type", "text/css")
			w.Write([]byte("/* fake css: " + r.URL.Path + " */"))
		case r.URL.Path == "/favicon.ico":
			w.Header().Set("Content-Type", "image/x-icon")
			w.Write([]byte("FAVICON"))
		default:
			// All other paths (including /, /logs, /metrics, ...) return
			// the SPA shell. The proxy strips /signoz, so the upstream
			// sees the bare path.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(signozHTML))
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	// The proxy under test. Mirrors the wiring in Mount() but isolated
	// for testability.
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	proxy.ErrorLog = nil
	proxy.ModifyResponse = func(r *http.Response) error {
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/html") {
			return nil
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			return readErr
		}
		r.Body.Close()
		rewritten := rewriteSigNozHTML(string(body), "/signoz")
		r.Body = io.NopCloser(strings.NewReader(rewritten))
		r.Header.Set("Content-Length", "")
		return nil
	}

	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/signoz") {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix("/signoz", proxy).ServeHTTP(w, r)
	}))
	defer controlPlane.Close()

	// 1. Fetch the proxied SPA shell.
	resp, err := http.Get(controlPlane.URL + "/signoz/logs")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from /signoz/logs, got %d", resp.StatusCode)
	}
	html := string(body)
	mustContain(t, html, `<base href="/signoz/" />`)
	mustContain(t, html, `src="./assets/index-XXXXX.js"`)
	mustContain(t, html, `href="css/uPlot.min.css"`)
	// Guard against the previous bug: the HTML must not produce any
	// URL that the browser would resolve to /signoz/signoz/... —
	// that's the double-prefix that made the SPA recursively fetch
	// itself instead of the JS bundle.
	mustNotContain(t, html, `/signoz/signoz/`)

	// 2. The script src is relative. With the base href /signoz/, the
	// browser will request /signoz/assets/index-XXXXX.js. The proxy
	// must forward that to upstream /assets/index-XXXXX.js and return
	// the JS bundle (not the SPA HTML).
	jsResp, err := http.Get(controlPlane.URL + "/signoz/assets/index-XXXXX.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBody, _ := io.ReadAll(jsResp.Body)
	jsResp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from /signoz/assets/index-XXXXX.js, got %d", jsResp.StatusCode)
	}
	if !strings.Contains(string(jsBody), "fake js bundle") {
		t.Fatalf("expected upstream to serve JS, got HTML instead — this is the regression:\n%s", string(jsBody))
	}
	if got := jsResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
		t.Fatalf("expected JS content type, got %q", got)
	}

	// 3. The CSS link is a bare relative path. With the base href
	// /signoz/, the browser requests /signoz/css/uPlot.min.css.
	cssResp, err := http.Get(controlPlane.URL + "/signoz/css/uPlot.min.css")
	if err != nil {
		t.Fatal(err)
	}
	cssBody, _ := io.ReadAll(cssResp.Body)
	cssResp.Body.Close()
	if !strings.Contains(string(cssBody), "fake css") {
		t.Fatalf("expected upstream to serve CSS, got:\n%s", string(cssBody))
	}
}
