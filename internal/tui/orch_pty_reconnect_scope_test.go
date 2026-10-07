// Package tui — orch_pty_reconnect_scope_test.go — THE LIVE OBSERVATION of the
// launch-directory workspace default surviving a reconnect (acceptance criterion 1).
//
// The rendered/unit tests (project_scope_reconnect_test.go) drive two App values in
// sequence. This file drives the REAL orch binary through the SAME two shells that
// cmd/orch's main loop actually produces, in a real pty, against a live
// Orchicon-shaped plane:
//
//  1. LAUNCH in a directory covered by a project, with a STORED session the plane
//     rejects. That is the real entry into the second-shell path: runShell's
//     launch-time credential probe answers Unauthenticated, main's loop calls
//     runConnection and then re-enters runShell — a NEW App (see the reconnect
//     topology: the in-place /connect overlay reuses the App, so this is the path
//     a reconnect can actually lose state on).
//  2. THE SECOND SHELL must still be handed the launch DIRECTORY (a fact about the
//     process, needed on every shell) while its PROMPT stays disarmed (a
//     first-shell-only rule). The observable consequence is the rail: its title
//     names the workspace derived from the launch directory, and no launch
//     question is asked.
//
// PRE-FIX (the directory blanked on a continuation) the second shell's rail reads
// "Conversations · All projects" and this test fails.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/creack/pty"
)

const (
	// ptyStaleSession is the stored API key the plane REJECTS: it is what makes the
	// launch enter the second-shell reconnect path.
	ptyStaleSession = "oc_pty_stale_session"
	// ptyFreshSession is the key the operator types on the connection screen.
	ptyFreshSession = "oc_pty_fresh_session"
	// ptyQASpace is short so the rail title (width-bounded) cannot truncate it.
	ptyQASpace = "Qaws"
)

// ptyReconnectAuth is the cheap AUTHENTICATED probe runShell opens with
// (probeIdentity): the stale session must come back Unauthenticated, which is the
// only code main treats as "ask for credentials again".
type ptyReconnectAuth struct {
	apiv1connect.UnimplementedAuthServiceHandler
}

func (h *ptyReconnectAuth) ListIdentities(ctx context.Context, req *connect.Request[v1.ListIdentitiesRequest]) (*connect.Response[v1.ListIdentitiesResponse], error) {
	if err := ptyAuthGate(req); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListIdentitiesResponse{}), nil
}

// ptyReconnectProjects serves the rail's project list — and the same auth gate, so
// the connection screen's API-key probe (ProjectService.ListProjects) is the thing
// that accepts the replacement key.
type ptyReconnectProjects struct {
	apiv1connect.UnimplementedProjectServiceHandler
	dir string
}

func (f *ptyReconnectProjects) ListProjects(ctx context.Context, req *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	if err := ptyAuthGate(req); err != nil {
		return nil, err
	}
	out := &v1.ListProjectsResponse{}
	out.Projects = append(out.Projects, &v1.Project{Id: "prj-qa", Name: ptyQASpace, ProjectDir: f.dir})
	return connect.NewResponse(out), nil
}

// ptyReconnectAsk serves the shell's conversations rail (empty is enough).
type ptyReconnectAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
}

func (f *ptyReconnectAsk) ListConversations(ctx context.Context, req *connect.Request[v1.ListConversationsRequest]) (*connect.Response[v1.ListConversationsResponse], error) {
	return connect.NewResponse(&v1.ListConversationsResponse{}), nil
}

func ptyAuthGate(req connect.AnyRequest) error {
	if strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer ") != ptyFreshSession {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("session expired"))
	}
	return nil
}

