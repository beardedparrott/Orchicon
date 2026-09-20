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
	// Visible is whether the CONTROL PLANE could see the directory when the check
	// ran. False is not a refusal: the operator may be about to add the path to a
	// project root, or may simply not care yet. It only changes what the question
	// SAYS.
	Visible bool
	// MCPServers is the tenant's MCP entry list, carried so the create form can offer
	// the selection. Empty means the list did not load, and the field is then absent.
	MCPServers []*apiv1.MCPServer
	// Images is the runtime-image list, carried so the create form's image field can be a
	// picker. Empty means the list did not load, and the field falls back to free text.
	Images []kit2.Option
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
	// visible is whether the CONTROL PLANE can see the directory — a different
	// question from whether the operator's shell can. A project_dir outside the
	// container's mounts is recorded happily and then silently useless: the plane
	// cannot "git -C" it for worktrees (internal/scheduler/worktree_reconciler.go),
	// and in container mode validateProjectDir deliberately SKIPS the existence
	// check, so nothing else would ever say so. The prompt is the one place the
	// operator is deciding, which makes it the right place to be told.
	visible bool
	// mcpServers are the tenant's MCP entries, loaded so the launch form can offer the
	// same selection the Work screen's create does.
	mcpServers []*apiv1.MCPServer
	// images are the runtime images, loaded for the same reason: the launch form's image
	// field is a picker, so its options must exist before the form is built.
	images []kit2.Option
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
		// ASK THE PLANE WHAT IT CAN SEE, before offering to create anything for this
		// directory. ListProjectFiles with dir_path browses a raw path on the PLANE's
		// filesystem (the frontend uses the same call to browse before a project
		// exists), so an error here means the plane cannot reach this path — and that
		// is worth saying at the moment of the decision rather than discovering it
		// when a worker cannot find the files.
		visible := true
		if _, err := cl.Projects.ListProjectFiles(ctx, connect.NewRequest(&apiv1.ListProjectFilesRequest{
			DirPath: dir,
		})); err != nil {
			visible = false
		}
		// The MCP selection AND the runtime-image options ride along on this same round trip
		// rather than a second one: the launch form offers the same fields as the Work screen's
		// create, and both need a list. A failure here just means the MCP field is absent and the
		// image field falls back to free text — it must not block a prompt whose purpose is to let
		// the operator work.
		var servers []*apiv1.MCPServer
		if cl.MCP != nil {
			if list, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{})); err == nil {
				servers = list.Msg.GetServers()
			}
		}
		var images []kit2.Option
		if cl.Images != nil {
			if lr, err := cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 100})); err == nil {
				for _, img := range lr.Msg.GetRuntimeImages() {
					images = append(images, kit2.Option{Value: img.GetTag(), Label: img.GetName() + " (" + img.GetTag() + ")"})
				}
			}
		}
		return launchPromptMsg{dir: dir, need: true, visible: visible, mcpServers: servers, images: images}
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
	for _, p := range projects {
		if dirInsideProject(p.GetProjectDir(), dir) {
			return true
		}
	}
	return false
}

