// worker_forms.go — the Execution screen's WORKER CRUD forms and their load
// plumbing.
//
// Item 6 in full: the header lifecycle (create / edit / delete / publish /
// deprecate / set-active) AND the version editor (the prompt + config surface the
// GUI's workers_.$id.tsx exposes). actions.go owns the chords and the guards;
// this file owns the typed input and the round trips that feed it.
//
// Discipline, mirroring actions.go:
//   - no RPC runs from Update; every load and every write is a tea.Cmd, and the
//     write goes through the ONE mutation executor (dock feedback + reconcile);
//   - a form is seeded from the SERVER's current values (never from a guess), so
//     a submit only sends what the operator changed where the proto allows it;
//   - an operation that cannot apply is REFUSED with the reason, never a silent
//     no-op.
package execution

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// workerOps identifies which form an interactive operation needs once the
// worker's current state has been loaded. The loads are async, so the chord
// records the OPERATION and the loaded message opens the matching form.
type workerOp string

const (
	opEditHeader  workerOp = "edit"
	opEditVersion workerOp = "edit-version"
	opPublish     workerOp = "publish"
	opSetActive   workerOp = "set-active"
)

// workerDetailMsg carries a worker plus its version trail — one pair of reads
// serves every operation that needs to know what state the worker is in.
type workerDetailMsg struct {
	op       workerOp
	workerID string
	worker   *apiv1.Worker
	versions []*apiv1.WorkerVersion
	err      error
}

// pendingWorkerOp remembers which operation to open once the load lands, so a
// late result for a worker the operator has since left is dropped.
func (m *Model) beginWorkerOp(workerID string, op workerOp) tea.Cmd {
	get, list := m.rpcGetWorker, m.rpcListWorkerVersions
	if get == nil || list == nil {
		return m.refuse("no worker client")
	}
	m.notice = "loading " + workerID + "…"
	m.workerOp = op
	m.workerOpID = workerID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		w, err := get(ctx, workerID)
		if err != nil {
			return workerDetailMsg{op: op, workerID: workerID, err: err}
		}
		out := workerDetailMsg{op: op, workerID: workerID, worker: w}
		// The version trail is best-effort: the header ops do not need it, and a
		// failure there must not block them.
		if vs, verr := list(ctx, workerID); verr == nil {
			out.versions = vs
		}
		return out
	}
}

// openWorkerOpForm routes a completed load to the form for its operation.
func (m *Model) openWorkerOpForm(msg workerDetailMsg) tea.Cmd {
	// A result for an operation the operator has moved on from is dropped.
	if m.workerOpID != msg.workerID || m.workerOp == "" {
		return nil
	}
	op := m.workerOp
	m.workerOp, m.workerOpID = "", ""
	if msg.err != nil {
		return m.refuse("worker load failed: " + msg.err.Error())
	}
	switch op {
	case opEditHeader:
		m.form = m.editWorkerForm(msg.worker)
		m.notice = ""
	case opEditVersion:
		f, err := m.editWorkerVersionForm(msg.worker, msg.versions)
		if err != nil {
			return m.refuse(err.Error())
		}
		m.form = f
		m.notice = ""
	case opPublish:
		draft := draftVersion(msg.versions)
		if draft == nil {
			return m.refuse("publish applies to a worker with a DRAFT version — " + msg.workerID +
				" has none; edit its version (V) to create one")
		}
		m.form = m.publishWorkerForm(msg.workerID, draft)
		m.notice = ""
	case opSetActive:
		published := publishedVersions(msg.versions)
		if len(published) == 0 {
			return m.refuse("no PUBLISHED version to activate on " + msg.workerID + " — publish one first (p)")
		}
		m.form = m.setActiveVersionForm(msg.workerID, published)
		m.notice = ""
	}
	return nil
}

// --- forms -----------------------------------------------------------------

