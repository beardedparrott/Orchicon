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
//     a submit only sends what the operator changed;
//   - an operation that cannot apply is REFUSED with the reason, never a silent
//     no-op;
//   - OPENING a form writes NOTHING. Every load is read-only, so esc is free:
//     "edit then cancel" cannot leave a draft behind, which is the state the
//     three-gesture client flow (revert → save → publish) produced — 20 workers
//     on the dev tenant still carry that debris.
//
// THE SAVE IS ONE OPERATION. Editing an existing worker sends its version fields
// with UpdateWorkerVersion{republish:true}, which the server performs in a single
// transaction (revert if published → update → republish). The worker is published
// before and after and the version number does NOT advance. Creating a version
// sends CreateWorkerVersion{publish:true}, which creates and publishes atomically.
// Neither path can strand a worker in draft, so this client never needs the
// client-side compensate that the GUI's per-gesture flow requires.
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
	"github.com/beardedparrott/orchicon/internal/tui/modelpick"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// workerOps identifies which form an interactive operation needs once the
// worker's current state has been loaded. The loads are async, so the chord
// records the OPERATION and the loaded message opens the matching form.
type workerOp string

const (
	// opCreateWorker creates a worker AND its first version in ONE call:
	// CreateWorker takes the first-version snapshot fields, so a new worker is
	// immediately usable rather than an empty shell the operator must edit before
	// it can do anything.
	opCreateWorker workerOp = "create"
	// opEditHeader edits an EXISTING worker: its version fields are saved with
	// republish, so the save ends published with the version number unchanged.
	// (The name is shared with the workflow lifecycle, where it means the same
	// thing: "open the editable form for this entity".)
	opEditHeader workerOp = "edit"
	// opNewVersion creates the worker's NEXT version and publishes it, advancing
	// the number. Nothing is created until the operator saves.
	opNewVersion workerOp = "new-version"
	opPublish    workerOp = "publish"
	opSetActive  workerOp = "set-active"
)

// workerDetailMsg carries a worker plus its version trail AND the tenant's roles
// — one pair of reads serves every operation that needs to know what state the
// worker is in, and the roles ride along because the plane-role picker is a field
// on all three worker forms.
type workerDetailMsg struct {
	op       workerOp
	workerID string
	worker   *apiv1.Worker
	versions []*apiv1.WorkerVersion
	roles    []*apiv1.Role
	err      error
}

// pendingWorkerOp remembers which operation to open once the load lands, so a
// late result for a worker the operator has since left is dropped.
//
// An EMPTY workerID is the create path: there is no worker to load, but the form
// still needs the role list, so it goes through the same load-and-open shape
// rather than opening synchronously with an empty picker.
func (m *Model) beginWorkerOp(workerID string, op workerOp) tea.Cmd {
	roles := m.rpcListRoles
	needsWorker := workerID != ""
	get, list := m.rpcGetWorker, m.rpcListWorkerVersions
	if needsWorker && (get == nil || list == nil) {
		return m.refuse("no worker client")
	}
	m.workerOp = op
	m.workerOpID = workerID
	if needsWorker {
		m.notice = "loading " + workerID + "…"
	} else {
		m.notice = "loading roles…"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out := workerDetailMsg{op: op, workerID: workerID}
		// Roles are BEST-EFFORT: a tenant whose roles are unreadable still gets a
		// working form (the picker then offers only "none"), and a roles failure
		// must not block an edit that has nothing to do with the role binding.
		if roles != nil {
			if rs, err := roles(ctx); err == nil {
				out.roles = rs
			}
		}
		if !needsWorker {
			return out
		}
		w, err := get(ctx, workerID)
		if err != nil {
			out.err = err
			return out
		}
		out.worker = w
		// The version trail is best-effort too; every form that needs it refuses
		// by name when it is missing.
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
	case opCreateWorker:
		m.Base.BeginDetailEdit("New worker", m.createWorkerForm(msg.roles))
		m.notice = ""
	case opEditHeader:
		f, err := m.editWorkerForm(msg.worker, msg.versions, msg.roles)
		if err != nil {
			return m.refuse(err.Error())
		}
		m.Base.BeginDetailEdit("Edit worker: "+msg.worker.GetName(), f)
		m.notice = ""
	case opNewVersion:
		f, err := m.newWorkerVersionForm(msg.worker, msg.versions, msg.roles)
		if err != nil {
			return m.refuse(err.Error())
		}
		m.Base.BeginDetailEdit("New version: "+msg.worker.GetName(), f)
		m.notice = ""
	case opPublish:
		draft := draftVersion(msg.versions)
		if draft == nil {
			return m.refuse("publish applies to a worker with a DRAFT version — " + msg.workerID +
				" has none; edit the worker (e) and save, which publishes in place")
		}
		m.Base.BeginDetailEdit("Publish", m.publishWorkerForm(msg.workerID, draft))
		m.notice = ""
	case opSetActive:
		published := publishedVersions(msg.versions)
		if len(published) == 0 {
			return m.refuse("no PUBLISHED version to activate on " + msg.workerID + " — publish one first (p)")
		}
		m.Base.BeginDetailEdit("Set active version", m.setActiveVersionForm(msg.workerID, published))
		m.notice = ""
	}
	return nil
}