// dirInsideProject reports whether dir is the project directory itself or lives under it.
//
// ONE IMPLEMENTATION, TWO CALLERS: the launch prompt's "is this directory unattached?" and the rail's "which
// project is the workspace I was launched in?". They are the same question about the same paths, and a second
// copy of the boundary rule is how the two would come to disagree — the launch prompt declaring a directory
// attached while the rail could not name its project, or the reverse.
func dirInsideProject(projectDir, dir string) bool {
	want := filepath.Clean(dir)
	pd := strings.TrimSpace(projectDir)
	if pd == "" {
		return false
	}
	got := filepath.Clean(pd)
	return got == want || strings.HasPrefix(want, got+string(filepath.Separator))
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
func (m *App) beginLaunchPrompt(dir string, visible bool, mcpServers []*apiv1.MCPServer, images []kit2.Option) {
	m.launch = &launchPrompt{Dir: dir, Phase: launchAsking, Visible: visible, MCPServers: mcpServers, Images: images}
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
//
// WHEN THE PLANE CANNOT SEE THE DIRECTORY, THE QUESTION SAYS SO — here, where the
// operator is deciding, rather than after a worker has failed to find their files.
// It is a WARNING and not a refusal: the path may be about to become visible (add
// it to a project root and re-create the container), or the operator may be
// creating the project now and mounting it later. Saying nothing would make the
// create look like it worked, because it does — the project is real, its directory
// is recorded, and nothing else in the system can tell that the plane cannot reach
// it.
func (m *App) launchQuestion() string {
	dir := ""
	visible := true
	if m.launch != nil {
		dir = m.launch.Dir
		visible = m.launch.Visible
	}
	lines := []string{
		theme.MenuTitle.Render("This directory isn't linked to an Orchicon project"),
		"",
		theme.DetailValue.Render(dir),
		"",
		theme.HintText.Render("A project is what gives workers a place to operate. This directory is"),
		theme.HintText.Render("not tied to one, so a worker asked to work here would not find your files."),
	}
	if !visible {
		lines = append(lines,
			"",
			theme.HintText.Render("⚠ The control plane cannot see this directory."),
			theme.HintText.Render("A project for it would be created and then unusable: workers run elsewhere"),
			theme.HintText.Render("and the plane cannot run git in a path it has not been given."),
			theme.HintText.Render("Grant reach with a project root, then re-create the container:"),
			theme.DetailValue.Render("ORCHICON_PROJECT_ROOTS=\"$HOME\" scripts/container.sh up dev"),
		)
	}
	lines = append(lines,
		"",
		theme.HintText.Render("enter / y   create a project for this directory"),
		theme.HintText.Render("n / esc     continue into orch without one"),
	)
	return strings.Join(lines, "\n")
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
	specs := append(work.ProjectFormFields(nil, m.launch.Images), kit2.FieldSpec{
		Name:        "project_dir",
		Label:       "Directory",
		Kind:        kit2.KText,
		Initial:     m.launch.Dir,
		Placeholder: "/home/me/projects/orchicon",
	})
	// The MCP selection is appended the same way, and is absent when the server list did
	// not load — the launch form offers what the Work screen offers, but never a control
	// it cannot populate.
	if spec := work.ProjectMCPField(m.launch.MCPServers, nil); spec != nil {
		specs = append(specs, *spec)
	}
	f := kit2.NewForm("New project for this directory", specs...)
	f.Set("name", name)
	f.Set("slug", launchProjectSlug(name))
	f.OnSubmit = m.launchSubmit
	// FOCUSED, OR IT DOES NOT LOOK EDITABLE. `Focused` is render-only (kit2.Form
	// draws the ▸ cursor marker and the placeholder only when it is true), so a form
	// that omits it renders every field as a plain "Label: value" line — which reads
	// as a read-only summary of the defaults, not as something to type into. The
	// operator hit exactly that: "didn't actually give me the option to edit any of
	// the fields, just simply ctrl+s to save defaults". Typing worked the whole time;
	// there was nothing on screen to say so.
	//
	// NewForm now defaults this to true (the class fix — the three other call sites
	// had each remembered it by hand), and this assignment is kept as the local
	// statement of intent.
	f.Focused = true
	m.launch.Phase = launchForm
	m.launch.Form = f
}

// launchSubmit creates the project, attaches the directory, and ACTIVATES it.
//
// THREE CALLS, IN THIS ORDER, and the order is forced rather than chosen: the create
// API carries no project_dir, so the project has to exist before its directory can be
// set; and activation is a separate transition (its UPDATE requires status='drafting').
// That the launch path is three calls while the Work screen's create is two reflects a
// deliberate difference downstream, not sloppiness here.
//
// ACTIVATION IS THE OPERATOR'S DECISION for this path specifically: "Yes it should
// activate it by default upon saving." It is the right default HERE because the
// prompt's entire premise is "I want to work in this directory" — leaving a project
// that cannot host a single work item would recreate the exact friction the prompt was
// built to remove. The Work screen's create does NOT activate, matching the GUI, where a
// project is configured before it is made active; see the note in projects.go.
//
// A FAILED ACTIVATION IS REPORTED AS SUCH, and says the project exists: the create and
// the directory have already succeeded, so a bare failure would send the operator
// hunting for a project that is really there — and the activate action is available on
// the project row if they want to retry.
func (m *App) launchSubmit(values map[string]string, multi map[string][]string) (tea.Cmd, error) {
	cl := m.clients
	name := strings.TrimSpace(values["name"])
	slug := strings.TrimSpace(values["slug"])
	image := strings.TrimSpace(values["default_runtime_image"])
	dir := strings.TrimSpace(values["project_dir"])
	goals := work.ParseGoals(values["goals"])
	// Only written when the operator actually had the field to choose from.
	mcpChosen := multi["mcp_servers"]
	mcpLoaded := m.launch != nil && len(m.launch.MCPServers) > 0
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
		if _, err := cl.Projects.ActivateProject(ctx, connect.NewRequest(&apiv1.ActivateProjectRequest{Id: id})); err != nil {
			return launchFailedMsg{err: fmt.Errorf(
				"the project %q was created with its directory, but could not be activated: %w — activate it from "+
					"the Work screen (select it, press a)", name, err)}
		}
		if mcpLoaded && cl.MCP != nil && len(mcpChosen) > 0 {
			if _, err := cl.MCP.SetProjectMCPServers(ctx, connect.NewRequest(&apiv1.ProjectMCPServersSetRequest{
				ProjectId:    id,
				McpServerIds: mcpChosen,
			})); err != nil {
				return launchFailedMsg{err: fmt.Errorf(
					"the project %q was created and activated, but its MCP servers could not be saved: %w", name, err)}
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
