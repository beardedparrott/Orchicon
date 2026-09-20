package control

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// newPickerModel builds a Control screen whose model-picker loads are faked, so
// the whole adapter → provider → model cascade can be driven deterministically
// without a plane.
func newPickerModel(t *testing.T) (*Model, *[]string) {
	t.Helper()
	m, _ := newWriteModel(t)
	loads := &[]string{}
	m.rpcModelKinds = func(context.Context) ([]string, []string, error) {
		*loads = append(*loads, "kinds")
		// The Dispatcher reports opencode FIRST; the picker must still default
		// to the preferred native kind.
		return []string{"opencode", "orchicon"}, []string{"orchicon"}, nil
	}
	m.rpcModelProviders = func(_ context.Context, kind string) ([]kit2.PickerOption, error) {
		*loads = append(*loads, "providers:"+kind)
		return []kit2.PickerOption{
			{Value: "anthropic", Label: "anthropic"},
			{Value: "openai", Label: "openai"},
		}, nil
	}
	m.rpcModelModels = func(_ context.Context, kind, provider string) ([]kit2.PickerOption, bool, error) {
		*loads = append(*loads, "models:"+kind+"/"+provider)
		return []kit2.PickerOption{
			{Value: "claude-sonnet-4", Label: "claude-sonnet-4", Meta: "200K ctx"},
			{Value: "claude-opus-4", Label: "claude-opus-4", Meta: "200K ctx"},
		}, false, nil
	}
	return m, loads
}

// feed runs a load cmd and routes its message back through Update, returning the
// next cmd (the cascade's next hop).
func feed(t *testing.T, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a load cmd, got nil")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("load cmd produced no message")
	}
	_, next := m.Update(msg)
	return next
}

func lastLoad(loads []string) string {
	if len(loads) == 0 {
		return ""
	}
	return loads[len(loads)-1]
}

// openSettingsModelPicker opens the settings form and activates the model field,
// returning the picker's first load cmd.
func openSettingsModelPicker(t *testing.T, m *Model) tea.Cmd {
	t.Helper()
	if !m.SelectSource("settings") {
		t.Fatal("no settings source")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}); cmd != nil {
		t.Fatal("opening the settings form must not dispatch")
	}
	if !m.formOpen() {
		t.Fatal("e did not open the settings form")
	}
	if !m.activeForm().FocusName("default_worker_model") {
		t.Fatal("the settings form has no default_worker_model field")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.modelPicker == nil {
		t.Fatal("enter on a model field must open the modal picker")
	}
	return cmd
}

// Enter on a model field opens the modal picker (it does NOT just advance to the
// next field), it claims the keyboard, and it seeds from the stored ref.
func TestModelFieldOpensTheModalPicker(t *testing.T) {
	m, _ := newPickerModel(t)
	m.settings = &apiv1.TenantSettings{DefaultWorkerModel: "opencode/anthropic/claude-sonnet-4"}

	cmd := openSettingsModelPicker(t, m)
	if !m.ClaimsKeys() {
		t.Fatal("an open picker must claim the keyboard")
	}
	if got := m.modelPicker.Adapter(); got != "opencode" {
		t.Fatalf("seeded adapter = %q, want the stored ref's adapter", got)
	}
	if got := m.modelPicker.Provider(); got != "anthropic" {
		t.Fatalf("seeded provider = %q, want anthropic", got)
	}
	if cmd == nil {
		t.Fatal("opening must dispatch the adapter-kinds load")
	}
	// The picker renders as a modal over the screen.
	if v := ansi.Strip(m.View()); !strings.Contains(v, "ADAPTER") {
		t.Fatalf("the picker is not rendered over the screen:\n%s", v)
	}
}

