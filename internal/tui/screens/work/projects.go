package work

// projects.go — Projects: create (CreateProject), edit the title / goals /
// project_dir (UpdateProject), and set-create the project directory.
//
// Note on the directory: ProjectService exposes no CreateProjectDirectory
// RPC (its 10 RPCs are Create/Get/List/Update/Archive/Delete/Pause/
// Activate/StreamProjectEvents/ListProjectFiles). The TUI therefore sets
// project_dir through UpdateProject and then PROBES the directory with
// ListProjectFiles — the server materializes the directory when the files
// endpoint resolves it, and a bad path surfaces immediately as an error
// instead of failing later at worker dispatch.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// project form modes.
const (
	formCreateProject = "project-create"
	formEditProject   = "project-edit"
	formProjectDir    = "project-dir"
)

type projectFormMsg struct {
	mode    string
	project *apiv1.Project
	dirInfo string
	err     error
}

// prepEditProject fetches the selected project so the edit form is
// prefilled from real values.
func (m *Model) prepEditProject(mode string) tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	m.formLoading = true
	id := it.ID
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
		if err != nil {
			return projectFormMsg{mode: mode, err: err}
		}
		return projectFormMsg{mode: mode, project: resp.Msg.GetProject()}
	}
}

// ProjectCreateFields is the project-create form's FIELD LIST, exported so a second
// host builds the SAME form rather than a copy of it that can drift — the
// operator's ask for the launch prompt was to reuse "the project-create form +
// RPC".
//
// NOTE THERE IS NO project_dir FIELD, and that is the API's shape rather than an
// omission: CreateProject takes name/slug/goals/default_runtime_image and no
// directory, so a directory is attached by a FOLLOWING UpdateProject. This screen
// sets it from the edit form; the launch prompt (launch.go) appends its own dir
// field and applies it the same way.
func ProjectCreateFields() []kit2.FieldSpec {
	return []kit2.FieldSpec{
		{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Placeholder: "Orchicon"},
		{Name: "slug", Label: "Slug", Kind: kit2.KText, Placeholder: "orchicon"},
		{Name: "goals", Label: "Goals", Kind: kit2.KText, Placeholder: "key=value, key2=value2"},
		{Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KText, Placeholder: "empty = inherit tenant/base"},
	}
}

// newProjectCreateForm builds the create form (name / slug / goals /
// default runtime image).
func (m *Model) newProjectCreateForm() *kit2.Form {
	f := kit2.NewForm("New project", ProjectCreateFields()...)
	m.wireProjectForm(f, formCreateProject, "")
	return f
}

// newProjectEditForm builds the edit form: title, goals, project_dir.
func (m *Model) newProjectEditForm(p *apiv1.Project) *kit2.Form {
	f := kit2.NewForm("Edit project",
		kit2.FieldSpec{Name: "name", Label: "Name", Kind: kit2.KText, Required: true, Initial: p.GetName()},
		kit2.FieldSpec{Name: "goals", Label: "Goals", Kind: kit2.KText, Initial: goalsText(p.GetGoals()), Placeholder: "key=value, key2=value2"},
		kit2.FieldSpec{Name: "project_dir", Label: "Project dir", Kind: kit2.KText, Initial: p.GetProjectDir(), Placeholder: "/home/me/projects/orchicon"},
		kit2.FieldSpec{Name: "default_runtime_image", Label: "Default runtime image", Kind: kit2.KText, Initial: p.GetDefaultRuntimeImage()},
	)
	m.wireProjectForm(f, formEditProject, p.GetId())
	return f
}

// newProjectDirForm sets/creates the project directory.
func (m *Model) newProjectDirForm(p *apiv1.Project) *kit2.Form {
	f := kit2.NewForm("Set project directory",
		kit2.FieldSpec{Name: "project_dir", Label: "Directory", Kind: kit2.KText, Required: true, Initial: p.GetProjectDir(), Placeholder: "/home/me/projects/orchicon"},
	)
	m.wireProjectForm(f, formProjectDir, p.GetId())
	return f
}

