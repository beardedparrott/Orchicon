package execution

// worker_model_surface_test.go — the worker's MODEL is visible in the detail pane
// and editable in both worker forms, at the width an operator actually uses.
//
// The operator:
//
//	"Workers in the TUI don't show the model in the detail view mode, nore the
//	 edit mode. You should be able to view the model in the detail pane when
//	 selecting a worker and edit them in edit more and edit version mode. It
//	 should have the same model picker as everything else in the TUI."
//
// Measured before this: the edit form and the version editor DID carry the model
// field wired to the shared picker, and the detail pane DID print the ref — but
// only inside the VERSIONS block, after the RAW ENUM status, so the line read
// "v1  worker_version_status_published  <ref>" and the ref was pushed past the
// pane edge. At a 100-column terminal it truncated to "claude/a". The fact was in
// the model and not on the screen, which is the same thing as absent.
//
// So these tests assert on the PAINTED frame and on the emitted FIELD, not on the
// data's presence in the object graph.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

const testModelRef = "claude/anthropic/claude-sonnet-5"

// modelWorkerPlane serves a PUBLISHED worker with one published version carrying a
// model — the shape the operator's own workers have.
func modelWorkerPlane(t *testing.T) *Model {
	t.Helper()
	w := &apiv1.Worker{
		Id: "w1", Name: "Quick Software Engineer", Slug: "quick-software-engineer",
		Status: apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED, CurrentVersion: 1,
		Purpose:     "Implements and ships the work item end to end.",
		Description: "An all-in-one software engineer for the Quick Work path.",
	}
	vs := []*apiv1.WorkerVersion{pubV("v1", 1, testModelRef)}
	m := probeWorkerModel(t, w, vs)
	if !m.Base.SelectSource("workers") {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems("workers", []kit2.Item{
		{ID: "w1", Title: w.GetName(), Meta: "published v1"},
	}, "")
	m.Base.SelectItem("workers", "w1")
	return m
}

// TestWorkerDetailShowsTheModel: the detail pane carries a `model` FIELD, and the
// ref survives the pane's width.
func TestWorkerDetailShowsTheModel(t *testing.T) {
	m := modelWorkerPlane(t)
	// The operator's own geometry — a wide pane and the narrow one the ref used to
	// die at.
	for _, sz := range [][2]int{{190, 48}, {120, 40}, {100, 40}} {
		m.SetSize(sz[0], sz[1])
		_, fields, body, err := m.detail(context.Background(), "workers", "w1")
		if err != nil {
			t.Fatalf("detail: %v", err)
		}
		var got string
		for _, f := range fields {
			if f.Key == "model" {
				got = f.Value
			}
		}
		if got != testModelRef {
			t.Fatalf("at %dx%d the detail's `model` field = %q, want the active version's ref",
				sz[0], sz[1], got)
		}
		// The trail must not print the raw enum either — it was 29 characters of wire
		// name between the operator and the model.
		if strings.Contains(body, "worker_version_status_") {
			t.Fatalf("at %dx%d the version trail still prints the raw enum:\n%s", sz[0], sz[1], body)
		}
		if !strings.Contains(body, "published") {
			t.Fatalf("at %dx%d the trail must still say the version's state:\n%s", sz[0], sz[1], body)
		}
	}
}

// TestWorkerDetailModelSurvivesThePaintedPane: the FIELD being right is not enough —
// what the operator reads is the painted frame.
func TestWorkerDetailModelSurvivesThePaintedPane(t *testing.T) {
	m := modelWorkerPlane(t)
	m.SetSize(100, 40)
	// THROUGH THE REAL PAINTED PATH: RequestDetail returns the command the base runs,
	// and running it lands the detailMsg the pane draws from. Calling detail() alone
	// would assert on a value nothing had painted.
	run(t, m, m.Base.RequestDetail("workers", "w1"))
	view := m.Base.View()
	if !strings.Contains(view, testModelRef) {
		t.Fatalf("the painted detail pane must show the WHOLE model ref (%q); the pane was the "+
			"reason it was invisible before:\n%s", testModelRef, view)
	}
}

// TestWorkerEditAndVersionFormsOpenTheModelPicker: the model is a form FIELD wired
// to the SHARED three-tier picker in every worker form mode that edits a version —
// the operator's "It should have the same model picker as everything else in the
// TUI."
//
// Each mode gets its OWN screen: the inline editor is host state, and closing one
// form to open another is a second, unrelated code path (the base's own esc
// handling) that would only add a way for this test to fail for the wrong reason.
func TestWorkerEditAndVersionFormsOpenTheModelPicker(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"e (edit)", keyEditWorker},
		{"V (new version)", keyEditVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := modelWorkerPlane(t)
			cmd, ok := m.handleActionKey(tc.key)
			if !ok {
				t.Fatalf("`%s` must be handled on the Workers pane", tc.key)
			}
			run(t, m, cmd)
			f := m.Base.DetailForm()
			if f == nil {
				t.Fatalf("%s did not open a form", tc.name)
			}
			var field *kit2.FieldSpec
			for i := range f.Specs {
				if f.Specs[i].Name == "model_ref" {
					field = &f.Specs[i]
				}
			}
			if field == nil {
				t.Fatalf("%s offers no model field", tc.name)
			}
			if field.Kind != kit2.KModel {
				t.Fatalf("%s's model field is %q, want the model picker kind", tc.name, field.Kind)
			}
			if field.Initial != testModelRef {
				t.Fatalf("%s seeded model_ref = %q, want the version's ref (%q) — a blank seed "+
					"would UNBIND the model on save", tc.name, field.Initial, testModelRef)
			}
			if f.OnOpenModelPicker == nil {
				t.Fatalf("%s's model field is not wired to the picker", tc.name)
			}
			// Activating the field must open the shared picker; committing must write
			// the chosen ref BACK INTO THE FIELD (the form's own submit persists it — a
			// write through the RPC here would be a second, competing one).
			for i := range f.Specs {
				if f.Specs[i].Name == "model_ref" {
					f.Cursor = i
				}
			}
			if _, consumed := f.HandleKey(kmsg("enter")); !consumed {
				t.Fatalf("%s: enter on the model field must be consumed", tc.name)
			}
			if m.modelPicker == nil {
				t.Fatalf("%s: enter on the model field must open the model picker", tc.name)
			}
			// The picker opened ON the model tier (the seeded ref puts it there), so the
			// cascade's lists must agree with that ref's own adapter/provider — or the
			// focus would be dragged back up a tier and `enter` would select a tier
			// rather than commit.
			m.modelPicker.SetAdapters([]string{"claude"}, nil)
			m.modelPicker.SetProviders("claude", []kit2.PickerOption{{Value: "anthropic"}})
			m.modelPicker.SetModels("claude", "anthropic", []kit2.PickerOption{
				{Value: "claude-sonnet-6"},
			}, false)
			m.modelPicker.HandleKey(kmsg("enter"))
			if !m.modelPicker.Done() {
				t.Fatalf("%s: enter on the model tier must commit", tc.name)
			}
			m.finishModelPicker(m.modelPicker)
			if got := m.Base.DetailForm().Values["model_ref"]; got != "claude/anthropic/claude-sonnet-6" {
				t.Fatalf("%s: the chosen ref must land in the field, got %q", tc.name, got)
			}
		})
	}
}

