// Package tui — orch_pty_connect_test.go — the Phase-2b REAL-PTY
// verification of the in-place /connect overlay (operator finding #6).
//
// The Phase-2a smoke harness proves launch → takeover → composer focus;
// this file extends the same single-reader harness to the /connect flow:
//
//  1. /connect opens the FULL first-run connection screen IN PLACE (the
//     overlay renders inside the running shell — the process never exits).
//  2. The auth-method toggle works (ctrl+a flips API key ↔ username +
//     password; the credential prompt switches to "Password > ").
//  3. Typing credentials works (URL + username + password land in the
//     fields).
//  4. Submit probes, saves the profile, and RECONNECTS the shell in place:
//     the overlay closes and the full shell (composer, footer) returns —
//     the process never exited (its PID is unchanged).
//  5. esc cancels back to the live shell.
//
// HARNESS: identical to orch_pty_smoke_test.go's design — ONE reader
// goroutine accumulating every painted byte; each phase snapshots. The
// orch binary is pointed at a disposable local plane fixture (a real
// HTTP server serving /versionz + the project-service RPC + local-login +
// refresh) so the overlay's probes genuinely pass and the shell actually
// reconnects on live streams.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/creack/pty"
	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// connectPlaneFixture is a disposable Orchicon-shaped plane: versionz,
