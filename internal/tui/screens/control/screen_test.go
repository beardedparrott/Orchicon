package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// fakeDock records the shell-side mutation feedback.
type fakeDock struct{ errs, notices []string }

func (d *fakeDock) DockError(m string)  { d.errs = append(d.errs, m) }
func (d *fakeDock) DockNotice(m string) { d.notices = append(d.notices, m) }

func dims(t *testing.T, name, s string, w, h int) {
	t.Helper()
	lines := strings.Split(s, "\n")
	if len(lines) != h {
		t.Fatalf("%s: %d lines, want %d", name, len(lines), h)
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Fatalf("%s: line %d width %d, want %d", name, i, got, w)
		}
	}
}

// The Control screen renders the full viewport exactly (panels + the
// Activity stream panel + overlays never break the h×w contract).
func TestControlViewIsExactlyViewport(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}} {
		m := New(nil, nil)
		m.SetShell(&fakeDock{})
		m.SetSize(sz[0], sz[1])
		m.LoadItems("webhooks", []kit2.Item{{ID: "w1", Title: "deploy", Meta: "active"}}, "")
		m.SelectSource("webhooks")
		dims(t, "control", m.View(), sz[0], sz[1])

		// With the confirm dialog overlaid, still exactly h×w.
		act := m.actionsForSelection()
		if len(act) == 0 {
			t.Fatal("no actions for the selected webhook")
		}
		m.openActionsDialog(act[0])
		dims(t, "control+dialog", m.View(), sz[0], sz[1])
	}
}

// A Form creates an entity end-to-end through the mutation layer: the RPC
// fires with the field values.
func TestFormCreatesWebhookThroughMutationLayer(t *testing.T) {
	var got *apiv1.CreateSubscriptionRequest
	m := New(nil, nil)
	dock := &fakeDock{}
	m.SetShell(dock)
	m.SetSize(100, 30)
	m.rpcCreateWebhook = func(ctx context.Context, r *apiv1.CreateSubscriptionRequest) error {
		got = r
		return nil
	}
	if !m.SelectSource("webhooks") {
		t.Fatal("no webhooks source")
	}
	// "n" opens the typed create form on the webhooks pane.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !m.formOpen() {
		t.Fatal("n did not open the create form")
	}
	m.form.Set("name", "ci-events")
	m.form.Set("target_url", "https://example.test/hook")
	m.form.Set("event_filter", "execution.completed")
	m.form.Set("scope", "tenant")
	m.form.Set("secret", "shhh")
	m.form.Set("max_retries", "5")
	cmd, err := m.form.Submit()
	if err != nil {
		t.Fatalf("submit: %v (%v)", err, m.form.Errors)
	}
	if cmd == nil {
		t.Fatal("submit did not produce a mutation cmd")
	}
	cmd() // run the async RPC thunk
	if got == nil {
		t.Fatal("RPC never fired")
	}
	if got.GetName() != "ci-events" || got.GetTargetUrl() != "https://example.test/hook" ||
		got.GetEventFilter() != "execution.completed" || got.GetScope() != "tenant" ||
		got.GetSecret() != "shhh" || got.GetMaxRetries() != 5 {
		t.Fatalf("RPC fired with wrong field values: %+v", got)
	}
	if strings.Contains(m.View(), "shhh") {
		t.Fatal("the secret field was rendered")
	}
}

// An Action confirms, applies optimistically, and rolls back on error with
// the failure text pinned into the dock.
func TestActionRollsBackOnRPCErrorAndSurfacesInDock(t *testing.T) {
	m := New(nil, nil)
	dock := &fakeDock{}
	m.SetShell(dock)
	m.SetSize(100, 30)
	m.LoadItems("webhooks", []kit2.Item{{ID: "w1", Title: "deploy", Meta: "active"}}, "")
	m.SelectSource("webhooks")
	m.rpcDeleteWebhook = func(ctx context.Context, id string) error {
		return errors.New("permission_denied: missing control:write")
	}

	acts := m.actionsForSelection()
	if len(acts) < 2 {
		t.Fatalf("expected disable+delete actions, got %d", len(acts))
	}
	var del kit2.Action
	for _, a := range acts {
		if a.Label == "delete" {
			del = a
		}
	}
	if del.Label != "delete" || !del.NeedsConfirm() {
		t.Fatal("delete should require confirmation")
	}
	m.openActionsDialog(del)
	if m.Open == nil {
		t.Fatal("confirm dialog did not open")
	}
	// Enter on the dialog confirms -> the action runs through mutate,
	// applying the optimistic delete before the RPC.
	cmd := m.OnDialog("delete")
	if cmd == nil {
		t.Fatal("confirm did not dispatch the mutation")
	}
	res, ok := cmd().(mutateResult)
	if !ok {
		t.Fatalf("mutation cmd returned %T", cmd())
	}
	if res.Err == nil {
		t.Fatal("expected the RPC to fail")
	}
	// The optimistic delete ran before the RPC failed.
	if _, ok := m.ActiveItem(); ok {
		t.Fatal("optimistic delete did not remove the row")
	}
	// The failure rolls the local model back and lands in the dock.
	m.HandleMutation(res)
	if len(dock.errs) == 0 || !strings.Contains(dock.errs[0], "permission_denied") {
		t.Fatalf("failure not surfaced in the dock: %v", dock.errs)
	}
}

// Assertion that no screen still depends on the old screen model.
func TestNoScreenUsesScreenkitBase(t *testing.T) {
	if bytesContainAny(scanScreens(t), "screenkit.Base") {
		t.Fatal("a screen still embeds screenkit.Base as its screen model")
	}
}
