package execution

// worker_save_test.go — WHAT A WORKER SAVE DOES, and what it must NOT do.
//
// The worker form work had one operator requirement above the rest: an edit is
// ONE save that leaves the worker PUBLISHED, and a cancel leaves NOTHING behind.
// worker_crud_test.go pins the plumbing (chords, inline host, key ownership);
// this file pins the behaviour of the three worker forms:
//
//	create       — carries the whole field set the GUI's create page carries;
//	edit    (e)  — header + version on one form, saved with republish;
//	new     (V)  — creates NOTHING until ctrl+s, then creates and publishes.
//
// The load-bearing assertions are the negative ones: opening a form writes
// nothing, and a save never asks the server for a draft. Those are the two halves
// of the state this replaces — the GUI's revert → save → publish chain, whose
// abandoned edits are the 20 stranded drafts sitting on the dev tenant.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// formHasField reports whether a form offers a field by name.
func formHasField(f *kit2.Form, name string) bool {
	if f == nil {
		return false
	}
	for _, s := range f.Specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// roleFixture is the tenant's role list for the picker tests.
func roleFixture() []*apiv1.Role {
	return []*apiv1.Role{
		{Id: "r_eng", Name: "Engineer", Scope: "tenant"},
		{Id: "r_ops", Name: "Operator", Scope: "tenant"},
	}
}

// THE CREATE FORM CARRIES WHAT THE GUI'S CREATE PAGE CARRIES.
//
// Measured before this: the create form offered 7 of the GUI's 16 writable
// fields, missing slug, role_ref, agents_md, permissions, gated_tools,
// budget_overrides, context_sources, concurrency_limit and version_note. The
// operator creates workers in both clients, so a field the GUI writes and the TUI
// cannot is a field that goes missing the moment they use the keyboard.
func TestWorkerCreateFormCarriesTheGuiFieldSet(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)

	var created *apiv1.CreateWorkerRequest
	m.rpcCreateWorker = func(_ context.Context, req *apiv1.CreateWorkerRequest) error {
		created = req
		return nil
	}

	f := m.createWorkerForm(roleFixture())
	for _, name := range []string{
		"name", "slug", "purpose", "description", "role_ref",
		"model_ref", "role", "skills", "behavior", "agents_md", "version_note",
		"context_sources", "permissions", "gated_tools", "budget_overrides", "concurrency_limit",
	} {
		if !formHasField(f, name) {
			t.Errorf("the create form must offer %q — the GUI's create page writes it", name)
		}
	}

	// Every one of them reaches the request. A field that renders but is never
	// read is worse than a missing one: it accepts typing and discards it.
	f.Set("name", "sweeper")
	f.Set("slug", "sweeper")
	f.Set("purpose", "sweep")
	f.Set("description", "does the sweeping")
	f.Set("role_ref", "r_eng")
	f.Set("model_ref", "orchicon/commandcode/deepseek/deepseek-v4-flash")
	f.Set("role", "you sweep")
	f.Set("skills", "sweeping")
	f.Set("behavior", "be brief")
	f.Set("agents_md", "agent rules")
	f.Set("version_note", "first")
	f.Set("context_sources", `["/p/ctx.md"]`)
	f.Set("permissions", `{"mcp":["fs"]}`)
	f.Set("gated_tools", `["fs.write"]`)
	f.Set("budget_overrides", `{"cost_usd":2}`)
	f.Set("concurrency_limit", "3")

	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if created == nil {
		t.Fatal("the submit must create the worker")
	}
	if created.GetSlug() != "sweeper" {
		t.Errorf("slug = %q, want sweeper", created.GetSlug())
	}
	if created.GetRoleRef() != "r_eng" {
		t.Errorf("role_ref = %q, want r_eng", created.GetRoleRef())
	}
	if created.GetAgentsMd() != "agent rules" {
		t.Errorf("agents_md = %q, want the typed value", created.GetAgentsMd())
	}
	if created.GetContextSources() != `["/p/ctx.md"]` {
		t.Errorf("context_sources = %q", created.GetContextSources())
	}
	if created.GetPermissions() != `{"mcp":["fs"]}` {
		t.Errorf("permissions = %q", created.GetPermissions())
	}
	if created.GetGatedTools() != `["fs.write"]` {
		t.Errorf("gated_tools = %q", created.GetGatedTools())
	}
	if created.GetBudgetOverrides() != `{"cost_usd":2}` {
		t.Errorf("budget_overrides = %q", created.GetBudgetOverrides())
	}
	if created.GetConcurrencyLimit() != 3 {
		t.Errorf("concurrency_limit = %d, want 3", created.GetConcurrencyLimit())
	}
	if created.GetVersionNote() != "first" {
		t.Errorf("version_note = %q, want first", created.GetVersionNote())
	}
	if created.GetModelRef() != "orchicon/commandcode/deepseek/deepseek-v4-flash" {
		t.Errorf("model_ref = %q, want the chosen ref", created.GetModelRef())
	}
}