// --- forms -----------------------------------------------------------------

// createWorkerForm collects the worker's header AND its first version's fields.
// CreateWorker takes the first-version snapshot fields in the SAME call, so a new
// worker is immediately usable instead of an empty shell.
//
// The field set mirrors the GUI's create page, minus the five fields the RPCs
// accept but no UI writes (execution_policy_ref, recovery_workflow_ref, labels,
// system_prompt, adapter): offering a field the GUI cannot show or edit is its
// own parity break — the two clients would disagree about a worker's contents.
func (m *Model) createWorkerForm(roles []*apiv1.Role) *kit2.Form {
	specs := []kit2.FieldSpec{
		{Name: "name", Label: "Name", Kind: kit2.KText, Required: true,
			Placeholder: "release-notes-writer"},
		{Name: "slug", Label: "Slug (optional)", Kind: kit2.KText,
			Placeholder: "derived from the name when empty"},
		{Name: "purpose", Label: "Purpose", Kind: kit2.KText,
			Placeholder: "one line: what this worker is for"},
		{Name: "description", Label: "Description (markdown)", Kind: kit2.KTextArea},
		{Name: "role_ref", Label: "Plane role", Kind: kit2.KSelect,
			Options:     roleOptions(roles, ""),
			Placeholder: "— none — (no plane access)",
		},
	}
	specs = append(specs, m.versionFields(nil)...)
	f := kit2.NewForm("New worker", specs...)
	f.Focused = true
	f.Width = 70
	f.OnOpenModelPicker = m.openFormModelPicker
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		limit, err := submitInt(v["concurrency_limit"], "concurrency limit")
		if err != nil {
			return nil, err
		}
		req := &apiv1.CreateWorkerRequest{
			Name:        strings.TrimSpace(v["name"]),
			Slug:        strings.TrimSpace(v["slug"]),
			Purpose:     strings.TrimSpace(v["purpose"]),
			Description: v["description"],
			RoleRef:     v["role_ref"],
		}
		setVersionFieldsOnCreate(req, v, limit)
		return m.Mutate(mutate.Request{
			Name: "create worker " + req.Name, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcCreateWorker(ctx, req) },
		}), nil
	}
	return f
}

