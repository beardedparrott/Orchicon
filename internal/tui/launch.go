package tui

// launch.go — THE LAUNCH-TIME PROJECT PROMPT.
//
// WHY IT EXISTS. `orch` opens on the Ask "New" page — nav.go calls it "the launch
// page, exactly what orch shows on start" — which is right for someone already
// working inside a project and wrong for someone who has just cd'd into a
// directory that no project points at. They land on a blank composer with nothing
// saying that the directory they are sitting in is unattached, and the first
// symptom is a worker that cannot find their files. The operator hit exactly this:
// "I then launched orch in a different directory and it went straight to the 'new'
// screen. I never got a pop-up to create a new project."
//
// SCOPE (operator decision): "Whenever someone launches orch, if the cwd is not
// tied to any current projects created." Not first-run-only — every launch, in any
// directory no project covers.
//
// DECLINING IS FREE AND IS NOT REMEMBERED (operator decision): "No it just goes
// into orch then and performs normally." So there is no state, no ledger, no
// don't-ask-again flag. The next launch in the same directory asks again, because a
// client that silently remembered a "no" would be hiding the question from the one
// person who might now want a different answer.
//
// THE TWO STEPS ARE ORDERED (operator decision): a blank screen carrying the
// QUESTION on its own first, then a CENTRED MODAL reusing the project-create form.
// The question is deliberately not a modal, because it is not interrupting
// anything — it is the entire first screen.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// launchPhase is where the prompt is in its two-step sequence.
type launchPhase int

const (
	// launchAsking is the blank screen carrying the question, alone.
	launchAsking launchPhase = iota
	// launchForm is the centred create form.
	launchForm
)

// launchPrompt is the whole state of the launch-time project prompt.
type launchPrompt struct {
	// Dir is the directory orch was launched from, cleaned and absolute: what the
	// question is about, and what a created project's project_dir is set to.
	Dir string
	// Phase is which step is showing.
	Phase launchPhase
	// Form is the create form, built when the operator says yes — built once, so
	// their typing survives every repaint.
	Form *kit2.Form
}

// launchPromptMsg is the launch check's result.
//
// need=false means "say nothing", and it covers TWO cases on purpose: the
// directory is already covered by a project, and the check could not be completed.
// The second is silent by design — a plane we cannot reach must never become a
// question the operator cannot answer, or a network blip turns into a modal
// standing between them and their work.
type launchPromptMsg struct {
	dir  string
	need bool
}

// launchCreatedMsg reports a successful create-and-attach.
type launchCreatedMsg struct{ projectID string }

// launchFailedMsg reports a create that did not go through, carrying the reason to
// show in the form.
type launchFailedMsg struct{ err error }

// checkLaunchProject answers "is this directory unattached?", off the UI thread.
func (m *App) checkLaunchProject() tea.Cmd {
	dir := m.launchDir
	cl := m.clients
	return func() tea.Msg {
		if dir == "" || cl == nil || cl.Projects == nil {
			return launchPromptMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{
			PageSize: 200,
		}))
		if err != nil {
			// A FAILED CHECK IS NOT A REASON TO ASK. The prompt answers a question
			// about the world; if we cannot see the world, stay quiet and let the
			// operator work.
			return launchPromptMsg{}
		}
		if dirTiedToProject(resp.Msg.GetProjects(), dir) {
			return launchPromptMsg{}
		}
		return launchPromptMsg{dir: dir, need: true}
	}
}

