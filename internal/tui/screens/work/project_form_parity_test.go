package work

// project_form_parity_test.go — THE PROJECT FORMS CARRY WHAT THE GUI CARRIES.
//
// The operator: "the project edit form is VERY light. It does not include the same
// options the GUI asks you to create... I would say all of that should be on there.
// The TUI should be almost identical in features."
//
// So the fields are asserted, and so is the WIRE: a field that renders but is never
// sent is worse than a missing one, because it looks like it worked.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// THE GOALS FIELD ROUND-TRIPS INSTEAD OF DESTROYING THEM.
//
// This is the bug the parity work uncovered, and it was silent: goalsText returned the
// RAW JSON, so an edit form prefilled `{"k":"v"}`. ParseGoals then splits on "," and
// "=", finds neither in a JSON object, and turns the whole document into goals whose
// KEY is the JSON text — so saving an UNTOUCHED edit replaced the project's goals with
// garbage. The form looked like it was showing the goals.
func TestGoalsTextRoundTripsThroughParseGoals(t *testing.T) {
	const doc = `{"ship it":"by friday","owner":"ops"}`
	text := goalsText(doc)
	if strings.Contains(text, "{") || strings.Contains(text, "}") || strings.Contains(text, `"`) {
		t.Fatalf("goalsText(%s) = %q — it must render the document as editable key=value text, not JSON: the "+
			"form's parser splits on ',' and '=', so raw JSON becomes goals whose KEY is the JSON itself", doc, text)
	}
	// And the round trip must PRESERVE the data, which is the property that was broken.
	got := map[string]string{}
	for _, g := range ParseGoals(text) {
		got[g.GetKey()] = g.GetValue()
	}
	want := map[string]string{"ship it": "by friday", "owner": "ops"}
	if len(got) != len(want) {
		t.Fatalf("round-tripped goals = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("round-tripped %q = %q, want %q", k, got[k], v)
		}
	}
}

// THE LEGACY git_strategy MARKER IS NOT SHOWN AS A GOAL — it has a field of its own,
// and rendering it would both show an internal marker and offer it for editing twice.
func TestGoalsTextOmitsTheLegacyGitStrategyGoal(t *testing.T) {
	text := goalsText(`{"__git_strategy":"pr","owner":"ops"}`)
	if strings.Contains(text, "__git_strategy") {
		t.Errorf("goalsText exposed the internal git-strategy marker: %q", text)
	}
	if !strings.Contains(text, "owner=ops") {
		t.Errorf("goalsText dropped a real goal while filtering the marker: %q", text)
	}
}

// A MALFORMED DOCUMENT RENDERS EMPTY RATHER THAN FAILING THE FORM OPEN, and an empty
// document is empty.
func TestGoalsTextToleratesJunk(t *testing.T) {
	for _, in := range []string{"", "   ", "not json", "[1,2,3]", "null"} {
		if got := goalsText(in); got != "" {
			t.Errorf("goalsText(%q) = %q, want empty", in, got)
		}
	}
}

// THE GIT STRATEGY FALLS BACK TO THE LEGACY GOAL. Projects created before the typed
// field existed carry their strategy in the goals document, and the GUI reads it that
// way too. WITHOUT THE FALLBACK, opening an older project would show the DEFAULT and
// saving would silently change a `pr` project to `local` — a setting the operator never
// touched, changed behind their back.
func TestGitStrategyFallsBackToTheLegacyGoal(t *testing.T) {
	// Typed field wins when set.
	typed := &apiv1.Project{GitStrategy: apiv1.GitStrategy_GIT_STRATEGY_NONE, Goals: `{"__git_strategy":"pr"}`}
	if got := gitStrategyField(typed); got != "none" {
		t.Errorf("gitStrategyField with a typed value = %q, want none (the typed field is authoritative)", got)
	}
	// Unset typed field: read the legacy goal.
	legacy := &apiv1.Project{GitStrategy: apiv1.GitStrategy_GIT_STRATEGY_UNSPECIFIED, Goals: `{"__git_strategy":"pr"}`}
	if got := gitStrategyField(legacy); got != "pr" {
		t.Errorf("gitStrategyField with only the legacy goal = %q, want pr — without the fallback, opening this "+
			"project would show the default and saving would overwrite its strategy", got)
	}
	// Neither: the create default.
	if got := gitStrategyField(&apiv1.Project{}); got != "local" {
		t.Errorf("gitStrategyField with nothing set = %q, want local", got)
	}
	if got := gitStrategyField(nil); got != "local" {
		t.Errorf("gitStrategyField(nil) = %q, want local (the create default)", got)
	}
}

// CONTEXT FILES ROUND-TRIP, and a comma-separated paste is accepted as separate paths
// rather than one absurd one.
func TestContextFilesRoundTrip(t *testing.T) {
	files := []string{"/home/me/projects/x/.env", "/home/me/notes"}
	text := contextFilesText(files)
	if got := ParseContextFiles(text); len(got) != 2 || got[0] != files[0] || got[1] != files[1] {
		t.Fatalf("context files round trip = %v, want %v", got, files)
	}
	// A paste of "a, b" — the natural mistake — is two paths.
	if got := ParseContextFiles("/a, /b"); len(got) != 2 {
		t.Errorf("ParseContextFiles(\"/a, /b\") = %v, want two paths", got)
	}
	// Blanks are dropped, so a trailing newline is harmless.
	if got := ParseContextFiles("/a\n\n  \n/b\n"); len(got) != 2 {
		t.Errorf("ParseContextFiles with blanks = %v, want two paths", got)
	}
	if got := ParseContextFiles("   "); len(got) != 0 {
		t.Errorf("ParseContextFiles of whitespace = %v, want none", got)
	}
}

