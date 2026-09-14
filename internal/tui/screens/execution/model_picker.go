// model_picker.go — the Execution screen's worker-model picker wiring.
//
// A worker's model_ref is pinned by a human and lives on the worker's version, so
// it is CHOSEN from the three-tier ModelPicker (adapter → provider → model, with
// search) rather than typed. The write is BulkUpdateWorkerModel, which sets
// model_ref on the worker and republishes the affected version IN PLACE — the
// version number does not advance (it mirrors the manual edit-then-republish
// flow), so this is a single-field edit and not a version fork.
//
// The shared cascade lives in internal/tui/modelpick; this file owns the screen's
// modal state, the write, and the thin load thunks.
package execution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The shared cascade's messages, aliased so the screen routes on one vocabulary.
type (
	modelKindsMsg     = modelpick.KindsMsg
	modelProvidersMsg = modelpick.ProvidersMsg
	modelModelsMsg    = modelpick.ModelsMsg
)

// beginSetModel opens the model picker for a worker, seeded from that worker's
// ACTIVE model_ref — which the workers list already carries
// (WorkerListItem.active_model_ref), so seeding costs no extra round trip.
func (m *Model) beginSetModel(workerID string) tea.Cmd {
	m.workerMu.Lock()
	current := m.workerModel[workerID]
	m.workerMu.Unlock()

	mp := kit2.NewModelPicker("Worker model — " + workerID)
	mp.PreferredAdapter = modelpick.NativeAdapterKind
	mp.SetScreen(m.w, m.h)
	mp.LoadAdapters = m.loadModelKinds
	mp.LoadProviders = m.loadModelProviders
	mp.LoadModels = m.loadModelModels
	m.modelPicker = mp
	m.modelPickerWorker = workerID
	// No Commit/Cancel callbacks: the SCREEN closes the modal in its own Update
	// once the picker reports Done (finishModelPicker) — a callback capturing `m`
	// would mutate a copy bubbletea has already replaced.
	kind, provider, model := modelpick.SplitRef(current)
	return mp.Open(kind, provider, model)
}

// finishModelPicker applies the picker's outcome and closes it. The SCREEN must
// do this, in the same Update that handled the key — see kit2.ModelPicker.Done.
func (m *Model) finishModelPicker(mp *kit2.ModelPicker) tea.Cmd {
	if !mp.Done() {
		return nil
	}
	ref, committed := mp.Ref(), mp.Committed()
	workerID, field := m.modelPickerWorker, m.modelPickerField
	m.modelPicker = nil
	m.modelPickerWorker, m.modelPickerField = "", ""
	if !committed {
		return nil
	}
	// A picker opened from a FORM FIELD writes the ref back into that field — the
	// field is the version's model_ref, and the form's own submit persists it.
	// Writing through rpcSetWorkerModel here as well would be a second, competing
	// write for the same value.
	if field != "" {
		if f := m.Base.DetailForm(); f != nil {
			f.Set(field, ref)
		}
		m.notice = "model chosen — ctrl+s saves the version"
		return nil
	}
	_ = workerID
	return m.setWorkerModel(workerID, ref)
}

// applyModelKinds pushes the adapter kinds into the picker and continues the
// cascade.
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
// so the cascade can be driven deterministically without a live plane.

func (m *Model) loadModelKinds() tea.Cmd {
	fn := m.rpcModelKinds
	return func() tea.Msg {
		if fn == nil {
			return modelKindsMsg{Err: errors.New("no AI gateway client")}
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
			return modelProvidersMsg{Adapter: kind, Err: errors.New("no provider client")}
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
			return modelModelsMsg{Adapter: kind, Provider: provider, Err: errors.New("no model client")}
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

// --- the write --------------------------------------------------------------

// setWorkerModel persists the chosen ref through the ONE mutation executor
// (dock feedback + reconcile of the Workers list).
func (m *Model) setWorkerModel(workerID, ref string) tea.Cmd {
	fn := m.rpcSetWorkerModel
	m.notice = "setting " + workerID + " model …"
	return m.Mutate(mutate.Request{
		Name:   "set worker model",
		Source: srcWorkers,
		Do: func(ctx context.Context) error {
			if fn == nil {
				return errors.New("no worker client")
			}
			return fn(ctx, workerID, ref)
		},
	})
}

// defaultSetWorkerModel is the real write: BulkUpdateWorkerModel with a single
// worker id. It reports a skipped worker (deprecated / retired / no published
// version / not found) or a per-worker error AS A FAILURE — a batch that
// "succeeded" with zero updates would otherwise read as a silent no-op.
func (m *Model) defaultSetWorkerModel(ctx context.Context, workerID, ref string) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	resp, err := m.cl.Workers.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: []string{workerID},
		ModelRef:  ref,
	}))
	if err != nil {
		return err
	}
	for _, res := range resp.Msg.GetResults() {
		if res.GetWorkerId() != workerID {
			continue
		}
		return workerModelOutcomeError(workerID, res)
	}
	return fmt.Errorf("the plane returned no result for worker %s", workerID)
}

// workerModelOutcomeError translates ONE per-worker result into an error (nil
// when the update succeeded). The proto guarantees exactly one outcome is set;
// an entirely empty result is still a failure, never an implicit success.
func workerModelOutcomeError(workerID string, res *apiv1.BulkUpdateWorkerModelResult) error {
	if res.GetUpdated() != nil {
		return nil
	}
	if s := res.GetSkipped(); s != nil {
		return fmt.Errorf("worker %s not updated: %s", workerID, workerModelSkipReason(s.GetReason()))
	}
	if e := res.GetError(); e != nil {
		return errors.New(e.GetMessage())
	}
	return fmt.Errorf("the plane returned no result for worker %s", workerID)
}

// workerModelSkipReason names why BulkUpdateWorkerModel declined a worker, in
// plain language (the enum alone tells an operator nothing).
func workerModelSkipReason(r apiv1.BulkUpdateWorkerModelSkipReason) string {
	switch r {
	case apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NOT_FOUND:
		return "the worker no longer exists"
	case apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_DEPRECATED:
		return "the worker is deprecated (deprecated workers cannot be edited)"
	case apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_RETIRED:
		return "the worker is retired"
	case apiv1.BulkUpdateWorkerModelSkipReason_BULK_UPDATE_WORKER_MODEL_SKIP_REASON_NO_PUBLISHED_VERSION:
		return "the worker has no published version to edit"
	default:
		return "the plane declined the update"
	}
}