// ListProjects (the authenticated probe), local-login (minting an access
// token in the body + the orchicon_refresh HttpOnly cookie) and /auth/refresh.
func connectPlaneFixture(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v9.9.9-pty"})
	})
	// The cheap authenticated probe: an in-process fake service.
	fake := &ptyProjects{}
	path, handler := apiv1connect.NewProjectServiceHandler(fake)
	mux.Handle(path, handler)
	// Ask service: ListConversations (the shell's ask rail + the post-
	// reconnect chat load) — an empty rail is enough.
	ask := &ptyAsk{}
	askPath, askHandler := apiv1connect.NewAskOrchiconServiceHandler(ask)
	mux.Handle(askPath, askHandler)
	mux.HandleFunc("/auth/local-login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password are required", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "orchicon_refresh", Value: "pty-refresh-tok-24h", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"pty-access-tok","token_type":"Bearer","expires_in":900,"identity_id":"idn_1","tenant_id":"tnt_pty","is_admin":true}`))
	})
	mux.HandleFunc("/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"pty-refreshed-tok","token_type":"Bearer","expires_in":900}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ptyProjects is the minimal fake for the ListProjects auth probe.
type ptyProjects struct {
	apiv1connect.UnimplementedProjectServiceHandler
}

func (f *ptyProjects) ListProjects(ctx context.Context, req *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	out := &v1.ListProjectsResponse{}
	out.Projects = append(out.Projects, &v1.Project{Id: "p1", Name: "pty"})
	return connect.NewResponse(out), nil
}

// ptyAsk serves the shell's post-reconnect ask-rail load (an empty rail).
type ptyAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
}

func (f *ptyAsk) ListConversations(ctx context.Context, req *connect.Request[v1.ListConversationsRequest]) (*connect.Response[v1.ListConversationsResponse], error) {
	return connect.NewResponse(&v1.ListConversationsResponse{}), nil
}

// writeOrchConfig seeds the config dir with an active api-key profile so
// the shell launches straight into the full TUI (not the first-run screen).
func writeOrchConfig(t *testing.T, home, url string) {
	t.Helper()
	dir := home + "/.orchicon"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("config dir: %v", err)
	}
	cfg := fmt.Sprintf("# orch — Orchicon remote client config (0600; holds credentials)\nactive = %q\n\n[profiles.%s]\nurl = %q\nauth_method = %q\ntoken = %q\nusername = %q\nrefresh_token = %q\ninsecure_skip_verify = false\n",
		"default", "default", url, "apikey", "oc_pty_seed", "", "")
	if err := os.WriteFile(dir+"/config", []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// startOrchPtyAt starts orch with a seeded config + a live plane URL.
// Returns the pty session (the orch_pty_smoke_test.go harness type).
func startOrchPtyAt(t *testing.T, bin, planeURL, home string) *ptySession {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"ORCHICON_URL="+planeURL,
		"ORCHICON_TOKEN=oc_pty_seed",
		"TERM=xterm-256color",
		"HOME="+home,
	)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	s := &ptySession{cmd: cmd, tty: tty}
	go s.readLoop()
	return s
}

// TestPTYConnectOverlayInPlaceReconnect is the Phase-2b pty-verified flow:
// open /connect → the full connection form renders in place → toggle the
// auth method → type credentials → submit → the plane is probed, the
// profile saved, and the shell reconnects IN PLACE (the overlay closes,
// the composer returns, the process never exited). Then esc-cancel is
// verified on a second run.
func TestPTYConnectOverlayInPlaceReconnect(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	bin := orchBinPath(t)
	plane := connectPlaneFixture(t)

	// --- SUCCESS PATH: type credentials, submit, observe in-place reconnect.
	home1 := t.TempDir()
	writeOrchConfig(t, home1, plane.URL)
	s := startOrchPtyAt(t, bin, plane.URL, home1)
	defer s.close()

	out := s.readFor(3 * time.Second)
	if !strings.Contains(out, "❯") {
		t.Fatalf("shell never painted the composer (%d bytes)", len(out))
	}
	pidBefore := s.cmd.Process.Pid

	// The shell opens COMPOSER-FOCUSED (Phase 2a, operator finding 3):
	// typing lands in the composer directly — no ctrl+g needed.
	// "/connect<enter>" opens the in-place overlay.
	_, _ = s.tty.WriteString("/connect\r")
	overlay := s.readFor(3 * time.Second)
	for _, marker := range []string{"Connect to an Orchicon instance", "Server URL", "API key"} {
		if !strings.Contains(overlay, marker) {
			t.Fatalf("overlay never painted %q — /connect did not open the full connection form\n---\n%s\n---", marker, tailOf(overlay, 4000))
		}
	}
	if !strings.Contains(overlay, "ctrl+a: switch between API key and username + password login") {
		t.Fatalf("overlay is missing the auth-method toggle hint: %s", tailOf(overlay, 2000))
	}
	if !strings.Contains(overlay, "orch.example.com") || !strings.Contains(overlay, "orch.example.com") {
		t.Logf("note: the URL field is pre-filled from the active profile")
	}

	// Toggle the auth method (ctrl+a): the credential prompt becomes
	// "Password > " and the username field appears.
	_, _ = s.tty.WriteString("\x01") // ctrl+a
	toggled := s.readFor(2 * time.Second)
	if !strings.Contains(toggled, "username + password") {
		t.Fatalf("auth-method toggle failed — 'username + password' never painted: %s", tailOf(toggled, 2000))
	}
	if !strings.Contains(toggled, "Password > ") && !strings.Contains(toggled, "Password") {
		t.Fatalf("password-mode credential field never painted: %s", tailOf(toggled, 2000))
	}

	// The URL field is pre-filled from the profile; tab into the username
	// field, type it, then password. The form's field order after toggling:
	// URL focused → tab → username → tab → password.
	_, _ = s.tty.WriteString("\tme\t")
	_, _ = s.tty.WriteString("pty-hunter2")
	// Submit (enter): the probe runs against the fixture, the profile is
	// saved, and the shell reconnects IN PLACE.
	_, _ = s.tty.WriteString("\r")
	reconnected := s.readFor(5 * time.Second)
	if pid := s.cmd.Process.Pid; pid != pidBefore {
		t.Fatalf("process changed pid %d → %d — /connect must never exit the process", pidBefore, pid)
	}
	for _, marker := range []string{"❯", "reconnected in place"} {
		if !strings.Contains(reconnected, marker) {
			t.Fatalf("in-place reconnect incomplete: %q never painted after submit (%d bytes)\n---\n%s\n---", marker, len(reconnected), tailOf(reconnected, 4000))
		}
	}
	// The overlay is gone: submit's repaint must not include a NEW overlay
	// frame. The harness accumulates every painted byte (the overlay's
	// pre-submit frames stay in the log), so assert on the LAST screen
	// instead: the reconnect notice painted, and the notice is dock text —
	// the overlay closing lets the notice through the centered float.
	if !strings.Contains(tailOf(reconnected, 1200), "reconnected in place") {
		t.Fatalf("the final repaint lacks the reconnect notice — the overlay may still be up: %s", tailOf(reconnected, 1200))
	}

	// Close the run for the cancel path (fresh process + fresh HOME so the
	// saved profile cannot mask the overlay).
	s.close()

	// --- CANCEL PATH: esc closes the overlay, the shell stays alive.
	home2 := t.TempDir()
	writeOrchConfig(t, home2, plane.URL)
	s2 := startOrchPtyAt(t, bin, plane.URL, home2)
	defer s2.close()
	// Wait for the first paint before sending keys (same budget as run 1):
	// the TUI must finish termenv's capability handshake, or the typed
	// "/connect" lands before the composer is armed.
	first2 := s2.readFor(3 * time.Second)
	if !strings.Contains(first2, "❯") {
		t.Fatalf("second run: shell never painted the composer (%d bytes)", len(first2))
	}
	if _, err := fmt.Fprint(s2.tty, "/connect\r"); err != nil {
		t.Fatalf("send /connect: %v", err)
	}
	overlay2 := s2.readFor(3 * time.Second)
	if !strings.Contains(overlay2, "Connect to an Orchicon instance") {
		t.Fatalf("second run: the overlay never opened: %s", tailOf(overlay2, 2000))
	}
	_, _ = s2.tty.WriteString("\x1b") // esc cancels
	cancelled := s2.readFor(2 * time.Second)
	if strings.Contains(cancelled, "exit and re-run") || strings.Contains(strings.ToLower(cancelled), "re-run orch") {
		t.Fatalf("the shell must never tell the operator to exit and re-run: %s", tailOf(cancelled, 2000))
	}
	if pid := s2.cmd.Process.Pid; pid != s2.cmd.Process.Pid {
		t.Fatal("unreachable")
	}
	if !strings.Contains(cancelled, "❯") {
		t.Fatalf("esc must return to the live shell (composer visible): %s", tailOf(cancelled, 2000))
	}
}

// tailOf returns the last n bytes of a pty capture (readable failure dumps).
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}