// editWorkerForm edits an EXISTING worker in place: the header fields and the
// version fields on one form, saved as one operation.
//
// WHY ONE FORM. The GUI's editor is one page; the TUI used to split it into "e
// edits the header (4 fields)" and "V edits the version", so pressing e showed 4
// fields where the GUI shows 13 and the operator read it as missing fields. The
// split was an artifact of the two RPCs, not of what the operator is doing.
//
// HEADER TEXT IS DRAFT-ONLY, so it is offered only while the worker actually is a
// draft. internal/db/worker.go:370 is the enforcement point —
//
//	AND (status = 'draft' OR $n = true)   -- $n = "only the role binding is set"
//
// — which means a published worker refuses name/description/purpose but accepts
// the role binding. Showing a text box the server will reject is worse than not
// showing it, so a published (or deprecated, or retired) worker is offered the
// role binding plus every version field, and the title says why.
func (m *Model) editWorkerForm(w *apiv1.Worker, versions []*apiv1.WorkerVersion, roles []*apiv1.Role) (*kit2.Form, error) {
	src := newestVersion(versions)
	if src == nil {
		return nil, errors.New("this worker has no version to edit — it cannot be edited through this form")
	}
	headerEditable := w.GetStatus() == apiv1.WorkerStatus_WORKER_STATUS_DRAFT

	var specs []kit2.FieldSpec
	if headerEditable {
		specs = append(specs,
			kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: w.GetName()},
			kit2.FieldSpec{Name: "purpose", Label: "Purpose", Kind: kit2.KText, Initial: w.GetPurpose()},
			kit2.FieldSpec{Name: "description", Label: "Description (markdown)", Kind: kit2.KTextArea, Initial: w.GetDescription()},
		)
	}
	specs = append(specs, kit2.FieldSpec{
		Name: "role_ref", Label: "Plane role", Kind: kit2.KSelect,
		Initial: w.GetRoleRef(), Options: roleOptions(roles, w.GetRoleRef()),
	})
	specs = append(specs, m.versionFields(src)...)

	title := "Edit worker: " + w.GetName()
	if !headerEditable {
		// State the omission rather than letting the operator hunt for the fields.
		title = "Edit worker: " + w.GetName() + " (" + workerStatusLabel(w.GetStatus()) + " — name/description are draft-only)"
	}
	f := kit2.NewForm(title, specs...)
	f.Focused = true
	f.Width = 70
	f.OnOpenModelPicker = m.openFormModelPicker

	workerID, versionID := w.GetId(), src.GetId()
	origName, origPurpose, origDesc := w.GetName(), w.GetPurpose(), w.GetDescription()
	origRole := w.GetRoleRef()
	orig := versionSnapshot(src)

	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		limit, err := submitInt(v["concurrency_limit"], "concurrency limit")
		if err != nil {
			return nil, err
		}
		// Three writes at most, and they stay SEPARATE operations because they are
		// separate server-side facts:
		//
		//  1. the header TEXT — its own call, and deliberately WITHOUT role_ref.
		//     The server refuses header text in the same request as the role
		//     binding on a published worker (service.go:329-332), so combining them
		//     would fail the whole save for a worker whose role the operator also
		//     just changed;
		//  2. the ROLE BINDING — its own call, because it is the one header field a
		//     published worker accepts (db/worker.go:370), so it must not be
		//     blocked by the draft-only rule that governs (1);
		//  3. the VERSION — one atomic republish (revert → update → publish in a
		//     single server transaction), leaving the worker published with the
		//     version number unchanged.
		//
		// (1) and (2) are not part of (3)'s transaction: the header and the version
		// live behind different RPCs, so there is no combined call to use. Ordering
		// them before (3) means the version save — the one that matters — is not
		// lost to a header failure, and vice versa.
		reqs := make([]mutate.Request, 0, 3)
		// The header text is compared ONLY when the form OFFERED it. On a published
		// worker those fields are not drawn, so their form values are the empty
		// string — and comparing that against the worker's real name would report
		// "changed" on every save and fire a header write the server must refuse
		// (name/description/purpose are draft-only, db/worker.go:370), failing a save
		// whose form never showed a name field.
		if headerEditable {
			name, purpose, desc := strings.TrimSpace(v["name"]), strings.TrimSpace(v["purpose"]), v["description"]
			if name != origName || purpose != origPurpose || desc != origDesc {
				reqs = append(reqs, mutate.Request{
					Name: "update worker " + workerID, Source: srcWorkers,
					Do: func(ctx context.Context) error {
						return m.rpcUpdateWorker(ctx, &apiv1.UpdateWorkerRequest{
							Id: workerID, Name: name, Purpose: purpose, Description: desc,
						})
					},
				})
			}
		}
		if roleRef := v["role_ref"]; roleRef != origRole {
			rr := roleRef
			reqs = append(reqs, mutate.Request{
				Name: "set plane role on " + workerID, Source: srcWorkers,
				Do: func(ctx context.Context) error {
					return m.rpcUpdateWorker(ctx, &apiv1.UpdateWorkerRequest{Id: workerID, RoleRef: &rr})
				},
			})
		}
		if !versionUnchanged(v, limit, orig) {
			req := &apiv1.UpdateWorkerVersionRequest{WorkerId: workerID, VersionId: versionID, Republish: true}
			setVersionFieldsU(req, v, limit)
			reqs = append(reqs, mutate.Request{
				Name: "save v" + strconv.Itoa(int(src.GetVersion())) + " of " + workerID, Source: srcWorkers,
				Do: func(ctx context.Context) error { return m.rpcUpdateWorkerVersion(ctx, req) },
			})
		}
		if len(reqs) == 0 {
			return nil, errors.New("nothing changed")
		}
		cmds := make([]tea.Cmd, 0, len(reqs))
		for _, r := range reqs {
			cmds = append(cmds, m.Mutate(r))
		}
		return batchWrites(cmds), nil
	}
	return f, nil
}