// THE CONCURRENCY FIELD DEGRADES TO "NO RESTRICTION" rather than failing the save or
// sending a negative the server would reject.
func TestParseMaxConcurrentRuns(t *testing.T) {
	cases := map[string]int32{
		"":     0,
		"0":    0,
		"4":    4,
		" 2 ":  2,
		"abc":  0,
		"-3":   0, // a negative limit is not a limit; never sent
		"1.5":  0,
		"9999": 9999,
	}
	for in, want := range cases {
		if got := parseMaxConcurrentRuns(in); got != want {
			t.Errorf("parseMaxConcurrentRuns(%q) = %d, want %d", in, got, want)
		}
	}
}

// THE POST-CREATE UPDATE IS SKIPPED WHEN IT HAS NOTHING TO SAY, so an ordinary create
// does not depend on a second call succeeding.
func TestApplyProjectPostCreateSkipsAnEmptyUpdate(t *testing.T) {
	p := newPlane()
	// The project must EXIST: the fake's UpdateProject returns NotFound for an unknown id
	// (it mirrors the server), so an unseeded fixture would pass the empty case for the
	// wrong reason — the call was skipped rather than the project being absent.
	p.seedProject("proj-1", "Thing")
	if err := applyProjectPostCreate(context.Background(), p, "proj-1", 0, nil); err != nil {
		t.Fatalf("applyProjectPostCreate with nothing to send: %v", err)
	}
	if len(p.projUpdated) != 0 {
		t.Errorf("a create with no override and no context files issued %d update(s); an unnecessary second "+
			"call makes a plain create fail when the update does", len(p.projUpdated))
	}

	// With something to say, it is sent.
	if err := applyProjectPostCreate(context.Background(), p, "proj-1", 3, []string{"/a"}); err != nil {
		t.Fatalf("applyProjectPostCreate: %v", err)
	}
	if len(p.projUpdated) != 1 {
		t.Fatalf("applyProjectPostCreate issued %d updates, want 1", len(p.projUpdated))
	}
	got := p.projUpdated[0]
	if got.GetMaxConcurrentRuns() != 3 {
		t.Errorf("max_concurrent_runs sent as %d, want 3", got.GetMaxConcurrentRuns())
	}
	if files := got.GetContextFiles().GetFiles(); len(files) != 1 || files[0] != "/a" {
		t.Errorf("context files sent as %v, want [/a]", got.GetContextFiles().GetFiles())
	}
	// The id must be the created project's, or the override lands on nothing.
	if got.GetId() != "proj-1" {
		t.Errorf("update targeted %q, want the created project", got.GetId())
	}
}

// THE CREATE FORM OFFERS EVERY FIELD THE GUI DOES, and the EDIT form prefills them.
func TestProjectFormsCarryTheGuiFields(t *testing.T) {
	want := []string{
		"name", "slug", "goals", "default_runtime_image",
		"git_strategy", "execution_mode", "max_concurrent_runs", "context_files",
	}
	m := newModel(t, newPlane())

	createFields := map[string]bool{}
	for _, s := range m.newProjectCreateForm().Specs {
		createFields[s.Name] = true
	}
	for _, w := range want {
		if !createFields[w] {
			t.Errorf("the CREATE form has no %q field — the GUI offers it, and the operator intends to work from "+
				"the TUI", w)
		}
	}
	// project_dir is NOT on create: the proto cannot accept it, and offering a field
	// whose value cannot be sent is worse than omitting it.
	if createFields["project_dir"] {
		t.Error("the create form offers project_dir, which CreateProject cannot carry — the value would be " +
			"silently dropped")
	}

	// The edit form carries the same set PLUS the directory, prefilled from the project.
	p := &apiv1.Project{
		Id: "proj-1", Name: "Thing", Slug: "thing", Goals: `{"owner":"ops"}`,
		ProjectDir:          "/home/me/projects/thing",
		DefaultRuntimeImage: "img:1",
		GitStrategy:         apiv1.GitStrategy_GIT_STRATEGY_PR,
		ExecutionMode:       apiv1.ExecutionMode_EXECUTION_MODE_LOCAL,
		MaxConcurrentRuns:   4,
		ContextFiles:        []string{"/home/me/notes"},
	}
	edit := m.newProjectEditForm(p)
	editFields := map[string]string{}
	for _, s := range edit.Specs {
		editFields[s.Name] = s.Initial
	}
	for _, w := range append(want, "project_dir") {
		if _, ok := editFields[w]; !ok {
			t.Errorf("the EDIT form has no %q field", w)
		}
	}
	// The prefills are the point of an edit form: a field that opens empty invites the
	// operator to overwrite a setting they cannot see.
	prefills := map[string]string{
		"name":                  "Thing",
		"slug":                  "thing",
		"goals":                 "owner=ops",
		"project_dir":           "/home/me/projects/thing",
		"default_runtime_image": "img:1",
		"git_strategy":          "pr",
		"execution_mode":        "local",
		"max_concurrent_runs":   "4",
		"context_files":         "/home/me/notes",
	}
	for field, wantVal := range prefills {
		if got := editFields[field]; got != wantVal {
			t.Errorf("edit form %q prefilled %q, want %q — saving an untouched edit must not change the project",
				field, got, wantVal)
		}
	}
}
