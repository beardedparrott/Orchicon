// worker_forms.go — the Execution screen's WORKER CRUD forms and their load plumbing.
package execution

import (
	"context"
	"encoding/json"
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
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
)

// workerOps identifies which form an interactive operation needs once the worker's current state
// has been loaded.
type workerOp string

const (
	// opCreateWorker creates a worker AND its first version in ONE call: CreateWorker takes the
	// first-version snapshot fields.
	opCreateWorker workerOp = "create"
	// opEditHeader edits an EXISTING worker: its version fields are saved with republish, so the save
	// ends published with the version number unchanged.
	opEditHeader workerOp = "edit"
	// opNewVersion creates the worker's NEXT version and publishes it, advancing
	// the number. Nothing is created until the operator saves.
	opNewVersion workerOp = "new-version"
	opPublish    workerOp = "publish"
	opSetActive  workerOp = "set-active"
)

// workerDetailMsg carries a worker plus its version trail AND the tenant's roles — one pair of
// reads serves every operation that needs to know what state the worker.
type workerDetailMsg struct {
	op       workerOp
	workerID string
	worker   *apiv1.Worker
	versions []*apiv1.WorkerVersion
	roles    []*apiv1.Role
	err      error
}

// pendingWorkerOp remembers which operation to open once the load lands, so a late result for a
// worker the operator has since left is dropped.
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
		// Roles are BEST-EFFORT: a tenant whose roles are unreadable still gets a working form (the picker
		// then offers only "none").
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
	// Header TEXT is offered unless the worker is RETIRED — that is the only status the server still
	// refuses (db.UpdateWorker).
	headerEditable := w.GetStatus() != apiv1.WorkerStatus_WORKER_STATUS_RETIRED

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
		title = "Edit worker: " + w.GetName() + " (" + workerStatusLabel(w.GetStatus()) + " — name/purpose/description are frozen)"
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
		// Three writes at most, and they stay SEPARATE operations because they are separate server-side
		// facts:
		reqs := make([]mutate.Request, 0, 2)
		// The text is compared ONLY when the form OFFERED it.
		var name, purpose, desc string
		textChanged := false
		if headerEditable {
			name, purpose, desc = strings.TrimSpace(v["name"]), strings.TrimSpace(v["purpose"]), v["description"]
			textChanged = name != origName || purpose != origPurpose || desc != origDesc
		}
		roleRef := v["role_ref"]
		roleChanged := roleRef != origRole
		if textChanged || roleChanged {
			req := &apiv1.UpdateWorkerRequest{Id: workerID}
			if textChanged {
				req.Name, req.Purpose, req.Description = name, purpose, desc
			}
			if roleChanged {
				rr := roleRef
				req.RoleRef = &rr
			}
			reqs = append(reqs, mutate.Request{
				Name: "update worker " + workerID, Source: srcWorkers,
				Do: func(ctx context.Context) error { return m.rpcUpdateWorker(ctx, req) },
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
func (m *Model) newWorkerVersionForm(w *apiv1.Worker, versions []*apiv1.WorkerVersion, roles []*apiv1.Role) (*kit2.Form, error) {
	// The new version is seeded from the LATEST PUBLISHED version — that is what the server's
	// CreateWorkerVersion copies.
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

// batchWrites collapses a save's write commands into the one tea.Cmd a submit returns.
func batchWrites(cmds []tea.Cmd) tea.Cmd {
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// versionFields is the per-version field set — ONE builder, so the create form, the edit form and
// the new-version form can never offer a different set for the same version.
func (m *Model) versionFields(src *apiv1.WorkerVersion) []kit2.FieldSpec {
	return []kit2.FieldSpec{
		// The model is a VERSION field (: the ref is versioned state), so it is edited with the
		// rest of the version — the GUI keeps it on workers_.$id.tsx for the same reason.
		{Name: "model_ref", Label: "Model", Kind: kit2.KModel, Initial: src.GetModelRef(),
			Placeholder: "— none — (enter to choose a model)"},
		// The structured prompt fields compose into system_prompt server-side; they
		// are the source of truth, so they are what the operator edits.
		{Name: "role", Label: "Role", Kind: kit2.KTextArea, Initial: src.GetRole()},
		{Name: "skills", Label: "Skills (prompt text)", Kind: kit2.KTextArea, Initial: src.GetSkills()},
		// SKILL FILES: paths, not prose — the same path-list idiom as projects' context_files (see the doc
		// comment on screens/work/projects.go's skillFilesLabel for why this asymmetry with the GUI's file.
		{Name: "skill_files", Label: "Skill files (abs paths, one per line)", Kind: kit2.KTextArea,
			Initial: strings.Join(src.GetSkillFiles(), "\n")},
		{Name: "behavior", Label: "Behavior", Kind: kit2.KTextArea, Initial: src.GetBehavior()},
		{Name: "agents_md", Label: "AGENTS.md", Kind: kit2.KTextArea, Initial: src.GetAgentsMd()},
		{Name: "version_note", Label: "Version note", Kind: kit2.KText, Initial: src.GetVersionNote()},
		{Name: "context_sources", Label: "Context sources (JSON)", Kind: kit2.KJSON, Initial: src.GetContextSources()},
		{Name: "permissions", Label: "Permissions (JSON)", Kind: kit2.KJSON, Initial: src.GetPermissions()},
		// MCP DEFINITIONS for THIS VERSION, INLINE — no mcp_servers row is created, so a published version
		// stays immutable and the version is what a dispatch pins to.
		{Name: "mcp_servers", Label: "MCP definitions (JSON)", Kind: kit2.KJSON,
			Initial: inlineJSONFromPermissions(src.GetPermissions())},
		{Name: "gated_tools", Label: "Gated tools (JSON)", Kind: kit2.KJSON, Initial: src.GetGatedTools()},
		{Name: "budget_overrides", Label: "Budget overrides (JSON)", Kind: kit2.KJSON, Initial: src.GetBudgetOverrides()},
		{Name: "concurrency_limit", Label: "Concurrency limit", Kind: kit2.KNumber,
			Initial: strconv.Itoa(int(src.GetConcurrencyLimit()))},
	}
}

// roleOptions builds the plane-role picker: an explicit "none" (the binding is optional — empty
// means no plane access), then every role the tenant returns.
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

// publishWorkerForm collects the optional publish note.
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

// setActiveVersionForm chooses which PUBLISHED version dispatch uses.
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

// newestVersion returns the highest-numbered version regardless of status — the version an edit
// writes to.
func newestVersion(vs []*apiv1.WorkerVersion) *apiv1.WorkerVersion {
	var best *apiv1.WorkerVersion
	for _, v := range vs {
		if best == nil || v.GetVersion() > best.GetVersion() {
			best = v
		}
	}
	return best
}

// versionEdit is the set of version values a form was seeded with, so a submit can tell whether the
// VERSION changed.
type versionEdit struct {
	modelRef, role, skills, skillFiles, behavior, agents, note  string
	contextSources, permissions, mcpServers, gatedTools, budget string
	limit                                                       int32
}

// versionSnapshot records the values a form started from.
func versionSnapshot(v *apiv1.WorkerVersion) versionEdit {
	return versionEdit{
		modelRef:       strings.TrimSpace(v.GetModelRef()),
		role:           v.GetRole(),
		skills:         v.GetSkills(),
		skillFiles:     strings.Join(v.GetSkillFiles(), "\n"),
		behavior:       v.GetBehavior(),
		agents:         v.GetAgentsMd(),
		note:           v.GetVersionNote(),
		contextSources: v.GetContextSources(),
		permissions:    v.GetPermissions(),
		mcpServers:     inlineJSONFromPermissions(v.GetPermissions()),
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
		v["skill_files"] == orig.skillFiles &&
		v["behavior"] == orig.behavior &&
		v["agents_md"] == orig.agents &&
		v["version_note"] == orig.note &&
		v["context_sources"] == orig.contextSources &&
		v["permissions"] == orig.permissions &&
		v["mcp_servers"] == orig.mcpServers &&
		v["gated_tools"] == orig.gatedTools &&
		v["budget_overrides"] == orig.budget &&
		limit == orig.limit
}

// setVersionFieldsOnCreate copies the form's values onto a CreateWorkerRequest's first-version
// snapshot fields.
func setVersionFieldsOnCreate(req *apiv1.CreateWorkerRequest, v map[string]string, limit int32) {
	req.Role, req.Skills, req.Behavior = v["role"], v["skills"], v["behavior"]
	req.AgentsMd, req.VersionNote = v["agents_md"], v["version_note"]
	req.ContextSources, req.Permissions = v["context_sources"], mergedPermissions(v)
	req.GatedTools, req.BudgetOverrides = v["gated_tools"], v["budget_overrides"]
	req.ModelRef = strings.TrimSpace(v["model_ref"])
	sfOnCreate := skillFilesJSON(v["skill_files"])
	req.SkillFiles = &sfOnCreate
	req.ConcurrencyLimit = limit
}

// setVersionFields copies the form's values onto a CreateWorkerVersion request.
// Every field is sent (a create has no "unchanged" to preserve).
func setVersionFields(req *apiv1.CreateWorkerVersionRequest, v map[string]string, limit int32) {
	role, skills, behavior, agents := v["role"], v["skills"], v["behavior"], v["agents_md"]
	// The permissions blob the panel OWNS the mcp_servers key of: merge the MCP
	// definitions field into the permissions field so the two controls agree.
	n, cs, perm := v["version_note"], v["context_sources"], mergedPermissions(v)
	gt, bo, mr := v["gated_tools"], v["budget_overrides"], strings.TrimSpace(v["model_ref"])
	req.Role, req.Skills, req.Behavior, req.AgentsMd = &role, &skills, &behavior, &agents
	req.VersionNote, req.ContextSources, req.Permissions = &n, &cs, &perm
	req.GatedTools, req.BudgetOverrides = &gt, &bo
	req.ModelRef = &mr
	sf := skillFilesJSON(v["skill_files"])
	req.SkillFiles = &sf
	req.ConcurrencyLimit = &limit
}

// setVersionFieldsU copies the form's values onto an UpdateWorkerVersion request
// (every mutable field is optional there, so all of them are sent).
func setVersionFieldsU(req *apiv1.UpdateWorkerVersionRequest, v map[string]string, limit int32) {
	role, skills, behavior, agents := v["role"], v["skills"], v["behavior"], v["agents_md"]
	// The permissions blob the panel OWNS the mcp_servers key of: merge the MCP
	// definitions field into the permissions field so the two controls agree.
	n, cs, perm := v["version_note"], v["context_sources"], mergedPermissions(v)
	gt, bo, mr := v["gated_tools"], v["budget_overrides"], strings.TrimSpace(v["model_ref"])
	req.Role, req.Skills, req.Behavior, req.AgentsMd = &role, &skills, &behavior, &agents
	req.VersionNote, req.ContextSources, req.Permissions = &n, &cs, &perm
	req.GatedTools, req.BudgetOverrides = &gt, &bo
	req.ModelRef = &mr
	sf := skillFilesJSON(v["skill_files"])
	req.SkillFiles = &sf
	req.ConcurrencyLimit = &limit
}

// skillFilesJSON encodes the skill_files field's path list as the JSON array the wire expects.
func skillFilesJSON(text string) string {
	files := mcpforms.ParseSkillPaths(text)
	if files == nil {
		files = []string{}
	}
	b, err := json.Marshal(files)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// mergedPermissions returns the version's permissions JSON with the form's mcp_servers array merged
// in — the panel OWNS that one key.
func mergedPermissions(v map[string]string) string {
	mcp := strings.TrimSpace(v["mcp_servers"])
	// An empty/absent MCP field AND a permissions blob that carries no mcp_servers key.
	if (mcp == "" || mcp == "[]") && !permissionsHasMCPServers(v["permissions"]) {
		return v["permissions"]
	}
	merged, err := mcpforms.MergeIntoPermissions(v["permissions"], v["mcp_servers"])
	if err != nil {
		return v["permissions"]
	}
	return merged
}

// permissionsHasMCPServers reports whether the permissions JSON already carries an mcp_servers key.
func permissionsHasMCPServers(permissions string) bool {
	var p map[string]json.RawMessage
	if err := json.Unmarshal([]byte(permissions), &p); err != nil {
		return false
	}
	_, ok := p["mcp_servers"]
	return ok
}

// inlineJSONFromPermissions renders a version's inline MCP specs as the JSON array the mcp_servers
// field shows.
func inlineJSONFromPermissions(permissions string) string {
	specs := mcpforms.ParseInline(permissions)
	if specs == nil {
		return "[]"
	}
	b, err := json.Marshal(specs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// openFormModelPicker opens the screen's model picker for a KModel field inside an open form.
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

// defaultListRoles loads the tenant's roles for the plane-role picker.
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