// createWorkerForm collects the worker's header and its first version's prompt
// fields. CreateWorker takes the first-version snapshot fields in the SAME call,
// so a new worker is immediately usable instead of an empty shell the operator
// has to edit before it can do anything.
//
// The MODEL is deliberately not here: a model_ref is a CHOICE made in the picker
// (m), and defaulting one silently would pin a model the operator never agreed
// to. Set it right after creating.
func (m *Model) createWorkerForm() *kit2.Form {
	f := kit2.NewForm("New worker",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true,
			Placeholder: "release-notes-writer"},
		kit2.FieldSpec{Name: "purpose", Label: "Purpose", Kind: kit2.KText,
			Placeholder: "one line: what this worker is for"},
		kit2.FieldSpec{Name: "description", Label: "Description (markdown)", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "role", Label: "Role (prompt)", Kind: kit2.KTextArea,
			Placeholder: "the worker's identity statement — structured prompt fields compose into system_prompt"},
		kit2.FieldSpec{Name: "skills", Label: "Skills (prompt)", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "behavior", Label: "Behavior (prompt)", Kind: kit2.KTextArea},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.CreateWorkerRequest{
			Name:        strings.TrimSpace(v["name"]),
			Purpose:     strings.TrimSpace(v["purpose"]),
			Description: v["description"],
			Role:        v["role"],
			Skills:      v["skills"],
			Behavior:    v["behavior"],
			// The model is a CHOICE made in the picker (m) after creation, so the
			// created worker starts with no pinned model rather than a guessed one.
		}
		return m.Mutate(mutate.Request{
			Name: "create worker " + req.Name, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcCreateWorker(ctx, req) },
		}), nil
	}
	return f
}

// editWorkerForm edits the worker HEADER. Only name/description/purpose are
// header-editable on a published worker (the proto says so); role_ref is
// deliberately absent — it is a role ID, and offering a free-text box for an ID
// nobody knows is how you get a broken binding. The GUI's role picker is a
// separate piece of work.
func (m *Model) editWorkerForm(w *apiv1.Worker) *kit2.Form {
	f := kit2.NewForm("Edit worker: "+w.GetName(),
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Initial: w.GetName()},
		kit2.FieldSpec{Name: "purpose", Label: "Purpose", Kind: kit2.KText, Initial: w.GetPurpose()},
		kit2.FieldSpec{Name: "description", Label: "Description (markdown)", Kind: kit2.KTextArea, Initial: w.GetDescription()},
	)
	f.Focused = true
	f.Width = 70
	id := w.GetId()
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.UpdateWorkerRequest{
			Id:          id,
			Name:        strings.TrimSpace(v["name"]),
			Purpose:     strings.TrimSpace(v["purpose"]),
			Description: v["description"],
		}
		return m.Mutate(mutate.Request{
			Name: "update worker " + id, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcUpdateWorker(ctx, req) },
		}), nil
	}
	return f
}

// editWorkerVersionForm is the version editor: the prompt fields and the
// per-version config the GUI exposes. It seeds from the current DRAFT when one
// exists (that is the mutable version), else from the newest version so the
// operator edits forward from what is live rather than from a blank form.
//
// Submitting UPDATES the draft when there is one, and otherwise CREATES a new
// draft carrying these values — so "edit the worker's prompt" works whether or
// not a draft already exists, without the operator having to know which.
func (m *Model) editWorkerVersionForm(w *apiv1.Worker, versions []*apiv1.WorkerVersion) (*kit2.Form, error) {
	src := draftVersion(versions)
	create := false
	if src == nil {
		src = newestVersion(versions)
		create = true
	}
	if src == nil {
		return nil, errors.New("this worker has no version to edit — publish or create one first")
	}

	title := "Edit version: " + w.GetName()
	if create {
		// No draft exists, so the submit CREATES one from these values. The title
		// says which so the operator is never surprised by which write fired.
		title = "Edit version (new draft): " + w.GetName()
	}
	f := kit2.NewForm(title,
		kit2.FieldSpec{Name: "role", Label: "Role", Kind: kit2.KTextArea, Initial: src.GetRole()},
		kit2.FieldSpec{Name: "skills", Label: "Skills", Kind: kit2.KTextArea, Initial: src.GetSkills()},
		kit2.FieldSpec{Name: "behavior", Label: "Behavior", Kind: kit2.KTextArea, Initial: src.GetBehavior()},
		kit2.FieldSpec{Name: "agents_md", Label: "AGENTS.md", Kind: kit2.KTextArea, Initial: src.GetAgentsMd()},
		kit2.FieldSpec{Name: "version_note", Label: "Version note", Kind: kit2.KText, Initial: src.GetVersionNote()},
		kit2.FieldSpec{Name: "context_sources", Label: "Context sources (JSON)", Kind: kit2.KJSON, Initial: src.GetContextSources()},
		kit2.FieldSpec{Name: "permissions", Label: "Permissions (JSON)", Kind: kit2.KJSON, Initial: src.GetPermissions()},
		kit2.FieldSpec{Name: "gated_tools", Label: "Gated tools (JSON)", Kind: kit2.KJSON, Initial: src.GetGatedTools()},
		kit2.FieldSpec{Name: "budget_overrides", Label: "Budget overrides (JSON)", Kind: kit2.KJSON, Initial: src.GetBudgetOverrides()},
		kit2.FieldSpec{Name: "concurrency_limit", Label: "Concurrency limit", Kind: kit2.KNumber,
			Initial: strconv.Itoa(int(src.GetConcurrencyLimit()))},
	)
	f.Focused = true
	f.Width = 70
	workerID, versionID := w.GetId(), src.GetId()
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		limit, err := submitInt(v["concurrency_limit"], "concurrency limit")
		if err != nil {
			return nil, err
		}
		if create {
			req := &apiv1.CreateWorkerVersionRequest{WorkerId: workerID}
			setVersionFields(req, v, limit)
			return m.Mutate(mutate.Request{
				Name: "create draft version for " + workerID, Source: srcWorkers,
				Do: func(ctx context.Context) error { return m.rpcCreateWorkerVersionWrite(ctx, req) },
			}), nil
		}
		req := &apiv1.UpdateWorkerVersionRequest{WorkerId: workerID, VersionId: versionID}
		setVersionFieldsU(req, v, limit)
		return m.Mutate(mutate.Request{
			Name: "save draft version " + versionID, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcUpdateWorkerVersion(ctx, req) },
		}), nil
	}
	return f, nil
}

