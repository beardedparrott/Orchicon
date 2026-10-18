package desktop

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// testSPA is a minimal stand-in for the built bundle. Tests inject it because
// frontend/dist holds only .gitkeep in a checkout that has not run
// `npm run build` — and the shell refuses to start without an index.html.
func testSPA() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<!doctype html><html><head><title>orchicon</title></head><body><div id=root></div></body></html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('spa')")},
	}
}

// newTestShell starts a Shell against the given fake plane.
func newTestShell(t *testing.T, planeURL string) *Shell {
	t.Helper()
	shell, err := New(Config{PlaneURL: planeURL, FrontendFS: testSPA(), Logf: t.Logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := shell.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := contextWithTimeout()
		defer cancel()
		_ = shell.Close(ctx)
	})
	return shell
}

// THE STREAMING ASSERTION — the highest-value test in this package.
//
// The SPA reads a Connect server-stream for the life of an execution
// (frontend/src/api/useStream.ts). A proxy that buffers, or a server with a
// WriteTimeout, turns that into a view that silently stops updating — the
// failure mode the brief names as most likely. So this asserts the OBSERVABLE
// property: frame 1 is readable from the shell origin BEFORE frame 2 exists.
func TestProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versionz" {
			_, _ = w.Write([]byte(`{"version":"v-test"}`))
			return
		}
		w.Header().Set("Content-Type", "application/connect+json")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("frame1"))
		flusher.Flush()
		<-release // frame 2 does not exist until the test says so
		_, _ = w.Write([]byte("frame2"))
		flusher.Flush()
	}))
	defer plane.Close()

	shell := newTestShell(t, plane.URL)
	resp, err := http.Get(shell.OriginURL() + "/orchicon.api.v1.ExecutionService/StreamExecutionEvents")
	if err != nil {
		t.Fatalf("GET through shell origin: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	got := make([]byte, 6)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(br, got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read frame 1: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frame 1 did not arrive before frame 2 was written — the shell origin buffers the stream, " +
			"so a live execution view would silently stop updating")
	}
	if string(got) != "frame1" {
		t.Fatalf("first frame = %q, want frame1", got)
	}

	close(release)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read frame 2: %v", err)
	}
	if string(rest) != "frame2" {
		t.Fatalf("second frame = %q, want frame2", rest)
	}
}

// Auth is NOT the shell's business: the bearer token, the cookie jar and the
// HttpOnly refresh cookie all have to survive the hop untouched, or the SPA's
// 401-refresh-and-retry (frontend/src/api/clients.ts) can never work.
func TestProxyForwardsAuthHeadersVerbatim(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versionz" {
			_, _ = w.Write([]byte(`{"version":"v-test"}`))
			return
		}
		w.Header().Set("Set-Cookie", "orchicon_refresh=abc; Path=/auth; HttpOnly; SameSite=Strict")
		fmt.Fprintf(w, "auth=%s cookie=%s", r.Header.Get("Authorization"), r.Header.Get("Cookie"))
	}))
	defer plane.Close()

	shell := newTestShell(t, plane.URL)
	req, err := http.NewRequest(http.MethodPost, shell.OriginURL()+"/orchicon.api.v1.AuthService/Refresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-123")
	req.Header.Set("Cookie", "orchicon_refresh=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through shell origin: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if want := "auth=Bearer tok-123 cookie=orchicon_refresh=abc"; string(body) != want {
		t.Errorf("plane saw %q, want %q — the shell must not rewrite or strip auth headers", body, want)
	}
	cookies := resp.Cookies()
	if len(cookies) == 0 || cookies[0].Name != "orchicon_refresh" || !cookies[0].HttpOnly {
		t.Errorf("Set-Cookie did not survive the shell: %v — without it the refresh flow cannot work in the webview", cookies)
	}
}

// AC2: the app renders the SPA it was BUILT with. The plane is deliberately
// serving a different UI; the shell must never hand it over.
func TestProxyDoesNotServeThePlaneUI(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/versionz" {
			_, _ = w.Write([]byte(`{"version":"v-test"}`))
			return
		}
		_, _ = w.Write([]byte("<html>PLANE UI v-other</html>"))
	}))
	defer plane.Close()

	shell := newTestShell(t, plane.URL)
	for _, path := range []string{"/", "/dashboard", "/index.html"} {
		resp, err := http.Get(shell.OriginURL() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "PLANE UI") {
			t.Errorf("GET %s served the PLANE's UI — the app must ship the embedded bundle, never a fetched one", path)
		}
		if !strings.Contains(string(body), `id=root`) {
			t.Errorf("GET %s did not serve the embedded index.html (got %q)", path, body)
		}
	}
}

