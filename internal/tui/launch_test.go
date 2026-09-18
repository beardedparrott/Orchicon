package tui

// launch_test.go — the launch-time project prompt.
//
// The operator, after rebuilding: "I then launched orch in a different directory
// and it went straight to the 'new' screen. I never got a pop-up to create a new
// project." Nothing was broken — the prompt did not exist. These pin what was
// built, and in particular the three decisions that are easy to undo by accident:
// WHEN it asks, that declining is remembered NOWHERE, and that the directory it
// creates is attached by the second call because the create API has no field for it.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// THE QUESTION IS ASKED WHEN THE DIRECTORY IS UNATTACHED, AND NOT OTHERWISE.
//
// The boundary case is the reason this is a table: /home/me/project-notes is NOT
// inside /home/me/project, and a string-prefix test says it is. Getting that wrong
// silently suppresses the prompt for a whole class of directories whose names
// merely start the same way.
func TestDirTiedToProject(t *testing.T) {
	projects := []*apiv1.Project{
		{Id: "p1", ProjectDir: "/home/me/projects/orchicon"},
		{Id: "p2", ProjectDir: "/srv/work" + string('/')}, // trailing separator
		{Id: "p3", ProjectDir: ""},                        // no dir: covers nothing
	}
	cases := []struct {
		dir  string
		want bool
		why  string
	}{
		{"/home/me/projects/orchicon", true, "the project's own directory"},
		{"/home/me/projects/orchicon/", true, "a trailing slash is the same directory"},
		{"/home/me/projects/orchicon/internal/tui", true,
			"INSIDE the project — launching from a subdirectory is the ordinary case and must not be asked about"},
		{"/home/me/projects/orchicon-notes", false,
			"a SEPARATE directory whose name shares a prefix — the reason the boundary is a path separator"},
		{"/home/me/projects", false, "the PARENT is not tied to the project inside it"},
		{"/tmp/scratch", false, "unrelated"},
		{"/srv/work/sub", true, "inside a project_dir with a trailing separator"},
		{"/srv/workshop", false, "prefix-sharing sibling of /srv/work again"},
	}
	for _, c := range cases {
		if got := dirTiedToProject(projects, c.dir); got != c.want {
			t.Errorf("dirTiedToProject(%q) = %v, want %v — %s", c.dir, got, c.want, c.why)
		}
	}
}

// AN EMPTY PROJECT SET MEANS EVERY DIRECTORY IS UNATTACHED, and a project with no
// directory configured covers nothing (there is no path to be tied to).
func TestDirTiedToProjectWithNoProjects(t *testing.T) {
	if dirTiedToProject(nil, "/tmp/anything") {
		t.Error("dirTiedToProject reported a tie with no projects at all")
	}
	if dirTiedToProject([]*apiv1.Project{{Id: "p", ProjectDir: ""}}, "/tmp/anything") {
		t.Error("a project with no project_dir covers nothing, but it was treated as covering this directory")
	}
	// Whitespace-only is the same as empty — the create path trims, so a dir of
	// spaces is not a path.
	if dirTiedToProject([]*apiv1.Project{{Id: "p", ProjectDir: "   "}}, "/tmp/anything") {
		t.Error("a whitespace-only project_dir was treated as a real path")
	}
}

