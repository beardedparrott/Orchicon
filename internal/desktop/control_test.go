package desktop

// control_test.go — PLANE CONTROL OVER THE SHELL'S OWN ORIGIN.
//
// The failure this whole file guards, in the operator's words: "under the new
// manage server section, even though it appears the instance is running it says
// 'Connecting / dev / port unknown / contacting the plane…' and has no controls
// lit up to click on to start, stop, or restart."
//
// Cause: `planeControl()` read `window.go.planebind.Service`, which exists only
// on Wails' own document. The app is served from the shell's loopback http origin
// (so the HttpOnly refresh cookie and the Connect server-stream work), so the
// binding was never there — the page resolved `null`, never probed, and rendered
// "contacting the plane…" for ever with every button inert.
//
// These tests drive the replacement transport end to end through a real HTTP
// server, because the bug was in the TRANSPORT, not in the decisions (those are
// already covered by internal/planebind).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/planebind"
)

// fakeControl is a Control whose answers are fixed, so the transport is the only
// thing under test.
type fakeControl struct {
	status   planebind.StatusDTO
	actions  []string // instances passed to Start, for the "which instance" assertion
	err      error
	lastPort int
}

func (f *fakeControl) Instances() ([]planebind.StatusDTO, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []planebind.StatusDTO{f.status}, nil
}

func (f *fakeControl) Status(string) (planebind.StatusDTO, error) {
	if f.err != nil {
		return planebind.StatusDTO{}, f.err
	}
	return f.status, nil
}

func (f *fakeControl) ResolveControlPort(port int) (planebind.ResolutionDTO, error) {
	f.lastPort = port
	return planebind.ResolutionDTO{Instance: "dev", ControlPort: port, Known: true}, nil
}

func (f *fakeControl) Start(instance string) (planebind.ActionDTO, error) {
	f.actions = append(f.actions, instance)
	return planebind.ActionDTO{Instance: instance, Action: "start", Outcome: "started", OK: true}, f.err
}

func (f *fakeControl) Stop(instance string) (planebind.ActionDTO, error) {
	f.actions = append(f.actions, instance)
	return planebind.ActionDTO{Instance: instance, Action: "stop", Outcome: "stopped", OK: true}, f.err
}

func (f *fakeControl) Restart(instance string) (planebind.ActionDTO, error) {
	f.actions = append(f.actions, instance)
	return planebind.ActionDTO{Instance: instance, Action: "restart", Outcome: "started", OK: true}, f.err
}

// controlShell starts a shell with the control surface installed and returns its
// origin plus the token.
func controlShell(t *testing.T, ctl Control, token string) (origin string, shell *Shell) {
	t.Helper()
	shell, err := New(Config{
		PlaneURL:     "http://127.0.0.1:1", // never dialled by these tests
		FrontendFS:   testSPA(),
		ControlToken: token,
		PrefsPath:    filepath.Join(t.TempDir(), "prefs.json"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shell.SetControl(ctl, token)
	if err := shell.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shell.Close(t.Context()) })
	return shell.OriginURL(), shell
}

func doRequest(t *testing.T, method, url, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set(controlTokenHeader, token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res, body
}

// TestControlStatusAnswersOverHTTP is the outermost assertion: a GET against the
// shell's own origin returns the instance's real state as JSON, which is what the
// page needs to leave "connecting".
func TestControlStatusAnswersOverHTTP(t *testing.T) {
	ctl := &fakeControl{status: planebind.StatusDTO{
		Instance: "dev", ControlPort: 8080, State: "connected", Detail: "200 OK", Healthy: true,
	}}
	origin, _ := controlShell(t, ctl, "tok")

	res, body := doRequest(t, http.MethodGet, origin+controlPath+"status?instance=dev", "tok")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body["State"] != "connected" {
		t.Errorf("state = %v, want connected — the page would stay on \"connecting\"", body["State"])
	}
	if body["ControlPort"] != float64(8080) {
		t.Errorf("controlPort = %v, want 8080 — the page renders \"port unknown\" without it", body["ControlPort"])
	}
}

// TestControlRequiresTheShellToken — a browser cannot read a cross-origin
// response, so it cannot learn the token; without it, every mutating call is
// refused.
func TestControlRequiresTheShellToken(t *testing.T) {
	ctl := &fakeControl{status: planebind.StatusDTO{Instance: "dev"}}
	origin, _ := controlShell(t, ctl, "tok")

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "not-the-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := doRequest(t, http.MethodPost, origin+controlPath+"start", tc.token)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — an unauthenticated caller must not reach plane control",
					res.StatusCode)
			}
		})
	}
	if len(ctl.actions) != 0 {
		t.Errorf("plane control ran %v without a valid token", ctl.actions)
	}
}

// TestControlNeverStartsWithoutAnInstanceLabel — the page must never act on "the
// plane" under a name nobody resolved.
func TestControlNeverStartsWithoutAnInstanceLabel(t *testing.T) {
	ctl := &fakeControl{}
	origin, _ := controlShell(t, ctl, "tok")

	req, _ := http.NewRequest(http.MethodPost, origin+controlPath+"start", strings.NewReader(`{}`))
	req.Header.Set(controlTokenHeader, "tok")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: a missing instance must be refused, not defaulted", res.StatusCode)
	}
	if len(ctl.actions) != 0 {
		t.Errorf("started %v with no instance named", ctl.actions)
	}
}