// HEADER TEXT IS OFFERED ONLY WHILE THE WORKER IS A DRAFT.
//
// internal/db/worker.go:370 makes name/description/purpose draft-only
// (`status = 'draft' OR <only the role binding is set>`). A published worker
// therefore cannot be renamed, so the form must not present a text box whose save
// the server refuses — and, crucially, it must not fire that write anyway. The
// first version of this code DID: the absent fields read as empty strings, which
// compared "changed" against the real name, so every save on a published worker
// carried a header write the server would reject.
func TestWorkerEditFormOffersHeaderTextOnlyOnADraft(t *testing.T) {
	versions := []*apiv1.WorkerVersion{pubV("v1", 1, "orchicon/deepseek/deepseek-flash")}

	t.Run("draft worker offers the header text", func(t *testing.T) {
		m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
		w := &apiv1.Worker{Id: "w1", Name: "writer", Purpose: "p", Description: "d",
			Status: apiv1.WorkerStatus_WORKER_STATUS_DRAFT}
		f, err := m.editWorkerForm(w, versions, roleFixture())
		if err != nil {
			t.Fatalf("edit form: %v", err)
		}
		for _, name := range []string{"name", "purpose", "description"} {
			if !formHasField(f, name) {
				t.Errorf("a draft worker must offer %q — the server accepts it there", name)
			}
		}
	})

	t.Run("published worker offers role and version, not header text", func(t *testing.T) {
		m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
		var headerWrites []*apiv1.UpdateWorkerRequest
		m.rpcUpdateWorker = func(_ context.Context, req *apiv1.UpdateWorkerRequest) error {
			headerWrites = append(headerWrites, req)
			return nil
		}
		w := &apiv1.Worker{Id: "w1", Name: "writer", Purpose: "p", Description: "d",
			Status: apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED}
		f, err := m.editWorkerForm(w, versions, roleFixture())
		if err != nil {
			t.Fatalf("edit form: %v", err)
		}
		for _, name := range []string{"name", "purpose", "description"} {
			if formHasField(f, name) {
				t.Errorf("a published worker must NOT offer %q — the server refuses header text there "+
					"(db/worker.go:370), so the field could only ever fail the save", name)
			}
		}
		for _, name := range []string{"role_ref", "model_ref", "role", "behavior", "agents_md"} {
			if !formHasField(f, name) {
				t.Errorf("a published worker must still offer %q", name)
			}
		}

		// Saving with only a VERSION change must not fire a header write at all.
		f.Set("behavior", "be brief")
		cmd, err := f.OnSubmit(f.Values, nil)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		runWrite(t, cmd)
		for _, req := range headerWrites {
			if req.GetName() != "" || req.GetDescription() != "" || req.GetPurpose() != "" {
				t.Errorf("a save whose form showed no header fields wrote one anyway: %+v", req)
			}
		}
	})
}

// THE ROLE BINDING IS SAVED ON ITS OWN — it is the one header field a published
// worker accepts, so it must not be bundled with the draft-only text (which would
// make the server refuse the pair) and it must not be dropped.
func TestWorkerEditSavesTheRoleBindingOnItsOwn(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	var headerWrites []*apiv1.UpdateWorkerRequest
	m.rpcUpdateWorker = func(_ context.Context, req *apiv1.UpdateWorkerRequest) error {
		headerWrites = append(headerWrites, req)
		return nil
	}
	w := &apiv1.Worker{Id: "w1", Name: "writer", RoleRef: "r_eng",
		Status: apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED}
	versions := []*apiv1.WorkerVersion{pubV("v1", 1, "orchicon/deepseek/deepseek-flash")}
	f, err := m.editWorkerForm(w, versions, roleFixture())
	if err != nil {
		t.Fatalf("edit form: %v", err)
	}

	f.Set("role_ref", "r_ops")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if len(headerWrites) != 1 {
		t.Fatalf("header writes = %d, want exactly 1 (the role binding)", len(headerWrites))
	}
	got := headerWrites[0]
	if got.GetRoleRef() != "r_ops" {
		t.Errorf("role_ref = %q, want r_ops", got.GetRoleRef())
	}
	if got.GetName() != "" || got.GetPurpose() != "" || got.GetDescription() != "" {
		t.Errorf("the role write must carry NO header text — the pair is refused on a published worker: %+v", got)
	}
}