// dirTiedToProject reports whether dir is covered by any project's project_dir.
//
// BOTH an exact match AND CONTAINMENT count, because "tied to" is not "equal to":
// launching from inside your project — a monorepo package, a docs/ subdirectory —
// is the ordinary case, and prompting there would be wrong. The directory IS tied
// to a project; it is just not its root.
//
// The containment boundary is a PATH SEPARATOR, not a string prefix:
// /home/me/project-notes is NOT inside /home/me/project, and a bare HasPrefix test
// says it is.
func dirTiedToProject(projects []*apiv1.Project, dir string) bool {
	want := filepath.Clean(dir)
	for _, p := range projects {
		pd := strings.TrimSpace(p.GetProjectDir())
		if pd == "" {
			continue
		}
		got := filepath.Clean(pd)
		if got == want || strings.HasPrefix(want, got+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// launchProjectName suggests a project name: the directory's own base name.
func launchProjectName(dir string) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	return base
}

// launchProjectSlug derives a slug from a name: lowercase, with runs of anything
// that is not alphanumeric collapsed to a single dash.
//
// It is only a PREFILL — the plane normalizes the slug it is given
// (project.normalizeSlug) and remains the authority.
func launchProjectSlug(name string) string {
	var b strings.Builder
	dashed := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dashed = false
			continue
		}
		if !dashed && b.Len() > 0 {
			b.WriteByte('-')
			dashed = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// beginLaunchPrompt raises the prompt: the blank screen carrying the question.
func (m *App) beginLaunchPrompt(dir string) {
	m.launch = &launchPrompt{Dir: dir, Phase: launchAsking}
}

// dismissLaunchPrompt closes the prompt and continues into the app normally — the
// single exit for every way of answering no (the n key, esc on the question, and
// esc inside the form).
//
// It deliberately does NOT "remember" the refusal anywhere; see the header.
func (m *App) dismissLaunchPrompt() {
	m.launch = nil
	m.ensureLoaded(m.active)
}

// launchQuestion is the first screen: the question, alone, with the directory it
// is about and the two answers.
func (m *App) launchQuestion() string {
	dir := ""
	if m.launch != nil {
		dir = m.launch.Dir
	}
	return strings.Join([]string{
		theme.MenuTitle.Render("This directory isn't linked to an Orchicon project"),
		"",
		theme.DetailValue.Render(dir),
		"",
		theme.HintText.Render("A project is what gives workers a place to operate. This directory is"),
		theme.HintText.Render("not tied to one, so a worker asked to work here would not find your files."),
		"",
		theme.HintText.Render("enter / y   create a project for this directory"),
		theme.HintText.Render("n / esc     continue into orch without one"),
	}, "\n")
}

// launchView paints the prompt.
//
// It REPLACES the whole frame rather than layering over the shell, so nothing
// behind it can read as "the app already started" — the operator's first screen is
// the question and nothing else. The form step keeps that same blank backing and
// centres the create modal on it.
func (m App) launchView(w, h int) string {
	lp := m.launch
	base := fillView("", w, h)
	if lp == nil {
		return base
	}
	if lp.Phase == launchAsking {
		return fillView(m.overlayCentered(base, m.launchQuestion()), w, h)
	}
	if lp.Form == nil {
		return base
	}
	// The form lays itself out in the PANEL'S INTERIOR, exactly as the rename and
	// category modals do: a form told it has the panel's OUTER width overflows the
	// border by its two cells, and the panel then truncates the field's right edge.
	lp.Form.Width = m.modalInnerWidth()
	return fillView(m.overlayCentered(base, m.modalPanel(lp.Form.View(), m.modalWidth())), w, h)
}

// openLaunchForm builds the create form, prefilled from the launch directory.
//
// IT REUSES THE WORK SCREEN'S FIELD LIST (work.ProjectCreateFields) rather than
// copying it, so the two create forms cannot drift apart — the operator's ask was
// to reuse "the project-create form + RPC".
//
// IT APPENDS A DIRECTORY FIELD, which the shared list does not have, because
// CreateProject takes no project_dir. The directory is therefore attached by the
// UpdateProject that follows the create, the same two-call shape the Work screen
// uses (work/projects.go). The field is editable and prefilled with the launch
// directory, per the operator's decision: "Yes prefill from cwd."
func (m *App) openLaunchForm() {
	if m.launch == nil {
		return
	}
	name := launchProjectName(m.launch.Dir)
	specs := append(work.ProjectCreateFields(), kit2.FieldSpec{
		Name:        "project_dir",
		Label:       "Directory",
		Kind:        kit2.KText,
		Initial:     m.launch.Dir,
		Placeholder: "/home/me/projects/orchicon",
	})
	f := kit2.NewForm("New project for this directory", specs...)
	f.Set("name", name)
	f.Set("slug", launchProjectSlug(name))
	f.OnSubmit = m.launchSubmit
	m.launch.Phase = launchForm
	m.launch.Form = f
}

// launchSubmit creates the project and then attaches the directory.
//
// TWO CALLS, IN THIS ORDER, and the order is forced rather than chosen: the create
// API carries no project_dir, so the project has to exist before its directory can
// be set. If the second call fails the project is left created-but-unattached, and
// the error says so plainly rather than reporting a bare failure that would send
// the operator looking for a project that does exist.
//
// The form's own validation runs BEFORE this (kit2.Form.Submit checks Required),
// so an empty name never reaches the plane.
func (m *App) launchSubmit(values map[string]string, _ map[string][]string) (tea.Cmd, error) {
	cl := m.clients
	name := strings.TrimSpace(values["name"])
	slug := strings.TrimSpace(values["slug"])
	image := strings.TrimSpace(values["default_runtime_image"])
	dir := strings.TrimSpace(values["project_dir"])
	goals := work.ParseGoals(values["goals"])
	return func() tea.Msg {
		if cl == nil || cl.Projects == nil {
			return launchFailedMsg{err: errors.New("not connected to a plane")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		created, err := cl.Projects.CreateProject(ctx, connect.NewRequest(&apiv1.CreateProjectRequest{
			Name:                name,
			Slug:                slug,
			Goals:               goals,
			DefaultRuntimeImage: image,
		}))
		if err != nil {
			return launchFailedMsg{err: err}
		}
		id := created.Msg.GetProject().GetId()
		if dir != "" {
			if _, err := cl.Projects.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
				Id:         id,
				ProjectDir: &dir,
			})); err != nil {
				return launchFailedMsg{err: fmt.Errorf(
					"the project %q was created, but its directory could not be set: %w", name, err)}
			}
		}
		return launchCreatedMsg{projectID: id}
	}, nil
}

// launchKey routes a keystroke while the prompt is up.
//
// The prompt owns EVERY key (see router.go) so no chord can reach the shell or a
// screen behind it — a keystroke answering the question must not also have done
// something else.
func (m *App) launchKey(k tea.KeyMsg) (*App, tea.Cmd) {
	lp := m.launch
	if lp == nil {
		return m, nil
	}
	if lp.Phase == launchAsking {
		switch k.String() {
		case "y", "Y", "enter":
			m.openLaunchForm()
			return m, nil
		case "n", "N", "esc", "q":
			m.dismissLaunchPrompt()
			return m, nil
		}
		// The question has exactly two answers. Anything else is inert rather than
		// falling through, so a stray key cannot leave the operator half-answered.
		return m, nil
	}
	if lp.Form == nil {
		return m, nil
	}
	cmd, handled := lp.Form.HandleKey(k)
	if !handled {
		// A form's esc means "the caller closes the form" (kit2.Form.HandleKey) — here
		// that is declining, with the same consequence as answering no.
		m.dismissLaunchPrompt()
		return m, nil
	}
	return m, cmd
}