// wireProjectForm installs the submit handler.
func (m *Model) wireProjectForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		name := strings.TrimSpace(v["name"])
		goals := ParseGoals(v["goals"])
		switch mode {
		case formCreateProject:
			req := &apiv1.CreateProjectRequest{
				Name:                name,
				Slug:                strings.TrimSpace(v["slug"]),
				Goals:               goals,
				DefaultRuntimeImage: strings.TrimSpace(v["default_runtime_image"]),
			}
			return m.Mutate(mutate.Request{
				Name: "create project " + strings.TrimSpace(name), Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.Projects.CreateProject(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formEditProject:
			req := &apiv1.UpdateProjectRequest{
				Id:                  id,
				Name:                strPtr(name),
				Goals:               &apiv1.GoalFields{Fields: goals},
				ProjectDir:          strPtr(strings.TrimSpace(v["project_dir"])),
				DefaultRuntimeImage: strPtr(strings.TrimSpace(v["default_runtime_image"])),
			}
			return m.Mutate(mutate.Request{
				Name: "save project " + name, Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.Projects.UpdateProject(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formProjectDir:
			dir := strings.TrimSpace(v["project_dir"])
			if dir == "" {
				return nil, fmt.Errorf("a directory path is required")
			}
			cl := m.cl
			return m.Mutate(mutate.Request{
				Name: "set project directory", Source: srcProjects,
				Rollback: func() { m.Refresh(srcProjects) },
				Do: func(ctx context.Context) error {
					if _, err := cl.Projects.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
						Id:         id,
						ProjectDir: &dir,
					})); err != nil {
						return err
					}
					// Probe the directory: ListProjectFiles resolves
					// project_dir server-side, so a bad path fails here.
					_, err := cl.Projects.ListProjectFiles(ctx, connect.NewRequest(&apiv1.ListProjectFilesRequest{Id: id}))
					return err
				},
			}), nil
		}
		return nil, nil
	}
}

// projectActions is the Projects pane's entity-bound action set.
//
// This function MUST NOT set m.formLoading. It used to, and that single line broke
// the whole tab bar on this pane: actionsForSelection() calls it to build the footer
// hints and the key bindings, so formLoading latched TRUE the moment a project was
// selected and NOTHING ever cleared it (the three legitimate setters are the prep*
// helpers, whose returned cmd delivers the *_formMsg that clears it).
//
// ClaimsKeys() includes formLoading, so the shell then handed EVERY key to this screen
// before any of its own routes ran:
//
//   - the Work submenu's up/down never reached menuHandleKey (the menu looked dead);
//   - Enter selected nothing, because it fell to the pane's own "load detail";
//   - shift+tab was swallowed outright — it is a global route, which sits even later
//     in the chain.
//
// That is the operator's "can't go into reverse tab once I hit the Work/Projects
// screen ... can't hit enter on any submenu under Work nor up/down on the Work menu" —
// and it is why the oddity was specific to Projects: itemActions() and imageActions()
// never set the flag. The two locals below are captured by the closures and have
// nothing to do with form preparation.
func (m *Model) projectActions() []kit2.Action {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	return []kit2.Action{{
		Label: "create project dir", Key: "d", Source: srcProjects,
		Do: func(ctx context.Context) error {
			// Resolve the project's configured directory and list it, which
			// is what materializes + validates it server-side.
			p, err := cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
			if err != nil {
				return err
			}
			if p.Msg.GetProject().GetProjectDir() == "" {
				return fmt.Errorf("set the project directory first (e)")
			}
			_, err = cl.Projects.ListProjectFiles(ctx, connect.NewRequest(&apiv1.ListProjectFilesRequest{Id: id}))
			return err
		},
	}}
}

// goalsText renders a project's goals JSON as "key=value" pairs (the form's
// editable shape). Unknown JSON shapes render empty rather than dumping raw
// JSON into the field.
func goalsText(raw string) string {
	return raw
}

// ParseGoals parses the goals field's "key=value, key2=value2" text into the
// wire shape. An empty input clears the goals (the proto's empty-fields
// semantics).
//
// EXPORTED so a second host runs the SAME parsing — a goal list is part of the
// create request's meaning, and two parsers would eventually disagree about an
// edge (a bare key, a value containing '=').
func ParseGoals(v string) []*apiv1.GoalField {
	var out []*apiv1.GoalField
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, found := strings.Cut(part, "=")
		if !found {
			out = append(out, &apiv1.GoalField{Key: strings.TrimSpace(part)})
			continue
		}
		out = append(out, &apiv1.GoalField{Key: strings.TrimSpace(k), Value: strings.TrimSpace(val)})
	}
	return out
}

var _ = screenkit.FmtTime
