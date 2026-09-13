// model_picker.go — the Control screen's MODEL picker wiring.
//
// Every model field on this screen is a kit2.KModel that opens the three-tier
// ModelPicker (adapter → provider → model, with search): a model_ref is a
// reference no operator can be expected to type. The shared cascade — the
// per-adapter data sources, the grammar handoff and the option projections —
// lives in internal/tui/modelpick; this file owns only the screen's modal state,
// the form-field write-back, and the thin load thunks.
package control

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The shared cascade's messages, aliased so the screen routes on one vocabulary.
type (
	modelKindsMsg     = modelpick.KindsMsg
	modelProvidersMsg = modelpick.ProvidersMsg
	modelModelsMsg    = modelpick.ModelsMsg
)

// openModelPicker installs the modal picker for one form field and seeds it from
// the field's current ref. Committing writes the ref back into that field, so the
// field then DISPLAYS adapter/provider/model — the operator's "the text it
// displays after the model is selected is the adapter/provider/model name".
func (m *Model) openModelPicker(field, current string) tea.Cmd {
	mp := kit2.NewModelPicker("Select model")
	mp.PreferredAdapter = modelpick.NativeAdapterKind
	mp.SetScreen(m.w, m.h)
	mp.LoadAdapters = m.loadModelKinds
	mp.LoadProviders = m.loadModelProviders
	mp.LoadModels = m.loadModelModels
	m.modelPicker = mp
	m.modelField = field
	mp.Commit = func(ref string) {
		if m.form != nil {
			m.form.Set(m.modelField, ref)
		}
		m.Notice("model set to " + ref)
		m.modelPicker = nil
	}
	mp.Cancel = func() { m.modelPicker = nil }
	kind, provider, model := modelpick.SplitRef(current)
	return mp.Open(kind, provider, model)
}

// applyModelKinds pushes the adapter kinds into the picker and lets the cascade
// continue.
func (m *Model) applyModelKinds(msg modelKindsMsg) tea.Cmd {
	if m.modelPicker == nil {
		return nil // a late load must never resurrect a closed picker
	}
	if msg.Err != nil {
		m.modelPicker.SetLoadErr(modelpick.FriendlyErr(msg.Err))
		return nil
	}
	m.modelPicker.SetAdapters(msg.Kinds, msg.AskCapable)
	return m.modelPicker.Sync()
}

// applyModelProviders pushes a provider list into the picker.
func (m *Model) applyModelProviders(msg modelProvidersMsg) tea.Cmd {
	if m.modelPicker == nil {
		return nil
	}
	if msg.Err != nil {
		m.modelPicker.SetLoadErr(modelpick.FriendlyErr(msg.Err))
		return nil
	}
	m.modelPicker.SetProviders(msg.Adapter, msg.Opts)
	return m.modelPicker.Sync()
}

// applyModelModels pushes a model list into the picker.
func (m *Model) applyModelModels(msg modelModelsMsg) tea.Cmd {
	if m.modelPicker == nil {
		return nil
	}
	if msg.Err != nil {
		m.modelPicker.SetLoadErr(modelpick.FriendlyErr(msg.Err))
		return nil
	}
	m.modelPicker.SetModels(msg.Adapter, msg.Provider, msg.Opts, msg.Degraded)
	return nil
}

// --- load commands ----------------------------------------------------------
//
// Each load runs through an rpc* thunk (assigned in New, overridable in a test)
// so the cascade can be driven deterministically without a live plane — the same
// discipline every other write/read on this screen uses.

func (m *Model) loadModelKinds() tea.Cmd {
	fn := m.rpcModelKinds
	return func() tea.Msg {
		if fn == nil {
			return modelKindsMsg{Err: errNoClient("ai gateway")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		kinds, ask, err := fn(ctx)
		return modelKindsMsg{Kinds: kinds, AskCapable: ask, Err: err}
	}
}

func (m *Model) loadModelProviders(kind string) tea.Cmd {
	fn := m.rpcModelProviders
	return func() tea.Msg {
		if fn == nil {
			return modelProvidersMsg{Adapter: kind, Err: errNoClient("provider")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		opts, err := fn(ctx, kind)
		return modelProvidersMsg{Adapter: kind, Opts: opts, Err: err}
	}
}

func (m *Model) loadModelModels(kind, provider string) tea.Cmd {
	fn := m.rpcModelModels
	return func() tea.Msg {
		if fn == nil {
			return modelModelsMsg{Adapter: kind, Provider: provider, Err: errNoClient("model")}
		}
		// CLI discovery shells out, so it gets a longer budget than a read.
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		opts, degraded, err := fn(ctx, kind, provider)
		return modelModelsMsg{Adapter: kind, Provider: provider, Opts: opts, Degraded: degraded, Err: err}
	}
}

// --- default thunks (the shared cascade) ------------------------------------

func (m *Model) defaultModelKinds(ctx context.Context) ([]string, []string, error) {
	return modelpick.FetchKinds(ctx, m.cl)
}

func (m *Model) defaultModelProviders(ctx context.Context, kind string) ([]kit2.PickerOption, error) {
	return modelpick.FetchProviders(ctx, m.cl, kind)
}

func (m *Model) defaultModelModels(ctx context.Context, kind, provider string) ([]kit2.PickerOption, bool, error) {
	return modelpick.FetchModels(ctx, m.cl, kind, provider)
}