// TestWorkerModelRefPrefersTheACTIVEVersion: a worker's versions are chosen
// explicitly, so the NEWEST published version is not necessarily the one dispatch
// uses. Reporting the newest would be a confident wrong answer.
func TestWorkerModelRefPrefersTheACTIVEVersion(t *testing.T) {
	vs := []*apiv1.WorkerVersion{
		pubV("v2", 2, "orchicon/commandcode/brand-new"),
		pubV("v1", 1, "claude/anthropic/older"),
	}
	// The worker is PINNED to v1 even though v2 exists.
	w := &apiv1.Worker{Id: "w1", CurrentVersion: 1}
	if got := workerModelRef(w, vs); got != "claude/anthropic/older" {
		t.Fatalf("workerModelRef = %q, want the ACTIVE version's ref (v1)", got)
	}
	// Pinned to the newest: same answer either way.
	w.CurrentVersion = 2
	if got := workerModelRef(w, vs); got != "orchicon/commandcode/brand-new" {
		t.Fatalf("workerModelRef = %q, want v2's ref", got)
	}
	// No current version reported: fall back to the newest, never to nothing.
	w.CurrentVersion = 0
	if got := workerModelRef(w, vs); got != "orchicon/commandcode/brand-new" {
		t.Fatalf("workerModelRef = %q, want the newest version's ref as the fallback", got)
	}
	// A worker with no version at all reports nothing — the pane omits the field
	// rather than drawing a blank.
	if got := workerModelRef(&apiv1.Worker{}, nil); got != "" {
		t.Fatalf("a versionless worker must report no model, got %q", got)
	}
}
