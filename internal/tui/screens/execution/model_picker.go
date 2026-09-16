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
	"strings"
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

// beginBulkSetModel opens the model picker for a SET of workers — the TUI's counterpart to the GUI's
// BulkChangeWorkerModelDialog.
//
// WHY A LIST AND NOT A SINGLE WORKER. A per-worker model edit is a FORM FIELD (e / V), and lowercase `m`
// was retired for exactly that reason; chaining ten forms is not a feature. So the picker's row-action
// entry point takes the marked selection, and everything downstream handles one-or-many uniformly — the
// RPC is BulkUpdateWorkerModel either way, so a single id is just the smallest batch rather than a
// second code path that can rot.
//
// It is seeded from the selected workers' SHARED model_ref when they all agree, which is the common case
// for a batch (and the one where the operator wants to see what they are changing FROM). When they
// differ there is nothing honest to seed with, so it opens at the preferred adapter instead of showing
// one worker's ref as if it were everyone's.
func (m *Model) beginBulkSetModel(ids []string) tea.Cmd {
	if len(ids) == 0 {
		return nil
	}
	m.workerMu.Lock()
	seed := m.workerModel[ids[0]]
	for _, id := range ids[1:] {
		if m.workerModel[id] != seed {
			seed = ""
			break
		}
	}
	m.workerMu.Unlock()

	title := fmt.Sprintf("Set model — %d workers", len(ids))
	if len(ids) == 1 {
		title = "Set model — " + ids[0]
	}
	mp := kit2.NewModelPicker(title)
	mp.PreferredAdapter = modelpick.NativeAdapterKind
	mp.SetScreen(m.w, m.h)
	mp.LoadAdapters = m.loadModelKinds
	mp.LoadProviders = m.loadModelProviders
	mp.LoadModels = m.loadModelModels
	m.modelPicker = mp
	m.modelPickerWorkers = append([]string(nil), ids...)
	// No Commit/Cancel callbacks: the SCREEN closes the modal in its own Update
	// once the picker reports Done (finishModelPicker) — a callback capturing `m`
	// would mutate a copy bubbletea has already replaced.
	kind, provider, model := modelpick.SplitRef(seed)
	return mp.Open(kind, provider, model)
}

// finishModelPicker applies the picker's outcome and closes it. The SCREEN must
// do this, in the same Update that handled the key — see kit2.ModelPicker.Done.
func (m *Model) finishModelPicker(mp *kit2.ModelPicker) tea.Cmd {
	if !mp.Done() {
		return nil
	}
	ref, committed := mp.Ref(), mp.Committed()
	workerIDs, field := m.modelPickerWorkers, m.modelPickerField
	m.modelPicker = nil
	m.modelPickerWorkers, m.modelPickerField = nil, ""
	if !committed {
		return nil
	}
	// A picker opened from a FORM FIELD writes the ref back into that field — the
	// field is the version's model_ref, and the form's own submit persists it.
	// Writing through the RPC here as well would be a second, competing write for
	// the same value.
	if field != "" {
		if f := m.Base.DetailForm(); f != nil {
			f.Set(field, ref)
		}
		m.notice = "model chosen — ctrl+s saves the version"
		return nil
	}
	if len(workerIDs) == 0 {
		return nil
	}
	return m.setWorkerModelRefs(workerIDs, ref)
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

// setWorkerModelRefs persists the chosen ref for a SET of workers through the ONE mutation executor
// (dock feedback + reconcile of the Workers list).
func (m *Model) setWorkerModelRefs(workerIDs []string, ref string) tea.Cmd {
	fn := m.rpcSetWorkerModel
	if len(workerIDs) == 1 {
		m.notice = "setting " + workerIDs[0] + " model …"
	} else {
		m.notice = fmt.Sprintf("setting %d worker models …", len(workerIDs))
	}
	ids := append([]string(nil), workerIDs...)
	return m.Mutate(mutate.Request{
		Name:   "set worker model",
		Source: srcWorkers,
		Do: func(ctx context.Context) error {
			if fn == nil {
				return errors.New("no worker client")
			}
			return fn(ctx, ids, ref)
		},
	})
}

// defaultSetWorkerModelRefs is the real write: ONE BulkUpdateWorkerModel call for the whole selection.
//
// THE BATCH IS ONE ROUND TRIP, not a loop of single writes — the RPC is built for it, and a loop would
// make a ten-worker change ten chances to half-apply. But a batch can PARTLY SUCCEED (a deprecated
// worker, a worker with no published version), and reporting only the first problem would hide the rest,
// so the outcomes are TALLIED: the operator needs to know "updated 3 of 5" and WHY the two did not take,
// in one message. Reasons are grouped rather than listed per worker, because five identical "no published
// version" lines is noise — and they are already written in plain language (workerModelSkipReason).
func (m *Model) defaultSetWorkerModelRefs(ctx context.Context, workerIDs []string, ref string) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	if len(workerIDs) == 0 {
		return errors.New("no workers selected")
	}
	resp, err := m.cl.Workers.BulkUpdateWorkerModel(ctx, connect.NewRequest(&apiv1.BulkUpdateWorkerModelRequest{
		WorkerIds: workerIDs,
		ModelRef:  ref,
	}))
	if err != nil {
		return err
	}
	updated := 0
	reasons := map[string]int{}
	var whyOrder []string
	addReason := func(why string, n int) {
		if _, ok := reasons[why]; !ok {
			whyOrder = append(whyOrder, why)
		}
		reasons[why] += n
	}
	// The plane reports one result per requested id; an id with NO result at all is itself a failure, or
	// a batch that silently dropped half the selection would read as a success.
	seen := map[string]bool{}
	for _, res := range resp.Msg.GetResults() {
		if id := res.GetWorkerId(); id != "" {
			seen[id] = true
		}
		if res.GetUpdated() != nil {
			updated++
			continue
		}
		why := ""
		if s := res.GetSkipped(); s != nil {
			why = workerModelSkipReason(s.GetReason())
		} else if e := res.GetError(); e != nil {
			why = e.GetMessage()
		}
		if why == "" {
			why = "the plane returned no result"
		}
		addReason(why, 1)
	}
	unreported := 0
	for _, id := range workerIDs {
		if !seen[id] {
			unreported++
		}
	}
	if unreported > 0 {
		addReason("the plane returned no result", unreported)
	}
	if len(whyOrder) == 0 {
		return nil
	}
	parts := make([]string, 0, len(whyOrder))
	failed := 0
	for _, why := range whyOrder {
		parts = append(parts, fmt.Sprintf("%d %s", reasons[why], why))
		failed += reasons[why]
	}
	return fmt.Errorf("updated %d of %d — %s", updated, updated+failed, strings.Join(parts, "; "))
}

// workerModelOutcomeError translates ONE per-worker result into an error (nil
// when the update succeeded). The proto guarantees exactly one outcome is set;
// an entirely empty result is still a failure, never an implicit success.
//
// It serves the single-worker path (the version editor's own model write), while the BULK path tallies
// outcomes itself so one message can report a partially-applied batch.
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
