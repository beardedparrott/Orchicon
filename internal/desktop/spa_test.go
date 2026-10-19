package desktop

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// THE DESKTOP SIGNAL MUST BE UNFORGEABLE FROM THE PAGE, so the injection is
// asserted on the properties that make it so rather than on its existence
// alone: writable:false + configurable:false + Object.freeze.
func TestShellInjectsDesktopFlagNonConfigurable(t *testing.T) {
	script := desktopScript(testInfo(), "test-token")
	for _, want := range []string{
		`Object.defineProperty(window,"` + desktopGlobal + `"`,
		"writable:false",
		"configurable:false",
		"Object.freeze(",
		`"desktop":true`,
		`"planeURL":"http://127.0.0.1:8080"`,
		`"instance":"dev"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the injected desktop flag is missing %q — without it the SPA (or any page script) could "+
				"redefine or mutate the flag, so a browser could claim to be the desktop app:\n%s", want, script)
		}
	}
}

// The flag must be IN the document the shell serves — not merely constructable.
func TestServedIndexCarriesDesktopFlag(t *testing.T) {
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v-test"}`))
	}))
	defer plane.Close()
	shell := newTestShell(t, plane.URL)

	resp, err := http.Get(shell.OriginURL() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), desktopGlobal) {
		t.Fatalf("the served index.html does not inject %s:\n%s", desktopGlobal, body)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("index.html is served with Cache-Control %q — a rebuilt app would run a stale bundle", cc)
	}
}

// AND THE PLANE MUST NEVER INJECT IT: that is what keeps the flag off in a
// browser. This is a SOURCE assertion on purpose — the plane's withFrontend
// only injects when an index.html is embedded, so a behavioural test would pass
// vacuously on a checkout that has not run `npm run build`.
func TestPlaneNeverInjectsDesktopFlag(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "orchicon", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob cmd/orchicon: %v (%d files)", err, len(files))
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if strings.Contains(readFile(t, path), desktopGlobal) {
			t.Errorf("%s mentions %s — the plane must never inject the desktop flag, or the browser would "+
				"detect itself as the desktop app", filepath.Base(path), desktopGlobal)
		}
	}
}

// No frontend embedded = no launch. Asserted because the alternative is an
// empty window with no explanation (the plane's equivalent trap has burned this
// repo once already).
func TestStartRefusesWhenNoFrontendIsEmbedded(t *testing.T) {
	shell, err := New(Config{PlaneURL: "http://127.0.0.1:9999", FrontendFS: fstest.MapFS{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := shell.Start(); err == nil {
		t.Fatal("Start succeeded with no embedded index.html — the app would open an empty window")
	} else if !strings.Contains(err.Error(), "EMBEDS NO FRONTEND") {
		t.Errorf("the fatal error does not say what is wrong: %v", err)
	}
}