// A ROLE THE TENANT NO LONGER RETURNS IS STILL OFFERED.
//
// A select can only submit a value it holds, so dropping an unrecognised current
// binding would silently CLEAR it on the next save — losing a binding the operator
// never touched. Roles get renamed and deleted; the worker keeps pointing at the
// old id until someone changes it deliberately.
func TestWorkerRolePickerKeepsAnUnknownCurrentBinding(t *testing.T) {
	opts := roleOptions(roleFixture(), "r_deleted")
	found := false
	for _, o := range opts {
		if o.Value == "r_deleted" {
			found = true
		}
	}
	if !found {
		t.Fatal("the picker must re-offer a current role the tenant no longer returns, " +
			"otherwise the next save clears a binding nobody changed")
	}
	// The empty binding is always offered: the role is optional, and empty means no
	// plane access.
	if len(opts) == 0 || opts[0].Value != "" {
		t.Fatalf("the picker must lead with an explicit none option, got %+v", opts)
	}
	// A KNOWN binding is not duplicated.
	for _, o := range roleOptions(roleFixture(), "r_eng") {
		if o.Value == "r_eng" && strings.Contains(o.Label, "not in this tenant") {
			t.Error("a role that IS in the list must not be labelled as unknown")
		}
	}
}

// THE NEW-VERSION FORM CREATES NOTHING UNTIL ctrl+s, then creates PUBLISHED.
//
// This is the operator's requirement 3 and 4 in one test: opening the form is
// read-only, so a cancel cannot leave the unpublished draft that an abandoned
// "New version" leaves today (20 of them on the dev tenant); and saving makes the
// new version the LIVE one in a single call.
func TestWorkerNewVersionFormCreatesNothingUntilSave(t *testing.T) {
	w := &apiv1.Worker{Id: "w1", Name: "writer", Status: apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED}
	m, _, _ := crudExec(t, w, []*apiv1.WorkerVersion{pubV("v1", 1, "orchicon/deepseek/deepseek-flash")})

	var created *apiv1.CreateWorkerVersionRequest
	m.rpcCreateWorkerVersionWrite = func(_ context.Context, req *apiv1.CreateWorkerVersionRequest) error {
		created = req
		return nil
	}

	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []kit2.Item{{ID: "w1", Title: "writer"}}, "")
	m.Base.SelectItem(srcWorkers, "w1")

	f := openWorkerForm(t, m, keyEditVersion)
	if f == nil {
		t.Fatal("V must open the new-version form")
	}
	if created != nil {
		t.Fatalf("opening the form must not create anything — a CANCEL has to leave no trace, got %+v", created)
	}
	// The title names the version it will create and where it copies from, so the
	// operator is never guessing which write is about to fire.
	if !strings.Contains(f.Title, "v2") {
		t.Errorf("title = %q, want it to name the version being created", f.Title)
	}

	f.Set("behavior", "be brief")
	cmd, err := f.OnSubmit(f.Values, nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	runWrite(t, cmd)
	if created == nil {
		t.Fatal("the save must create the version")
	}
	if !created.GetPublish() {
		t.Error("the new-version save must set publish — otherwise a failure after the create strands an " +
			"unpublished draft, which is the state this whole change removes")
	}
	if got := created.GetWorkerId(); got != "w1" {
		t.Errorf("worker_id = %q, want w1", got)
	}
	if got := created.GetBehavior(); got != "be brief" {
		t.Errorf("behavior = %q, want the edited value", got)
	}
}

// THE FORM FOLLOWS ITS CURSOR.
//
// A worker form is taller than the detail pane on a laptop-sized terminal
// (createWorkerForm renders 10 rows before this change and ~20 after; the edit
// form 15). The pane used to build a fresh Panel on every frame, and
// Panel.SetContent CLAMPS a scroll that starts at 0 — so the lower fields were not
// merely off-screen, they were UNREACHABLE: the operator was typing into fields
// they could not see. The assertion is the operator-facing one: move the cursor
// and the field you are on is on screen.
func TestWorkerFormScrollsToFollowTheCursor(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	// A SHORT pane, which is the terminal shape the operator actually edits in.
	m.Base.SetSize(120, 16)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	f := openWorkerForm(t, m, keyNewWorker)
	if f == nil {
		t.Fatal("n must open the create form")
	}

	const lastLabel = "Concurrency limit"
	if strings.Contains(m.Base.View(), lastLabel) {
		t.Fatalf("precondition failed: %q is on screen before the cursor reaches it, so this test "+
			"cannot tell scrolling from luck — the fixture's pane is not short enough", lastLabel)
	}

	if !f.FocusName("concurrency_limit") {
		t.Fatal("fixture: the create form must have a concurrency_limit field")
	}
	if !strings.Contains(m.Base.View(), lastLabel) {
		t.Fatalf("the field under the cursor is NOT on screen after moving to it: the pane does not "+
			"scroll to follow the form's cursor, so the operator types blind (form renders %d rows "+
			"against a %d-row pane)", strings.Count(f.View(), "\n")+1, 16)
	}
}