// newWorkerVersionForm creates the worker's NEXT version and publishes it.
//
// NOTHING IS CREATED UNTIL ctrl+s. The version row is created and published in
// the same server call, so esc leaves no trace at all — which is the whole point
// of requirement "new version, then cancel: the latest is still in published
// form". The GUI creates the draft on opening the editor, which is why an
// abandoned "New version" leaves an unpublished draft behind: 20 of them are
// sitting on the dev tenant right now.
func (m *Model) newWorkerVersionForm(w *apiv1.Worker, versions []*apiv1.WorkerVersion, roles []*apiv1.Role) (*kit2.Form, error) {
	// The new version is seeded from the LATEST PUBLISHED version — that is what
	// the server's CreateWorkerVersion copies from, so the form must show what the
	// save will actually start from rather than a draft nobody published.
	src := newestVersion(versions)
	if published := publishedVersions(versions); len(published) > 0 {
		src = published[0]
	}
	if src == nil {
		return nil, errors.New("this worker has no version to base a new one on")
	}
	next := src.GetVersion() + 1

	specs := []kit2.FieldSpec{
		kit2.FieldSpec{Name: "role_ref", Label: "Plane role", Kind: kit2.KSelect,
			Initial: w.GetRoleRef(), Options: roleOptions(roles, w.GetRoleRef())},
	}
	specs = append(specs, m.versionFields(src)...)

	f := kit2.NewForm("New version v"+strconv.Itoa(int(next))+" of "+w.GetName()+" (copied from v"+strconv.Itoa(int(src.GetVersion()))+", saved published)", specs...)
	f.Focused = true
	f.Width = 70
	f.OnOpenModelPicker = m.openFormModelPicker

	workerID := w.GetId()
	origRole := w.GetRoleRef()
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		limit, err := submitInt(v["concurrency_limit"], "concurrency limit")
		if err != nil {
			return nil, err
		}
		reqs := make([]mutate.Request, 0, 2)
		if roleRef := v["role_ref"]; roleRef != origRole {
			rr := roleRef
			reqs = append(reqs, mutate.Request{
				Name: "set plane role on " + workerID, Source: srcWorkers,
				Do: func(ctx context.Context) error {
					return m.rpcUpdateWorker(ctx, &apiv1.UpdateWorkerRequest{Id: workerID, RoleRef: &rr})
				},
			})
		}
		req := &apiv1.CreateWorkerVersionRequest{WorkerId: workerID, Publish: true}
		setVersionFields(req, v, limit)
		reqs = append(reqs, mutate.Request{
			Name: "create v" + strconv.Itoa(int(next)) + " of " + workerID, Source: srcWorkers,
			Do: func(ctx context.Context) error { return m.rpcCreateWorkerVersionWrite(ctx, req) },
		})
		cmds := make([]tea.Cmd, 0, len(reqs))
		for _, r := range reqs {
			cmds = append(cmds, m.Mutate(r))
		}
		return batchWrites(cmds), nil
	}
	return f, nil
}