// THE NAME AND SLUG PREFILLS, derived from the directory.
func TestLaunchPrefillsFromTheDirectory(t *testing.T) {
	if got := launchProjectName("/home/me/projects/my-thing"); got != "my-thing" {
		t.Errorf("launchProjectName = %q, want %q", got, "my-thing")
	}
	if got := launchProjectName("/home/me/projects/my-thing/"); got != "my-thing" {
		t.Errorf("a trailing slash changed the name: %q", got)
	}
	// The slug is the name lowercased with runs of punctuation collapsed, and it is
	// only a PREFILL — the plane normalizes what it is given.
	cases := map[string]string{
		"My Thing":      "my-thing",
		"orchicon":      "orchicon",
		"Foo__Bar":      "foo-bar",
		"  spaced  ":    "spaced",
		"dots.and.dash": "dots-and-dash",
		"---":           "",
	}
	for in, want := range cases {
		if got := launchProjectSlug(in); got != want {
			t.Errorf("launchProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE PROMPT RUNS ITS TWO STEPS IN ORDER: the question alone, then the form.
//
// The operator asked for this explicitly — a blank screen carrying the question
// first, and the create form as a CENTRED MODAL after it. A single-step version
// would be a different feature, so the ordering is asserted rather than assumed.
func TestThePromptAsksBeforeItShowsTheForm(t *testing.T) {
	m := newTestApp()
	m.launchDir = "/tmp/scratch"
	m.beginLaunchPrompt("/tmp/scratch", true, nil)

	if m.launch == nil {
		t.Fatal("beginLaunchPrompt did not raise the prompt")
	}
	if m.launch.Phase != launchAsking {
		t.Fatalf("the prompt opened in phase %v, want the question first", m.launch.Phase)
	}
	// THE QUESTION SCREEN IS BLANK APART FROM THE QUESTION: it must not be the shell
	// with a box on top, or the first thing the operator sees is an app that looks
	// like it has already started.
	asking := m.launchView(100, 30)
	if strings.Contains(asking, "Ask Orchicon") {
		t.Error("the question screen still shows the shell's tab bar, so it reads as an app that already " +
			"started rather than as the question")
	}
	if !strings.Contains(asking, "/tmp/scratch") {
		t.Error("the question screen does not name the directory it is asking about, so the operator cannot " +
			"tell which directory is unattached")
	}

	// Saying yes moves to the form, and only then.
	next, _ := m.launchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if next.launch == nil || next.launch.Phase != launchForm {
		t.Fatal("answering yes did not open the create form")
	}
	form := next.launchView(100, 30)
	if !strings.Contains(form, "New project") {
		t.Error("the form step does not render the create form")
	}
	if !strings.Contains(form, "/tmp/scratch") {
		t.Error("the form does not show the prefilled directory, so the prefill the operator asked for is not " +
			"reaching the screen")
	}
}

// DECLINING IS NOT REMEMBERED — ANYWHERE.
//
// The operator: "No it just goes into orch then and performs normally." This asserts
// the second half of that as an ABSENCE, because the tempting "improvement" is to
// write the refusal somewhere so the prompt stops asking — and that would hide the
// question from the one person who might now want a different answer.
func TestDecliningLeavesNoRecordAndNoPrompt(t *testing.T) {
	m := newTestApp()
	m.beginLaunchPrompt("/tmp/scratch", true, nil)

	next, _ := m.launchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if next.launch != nil {
		t.Fatal("declining left the prompt up")
	}
	// The frame is the NORMAL shell again: no launch screen, and no question text.
	frame := next.viewFrame()
	if strings.Contains(frame, "isn't linked to an Orchicon project") {
		t.Error("the question is still painted after declining")
	}
	if strings.Contains(frame, "/tmp/scratch") {
		t.Error("the launch directory is still painted after declining")
	}
}

// ESC ON THE QUESTION AND ESC IN THE FORM BOTH DECLINE.
//
// esc reaches the form's HandleKey, which reports handled=false to mean "the caller
// closes this" — the prompt has to translate that into the same exit as answering
// no, or esc would dead-end on a screen with no way forward.
func TestEscDeclinesFromBothSteps(t *testing.T) {
	// From the question.
	m := newTestApp()
	m.beginLaunchPrompt("/tmp/scratch", true, nil)
	next, _ := m.launchKey(tea.KeyMsg{Type: tea.KeyEsc})
	if next.launch != nil {
		t.Error("esc on the question did not decline")
	}

	// From the form.
	m2 := newTestApp()
	m2.beginLaunchPrompt("/tmp/scratch", true, nil)
	withForm, _ := m2.launchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if withForm.launch == nil || withForm.launch.Form == nil {
		t.Fatal("fixture: the form did not open")
	}
	afterEsc, _ := withForm.launchKey(tea.KeyMsg{Type: tea.KeyEsc})
	if afterEsc.launch != nil {
		t.Error("esc in the form did not close the prompt, so there is no way out of the form")
	}
}

// THE CREATE FORM CARRIES THE SHARED FIELDS PLUS A DIRECTORY.
//
// The directory field exists on THIS form and not on the Work screen's create form,
// and the asymmetry is the API's: CreateProject takes no project_dir, so the
// directory is attached by the UpdateProject that follows. A launch form without the
// field could not do that at all.
func TestTheLaunchFormCarriesTheDirectoryField(t *testing.T) {
	m := newTestApp()
	m.beginLaunchPrompt("/home/me/projects/thing", true, nil)
	m.openLaunchForm()
	if m.launch.Form == nil {
		t.Fatal("openLaunchForm did not build a form")
	}
	names := map[string]bool{}
	for _, s := range m.launch.Form.Specs {
		names[s.Name] = true
	}
	for _, want := range []string{"name", "slug", "goals", "default_runtime_image", "project_dir"} {
		if !names[want] {
			t.Errorf("the launch form has no %q field — it must reuse the shared create fields AND add the "+
				"directory, which the create API cannot accept", want)
		}
	}
	// The prefills the operator asked for: the directory from the cwd, and a name
	// derived from it.
	if got := m.launch.Form.Values["project_dir"]; got != "/home/me/projects/thing" {
		t.Errorf("project_dir prefill = %q, want the launch directory (the operator: \"Yes prefill from cwd\")", got)
	}
	if got := m.launch.Form.Values["name"]; got != "thing" {
		t.Errorf("name prefill = %q, want %q", got, "thing")
	}
	if got := m.launch.Form.Values["slug"]; got != "thing" {
		t.Errorf("slug prefill = %q, want %q", got, "thing")
	}
}

// THE CHECK FAILS SILENTLY, so a plane that cannot be listed never becomes a
// question standing between the operator and their work.
func TestTheCheckSaysNothingWithoutAClient(t *testing.T) {
	m := newTestApp()
	m.launchDir = "/tmp/scratch"
	// newTestApp builds with an empty client set, so the RPC cannot be made.
	msg := m.checkLaunchProject()()
	lm, ok := msg.(launchPromptMsg)
	if !ok {
		t.Fatalf("the check returned %T, want a launchPromptMsg", msg)
	}
	if lm.need {
		t.Error("the check asked for the prompt without being able to list projects — a failure to look must " +
			"never turn into a question the operator cannot answer")
	}
}

// NOTHING IS ASKED WHEN THERE IS NO LAUNCH DIRECTORY, which is what keeps the prompt
// opt-in for every existing caller (tests, embedders, and any future host).
func TestNoLaunchDirMeansNoPrompt(t *testing.T) {
	m := newTestApp()
	if m.launchDir != "" {
		t.Fatalf("fixture: newTestApp set a launch dir (%q) — the prompt would fire in every test", m.launchDir)
	}
	if lm := m.checkLaunchProject()().(launchPromptMsg); lm.need {
		t.Error("a prompt was requested with no launch directory")
	}
}

// THE WARNING APPEARS WHEN THE PLANE CANNOT SEE THE DIRECTORY, AND NOT OTHERWISE.
func TestTheQuestionWarnsWhenThePlaneCannotSeeTheDirectory(t *testing.T) {
	// The plane CAN see it: no warning, and nothing about mounting.
	ok := newTestApp()
	ok.beginLaunchPrompt("/tmp/scratch", true, nil)
	if got := ok.launchView(100, 30); strings.Contains(got, "cannot see") {
		t.Error("the question warns that the plane cannot see a directory it CAN see")
	}

	// The plane CANNOT see it: say so, and say what to do about it.
	blind := newTestApp()
	blind.beginLaunchPrompt("/mnt/elsewhere/thing", false, nil)
	got := blind.launchView(100, 30)
	if !strings.Contains(got, "cannot see") {
		t.Error("the question does not warn that the control plane cannot see the directory — the project " +
			"would be created looking perfectly successful and then be unusable, because in container mode " +
			"validateProjectDir deliberately skips the existence check and nothing else would ever say so")
	}
	// And it must remain ASKABLE — a warning, not a refusal: the operator may be
	// about to grant reach, or may want the project now and the mount later.
	if !strings.Contains(got, "create a project for this directory") {
		t.Error("the warning replaced the question, so the operator cannot proceed")
	}
	if !strings.Contains(got, "ORCHICON_PROJECT_ROOTS") {
		t.Error("the warning does not name the remedy, so the operator is told about a problem they cannot fix")
	}
}
