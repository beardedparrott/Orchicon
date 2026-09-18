package work

// project_form_guidance_test.go — THE RUNTIME-IMAGE PICKER AND THE CONTEXT-FILES GUIDANCE.
//
// Two operator reports:
//   "when creating a project, the runtime container [should] be able to be selected from a list.
//    We do something similar in other areas for workers, workflows, etc."
//   "how do we handle project context files on the project create/edit form? There isn't really
//    any guidance here."

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

func imageOpts() []kit2.Option {
	return []kit2.Option{
		{Value: "rt:1", Label: "runtime (rt:1)"},
		{Value: "gui:1", Label: "gui (gui:1)"},
	}
}

// THE FIELD IS A PICKER WHEN THE LIST LOADED, so the operator chooses an image rather than
// remembering a tag — the same control the work-item form gives.
func TestRuntimeImageIsAPickerWhenTheListLoaded(t *testing.T) {
	f := runtimeImageField("", imageOpts())
	if f.Kind != kit2.KPicker {
		t.Fatalf("the runtime image field is %q, want a picker — the operator asked to select it "+
			"from a list, as workers and workflows already are", f.Kind)
	}
	// EVERY IMAGE IS OFFERED, plus an explicit "inherit" — because an empty default_runtime_image
	// is a real choice (fall back to the tenant default), not an unfilled field.
	values := map[string]string{}
	for _, o := range f.Options {
		values[o.Value] = o.Label
	}
	for _, want := range []string{"rt:1", "gui:1", ""} {
		if _, ok := values[want]; !ok {
			t.Errorf("the picker does not offer %q; options: %v", want, f.Options)
		}
	}
	if lbl := values[""]; !strings.Contains(strings.ToLower(lbl), "inherit") {
		t.Errorf("the empty option reads %q — it must say it means \"inherit the tenant default\", "+
			"or choosing nothing looks like an unimplemented field", lbl)
	}
	// The labels name the image, not just its tag: a tag alone is not a choice a human can make.
	if !strings.Contains(values["rt:1"], "runtime") {
		t.Errorf("the option label is %q, want the image's name", values["rt:1"])
	}
}

// A PROJECT'S CURRENT IMAGE IS ALWAYS AN OPTION, even when the fetched list does not contain it —
// an image that has since been deleted. Without this, opening the form would show no selection and
// SAVING WOULD CLEAR a setting the operator never touched.
func TestTheCurrentImageSurvivesAMissingList(t *testing.T) {
	f := runtimeImageField("gone:9", imageOpts())
	var found bool
	for _, o := range f.Options {
		if o.Value == "gone:9" {
			found = true
			if !strings.Contains(o.Label, "current") {
				t.Errorf("the synthetic option for a missing image reads %q — it should say it is the "+
					"current value rather than a live choice", o.Label)
			}
		}
	}
	if !found {
		t.Fatalf("the project's current image is not among the options (%v) — saving an untouched edit "+
			"would silently clear it", f.Options)
	}
	if f.Initial != "gone:9" {
		t.Errorf("the field's initial value is %q, want the project's current image", f.Initial)
	}
}

// AND IT FALLS BACK TO FREE TEXT WHEN THE LIST DID NOT LOAD, rather than vanishing: the operator
// can always type a tag, and a failed ListRuntimeImages must not cost them the ability to set one.
func TestRuntimeImageStaysUsableWithoutAList(t *testing.T) {
	f := runtimeImageField("rt:1", nil)
	if f.Kind != kit2.KText {
		t.Errorf("with no options the field is %q, want plain text — a picker with nothing in it "+
			"cannot set anything", f.Kind)
	}
	if f.Initial != "rt:1" {
		t.Errorf("the fallback field lost the current value: %q", f.Initial)
	}
	if f.Placeholder == "" {
		t.Error("the fallback field has no placeholder, so nothing says that empty means inherit")
	}
}

// THE CONTEXT-FILES FIELD CARRIES ITS GUIDANCE SOMEWHERE THE OPERATOR WILL SEE IT.
//
// Asserted over label AND placeholder TOGETHER, deliberately. The earlier version of this test pinned the
// phrasing ("abs paths inside the project dir" on the label, "one path per line · a directory is read in
// full" in the placeholder) and broke the moment the wording was shortened to fit the pane — which is a
// test asserting PROSE rather than behaviour, and prose is exactly what has to change when a label
// overflows (the operator's next report was that this label ran off the screen).
//
// What must hold is that the field tells the operator the things they cannot infer from its name, and how
// to enter the values.
func TestContextFilesFieldCarriesItsGuidance(t *testing.T) {
	f := ProjectFormFields(nil, nil)
	var got *kit2.FieldSpec
	for i := range f {
		if f[i].Name == "context_files" {
			got = &f[i]
		}
	}
	if got == nil {
		t.Fatal("no context_files field")
	}
	// The label is the part ALWAYS drawn — the placeholder is replaced by the caret the moment the field
	// takes the cursor, which is exactly when it is being read — so the load-bearing rule belongs there.
	label := strings.ToLower(got.Label)
	if !strings.Contains(label, "project dir") {
		t.Errorf("the context-files label %q does not say the paths belong to the project directory. That is the "+
			"expensive mistake this guidance exists to prevent: a path outside it is recorded happily and is then "+
			"invisible to the worker, which is the worst of both worlds.", got.Label)
	}
	// The rest may live in either half — what matters is that the field says it at all.
	combined := strings.ToLower(got.Label + " " + got.Placeholder)
	if !strings.Contains(combined, "dir") {
		t.Errorf("neither the label nor the placeholder says a DIRECTORY is allowed (%q / %q). The field's name "+
			"reads like it wants files only, and a directory (read in full) is the more useful choice.",
			got.Label, got.Placeholder)
	}
	if !strings.Contains(combined, "per line") && !strings.Contains(combined, "one path") {
		t.Errorf("the field does not say the input shape (one path per line): %q / %q", got.Label, got.Placeholder)
	}
	// And it is still a multi-line field, or "one per line" is a lie.
	if got.Kind != kit2.KTextArea {
		t.Errorf("the context-files field is %q, want a textarea for one-path-per-line input", got.Kind)
	}
}

// THE FORM AS A WHOLE OFFERS THE PICKER, not just the field builder in isolation — the wiring is
// what the operator sees.
func TestTheProjectFormsUseTheImagePicker(t *testing.T) {
	m := newModel(t, newPlane())
	p := &apiv1.Project{Id: "proj-1", Name: "Thing", DefaultRuntimeImage: "rt:1"}

	create := m.newProjectCreateFormWith(projectFormData{images: imageOpts()})
	if spec := create.Spec("default_runtime_image"); spec == nil || spec.Kind != kit2.KPicker {
		t.Errorf("the CREATE form's runtime image field is %v, want a picker fed by the image list",
			spec != nil && spec.Kind == kit2.KPicker)
	}
	edit := m.newProjectEditFormWith(p, projectFormData{images: imageOpts()})
	spec := edit.Spec("default_runtime_image")
	if spec == nil || spec.Kind != kit2.KPicker {
		t.Fatal("the EDIT form's runtime image field is not a picker")
	}
	if spec.Initial != "rt:1" {
		t.Errorf("the edit form's image field prefilled %q, want the project's image", spec.Initial)
	}
	// Without the list, both fall back rather than losing the field.
	plain := m.newProjectCreateFormWith(projectFormData{})
	if s := plain.Spec("default_runtime_image"); s == nil || s.Kind != kit2.KText {
		t.Errorf("with no images the create form's image field is not a text fallback")
	}
}