// ptyReconnectPlane is the disposable Orchicon-shaped plane this test drives: the
// version handshake, the auth probe, the project list (with the project's
// DIRECTORY), and the ask rail.
func ptyReconnectPlane(t *testing.T, projectDir string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/versionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v9.9.9-pty-reconnect"})
	})
	authPath, authHandler := apiv1connect.NewAuthServiceHandler(&ptyReconnectAuth{})
	mux.Handle(authPath, authHandler)
	projPath, projHandler := apiv1connect.NewProjectServiceHandler(&ptyReconnectProjects{dir: projectDir})
	mux.Handle(projPath, projHandler)
	askPath, askHandler := apiv1connect.NewAskOrchiconServiceHandler(&ptyReconnectAsk{})
	mux.Handle(askPath, askHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeReconnectScopeConfig seeds an api-key profile with the STALE session, so the
// shell launches straight into the credential check (not the first-run screen).
func writeReconnectScopeConfig(t *testing.T, home, url, token string) {
	t.Helper()
	dir := home + "/.orchicon"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("config dir: %v", err)
	}
	cfg := fmt.Sprintf("# orch — Orchicon remote client config (0600; holds credentials)\nactive = %q\n\n[profiles.%s]\nurl = %q\nauth_method = %q\ntoken = %q\nusername = %q\nrefresh_token = %q\ninsecure_skip_verify = false\n",
		"default", "default", url, "apikey", token, "", "")
	if err := os.WriteFile(dir+"/config", []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// startOrchPtyIn launches the real binary with its WORKING DIRECTORY set — the
// launch directory is the whole subject of this test — and an isolated HOME.
func startOrchPtyIn(t *testing.T, bin, workDir, home string) *ptySession {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Dir = workDir
	// The HOME this session must use has to be the ONLY one in the environment:
	// appending to os.Environ() leaves the inherited HOME in front of it, and a
	// first-match getenv (what the child's os.UserHomeDir does) then resolves the
	// WRONG config dir — the launch would open the first-run screen and never reach
	// the reconnect this test is about. Same for the ORCHICON_* overrides and XDG,
	// which would otherwise replace the file profile under test.
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "ORCHICON_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "TERM=xterm-256color", "HOME="+home)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	s := &ptySession{cmd: cmd, tty: tty}
	go s.readLoop()
	return s
}

// ptyANSI strips SGR/cursor/OSC sequences so assertions can match the text the
// operator reads rather than the escape stream that styles it.
var ptyANSI = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]|\\x1b\\][^\\x07\\x1b]*(?:\\x07|\\x1b\\\\)")

func ptyPlain(s string) string { return ptyANSI.ReplaceAllString(s, "") }

// waitForReconnectScopeText polls the session's accumulating paint log until needle
// appears (the persistent reader keeps draining, so no bytes are missed between
// polls).
func waitForReconnectScopeText(t *testing.T, s *ptySession, needle string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		// Match on the PLAIN text: the live paint log carries the SGR/cursor escapes
		// that style the shell, and a title or a notice is a styled span — so the
		// literal phrase is only contiguous once the escapes are removed.
		out := ptyPlain(s.readFor(250 * time.Millisecond))
		if strings.Contains(out, needle) {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live orch session never painted %q within %s (%d bytes painted)\n---\n%s\n---",
				needle, d, len(out), tailOf(out, 4000))
		}
	}
}