// publishWorkerForm collects the optional publish note. The DRAFT is published
// (that is what PublishWorkerVersion does); the form names which one so the
// operator is never guessing what a publish will ship.
func (m *Model) publishWorkerForm(workerID string, draft *apiv1.WorkerVersion) *kit2.Form {
	f := kit2.NewForm("Publish v"+strconv.Itoa(int(draft.GetVersion()))+" of "+workerID,
		kit2.FieldSpec{Name: "note", Label: "Version note", Kind: kit2.KText,
			Placeholder: "what changed in this version (optional)"},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		req := &apiv1.PublishWorkerVersionRequest{WorkerId: workerID, VersionNote: strings.TrimSpace(v["note"])}
		return m.Mutate(mutate.Request{
			Name: "publish " + workerID, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcPublishWorkerVersion(ctx, req) },
		}), nil
	}
	return f
}

// setActiveVersionForm chooses which PUBLISHED version dispatch uses. It is a
// select rather than a free-text version number: the version is a choice from a
// known set (the same reason the model is a picker, not a text box).
func (m *Model) setActiveVersionForm(workerID string, published []*apiv1.WorkerVersion) *kit2.Form {
	opts := make([]kit2.Option, 0, len(published))
	for _, v := range published {
		label := "v" + strconv.Itoa(int(v.GetVersion()))
		if ref := v.GetModelRef(); ref != "" {
			label += "  " + ref
		}
		opts = append(opts, kit2.Option{Value: strconv.Itoa(int(v.GetVersion())), Label: label})
	}
	f := kit2.NewForm("Set active version: "+workerID,
		kit2.FieldSpec{Name: "version", Label: "Version", Kind: kit2.KSelect, Required: true,
			Initial: opts[0].Value, Options: opts},
	)
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		n, err := strconv.Atoi(strings.TrimSpace(v["version"]))
		if err != nil {
			return nil, errors.New("pick a version")
		}
		return m.Mutate(mutate.Request{
			Name: "set active version on " + workerID, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcSetActiveWorkerVersion(ctx, workerID, int32(n)) },
		}), nil
	}
	return f
}

// --- version helpers -------------------------------------------------------

// draftVersion returns the worker's mutable draft version, or nil.
func draftVersion(vs []*apiv1.WorkerVersion) *apiv1.WorkerVersion {
	for _, v := range vs {
		if v.GetStatus() == apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_DRAFT {
			return v
		}
	}
	return nil
}

// publishedVersions returns the selectable versions, newest first (the list
// arrives that way from the server).
func publishedVersions(vs []*apiv1.WorkerVersion) []*apiv1.WorkerVersion {
	out := make([]*apiv1.WorkerVersion, 0, len(vs))
	for _, v := range vs {
		if v.GetStatus() == apiv1.WorkerVersionStatus_WORKER_VERSION_STATUS_PUBLISHED {
			out = append(out, v)
		}
	}
	return out
}

// newestVersion returns the highest-numbered version regardless of status.
func newestVersion(vs []*apiv1.WorkerVersion) *apiv1.WorkerVersion {
	var best *apiv1.WorkerVersion
	for _, v := range vs {
		if best == nil || v.GetVersion() > best.GetVersion() {
			best = v
		}
	}
	return best
}