// TestControlEndpointsAreNotProxiedToThePlane — the control prefix must be
// answered by the shell. If a plane path ever claimed it, the app would be asking
// the plane to control itself.
func TestControlEndpointsAreNotProxiedToThePlane(t *testing.T) {
	if isPlanePath(controlPath) {
		t.Fatalf("%s is classified as a PLANE path — it would be proxied instead of answered", controlPath)
	}
	for i := 0; i < len(controlPath); i++ {
		if isPlanePath(controlPath[:i]) {
			t.Fatalf("a prefix of %s (%q) is a plane path, so the proxy would swallow it",
				controlPath, controlPath[:i])
		}
	}
}

// TestUsePlaneSwitchesTheInstance — choosing an instance must retarget the window
// and REMEMBER it. Previously the plane was fixed at launch, so a person running
// dev and prod had no way to switch.
func TestUsePlaneSwitchesTheInstance(t *testing.T) {
	prefs := filepath.Join(t.TempDir(), "prefs.json")
	shell, err := New(Config{
		PlaneURL:   "http://127.0.0.1:8080",
		FrontendFS: testSPA(),
		PrefsPath:  prefs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := shell.PlaneURL(); got != "http://127.0.0.1:8080" {
		t.Fatalf("starting plane = %q", got)
	}
	info, err := shell.UsePlane("prod")
	if err != nil {
		t.Fatalf("UsePlane(prod): %v", err)
	}
	if !strings.HasSuffix(info.PlaneURL, ":8091") {
		t.Errorf("prod plane URL = %q, want the shared table's 8091", info.PlaneURL)
	}
	if info.Instance != "prod" {
		t.Errorf("instance = %q, want prod", info.Instance)
	}
	if got := shell.PlaneURL(); !strings.HasSuffix(got, ":8091") {
		t.Errorf("the window still points at %q — the proxy would keep talking to the old plane", got)
	}
	// Remembered for the next launch.
	if got := LoadChosenInstance(prefs); got != "prod" {
		t.Errorf("remembered instance = %q, want prod", got)
	}

	// An unknown name is REFUSED rather than defaulted to something real.
	if _, err := shell.UsePlane("nope"); err == nil {
		t.Error("UsePlane(nope) succeeded — an unknown instance must not be silently mapped")
	}
	if got := shell.PlaneURL(); !strings.HasSuffix(got, ":8091") {
		t.Errorf("a refused switch moved the plane to %q", got)
	}
}

// TestPrefsSurviveACorruptFile — a bad preference must never stop the app opening.
func TestPrefsSurviveACorruptFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "prefs.json")
	if err := SaveChosenInstance(good, "test"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := LoadChosenInstance(good); got != "test" {
		t.Errorf("round trip = %q, want test", got)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadChosenInstance(bad); got != "" {
		t.Errorf("a corrupt preference returned %q; the caller must fall back to the default", got)
	}
	if got := LoadChosenInstance(filepath.Join(dir, "absent.json")); got != "" {
		t.Errorf("an absent preference returned %q, want \"\"", got)
	}
}

// TestBootstrapDocumentNavigatesInsteadOfRedirecting pins the fix for the white
// screen: a served document that navigates, never a 3xx the webview renders as
// text.
func TestBootstrapDocumentNavigatesInsteadOfRedirecting(t *testing.T) {
	const origin = "http://127.0.0.1:34567"
	doc := BootstrapHTML(origin)

	if !strings.Contains(doc, "location.replace(") {
		t.Fatal("the bootstrap document does not navigate — the window would sit on it for ever")
	}
	if !strings.Contains(doc, `"`+origin+`"`) {
		t.Errorf("the bootstrap document does not name the shell origin %q", origin)
	}
	// A visible way in when script is blocked: never a blank window.
	if !strings.Contains(doc, "<a href=") {
		t.Error("the bootstrap document has no fallback link; a blocked script would leave a blank window")
	}
	// It must be a real document, not a redirect body.
	if strings.Contains(strings.ToLower(doc), "temporary redirect") {
		t.Error("the bootstrap document mentions \"temporary redirect\" — that is the response body the " +
			"webview rendered instead of navigating")
	}
}

// TestBootstrapDocumentCannotBeBrokenByTheOrigin — the origin is interpolated into
// an inline script, so a quote in it must not escape.
func TestBootstrapDocumentCannotBeBrokenByTheOrigin(t *testing.T) {
	doc := BootstrapHTML(`http://127.0.0.1:1"><script>alert(1)</script>`)
	if strings.Contains(doc, `"><script>alert(1)`) {
		t.Fatal("a quote in the origin escaped the inline script")
	}
	if !strings.Contains(doc, `\u0022`) && !strings.Contains(doc, `\"`) {
		t.Error("the origin was not escaped for a JS string literal")
	}
}