// TestPTYReconnectKeepsTheLaunchDirectoryWorkspace is acceptance criterion 1: a real
// orch launched from a directory covered by a project, bounced through the
// reconnect, opens AFTER it with that project selected — the rail scoped to it and
// its title naming it — exactly as the first launch would.
func TestPTYReconnectKeepsTheLaunchDirectoryWorkspace(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	bin := orchBinPath(t)

	launchDir := t.TempDir() // the directory the operator is sitting in
	plane := ptyReconnectPlane(t, launchDir)
	home := t.TempDir()
	writeReconnectScopeConfig(t, home, plane.URL, ptyStaleSession)

	s := startOrchPtyIn(t, bin, launchDir, home)
	defer s.close()

	// PHASE 1 — the stored session is rejected, so NO shell paints: the connection
	// screen comes up WITH THE REASON (not a surprise first-run form).
	before := waitForReconnectScopeText(t, s, "could not be authenticated", 25*time.Second)
	if strings.Contains(before, "Conversations · ") {
		t.Fatalf("a shell painted the rail before the credential was replaced — this run did not take the "+
			"reconnect path, so it cannot observe the defect (%d bytes)", len(before))
	}

	// Re-auth, exactly as the operator does it: the URL is pre-filled and focused;
	// tab to the credential field, clear the parked stale key, type a good one, enter.
	time.Sleep(500 * time.Millisecond)
	_, _ = s.tty.WriteString("\t")
	time.Sleep(300 * time.Millisecond)
	_, _ = s.tty.WriteString("\x15") // ctrl+u — clear the parked stale key
	_, _ = s.tty.WriteString(ptyFreshSession)
	_, _ = s.tty.WriteString("\r")

	// PHASE 2 — THE SECOND SHELL. The launch directory was computed before the
	// reconnect and must still reach this shell, so the workspace it derives is the one
	// that directory belongs to.
	//
	// The workspace is read through the shell's OWN surface: the rail only paints in
	// the Ask tab's conversation view (railVisible: askNew shows the hero), so the
	// observable is the /project picker's notice — the picker opens with its cursor ON
	// the live scope, so selecting it names the scope the shell actually holds.
	// PRE-FIX that line reads "project: All projects".
	waitForReconnectScopeText(t, s, "connected ·", 30*time.Second)
	_, _ = s.tty.WriteString("/project\r")
	time.Sleep(1500 * time.Millisecond)
	_, _ = s.tty.WriteString("\r")
	out := waitForReconnectScopeText(t, s, "project: "+ptyQASpace, 20*time.Second)

	// The workspace must NOT have fallen back to All projects (the pre-fix symptom).
	if strings.Contains(out, "project: All projects") {
		t.Errorf("the reconnect shell's workspace reads \"All projects\" — the launch-directory " +
			"default was lost across the reconnect (this is the bug)")
	}
	// THE CONTINUATION DID NOT NAG (criterion 2): no launch question, in a directory
	// the plane WOULD have offered to attach.
	if strings.Contains(out, "This directory isn't linked to an Orchicon project") {
		t.Error("the reconnect shell raised the launch prompt — re-asking on a continuation is exactly the " +
			"nagging the prompt exists to avoid")
	}
}

// The CONTROL for the test above: an unattached launch directory is silent on the
// reconnect too — criterion 2's other half, and the reason a passing run of the test
// above cannot be explained by "the prompt fired and set the scope".
//
// A directory the plane does not know must leave the rail on All projects with no
// question: the reconnect is a continuation, so the FIRST shell would have asked and
// this one must not.
func TestPTYReconnectFromAnUnattachedDirectoryAsksNothing(t *testing.T) {
	skipInteractivePTY(t)
	if testing.Short() {
		t.Skip("real-pty: skipped in -short")
	}
	bin := orchBinPath(t)

	unattached := t.TempDir()                  // NOT the project's directory
	plane := ptyReconnectPlane(t, t.TempDir()) // the project lives somewhere else
	home := t.TempDir()
	writeReconnectScopeConfig(t, home, plane.URL, ptyStaleSession)

	s := startOrchPtyIn(t, bin, unattached, home)
	defer s.close()

	waitForReconnectScopeText(t, s, "could not be authenticated", 25*time.Second)
	time.Sleep(500 * time.Millisecond)
	_, _ = s.tty.WriteString("\t")
	time.Sleep(300 * time.Millisecond)
	_, _ = s.tty.WriteString("\x15")
	_, _ = s.tty.WriteString(ptyFreshSession)
	_, _ = s.tty.WriteString("\r")

	waitForReconnectScopeText(t, s, "connected ·", 30*time.Second)
	_, _ = s.tty.WriteString("/project\r")
	time.Sleep(1500 * time.Millisecond)
	_, _ = s.tty.WriteString("\r")
	out := waitForReconnectScopeText(t, s, "project: All projects", 20*time.Second)
	if strings.Contains(out, "This directory isn't linked to an Orchicon project") {
		t.Error("the reconnect shell in an UNATTACHED directory asked to create a project — a continuation must " +
			"never re-ask, in ANY directory")
	}
}