// The origin decision must not quietly become a CORS decision: the shell adds
// no Access-Control-* headers, because same-origin needs none and adding them
// would widen what the plane accepts.
func TestShellAddsNoCORSHeaders(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v-test"}`))
	}))
	defer plane.Close()
	shell := newTestShell(t, plane.URL)
	for _, path := range []string{"/", "/versionz", "/orchicon.api.v1.ExecutionService/GetExecution"} {
		resp, err := http.Get(shell.OriginURL() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		for name := range resp.Header {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Errorf("GET %s carries %s — the shell must not add CORS headers", path, name)
			}
		}
	}
}

// ---- prefix-table drift guards -------------------------------------------------

var pathLiteralRe = regexp.MustCompile(`"(/[^"]*)"`)

func coveredByProxy(p string) bool {
	for _, prefix := range proxyPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// TestProxyPrefixesCoverPlanePassthrough reads the PLANE'S OWN definition of
// "not the SPA" (withFrontend's passthrough list in cmd/orchicon/serve_state.go)
// and asserts the shell forwards every path on it. If the plane adds one and the
// shell does not, the app gets index.html where it expected JSON.
func TestProxyPrefixesCoverPlanePassthrough(t *testing.T) {
	src := readFile(t, filepath.Join("..", "..", "cmd", "orchicon", "serve_state.go"))
	start := strings.Index(src, "func withFrontend(")
	if start < 0 {
		t.Fatal("withFrontend not found in serve_state.go — did it move?")
	}
	end := strings.Index(src[start:], "cleanPath := strings.TrimPrefix")
	if end < 0 {
		t.Fatal("the passthrough block's end was not found — the guard cannot read the list")
	}
	segment := src[start : start+end]
	literals := pathLiteralRe.FindAllStringSubmatch(segment, -1)
	if len(literals) < 5 {
		t.Fatalf("found only %d passthrough paths — the guard would pass vacuously", len(literals))
	}
	for _, m := range literals {
		if len(m[1]) < 2 {
			continue // "/" is the file-server prefix, not a passthrough path
		}
		if !coveredByProxy(m[1]) {
			t.Errorf("the plane passes %q straight through to the API, but the shell's proxyPrefixes does not cover "+
				"it — in the desktop app that path would be answered with index.html", m[1])
		}
	}
}

// TestProxyCoversEveryPlaneRoute reads the routes internal/api actually registers
// and asserts the shell forwards each one, so a new plane route is a failing test
// rather than a blank page in the app.
func TestProxyCoversEveryPlaneRoute(t *testing.T) {
	apiSrc := readFile(t, filepath.Join("..", "api", "api.go"))
	routeRe := regexp.MustCompile(`mux\.(?:Handle|HandleFunc)\("([^"]+)"`)
	var seen int
	for _, m := range routeRe.FindAllStringSubmatch(apiSrc, -1) {
		seen++
		if !coveredByProxy(m[1]) {
			t.Errorf("internal/api mounts %q but the shell does not proxy it", m[1])
		}
	}
	if seen == 0 {
		t.Fatal("found no mux routes in internal/api/api.go — the guard would pass vacuously")
	}

	// Every generated Connect procedure is served under the API prefix; assert the
	// prefix that carries ALL of them is forwarded.
	gen, err := filepath.Glob(filepath.Join("..", "..", "api", "gen", "go", "orchicon", "api", "v1", "apiv1connect", "*.connect.go"))
	if err != nil || len(gen) == 0 {
		t.Fatalf("glob generated connect clients: %v (%d files)", err, len(gen))
	}
	procRe := regexp.MustCompile(`Procedure = "([^"]+)"`)
	var procs int
	for _, path := range gen {
		for _, m := range procRe.FindAllStringSubmatch(readFile(t, path), -1) {
			procs++
			if !coveredByProxy(m[1]) {
				t.Errorf("Connect procedure %q is not proxy-covered by the shell", m[1])
			}
		}
	}
	if procs == 0 {
		t.Fatal("found no Connect procedures — the guard would pass vacuously")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
