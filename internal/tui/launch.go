package tui

// launch.go — THE LAUNCH-TIME PROJECT PROMPT.

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
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
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
	// Visible is whether the CONTROL PLANE could see the directory when the check ran.
	Visible bool
	// MCPServers was REMOVED with the tenant selection: the launch form now DEFINES project-owned MCP
	// entries (work.ProjectMCPDefinitionsField) rather than selecting from the tenant list.
	Images []kit2.Option
}

// launchPromptMsg is the launch check's result.
type launchPromptMsg struct {
	dir  string
	need bool
	// visible is whether the CONTROL PLANE can see the directory — a different question from whether
	// the operator's shell can.
	visible bool
	// mcpServers was REMOVED: the launch form no longer selects from a tenant list.
	images []kit2.Option
}

// launchCreatedMsg reports a successful create-and-attach.
type launchCreatedMsg struct {
	projectID string
	name      string
}

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
			// A FAILED CHECK IS NOT A REASON TO ASK.
			return launchPromptMsg{}
		}
		if dirTiedToProject(resp.Msg.GetProjects(), dir) {
			return launchPromptMsg{}
		}
		// ASK THE PLANE WHAT IT CAN SEE, before offering to create anything for this directory.
		visible := true
		if _, err := cl.Projects.ListProjectFiles(ctx, connect.NewRequest(&apiv1.ListProjectFilesRequest{
			DirPath: dir,
		})); err != nil {
			visible = false
		}
		// The MCP selection AND the runtime-image options ride along on this same round trip rather than a
		// second one: the launch form offers the same fields as the Work screen's create.
		var images []kit2.Option
		if cl.Images != nil {
			if lr, err := cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 100})); err == nil {
				for _, img := range lr.Msg.GetRuntimeImages() {
					images = append(images, kit2.Option{Value: img.GetTag(), Label: img.GetName() + " (" + img.GetTag() + ")"})
				}
			}
		}
		return launchPromptMsg{dir: dir, need: true, visible: visible, images: images}
	}
}

// dirTiedToProject reports whether dir is covered by any project's project_dir.
func dirTiedToProject(projects []*apiv1.Project, dir string) bool {
	for _, p := range projects {
		if dirInsideProject(p.GetProjectDir(), dir) {
			return true
		}
	}
	return false
}

// dirInsideProject reports whether dir is the project directory itself or lives under it.
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

// launchProjectSlug derives a slug from a name: lowercase, with runs of anything that is not
// alphanumeric collapsed to a single dash.
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
func (m *App) beginLaunchPrompt(dir string, visible bool, images []kit2.Option) {
	m.launch = &launchPrompt{Dir: dir, Phase: launchAsking, Visible: visible, Images: images}
}

// dismissLaunchPrompt closes the prompt and continues into the app normally — the single exit for
// every way of answering no (the n key, esc on the question, and esc inside the form).
func (m *App) dismissLaunchPrompt() {
	m.launch = nil
	m.ensureLoaded(m.active)
}

// WHEN THE PLANE CANNOT SEE THE DIRECTORY, THE QUESTION SAYS SO — here, where the operator is
// deciding, rather than after a worker has failed to find their files.
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
	// The form lays itself out in the PANEL'S INTERIOR, exactly as the rename and category modals do: a
	// form told it has the panel's OUTER width overflows the border by its two cells.
	lp.Form.Width = m.modalInnerWidth()
	return fillView(m.overlayCentered(base, m.modalPanel(lp.Form.View(), m.modalWidth())), w, h)
}

// openLaunchForm builds the create form, prefilled from the launch directory.
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
	// The MCP definitions the operator seeds are appended the same way — an OWNED definition seed, not
	// a selection: under ownership the launch form DEFINES project-owned entries.
	specs = append(specs, work.ProjectMCPDefinitionsField())
	f := kit2.NewForm("New project for this directory", specs...)
	f.Set("name", name)
	f.Set("slug", launchProjectSlug(name))
	f.OnSubmit = m.launchSubmit
	// FOCUSED, OR IT DOES NOT LOOK EDITABLE.
	f.Focused = true
	m.launch.Phase = launchForm
	m.launch.Form = f
}

// launchSubmit creates the project, attaches the directory, and ACTIVATES it.
func (m *App) launchSubmit(values map[string]string, multi map[string][]string) (tea.Cmd, error) {
	cl := m.clients
	name := strings.TrimSpace(values["name"])
	slug := strings.TrimSpace(values["slug"])
	image := strings.TrimSpace(values["default_runtime_image"])
	dir := strings.TrimSpace(values["project_dir"])
	goals := work.ParseGoals(values["goals"])
	// The MCP definitions the operator seeded are OWNED rows, created after the project exists
	// (CreateProject carries no MCP field).
	mcpDefs := mcpforms.ParseInline(values["mcp_servers"])
	_ = multi
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
		// The operator's MCP definitions become project-owned rows.
		if len(mcpDefs) > 0 {
			if cl.MCP == nil {
				return launchFailedMsg{err: fmt.Errorf(
					"the project %q was created and activated, but its MCP definitions could not be saved: no MCP client", name)}
			}
			for _, spec := range mcpDefs {
				req := work.InlineSpecToCreateRequest(spec, id)
				if _, err := cl.MCP.CreateMCPServer(ctx, connect.NewRequest(req)); err != nil {
					return launchFailedMsg{err: fmt.Errorf(
						"the project %q was created and activated, but its MCP definition %q could not be saved: %w", name, spec.ID, err)}
				}
			}
		}
		return launchCreatedMsg{projectID: id, name: name}
	}, nil
}

// launchKey routes a keystroke while the prompt is up.
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