// batchWrites collapses a save's write commands into the one tea.Cmd a submit
// returns. A SINGLE write is returned directly rather than wrapped: tea.Batch
// yields a BatchMsg that the runtime unpacks, and wrapping one command in an
// envelope makes the command opaque to anything that inspects it — including the
// screen's own tests, which read the write's result. There is nothing to batch, so
// there is nothing to wrap.
func batchWrites(cmds []tea.Cmd) tea.Cmd {
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// versionFields is the per-version field set — ONE builder, so the create form,
// the edit form and the new-version form can never offer a different set for the
// same version. A divergence there is a field the operator can set in one mode
// and not another, which reads as a field that silently lost its value.
//
// src is nil for the create form (there is no version yet).
func (m *Model) versionFields(src *apiv1.WorkerVersion) []kit2.FieldSpec {
	return []kit2.FieldSpec{
		// The model is a VERSION field (ADR-0003: the ref is versioned state), so it
		// is edited with the rest of the version — the GUI keeps it on
		// workers_.$id.tsx for the same reason. A REFERENCE, not text: activating the
		// field opens the screen's model picker, so a ref can never be mistyped.
		{Name: "model_ref", Label: "Model", Kind: kit2.KModel, Initial: src.GetModelRef(),
			Placeholder: "— none — (enter to choose a model)"},
		// The structured prompt fields compose into system_prompt server-side; they
		// are the source of truth, so they are what the operator edits.
		{Name: "role", Label: "Role", Kind: kit2.KTextArea, Initial: src.GetRole()},
		{Name: "skills", Label: "Skills", Kind: kit2.KTextArea, Initial: src.GetSkills()},
		{Name: "behavior", Label: "Behavior", Kind: kit2.KTextArea, Initial: src.GetBehavior()},
		{Name: "agents_md", Label: "AGENTS.md", Kind: kit2.KTextArea, Initial: src.GetAgentsMd()},
		{Name: "version_note", Label: "Version note", Kind: kit2.KText, Initial: src.GetVersionNote()},
		{Name: "context_sources", Label: "Context sources (JSON)", Kind: kit2.KJSON, Initial: src.GetContextSources()},
		{Name: "permissions", Label: "Permissions (JSON)", Kind: kit2.KJSON, Initial: src.GetPermissions()},
		{Name: "gated_tools", Label: "Gated tools (JSON)", Kind: kit2.KJSON, Initial: src.GetGatedTools()},
		{Name: "budget_overrides", Label: "Budget overrides (JSON)", Kind: kit2.KJSON, Initial: src.GetBudgetOverrides()},
		{Name: "concurrency_limit", Label: "Concurrency limit", Kind: kit2.KNumber,
			Initial: strconv.Itoa(int(src.GetConcurrencyLimit()))},
	}
}

// roleOptions builds the plane-role picker: an explicit "none" (the binding is
// optional — empty means no plane access), then every role the tenant returns.
//
// The worker's CURRENT binding is re-offered when the list does not contain it (a
// deleted or unreadable role). Without that, a select could not show the value it
// holds, and the next save would silently CLEAR a binding the operator never
// touched — a select can only submit a value it offers.
func roleOptions(roles []*apiv1.Role, current string) []kit2.Option {
	opts := []kit2.Option{{Value: "", Label: "— none — (no plane access)"}}
	seen := false
	for _, r := range roles {
		label := r.GetName()
		if s := r.GetScope(); s != "" {
			label += " (" + s + ")"
		}
		if r.GetId() == current {
			seen = true
		}
		opts = append(opts, kit2.Option{Value: r.GetId(), Label: label})
	}
	if current != "" && !seen {
		opts = append(opts, kit2.Option{
			Value: current,
			Label: current + " (not in this tenant's role list)",
		})
	}
	return opts
}

// publishWorkerForm collects the optional publish note. The DRAFT is published
// (that is what PublishWorkerVersion does); the form names which one so the
// operator is never guessing what a publish will ship.
//
// This is the path for a worker that ALREADY carries a draft — the state older
// clients left behind. A normal edit no longer needs it: saving an edit publishes
// in place.
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

// newestVersion returns the highest-numbered version regardless of status — the
// version an edit writes to.
//
// The latest, not "the active published version", because that is what the
// platform already treats as the worker's current content: a model change edits
// the latest version (BulkUpdateWorkerModel → GetLatestWorkerVersion(…, false)
// then revert → update → republish), and the GUI's page defaults its editor to
// the draft when one exists. Targeting the latest is also SELF-HEALING: saving it
// re-publishes THAT version, so an abandoned draft left by an older client stops
// being an orphan the moment the operator edits the worker.
func newestVersion(vs []*apiv1.WorkerVersion) *apiv1.WorkerVersion {
	var best *apiv1.WorkerVersion
	for _, v := range vs {
		if best == nil || v.GetVersion() > best.GetVersion() {
			best = v
		}
	}
	return best
}

// versionEdit is the set of version values a form was seeded with, so a submit
// can tell whether the VERSION changed. Without it every save would republish the
// version — a write, an outbox event and an audit row — even when the operator
// only renamed the worker.
type versionEdit struct {
	modelRef, role, skills, behavior, agents, note  string
	contextSources, permissions, gatedTools, budget string
	limit                                           int32
}

// versionSnapshot records the values a form started from.
func versionSnapshot(v *apiv1.WorkerVersion) versionEdit {
	return versionEdit{
		modelRef:       strings.TrimSpace(v.GetModelRef()),
		role:           v.GetRole(),
		skills:         v.GetSkills(),
		behavior:       v.GetBehavior(),
		agents:         v.GetAgentsMd(),
		note:           v.GetVersionNote(),
		contextSources: v.GetContextSources(),
		permissions:    v.GetPermissions(),
		gatedTools:     v.GetGatedTools(),
		budget:         v.GetBudgetOverrides(),
		limit:          v.GetConcurrencyLimit(),
	}
}

// versionUnchanged reports whether every version field still holds its seeded
// value.
func versionUnchanged(v map[string]string, limit int32, orig versionEdit) bool {
	return strings.TrimSpace(v["model_ref"]) == orig.modelRef &&
		v["role"] == orig.role &&
		v["skills"] == orig.skills &&
		v["behavior"] == orig.behavior &&
		v["agents_md"] == orig.agents &&
		v["version_note"] == orig.note &&
		v["context_sources"] == orig.contextSources &&
		v["permissions"] == orig.permissions &&
		v["gated_tools"] == orig.gatedTools &&
		v["budget_overrides"] == orig.budget &&
		limit == orig.limit
}

// setVersionFieldsOnCreate copies the form's values onto a CreateWorkerRequest's
// first-version snapshot fields. UpdateWorkerVersion has its own pair of setters
// below; this one exists because CreateWorker takes the version fields at the
// HEADER level (no version object), so the same form feeds a different message.
func setVersionFieldsOnCreate(req *apiv1.CreateWorkerRequest, v map[string]string, limit int32) {
	req.Role, req.Skills, req.Behavior = v["role"], v["skills"], v["behavior"]
	req.AgentsMd, req.VersionNote = v["agents_md"], v["version_note"]
	req.ContextSources, req.Permissions = v["context_sources"], v["permissions"]
	req.GatedTools, req.BudgetOverrides = v["gated_tools"], v["budget_overrides"]
	req.ModelRef = strings.TrimSpace(v["model_ref"])
	req.ConcurrencyLimit = limit
}

// setVersionFields copies the form's values onto a CreateWorkerVersion request.
// Every field is sent (a create has no "unchanged" to preserve).
func setVersionFields(req *apiv1.CreateWorkerVersionRequest, v map[string]string, limit int32) {
	role, skills, behavior, agents := v["role"], v["skills"], v["behavior"], v["agents_md"]
	n, cs, perm := v["version_note"], v["context_sources"], v["permissions"]
	gt, bo, mr := v["gated_tools"], v["budget_overrides"], strings.TrimSpace(v["model_ref"])
	req.Role, req.Skills, req.Behavior, req.AgentsMd = &role, &skills, &behavior, &agents
	req.VersionNote, req.ContextSources, req.Permissions = &n, &cs, &perm
	req.GatedTools, req.BudgetOverrides = &gt, &bo
	req.ModelRef = &mr
	req.ConcurrencyLimit = &limit
}

// setVersionFieldsU copies the form's values onto an UpdateWorkerVersion request
// (every mutable field is optional there, so all of them are sent).
func setVersionFieldsU(req *apiv1.UpdateWorkerVersionRequest, v map[string]string, limit int32) {
	role, skills, behavior, agents := v["role"], v["skills"], v["behavior"], v["agents_md"]
	n, cs, perm := v["version_note"], v["context_sources"], v["permissions"]
	gt, bo, mr := v["gated_tools"], v["budget_overrides"], strings.TrimSpace(v["model_ref"])
	req.Role, req.Skills, req.Behavior, req.AgentsMd = &role, &skills, &behavior, &agents
	req.VersionNote, req.ContextSources, req.Permissions = &n, &cs, &perm
	req.GatedTools, req.BudgetOverrides = &gt, &bo
	req.ModelRef = &mr
	req.ConcurrencyLimit = &limit
}

// openFormModelPicker opens the screen's model picker for a KModel field inside
// an open form. The chosen ref is written back into that field when the picker
// reports Done (see finishModelPicker).
func (m *Model) openFormModelPicker(field, current string) tea.Cmd {
	m.modelPickerField = field
	mp := kit2.NewModelPicker("Select model")
	mp.PreferredAdapter = modelpick.NativeAdapterKind
	mp.SetScreen(m.w, m.h)
	mp.LoadAdapters = m.loadModelKinds
	mp.LoadProviders = m.loadModelProviders
	mp.LoadModels = m.loadModelModels
	m.modelPicker = mp
	kind, provider, model := modelpick.SplitRef(current)
	return mp.Open(kind, provider, model)
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

// defaultListRoles loads the tenant's roles for the plane-role picker. Paged to
// the same generous bound the other pickers use: the picker is a selection over
// what the tenant has, and a partial list would offer a subset as if it were all
// of them.
func (m *Model) defaultListRoles(ctx context.Context) ([]*apiv1.Role, error) {
	if m.cl == nil || m.cl.Auth == nil {
		return nil, errors.New("no auth client")
	}
	resp, err := m.cl.Auth.ListRoles(ctx, connect.NewRequest(&apiv1.ListRolesRequest{PageSize: 500}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRoles(), nil
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
