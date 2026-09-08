package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

type qaFE struct{ apiv1connect.UnimplementedFileEditServiceHandler }
func (f *qaFE) GetSessionFileEdits(ctx context.Context, _ *connect.Request[apiv1.GetSessionFileEditsRequest]) (*connect.Response[apiv1.GetSessionFileEditsResponse], error) {
	return connect.NewResponse(&apiv1.GetSessionFileEditsResponse{
		Edits: []*apiv1.FileEdit{{Id: "e1", Path: "docs/x.md", Kind: "modify", Seq: 1,
			UnifiedDiff: "--- a/docs/x.md\n+++ b/docs/x.md\n@@ -1,3 +1,3 @@\n line1\n-line2\n+line2x\n line3\n"}},
		MaxSeq: 1,
	}), nil
}
type qaAuth struct{ apiv1connect.UnimplementedAuthServiceHandler }
func (f *qaAuth) ListIdentities(context.Context, *connect.Request[apiv1.ListIdentitiesRequest]) (*connect.Response[apiv1.ListIdentitiesResponse], error) {
	return connect.NewResponse(&apiv1.ListIdentitiesResponse{Identities: []*apiv1.Identity{{Id: "id1", TenantId: "tnt"}}}), nil
}

// qaApp returns a shell wired to a live mock FileEditService + AuthService.
func qaApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	ff := &qaFE{}
	fp, fph := apiv1connect.NewFileEditServiceHandler(ff)
	fa := &qaAuth{}
	ap, aph := apiv1connect.NewAuthServiceHandler(fa)
	mux := http.NewServeMux()
	mux.Handle(fp, fph)
	mux.Handle(ap, aph)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL, Token: "oc_test"}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "p", URL: "https://x"}, "v9")
	ex := &diffStubOwner{detailID: "exec-1"}
	m.RegisterScreen(TabExecution, ex)
	m.setFocus(focusContent)
	m.width, m.height = 120, 40
	m.SwitchTo(TabExecution)
	return m, srv
}

// openPaneViaD presses D and consumes the fetch cmd (bubbletea runs Cmds
// off-loop), returning the updated App.
func openPaneViaD(m *App) *App {
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	m = nm.(*App)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			nm2, _ := m.Update(msg)
			m = nm2.(*App)
		}
	}
	return m
}

func TestQADiffPaneOpenEndToEnd(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	m, _ := qaApp(t)
	m = openPaneViaD(m)
	if !m.diffOpen {
		t.Fatal("D did not open the pane")
	}
	if !m.diffPane.HasOwner() {
		t.Fatal("pane has no owner after open")
	}
	view := ansi.Strip(m.View())
	t.Logf("=== shell view, pane open, after fetch ===")
	t.Logf("\n%s", view)
	if !strings.Contains(strings.ToLower(view), "line2x") {
		t.Errorf("pane rail did not render diff content (no 'line2x')")
	}
	if !strings.Contains(view, "✕") {
		t.Errorf("pane tab bar missing the docked ✕ close button")
	}
}

func TestQADiffPaneMouseCloseEndToEnd(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	m, _ := qaApp(t)
	m = openPaneViaD(m)
	if !m.diffOpen || !m.diffPane.HasOwner() {
		t.Fatal("pane not open with owner before mouse-close")
	}
	// Click the ✕ close button (glyphX content-relative + 1 border, row 2).
	gx := m.diffPane.GlyphX()
	nm, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: gx + 1, Y: 2})
	m = nm.(*App)
	if m.diffOpen {
		t.Fatalf("mouse click on ✕ did not close the pane")
	}
	view := ansi.Strip(m.View())
	if strings.Contains(view, "line2x") {
		t.Errorf("pane content still rendered after close")
	}
	t.Logf("=== shell view after mouse-close (full width restored) ===")
	t.Logf("\n%s", view)
}