// The full three-step walk lands the committed adapter/provider/model ref in the
// FORM FIELD — the operator's "the text it displays after the model is selected
// is the adapter/provider/model name".
func TestModelPickerCascadeCommitsIntoTheFormField(t *testing.T) {
	m, loads := newPickerModel(t)
	m.settings = &apiv1.TenantSettings{}

	cmd := openSettingsModelPicker(t, m)
	// Nothing is seeded yet: the stored ref is empty and the adapter kinds have
	// not landed (the picker adopts the preferred kind when they do).
	if got := m.modelPicker.Adapter(); got != "" {
		t.Fatalf("seeded adapter = %q, want nothing until the kinds load", got)
	}

	// Stage 1: the adapter kinds arrive. The preferred kind is listed first and
	// adopted for the fresh selection, and the provider tier is requested.
	cmd = feed(t, m, cmd)
	if got := lastLoad(*loads); got != "kinds" {
		t.Fatalf("last load = %q, want kinds", got)
	}
	if got := m.modelPicker.Adapter(); got != "orchicon" {
		t.Fatalf("a fresh selection must default to the preferred adapter, got %q", got)
	}
	if cmd == nil {
		t.Fatal("the adapter tier must request its providers")
	}

	// Stage 2: providers arrive. Nothing further loads until a provider is
	// CHOSEN — focus alone must not fire an RPC.
	cmd = feed(t, m, cmd)
	if got := lastLoad(*loads); got != "providers:orchicon" {
		t.Fatalf("last load = %q, want the provider tier", got)
	}
	if cmd != nil {
		t.Fatal("nothing should load before the operator chooses a provider")
	}

	// Stage 3: choosing the adapter advances to the provider tier (no refetch).
	if _, next := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); next != nil {
		t.Fatal("choosing an adapter with its providers loaded must not re-fetch")
	}
	// Stage 4: choosing the provider fires the MODEL load for that provider.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("choosing a provider must request its models")
	}
	if got := m.modelPicker.Provider(); got != "anthropic" {
		t.Fatalf("provider = %q, want anthropic", got)
	}
	cmd = feed(t, m, cmd)
	if got := lastLoad(*loads); got != "models:orchicon/anthropic" {
		t.Fatalf("last load = %q, want the model tier", got)
	}
	if cmd != nil {
		t.Fatal("models landing must not trigger a further load")
	}

	// Stage 5: choosing the model commits it into the form field.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.modelPicker != nil {
		t.Fatal("committing must close the picker")
	}
	if got := m.activeForm().Values["default_worker_model"]; got != "orchicon/anthropic/claude-sonnet-4" {
		t.Fatalf("form field = %q, want the committed ref", got)
	}
}

// esc backs out and leaves the field untouched.
func TestModelPickerEscLeavesTheFieldUnchanged(t *testing.T) {
	m, _ := newPickerModel(t)
	m.settings = &apiv1.TenantSettings{DefaultWorkerModel: "opencode/anthropic/claude-sonnet-4"}
	openSettingsModelPicker(t, m)

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelPicker != nil {
		t.Fatal("esc must close the picker")
	}
	if got := m.activeForm().Values["default_worker_model"]; got != "opencode/anthropic/claude-sonnet-4" {
		t.Fatalf("esc must not change the field, got %q", got)
	}
	if !m.formOpen() {
		t.Fatal("esc must leave the form open (it closed only the picker)")
	}
}

// While the picker is up it owns the keyboard: a screen chord must not reach the
// form underneath it.
func TestModelPickerOwnsTheKeyboardAboveTheForm(t *testing.T) {
	m, _ := newPickerModel(t)
	m.settings = &apiv1.TenantSettings{}
	openSettingsModelPicker(t, m)

	before := m.form
	// "e" is the settings-edit chord; if it reached the screen it would REBUILD
	// the form underneath the picker.
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m.form != before {
		t.Fatal("a screen chord reached the form while the picker owned the keyboard")
	}
	if m.modelPicker == nil {
		t.Fatal("the picker must stay open")
	}
	// A click inside the picker is consumed by it, not by the pane behind it.
	_, _ = m.Update(tea.MouseMsg{X: 5, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.form != before {
		t.Fatal("a click reached the form while the picker owned the mouse")
	}
}

// A load landing AFTER the picker closed is ignored (never a panic, never a
// resurrected modal).
func TestModelPickerIgnoresALateLoadAfterClose(t *testing.T) {
	m, _ := newPickerModel(t)
	m.settings = &apiv1.TenantSettings{}
	openSettingsModelPicker(t, m)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelPicker != nil {
		t.Fatal("precondition: the picker is closed")
	}
	// A late adapter-kinds result must be dropped.
	m.Update(modelKindsMsg{Kinds: []string{"orchicon"}, AskCapable: []string{"orchicon"}})
	m.Update(modelProvidersMsg{Adapter: "orchicon", Opts: []kit2.PickerOption{{Value: "anthropic"}}})
	m.Update(modelModelsMsg{Adapter: "orchicon", Provider: "anthropic"})
	if m.modelPicker != nil {
		t.Fatal("a late load must not resurrect the picker")
	}
}

// A load failure is surfaced IN the picker (never a silently blank list).
func TestModelPickerSurfacesALoadFailure(t *testing.T) {
	m, _ := newPickerModel(t)
	m.settings = &apiv1.TenantSettings{}
	openSettingsModelPicker(t, m)

	m.Update(modelKindsMsg{Err: errNoClient("ai gateway")})
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "⚠") {
		t.Fatalf("a failed load must be shown in the picker:\n%s", v)
	}
}