// setVersionFields copies the form's values onto a CreateWorkerVersion request.
// Every field is sent (a create has no "unchanged" to preserve).
func setVersionFields(req *apiv1.CreateWorkerVersionRequest, v map[string]string, limit int32) {
	role, skills, behavior, agents := v["role"], v["skills"], v["behavior"], v["agents_md"]
	n, cs, perm := v["version_note"], v["context_sources"], v["permissions"]
	gt, bo := v["gated_tools"], v["budget_overrides"]
	req.Role, req.Skills, req.Behavior, req.AgentsMd = &role, &skills, &behavior, &agents
	req.VersionNote, req.ContextSources, req.Permissions = &n, &cs, &perm
	req.GatedTools, req.BudgetOverrides = &gt, &bo
	req.ConcurrencyLimit = &limit
}

// setVersionFieldsU copies the form's values onto an UpdateWorkerVersion request
// (every mutable field is optional there, so all of them are sent).
func setVersionFieldsU(req *apiv1.UpdateWorkerVersionRequest, v map[string]string, limit int32) {
	role, skills, behavior, agents := v["role"], v["skills"], v["behavior"], v["agents_md"]
	n, cs, perm := v["version_note"], v["context_sources"], v["permissions"]
	gt, bo := v["gated_tools"], v["budget_overrides"]
	req.Role, req.Skills, req.Behavior, req.AgentsMd = &role, &skills, &behavior, &agents
	req.VersionNote, req.ContextSources, req.Permissions = &n, &cs, &perm
	req.GatedTools, req.BudgetOverrides = &gt, &bo
	req.ConcurrencyLimit = &limit
}

// submitInt parses a numeric field, tolerating an empty (cleared) value.
func submitInt(raw, label string) (int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(label + " must be a number")
	}
	return int32(n), nil
}

// workerStatusOf reads the worker's status out of a list row's meta, which
// fetchWorkers builds as "<status> v<version>".
func workerStatusOf(meta string) string {
	if i := strings.IndexByte(meta, ' '); i > 0 {
		return strings.ToLower(meta[:i])
	}
	return strings.ToLower(meta)
}

// rpcCreateWorker / rpcUpdateWorker / rpcDeleteWorker and the version writes.
// Each is a thin RPC thunk so tests can drive the whole flow without a plane
// (the same discipline the rest of the screen's writes follow).

// --- the load thunks' real implementations ---------------------------------

func (m *Model) defaultGetWorker(ctx context.Context, id string) (*apiv1.Worker, error) {
	if m.cl == nil || m.cl.Workers == nil {
		return nil, errors.New("no worker client")
	}
	resp, err := m.cl.Workers.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetWorker(), nil
}

func (m *Model) defaultListWorkerVersions(ctx context.Context, id string) ([]*apiv1.WorkerVersion, error) {
	if m.cl == nil || m.cl.Workers == nil {
		return nil, errors.New("no worker client")
	}
	resp, err := m.cl.Workers.ListWorkerVersions(ctx, connect.NewRequest(&apiv1.ListWorkerVersionsRequest{WorkerId: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetVersions(), nil
}

func (m *Model) defaultCreateWorker(ctx context.Context, req *apiv1.CreateWorkerRequest) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.CreateWorker(ctx, connect.NewRequest(req))
	return err
}

func (m *Model) defaultUpdateWorker(ctx context.Context, req *apiv1.UpdateWorkerRequest) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.UpdateWorker(ctx, connect.NewRequest(req))
	return err
}

func (m *Model) defaultDeleteWorker(ctx context.Context, id string) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.DeleteWorker(ctx, connect.NewRequest(&apiv1.DeleteWorkerRequest{Id: id}))
	return err
}

func (m *Model) defaultPublishWorkerVersion(ctx context.Context, req *apiv1.PublishWorkerVersionRequest) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.PublishWorkerVersion(ctx, connect.NewRequest(req))
	return err
}

func (m *Model) defaultDeprecateWorker(ctx context.Context, id string) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.DeprecateWorker(ctx, connect.NewRequest(&apiv1.DeprecateWorkerRequest{WorkerId: id}))
	return err
}

func (m *Model) defaultSetActiveWorkerVersion(ctx context.Context, workerID string, version int32) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.SetActiveWorkerVersion(ctx, connect.NewRequest(&apiv1.SetActiveWorkerVersionRequest{
		WorkerId: workerID, Version: version,
	}))
	return err
}

func (m *Model) defaultUpdateWorkerVersion(ctx context.Context, req *apiv1.UpdateWorkerVersionRequest) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.UpdateWorkerVersion(ctx, connect.NewRequest(req))
	return err
}

func (m *Model) defaultCreateWorkerVersion(ctx context.Context, req *apiv1.CreateWorkerVersionRequest) error {
	if m.cl == nil || m.cl.Workers == nil {
		return errors.New("no worker client")
	}
	_, err := m.cl.Workers.CreateWorkerVersion(ctx, connect.NewRequest(req))
	return err
}
